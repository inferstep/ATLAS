"""A candidate the Lens could not score is unscored, never neutral.

One candidate in the r4 acquisition was 2,055 tokens against a 2,048-token
physical batch. llama-server refused the embedding with HTTP 500, the Lens
answered with its error defaults (energy 0.0, normalized 0.5, gx 0.5), and
the candidate entered selection carrying the LOWEST energy in the pool: the
min-energy selector reads 0.0 as the best code it has ever seen.

The Lens now reports that as a typed failure. This file pins what V3 does
with it: the candidate keeps its identity and its sandbox result, carries
the failure on its record, is ranked after every scored candidate, and can
only be delivered as the last verified candidate standing, saying so.
"""

import base64
import io
import json
import sys
import urllib.error
from pathlib import Path
from types import SimpleNamespace

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402
import pipeline as v3pipeline  # noqa: E402
import scoring  # noqa: E402
from stages.cxgx_gate import FLOOR_TIER, K_FLOOR  # noqa: E402

# Candidate bytes. The marker decides what the fake Lens answers.
SCORED_A = "def a():  # ENERGY=3.0\n    return 1\n"
SCORED_B = "def b():  # ENERGY=7.0\n    return 2\n"
LONG = "def long_one():  # TOO-LONG\n    return 3\n"

CAPACITY_FAILURE = {
    "kind": "embed_capacity", "input_tokens": 2055, "capacity_tokens": 2048,
    "detail": "input (2055 tokens) is too large to process. increase the "
              "physical batch size (current batch size: 2048)",
}


def _energy_of(text):
    marker = "# ENERGY="
    if marker in text:
        return float(text.split(marker, 1)[1].split("\n", 1)[0])
    return 5.0


def _gx_score_payload(text, shape):
    if "TOO-LONG" in text:
        if shape == "typed":
            return {"enabled": True, "scored": False, "cx_energy": None,
                    "cx_normalized": None, "cx_calibrated": False,
                    "gx_score": None, "gx_available": False,
                    "verdict": "unscored", "failure": dict(CAPACITY_FAILURE),
                    "error": "EmbeddingCapacityError: combined evaluation failed"}
        # The shape a Lens without the typed boundary answers with.
        return {"cx_energy": 0.0, "cx_normalized": 0.5, "cx_calibrated": False,
                "gx_score": 0.5, "verdict": "error", "enabled": True,
                "gx_available": False,
                "error": "HTTPError: combined evaluation failed (see service log)"}
    e = _energy_of(text)
    return {"enabled": True, "scored": True, "cx_energy": e,
            "cx_normalized": e / 10.0, "cx_calibrated": True,
            "gx_score": 0.8, "gx_available": True, "verdict": "likely_correct",
            "thresholds": {"off_rails": 0.3, "low": 0.4, "severe": 0.2,
                           "severe_mean": 0.5}}


def _per_step_payload(text, shape):
    if "TOO-LONG" in text:
        if shape == "typed":
            return {"enabled": True, "scored": False, "gx_available": False,
                    "per_step": [], "aggregate": {}, "n_tokens": 0,
                    "failure": dict(CAPACITY_FAILURE),
                    "error": "EmbeddingCapacityError: per-step evaluation failed"}
        return {"enabled": True, "gx_available": False, "per_step": [],
                "aggregate": {}, "n_tokens": 0,
                "error": "HTTPError: per-step evaluation failed (see service log)"}
    return {"enabled": True, "scored": True, "gx_available": True,
            "n_tokens": 12, "latency_ms": 1.0,
            "thresholds": {"off_rails": 0.3, "low": 0.4, "severe": 0.2,
                           "severe_mean": 0.5},
            "aggregate": {"first_off_rails_idx": -1, "gx_score_min": 0.6,
                          "gx_score_mean": 0.7, "cx_norm_max": 0.4,
                          "cx_norm_mean": 0.3}}


class _Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def _install_fake_lens(monkeypatch, shape="typed", per_step_shape=None):
    """Answer V3's two Lens calls from the request body, by candidate."""
    per_step_shape = per_step_shape or shape

    def fake_urlopen(req, timeout=None):
        text = json.loads(req.data.decode())["text"]
        url = req.full_url
        if url.endswith("/internal/lens/gx-score"):
            payload = _gx_score_payload(text, shape)
        elif url.endswith("/internal/lens/score-per-step"):
            payload = _per_step_payload(text, per_step_shape)
        else:
            raise AssertionError(f"unexpected lens call {url}")
        return _Response(json.dumps(payload).encode())
    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake_urlopen)


# --- the client --------------------------------------------------------------------------

def test_typed_capacity_failure_yields_no_energy(monkeypatch):
    _install_fake_lens(monkeypatch)
    out = scoring.score_candidate_combined(LONG)
    assert out["cx_energy"] is None and out["cx_normalized"] is None
    assert out["gx_score"] is None and out["gx_available"] is False
    assert out["cx_calibrated"] is False
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == "embed_capacity"
    assert out["failure"]["input_tokens"] == 2055
    assert out["failure"]["capacity_tokens"] == 2048
    assert scoring.score_candidate(LONG) == (None, None, False)


