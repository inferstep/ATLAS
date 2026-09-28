"""D8: the live service writes stage telemetry.

V3PipelineService used to construct every stage without telemetry_dir, so
all documented telemetry/*.jsonl was bench-only and the live orchestrator
was unmeasurable. The service now resolves ATLAS_V3_TELEMETRY_DIR, passes
it to the stages, and pipeline.run appends one pipeline_summary.jsonl line
per task — fail-soft in every direction.
"""

import base64
import hashlib
import json
import os
import stat
import subprocess
import sys
import textwrap
from pathlib import Path
from types import SimpleNamespace

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402
import pipeline as v3pipeline  # noqa: E402
import scoring  # noqa: E402

VETO_PER_STEP = {
    "gx_score_min": 0.01,
    "gx_score_mean": 0.02,
    "cx_norm_max": 0.5,
    "first_off_rails_idx": 0,
    "n_tokens": 10,
    "thresholds": {"severe": 0.30},
}


class FakeLLM:
    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return "<think>still thinking", 3, 1.0


class PassingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return True, "ok", ""


class FakeEmbed:
    def __call__(self, text):
        return []


def _make_service(monkeypatch):
    monkeypatch.setattr(adapters, "LLMAdapter", FakeLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", PassingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (1.0, 0.1, False))
    monkeypatch.setattr(
        scoring, "score_candidate_per_step", lambda code: dict(VETO_PER_STEP))

    service = v3pipeline.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            (_ for _ in ()).throw(RuntimeError("unavailable")))
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=["def a():\n    pass\n",
                                        "def b():\n    pass\n",
                                        "def c():\n    pass\n"],
                            total_tokens=0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service


def test_env_dir_reaches_stages_and_summary_is_written(monkeypatch, tmp_path):
    tdir = tmp_path / "telemetry"
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", str(tdir))
    service = _make_service(monkeypatch)

    # The resolved dir is wired through to the stages the service builds.
    assert service.telemetry_dir == tdir
    fresh = v3pipeline.V3PipelineService()
    assert fresh.plan_search.telemetry_dir == tdir
    assert fresh.pr_cot.telemetry_dir == tdir
    assert fresh.refinement_loop.telemetry_dir == tdir

    result = service.run("write a real dashboard", task_id="d8-summary")

    summary_file = tdir / "pipeline_summary.jsonl"
    assert summary_file.exists()
    lines = summary_file.read_text().strip().splitlines()
    assert len(lines) == 1
    line = json.loads(lines[0])
    assert line["schema"] == "v3_pipeline_summary_v1"
    assert line["task_id"] == "d8-summary"
    assert line["passed"] is False
    assert line["phase_solved"] == "none"
    # Fully mocked runs can complete inside a millisecond and round to 0.
    assert line["total_time_ms"] >= 0

    phases = {p["phase"]: p for p in line["phases"]}
    # Probe ran, generation ran, all sandbox-passers were vetoed, repair
    # ran, and the fallback closed the run.
    for expected in ("probe", "generation", "sandbox", "veto",
                     "repair_pr_cot", "fallback"):
        assert expected in phases, f"missing phase {expected}: {line['phases']}"
    assert phases["fallback"]["outcome"] == "fallback_all_vetoed"
    for p in line["phases"]:
        assert p["duration_ms"] >= 0

    assert len(line["veto_events"]) == 3
    assert all(v["stage"] == "lens_veto" for v in line["veto_events"])

    # And the events in the result agree with what was summarized.
    assert result["phase_solved"] == "none"


def test_disable_value_turns_telemetry_off(monkeypatch, tmp_path):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    service = _make_service(monkeypatch)
    assert service.telemetry_dir is None

    result = service.run("write a real dashboard", task_id="d8-off")
    assert result["phase_solved"] == "none"
    assert not list(tmp_path.iterdir())


def test_unwritable_dir_disables_without_breaking_generation(monkeypatch, tmp_path):
    blocker = tmp_path / "not-a-dir"
    blocker.write_text("file, not a directory")
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", str(blocker / "telemetry"))
    service = _make_service(monkeypatch)
    assert service.telemetry_dir is None

    result = service.run("write a real dashboard", task_id="d8-unwritable")
    assert result["phase_solved"] == "none"