def test_a_lens_error_answer_without_the_typed_fields_is_still_unscored(monkeypatch):
    """`verdict: "error"` with numbers attached is not a score either."""
    _install_fake_lens(monkeypatch, shape="untyped")
    out = scoring.score_candidate_combined(LONG)
    assert out["cx_energy"] is None
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == "lens_error"


def test_per_step_typed_failure_is_reported_not_dropped(monkeypatch):
    _install_fake_lens(monkeypatch)
    out = scoring.score_candidate_per_step(LONG)
    assert "n_tokens" not in out
    assert out["failure"]["kind"] == "embed_capacity"
    assert out["failure"]["input_tokens"] == 2055


def test_per_step_untyped_error_answer_is_unscored(monkeypatch):
    _install_fake_lens(monkeypatch, shape="untyped")
    out = scoring.score_candidate_per_step(LONG)
    assert "n_tokens" not in out
    assert out["failure"]["kind"] == "lens_error"


# The lens is required (docs/adr/0011-the-lens-is-required.md). A lens that
# cannot score stops the run with the reason; it never answers neutral
# scores and is never an unscored candidate. An input the lens declines is.

def test_a_lens_http_error_means_the_lens_is_unavailable(monkeypatch):
    def raise_500(req, timeout=None):
        raise urllib.error.HTTPError(req.full_url, 500, "boom", {}, io.BytesIO(b"{}"))
    monkeypatch.setattr(scoring.urllib.request, "urlopen", raise_500)
    with pytest.raises(scoring.LensUnavailable) as err:
        scoring.score_candidate_combined(SCORED_A)
    assert "500" in err.value.reason
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_per_step(SCORED_A)


def test_an_unreachable_lens_is_unavailable(monkeypatch):
    def refuse(req, timeout=None):
        raise urllib.error.URLError("connection refused")
    monkeypatch.setattr(scoring.urllib.request, "urlopen", refuse)
    with pytest.raises(scoring.LensUnavailable) as err:
        scoring.score_candidate_combined(SCORED_A)
    assert "lens_unreachable" in err.value.reason
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_per_step(SCORED_A)


def test_a_disabled_lens_is_unavailable_not_neutral(monkeypatch):
    def disabled(req, timeout=None):
        return _Response(json.dumps({"enabled": False}).encode())
    monkeypatch.setattr(scoring.urllib.request, "urlopen", disabled)
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_combined(SCORED_A)
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_per_step(SCORED_A)


@pytest.mark.parametrize("kind", sorted(scoring.LENS_DOWN_KINDS))
def test_a_model_server_fault_is_the_lens_being_unavailable(monkeypatch, kind):
    """The lens names why it could not score. A fault of the lens or of its
    model server stops the run; only a declined input is an unscored
    candidate (the embed_capacity tests above)."""
    answer = {"enabled": True, "scored": False,
              "failure": {"kind": kind, "detail": "down"}}

    def reply(req, timeout=None):
        return _Response(json.dumps(answer).encode())
    monkeypatch.setattr(scoring.urllib.request, "urlopen", reply)
    with pytest.raises(scoring.LensUnavailable) as err:
        scoring.score_candidate_combined(SCORED_A)
    assert kind in err.value.reason
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_per_step(SCORED_A)


# --- the pipeline -------------------------------------------------------------------------

class FakeLLM:
    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return "<think>still thinking", 3, 1.0


class ProbeLLM(FakeLLM):
    """The probe itself is the long candidate."""

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return f"```python\n{LONG}```", 3, 1.0


class PassingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return True, "ok", ""


class FakeEmbed:
    def __call__(self, text):
        return []


def _service(monkeypatch, plan_codes, llm_cls=FakeLLM):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "3600")
    monkeypatch.setattr(adapters, "LLMAdapter", llm_cls)
    monkeypatch.setattr(adapters, "SandboxAdapter", PassingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    service = v3pipeline.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            SimpleNamespace(test_cases=[], generation_tokens=0))
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=list(plan_codes), total_tokens=0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service


def _events(result, stage):
    return [e for e in result["events"] if e["stage"] == stage]


def test_an_unscored_candidate_never_beats_a_scored_one(monkeypatch):
    """Two sandbox-passing candidates, one unscored: the scored one wins."""
    _install_fake_lens(monkeypatch)
    service = _service(monkeypatch, [LONG, SCORED_A])
    result = service.run("sum two ints", task_id="unscored-1")

    assert result["passed"] is True
    assert result["code"] == SCORED_A
    selected = _events(result, "selected")[0]["data"]
    assert selected["energy"] == 3.0
    assert selected["lens_scored"] is True
    assert result["winning_score"] == 0.3

    unscored = _events(result, "lens_unscored")
    assert len(unscored) == 1
    data = unscored[0]["data"]
    assert data["kind"] == "embed_capacity"
    assert data["input_tokens"] == 2055
    assert data["capacity_tokens"] == 2048