# =====================================================================
# Benchmark-only candidate-pool capture.
#
# Suite A retained one number per verification -- a boolean -- so a
# candidate that scored 9/10 and one that scored 0/10 reached the contract
# record identically, and every rejected candidate's bytes were gone by the
# time the run ended. That made the measured 0/N pattern unattributable:
# nothing retained could separate a wrong candidate from a wrong answer
# key. Capture writes the pool, the per-case expected/actual pairs and the
# selection identities to one file, off unless a path is configured, and
# changes no decision on the way.
# =====================================================================

# Final newline included: the fenced body comes out of extract_code exactly
# as written, so this is the byte string the probe actually enters the
# pipeline with, and capture is required to reproduce it exactly rather than
# approximately.
CAP_ZERO = "def solve(x):\n    return x + 1\n"
CAP_ONE = "def solve(x):\n    return x + 2"
CAP_TWO = "def solve(x):\n    return x + 3"

CAP_PASS = (True, "SELF_TEST_PASS\n", "")
CAP_INPUTS = ("0", "1", "2", "3", "4")

# Well clear of the severe band: the lens veto is not what this measures,
# and a vetoed pool leaves no selection to summarize.
CAP_PER_STEP = {
    "gx_score_min": 0.80,
    "gx_score_mean": 0.90,
    "cx_norm_max": 0.5,
    "first_off_rails_idx": -1,
    "n_tokens": 10,
    "thresholds": {"severe": 0.30, "severe_mean": 0.40},
}

# Per candidate, per case input: what the sandbox reports back. The three
# scores differ so capture has something to tell apart. None of them decides
# anything: the cases are model-generated and carry no rejection authority.
CAP_SANDBOX_TABLE = {
    # 1/5, and each failure a different kind.
    (CAP_ZERO, "0"): CAP_PASS,
    (CAP_ZERO, "1"): (False, "", "AssertionError: got 2"),
    (CAP_ZERO, "2"): (False, "", "Traceback (most recent call last):\n"
                                 "NameError: name 'q' is not defined"),
    (CAP_ZERO, "3"): (False, "", "execution timed out after 15s"),
    (CAP_ZERO, "4"): (False, "", "AssertionError: got 5"),
    # 3/5.
    (CAP_ONE, "0"): CAP_PASS,
    (CAP_ONE, "1"): CAP_PASS,
    (CAP_ONE, "2"): CAP_PASS,
    (CAP_ONE, "3"): (False, "", "AssertionError: got 5"),
    (CAP_ONE, "4"): (False, "", "AssertionError: got 6"),
    # 2/5: distinguishable from 1/5 only because capture kept the score.
    (CAP_TWO, "0"): CAP_PASS,
    (CAP_TWO, "1"): CAP_PASS,
    (CAP_TWO, "2"): (False, "", "AssertionError: got 5"),
    (CAP_TWO, "3"): (False, "", "AssertionError: got 6"),
    (CAP_TWO, "4"): (False, "", "AssertionError: got 7"),
}


class CaptureSandbox:
    """Runs candidates and their generated cases from a fixed table."""

    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", language="python", timeout=15, **_):
        if "SELF_TEST_PASS" not in code:
            return True, "ok", ""
        candidate = next((c for c in (CAP_ZERO, CAP_ONE, CAP_TWO)
                          if code.startswith(c)), None)
        case = next((i for i in CAP_INPUTS if f"_i='{i}'" in code), None)
        if candidate is None or case is None:
            return False, "", "unmatched self-test"
        return CAP_SANDBOX_TABLE[(candidate, case)]

    def syntax_check(self, code, language, filename=""):
        return True, "", ""


class CaptureLLM:
    """Returns candidate zero's source for every probe attempt."""

    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        # The fence closes right after the artifact's own terminator: what is
        # inside the fence is exactly CAP_ZERO, and that is what comes out.
        return f"```python\n{CAP_ZERO}```", 5, 1.0


def _capture_cases():
    return [SimpleNamespace(input_str=i, expected_output=str(int(i) + 1))
            for i in CAP_INPUTS]


def _capture_service(monkeypatch):
    """A service whose pool is candidate zero plus two generated rivals."""
    monkeypatch.setattr(adapters, "LLMAdapter", CaptureLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", CaptureSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (1.0, 0.1, False))
    monkeypatch.setattr(scoring, "score_candidate_combined",
                        lambda code: dict(scoring.NEUTRAL_COMBINED))
    monkeypatch.setattr(scoring, "score_candidate_per_step",
                        lambda code: dict(CAP_PER_STEP))
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")

    service = v3pipeline.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            SimpleNamespace(test_cases=_capture_cases(), generation_tokens=0))
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None,
        budget_tier="standard":
            SimpleNamespace(candidates=[CAP_ONE, CAP_TWO], total_tokens=0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service


def _capture_records(path: Path):
    return [json.loads(line) for line in
            path.read_text().splitlines() if line.strip()]


def _of_type(records, kind):
    return [r for r in records if r.get("type") == kind]


def test_interrupted_capture_flushes_pending_lens_candidate(
        monkeypatch, tmp_path):
    """A scored candidate survives cancellation before sandbox evaluation."""
    sink = tmp_path / "pending.jsonl"
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    capture = v3pipeline._PoolCapture.from_env()
    capture.bind("pending-task")
    capture.identify("pending-request", "pending-invocation")
    capture.note_lens_candidate(
        role="generated", index=1, code=CAP_ONE,
        phase="plansearch_scored",
        lens={"energy": 1.25, "energy_norm": 0.4,
              "energy_calibrated": True,
              "token_assertion": {"input_tokens": 10},
              "per_step_token_assertion": {"input_tokens": 10}})
    capture.close(None)

    evaluations = _of_type(_capture_records(sink), "candidate_evaluation")
    assert len(evaluations) == 1
    rec = evaluations[0]
    assert rec["role"] == "generated"
    assert rec["phase"] == "plansearch_scored"
    assert rec["accepted"] is False
    assert rec["contract_record_source"] == "not_built_in_production"
    assert rec["lens"]["energy"] == 1.25


def _run_captured(monkeypatch, tmp_path, task_id="cap"):
    sink = tmp_path / "pool.jsonl"
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    service = _capture_service(monkeypatch)
    result = service.run("add one to the number on stdin", task_id=task_id,
                         file_path="/workspace/e2e/solve.py")
    return result, sink


def test_capture_is_inert_without_the_environment_variable(monkeypatch, tmp_path):
    """Unset means no sink is opened and no file appears, not an empty file."""
    monkeypatch.delenv(v3pipeline.CAPTURE_ENV, raising=False)
    capture = v3pipeline._PoolCapture.from_env()
    assert capture.enabled is False
    assert capture.path is None

    service = _capture_service(monkeypatch)
    service.run("add one", task_id="cap-off", file_path="/workspace/e2e/solve.py")
    assert list(tmp_path.iterdir()) == []


def test_exact_candidate_bytes_round_trip_with_hash_and_length(monkeypatch, tmp_path):
    """The pool is retained as bytes, not as a summary of bytes."""
    _, sink = _run_captured(monkeypatch, tmp_path)
    evaluations = _of_type(_capture_records(sink), "candidate_evaluation")
    assert evaluations, "no candidate_evaluation records written"

    seen = set()
    for rec in evaluations:
        raw = base64.b64decode(rec["code_b64"])
        assert raw.decode("utf-8") in (CAP_ZERO, CAP_ONE, CAP_TWO)
        assert rec["code_sha256"] == hashlib.sha256(raw).hexdigest()
        assert rec["code_bytes"] == len(raw)
        seen.add(raw.decode("utf-8"))
    assert seen == {CAP_ZERO, CAP_ONE, CAP_TWO}


def test_per_case_expected_and_actual_outcomes_are_retained(monkeypatch, tmp_path):
    """Input, generated expected, observed actual and a classified outcome --
    the four fields that decide whether the answer key or the code was wrong."""
    _, sink = _run_captured(monkeypatch, tmp_path)
    evaluations = _of_type(_capture_records(sink), "candidate_evaluation")
    by_code = {base64.b64decode(r["code_b64"]).decode(): r for r in evaluations}

    zero = by_code[CAP_ZERO]["oracle"]
    assert zero["cases_total"] == 5
    assert zero["cases_passed"] == 1
    outcomes = {c["input"]: c["outcome"] for c in zero["cases"]}
    assert outcomes == {"0": "pass", "1": "wrong_answer",
                        "2": "execution_error", "3": "timeout",
                        "4": "wrong_answer"}
    wrong = next(c for c in zero["cases"] if c["input"] == "1")
    assert wrong["expected"] == "2"
    assert wrong["actual"] == "2"
    assert wrong["passed"] is False

    one = by_code[CAP_ONE]["oracle"]
    assert (one["cases_passed"], one["cases_total"]) == (3, 5)
    two = by_code[CAP_TWO]["oracle"]
    assert (two["cases_passed"], two["cases_total"]) == (2, 5)


def test_passed_and_total_survive_the_boolean_the_pipeline_still_uses(
        monkeypatch, tmp_path):
    """Production keeps its accept/reject boolean; capture keeps the score
    that boolean discards."""
    _, sink = _run_captured(monkeypatch, tmp_path)
    evaluations = _of_type(_capture_records(sink), "candidate_evaluation")
    by_code = {base64.b64decode(r["code_b64"]).decode(): r for r in evaluations}
    # Model-generated cases no longer reject, so `accepted` is the trusted
    # part — the candidate executed. The scores behind it are still kept,
    # and still tell 2/5 from 1/5, which is the point: production uses one
    # boolean, capture keeps the number that boolean cannot carry.
    assert by_code[CAP_TWO]["oracle"]["cases_passed"] == 2
    assert by_code[CAP_ZERO]["oracle"]["cases_passed"] == 1
    assert by_code[CAP_TWO]["oracle"]["cases_total"] == 5


def test_candidate_zero_is_identified_as_such(monkeypatch, tmp_path):
    """Candidate zero is the probe, not merely index 0 of a sorted pool."""
    _, sink = _run_captured(monkeypatch, tmp_path)
    evaluations = _of_type(_capture_records(sink), "candidate_evaluation")
    zeros = [r for r in evaluations if r["role"] == "candidate_zero"]
    assert len(zeros) == 1
    assert base64.b64decode(zeros[0]["code_b64"]).decode() == CAP_ZERO
    assert {r["role"] for r in evaluations} >= {"candidate_zero", "generated"}


def test_selection_summary_names_only_captured_identities(monkeypatch, tmp_path):
    """A selection that references a hash no capture record carries cannot be
    adjudicated offline."""
    result, sink = _run_captured(monkeypatch, tmp_path)
    records = _capture_records(sink)
    summaries = _of_type(records, "selection_summary")
    assert len(summaries) == 1
    summary = summaries[0]
    known = {r["code_sha256"] for r in _of_type(records, "candidate_evaluation")}
    assert set(summary["pool"]) <= known
    assert summary["service_returned_candidate_hash"] in known
    assert summary["service_returned_candidate_hash"] == \
        hashlib.sha256(result["code"].encode()).hexdigest()
    for key in ("lens_index", "evidence_index", "verified_index",
                "selection_status", "selection_reason", "tied_count",
                "incomparable_count", "ineligible_count", "session_id",
                "phase"):
        assert key in summary, key


def test_capture_status_closes_the_file(monkeypatch, tmp_path):
    _, sink = _run_captured(monkeypatch, tmp_path)
    status = _of_type(_capture_records(sink), "capture_status")
    assert len(status) == 1
    assert status[0]["limit_reached"] is False
    assert status[0]["write_error"] == ""
    assert status[0]["records_written"] > 0
    assert status[0]["bytes_written"] > 0
    assert status[0]["max_bytes"] == v3pipeline.CAPTURE_DEFAULT_MAX_BYTES


def test_capture_changes_no_decision_and_no_public_surface(monkeypatch, tmp_path):
    """Off versus on must be indistinguishable to every caller."""
    monkeypatch.delenv(v3pipeline.CAPTURE_ENV, raising=False)
    off = _capture_service(monkeypatch).run(
        "add one", task_id="cap-cmp", file_path="/workspace/e2e/solve.py")
    _, sink = _run_captured(monkeypatch, tmp_path, task_id="cap-cmp")
    on = _capture_service(monkeypatch).run(
        "add one", task_id="cap-cmp", file_path="/workspace/e2e/solve.py")

    def comparable(r):
        return {k: v for k, v in r.items()
                if k not in ("total_time_ms", "events")}

    assert comparable(off) == comparable(on)
    assert [(e.get("stage"), e.get("detail")) for e in off["events"]] == \
        [(e.get("stage"), e.get("detail")) for e in on["events"]]

    # No candidate's source may ride out on an event, and the two rejected
    # candidates must not appear anywhere outside the sink.
    blob = json.dumps(on["events"])
    for code in (CAP_ZERO, CAP_TWO):
        assert code not in blob
        assert code[:20] not in blob
    assert sink.exists()


def test_a_capture_failure_never_changes_the_result(monkeypatch, tmp_path):
    """An unwritable sink degrades the diagnostic, never the pipeline."""
    blocker = tmp_path / "not-a-dir"
    blocker.write_text("file, not a directory")
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(blocker / "pool.jsonl"))
    broken = _capture_service(monkeypatch).run(
        "add one", task_id="cap-fail", file_path="/workspace/e2e/solve.py")

    monkeypatch.delenv(v3pipeline.CAPTURE_ENV, raising=False)
    clean = _capture_service(monkeypatch).run(
        "add one", task_id="cap-fail", file_path="/workspace/e2e/solve.py")
    assert broken["passed"] == clean["passed"]
    assert broken["code"] == clean["code"]
    assert broken["phase_solved"] == clean["phase_solved"]