def test_the_r4_lens_answer_shape_no_longer_wins_selection(monkeypatch):
    """The exact defaults the r4 Lens answered with, through the real client."""
    _install_fake_lens(monkeypatch, shape="untyped")
    service = _service(monkeypatch, [LONG, SCORED_A, SCORED_B])
    result = service.run("sum two ints", task_id="unscored-r4")

    assert result["code"] == SCORED_A
    assert _events(result, "selected")[0]["data"]["energy"] == 3.0


def test_unscored_candidates_rank_after_every_scored_candidate(monkeypatch):
    _install_fake_lens(monkeypatch)
    service = _service(monkeypatch, [LONG, SCORED_B, SCORED_A])
    result = service.run("sum two ints", task_id="unscored-order")

    order = [e["data"]["index"] for e in _events(result, "sandbox_pass")]
    # Generated in order LONG=0, B=1, A=2; verified easiest-first with the
    # unscored candidate last.
    assert order == [2, 1, 0]
    assert result["code"] == SCORED_A


def test_the_only_verified_candidate_is_delivered_and_says_it_is_unscored(monkeypatch):
    _install_fake_lens(monkeypatch)
    service = _service(monkeypatch, [LONG])
    result = service.run("sum two ints", task_id="unscored-only")

    assert result["passed"] is True
    assert result["code"] == LONG
    selected = _events(result, "selected")[0]["data"]
    assert selected["lens_scored"] is False
    assert selected["energy"] is None
    assert selected["lens_failure"]["kind"] == "embed_capacity"
    assert result["winning_score"] is None


def test_an_unscored_probe_allocates_the_floor_and_says_why(monkeypatch):
    _install_fake_lens(monkeypatch)
    # The probe is LONG (unscored) and passes; early return is off in this
    # test because the sandbox suite is empty, so allocation must run.
    monkeypatch.setattr(PassingSandbox, "__call__",
                        lambda self, code, test_input="", **_: (False, "", "boom"))
    service = _service(monkeypatch, [SCORED_A], llm_cls=ProbeLLM)
    result = service.run("sum two ints", task_id="unscored-probe")

    scored = _events(result, "probe_scored")[0]["data"]
    assert scored["scored"] is False
    assert scored["failure"]["kind"] == "embed_capacity"
    assert scored["gx_available"] is False
    alloc = _events(result, "phase2_allocated")[0]["data"]
    assert alloc["reason"] == "unscored"
    assert alloc["tier"] == FLOOR_TIER and alloc["k"] == K_FLOOR


def test_the_pool_record_carries_the_failure(monkeypatch, tmp_path):
    _install_fake_lens(monkeypatch)
    sink = tmp_path / "pool.jsonl"
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    service = _service(monkeypatch, [LONG, SCORED_A])
    service.run("sum two ints", task_id="unscored-pool",
                file_path="/workspace/e2e/solve.py")

    by_code = {}
    for line in sink.read_text().splitlines():
        rec = json.loads(line)
        if rec.get("type") == "candidate_evaluation" and rec.get("role") == "generated":
            by_code[base64.b64decode(rec["code_b64"]).decode()] = rec
    assert by_code[SCORED_A]["lens"]["energy"] == 3.0
    assert by_code[SCORED_A]["lens"]["failure"] is None
    long_lens = by_code[LONG]["lens"]
    assert long_lens["energy"] is None
    assert long_lens["energy_calibrated"] is False
    assert long_lens["failure"]["kind"] == "embed_capacity"
    assert long_lens["failure"]["input_tokens"] == 2055


# --- the lens goes down during a run ------------------------------------------------------

def _lens_down_after(monkeypatch, n_answers):
    """A lens that answers `n_answers` calls, then stops answering."""
    calls = {"n": 0}

    def fake_urlopen(req, timeout=None):
        calls["n"] += 1
        if calls["n"] > n_answers:
            raise urllib.error.URLError("connection refused")
        text = json.loads(req.data.decode())["text"]
        url = req.full_url
        payload = (_gx_score_payload(text, "typed") if url.endswith("/gx-score")
                   else _per_step_payload(text, "typed"))
        return _Response(json.dumps(payload).encode())
    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake_urlopen)


def test_a_lens_that_stops_answering_stops_the_run(monkeypatch):
    """The generation stage catches its own failures broadly; a lens that
    cannot score must not be swallowed there and ranked around. The run
    stops, and the caller learns why."""
    _lens_down_after(monkeypatch, 0)
    service = _service(monkeypatch, [SCORED_A, SCORED_B])
    with pytest.raises(scoring.LensUnavailable) as err:
        service.run("sum two ints", task_id="lens-down-probe")
    assert "lens_unreachable" in err.value.reason


def test_a_lens_that_fails_mid_generation_is_not_swallowed(monkeypatch):
    """The probe is scored (one gx-score call), then the lens goes down while
    the generated candidates are scored inside the PlanSearch stage, whose
    handler catches Exception."""
    _lens_down_after(monkeypatch, 1)
    service = _service(monkeypatch, [SCORED_A, SCORED_B], llm_cls=ProbeLLM)
    monkeypatch.setattr(PassingSandbox, "__call__",
                        lambda self, code, test_input="", **_: (False, "", "boom"))
    with pytest.raises(scoring.LensUnavailable):
        service.run("sum two ints", task_id="lens-down-mid")