def test_a_relative_path_or_missing_parent_disables_capture(monkeypatch, tmp_path):
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, "pool.jsonl")
    assert v3pipeline._PoolCapture.from_env().enabled is False

    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(tmp_path / "gone" / "p.jsonl"))
    assert v3pipeline._PoolCapture.from_env().enabled is False


def test_capture_refuses_to_follow_a_pre_existing_symlink(monkeypatch, tmp_path):
    target = tmp_path / "target.jsonl"
    target.write_text("")
    link = tmp_path / "pool.jsonl"
    link.symlink_to(target)
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(link))
    capture = v3pipeline._PoolCapture.from_env()
    assert capture.enabled is False
    assert target.read_text() == ""


def test_the_sink_is_owner_only(monkeypatch, tmp_path):
    _, sink = _run_captured(monkeypatch, tmp_path)
    mode = stat.S_IMODE(sink.stat().st_mode)
    assert mode == 0o600, oct(mode)


def test_the_byte_cap_never_emits_a_partial_line(monkeypatch, tmp_path):
    """A record that will not fit is not written at all."""
    sink = tmp_path / "pool.jsonl"
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    monkeypatch.setattr(v3pipeline, "CAPTURE_DEFAULT_MAX_BYTES", 900)
    service = _capture_service(monkeypatch)
    service.run("add one", task_id="cap-limit",
                file_path="/workspace/e2e/solve.py")

    text = sink.read_text()
    assert text.endswith("\n")
    records = _capture_records(sink)
    for line in text.splitlines():
        json.loads(line)          # every line is a complete record
    assert len(sink.read_bytes()) <= 900
    marker = _of_type(records, "capture_status")
    assert marker and marker[-1]["limit_reached"] is True


def test_concurrent_writers_append_complete_unique_records(tmp_path):
    """Two V3 worker processes share one sink; flock is the only reason the
    lines do not interleave."""
    sink = tmp_path / "pool.jsonl"
    writer = textwrap.dedent(f"""
        import os, sys
        sys.path.insert(0, {str(PROJECT_ROOT / "v3-service")!r})
        os.environ["ATLAS_V3_CAPTURE_POOL"] = {str(sink)!r}
        import pipeline
        cap = pipeline._PoolCapture.from_env()
        assert cap.enabled
        tag = sys.argv[1]
        for i in range(200):
            cap.write({{"type": "candidate_evaluation", "session_id": tag,
                       "candidate_index": i, "code_b64": "A" * 400}})
        cap.close()
    """)
    script = tmp_path / "writer.py"
    script.write_text(writer)
    procs = [subprocess.Popen([sys.executable, str(script), f"w{i}"])
             for i in range(4)]
    for p in procs:
        assert p.wait() == 0

    lines = [line for line in sink.read_text().splitlines() if line.strip()]
    parsed = [json.loads(line) for line in lines]
    evaluations = [r for r in parsed if r["type"] == "candidate_evaluation"]
    assert len(evaluations) == 800
    assert len({(r["session_id"], r["candidate_index"])
                for r in evaluations}) == 800


def test_capture_output_is_excluded_from_git():
    """The sink is a benchmark artifact; a committed pool is a code leak."""
    check = subprocess.run(
        ["git", "check-ignore", "-q",
         "redteam/runs/diagnostic/pool.jsonl"],
        cwd=str(PROJECT_ROOT))
    assert check.returncode == 0, "the capture location is not gitignored"


def test_candidate_bytes_reach_no_serialiser_or_emitter():
    """Source sentinel: the writer is referenced only where the pool exists.
    A capture call inside an emitter, a response builder or a log formatter is
    how benchmark-only bytes become production output."""
    v3dir = PROJECT_ROOT / "v3-service"
    owners = sorted(p.name for p in v3dir.glob("*.py")
                    if "_PoolCapture" in p.read_text()
                    or "CAPTURE_ENV" in p.read_text())
    assert owners == ["pipeline.py"], owners

    source = (v3dir / "pipeline.py").read_text()
    assert "code_b64" in source
    for module in ("main.py", "adapters.py", "contract.py", "scoring.py",
                   "structured_log.py"):
        text = (v3dir / module).read_text()
        assert "code_b64" not in text, module
        assert "ATLAS_V3_CAPTURE_POOL" not in text, module


# =====================================================================
# The incumbent, measured — in a shadow pool that decides nothing.
#
# `baseline_code` is the artifact V3 was asked to improve on. It reaches
# the service as prose in the problem statement and nothing else: no
# adapter, no contract record, no execution, no place in `passing` or the
# selection pool. So a selection that concludes best_not_closure_eligible
# has ranked the generated candidates against each other and said nothing
# about whether the winner beats what it would replace. These record the
# incumbent through the SAME adapter->contract path, keep it in a separate
# pool, and prove the live path is byte-identical either way.
# =====================================================================

INCUMBENT = "def solve(x):\n    return x + 99"


def _of(records, kind):
    return [r for r in records if r.get("type") == kind]


def _run_with_incumbent(monkeypatch, tmp_path, *, capture=True,
                        baseline=INCUMBENT, task_id="inc"):
    sink = tmp_path / "pool.jsonl"
    if capture:
        monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    else:
        monkeypatch.delenv(v3pipeline.CAPTURE_ENV, raising=False)
    service = _capture_service(monkeypatch)
    result = service.run("add one", task_id=task_id,
                         file_path="/workspace/e2e/solve.py",
                         baseline_code=baseline)
    return result, sink


def test_the_incumbent_is_evaluated_exactly_once(monkeypatch, tmp_path):
    """One canonical evaluation, not one per candidate and not zero."""
    calls = []
    real = v3pipeline._evaluate_candidate
    monkeypatch.setattr(v3pipeline, "_evaluate_candidate",
                        lambda fp, code, *a, **k: (calls.append(code),
                                                   real(fp, code, *a, **k))[1])
    _, sink = _run_with_incumbent(monkeypatch, tmp_path)
    assert calls.count(INCUMBENT) == 1, calls.count(INCUMBENT)

    observations = _of(_capture_records(sink), "incumbent_observation")
    assert len(observations) == 1


def test_the_incumbent_record_describes_the_exact_baseline_bytes(
        monkeypatch, tmp_path):
    _, sink = _run_with_incumbent(monkeypatch, tmp_path)
    obs = _of(_capture_records(sink), "incumbent_observation")[0]
    raw = base64.b64decode(obs["code_b64"])
    assert raw.decode() == INCUMBENT
    assert obs["code_sha256"] == hashlib.sha256(raw).hexdigest()
    assert obs["code_bytes"] == len(raw)
    assert obs["contract_record"]["candidate_content_hash"] == obs["code_sha256"]
    assert obs["role"] == "incumbent_baseline"
    assert obs["pool"] == "shadow_comparison"
    assert obs["influences_live_selection"] is False


def test_the_incumbent_shares_the_task_identity_of_the_candidates(
        monkeypatch, tmp_path):
    """Same contract, artifact scope and evaluation context — otherwise the
    two records are incomparable and the comparison is meaningless."""
    _, sink = _run_with_incumbent(monkeypatch, tmp_path)
    records = _capture_records(sink)
    obs = _of(records, "incumbent_observation")[0]["contract_record"]
    cand = next(r["contract_record"] for r in _of(records, "candidate_evaluation")
                if r.get("contract_record"))
    for field in ("contract_id", "contract_version", "artifact_scope",
                  "evaluation_context_hash"):
        assert obs[field] == cand[field], field
    assert obs["candidate_content_hash"] != cand["candidate_content_hash"]


def test_the_incumbent_never_enters_a_live_list_or_decision(
        monkeypatch, tmp_path):
    result, sink = _run_with_incumbent(monkeypatch, tmp_path)
    records = _capture_records(sink)
    obs = _of(records, "incumbent_observation")[0]

    # Not the returned code, not a pool member, not the selection.
    assert result["code"] != INCUMBENT
    summary = _of(records, "selection_summary")[0]
    assert obs["code_sha256"] not in summary["pool"]
    assert summary["service_returned_candidate_hash"] != obs["code_sha256"]
    assert summary["verified_index"] is None
    # Not in the candidate pool capture either: it has its own record type.
    assert all(r["code_sha256"] != obs["code_sha256"]
               for r in _of(records, "candidate_evaluation"))
    # And nothing about it reaches the envelope the proxy reads.
    assert result.get("evidence_record", {}).get(
        "candidate_content_hash") != obs["code_sha256"]


def test_capture_off_leaves_the_live_path_byte_identical(monkeypatch, tmp_path):
    """Default behaviour must not depend on the diagnostic being on."""
    off, _ = _run_with_incumbent(monkeypatch, tmp_path, capture=False,
                                 task_id="inc-cmp")
    on, sink = _run_with_incumbent(monkeypatch, tmp_path, task_id="inc-cmp")

    def comparable(r):
        return {k: v for k, v in r.items()
                if k not in ("total_time_ms", "events", "consensus")}

    assert comparable(off) == comparable(on)
    assert [(e.get("stage"), e.get("detail")) for e in off["events"]] == \
        [(e.get("stage"), e.get("detail")) for e in on["events"]]


def test_capture_off_evaluates_no_incumbent_at_all(monkeypatch, tmp_path):
    """Off means the work is not done, not that its output is discarded."""
    calls = []
    real = v3pipeline._evaluate_candidate
    monkeypatch.setattr(v3pipeline, "_evaluate_candidate",
                        lambda fp, code, *a, **k: (calls.append(code),
                                                   real(fp, code, *a, **k))[1])
    _run_with_incumbent(monkeypatch, tmp_path, capture=False)
    assert INCUMBENT not in calls
    assert list(tmp_path.iterdir()) == []


def test_an_unevaluable_incumbent_is_recorded_truthfully(monkeypatch, tmp_path):
    """Never synthesise a result for it."""
    real = v3pipeline._evaluate_candidate

    def _boom_on_incumbent(fp, code, *a, **k):
        if code == INCUMBENT:
            raise RuntimeError("adapter unavailable")
        return real(fp, code, *a, **k)

    monkeypatch.setattr(v3pipeline, "_evaluate_candidate", _boom_on_incumbent)
    sink = tmp_path / "pool.jsonl"
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))
    service = _capture_service(monkeypatch)
    result = service.run("add one", task_id="inc-broken",
                         file_path="/workspace/e2e/solve.py",
                         baseline_code=INCUMBENT)
    # The run itself is unaffected: an incumbent that cannot be measured is
    # a gap in the diagnostic, never a change to the pipeline.
    assert result["phase_solved"] != "none" or result["code"] is not None
    obs = _of(_capture_records(sink), "incumbent_observation")
    assert len(obs) == 1
    assert obs[0]["contract_record"] is None
    assert obs[0]["evaluation"].startswith("unevaluated:")


def test_the_three_roles_stay_distinct(monkeypatch, tmp_path):
    """Incumbent, phase-zero probe and generated alternatives are three
    different artifacts and three different names."""
    _, sink = _run_with_incumbent(monkeypatch, tmp_path)
    records = _capture_records(sink)
    incumbent = {r["code_sha256"] for r in _of(records, "incumbent_observation")}
    probe = {r["code_sha256"] for r in _of(records, "candidate_evaluation")
             if r["role"] == "candidate_zero"}
    generated = {r["code_sha256"] for r in _of(records, "candidate_evaluation")
                 if r["role"] == "generated"}
    assert incumbent and probe and generated
    assert not (incumbent & probe) and not (incumbent & generated)
    assert not (probe & generated)


def test_no_new_field_reaches_any_public_surface():
    """The shadow pool is a file. It is not a response, an envelope or a
    wire type, and no Go field exists for it."""
    v3dir = PROJECT_ROOT / "v3-service"
    response_block = (v3dir / "main.py").read_text().split(
        "response = {", 1)[1].split("}", 1)[0]
    for leaked in ("incumbent", "shadow", "consensus", "cluster"):
        assert leaked not in response_block, leaked
    for module in ("contract.py", "adapters.py"):
        text = (v3dir / module).read_text()
        assert "incumbent" not in text, module
    go = (PROJECT_ROOT / "proxy" / "types.go").read_text()
    for leaked in ("incumbent", "Incumbent", "shadow_comparison"):
        assert leaked not in go, leaked
