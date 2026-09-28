"""A frozen diagnostic counts generated code with the serving tokenizer first."""

import io
import json
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import scoring  # noqa: E402
import pipeline  # noqa: E402


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False


def _enabled_score():
    return {"enabled": True, "scored": True, "cx_energy": 2.0,
            "cx_normalized": 0.2, "cx_calibrated": True,
            "gx_score": 0.8, "gx_available": True,
            "verdict": "likely_correct"}


def _bind(monkeypatch, capacity="2112", margin="312"):
    monkeypatch.setenv(scoring.SCORING_CAPACITY_ENV, capacity)
    monkeypatch.setenv(scoring.SCORING_MARGIN_ENV, margin)


def test_same_model_tokenizer_runs_before_lens_and_records_margin(monkeypatch):
    _bind(monkeypatch)
    calls = []

    def fake(req, timeout=None):
        calls.append((req.full_url, json.loads(req.data)))
        if req.full_url.endswith("/tokenize"):
            return Response(json.dumps({"tokens": list(range(1800))}).encode())
        return Response(json.dumps(_enabled_score()).encode())

    monkeypatch.setattr(scoring.adapters, "INFERENCE_URL", "http://qualified-model")
    monkeypatch.setattr(scoring.adapters, "LENS_URL", "http://lens")
    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake)
    out = scoring.score_candidate_combined("candidate")

    assert [url for url, _ in calls] == [
        "http://qualified-model/tokenize", "http://lens/internal/lens/gx-score"]
    assert calls[0][1] == {"content": "candidate", "add_special": True}
    assert out["token_assertion"] == {
        "input_tokens": 1800, "capacity_tokens": 2112,
        "margin_tokens": 312, "max_input_tokens": 1800}
    energy = scoring.score_candidate("candidate")
    assert energy.token_assertion["input_tokens"] == 1800


def test_candidate_evidence_preserves_both_pre_score_assertions(monkeypatch):
    _bind(monkeypatch)

    def fake(req, timeout=None):
        if req.full_url.endswith("/tokenize"):
            return Response(json.dumps({"tokens": [1, 2, 3]}).encode())
        if req.full_url.endswith("/internal/lens/gx-score"):
            return Response(json.dumps(_enabled_score()).encode())
        if req.full_url.endswith("/internal/lens/score-per-step"):
            return Response(json.dumps({
                "enabled": True, "scored": True, "gx_available": True,
                "n_tokens": 3, "latency_ms": 1,
                "thresholds": {"off_rails": 0.3, "low": 0.4, "severe": 0.2},
                "aggregate": {"first_off_rails_idx": -1, "gx_score_min": 0.6,
                              "gx_score_mean": 0.7, "cx_norm_max": 0.4,
                              "cx_norm_mean": 0.3},
            }).encode())
        raise AssertionError(req.full_url)

    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake)
    view = pipeline._lens_view("candidate")
    assert view["token_assertion"]["input_tokens"] == 3
    assert view["per_step_token_assertion"]["input_tokens"] == 3
    assert view["lens_failure"] is None


def test_over_bound_is_typed_unscored_without_contacting_lens(monkeypatch):
    _bind(monkeypatch)
    calls = []

    def fake(req, timeout=None):
        calls.append(req.full_url)
        assert req.full_url.endswith("/tokenize")
        return Response(json.dumps({"tokens": list(range(1801))}).encode())

    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake)
    out = scoring.score_candidate_combined("candidate")

    assert calls == [f"{scoring.adapters.INFERENCE_URL}/tokenize"]
    assert out["verdict"] == "unscored"
    assert out["cx_energy"] is None and out["gx_score"] is None
    assert out["failure"] == {
        "kind": "embed_capacity", "input_tokens": 1801,
        "capacity_tokens": 2112, "margin_tokens": 312,
        "max_input_tokens": 1800,
        "detail": "candidate has 1801 tokens; diagnostic scoring bound is 1800 (2112 qualified minus 312 margin)",
        "stage": "pre_lens_token_assertion"}


def test_incomplete_or_malformed_guard_configuration_fails_closed(monkeypatch):
    monkeypatch.setenv(scoring.SCORING_CAPACITY_ENV, "2112")
    out = scoring.score_candidate_combined("candidate")
    assert out["failure"]["kind"] == "token_assertion_error"
    assert out["failure"]["stage"] == "pre_lens_token_assertion"

    monkeypatch.setenv(scoring.SCORING_MARGIN_ENV, "0312")
    out = scoring.score_candidate_combined("candidate")
    assert out["failure"]["kind"] == "token_assertion_error"


def test_malformed_tokenizer_answer_fails_closed_before_lens(monkeypatch):
    _bind(monkeypatch)
    monkeypatch.setattr(
        scoring.urllib.request, "urlopen",
        lambda req, timeout=None: Response(json.dumps({"tokens": [1, True]}).encode()),
    )
    out = scoring.score_candidate_combined("candidate")
    assert out["failure"]["kind"] == "token_assertion_error"
    assert out["verdict"] == "unscored"


def test_unconfigured_product_path_remains_one_lens_call(monkeypatch):
    calls = []

    def fake(req, timeout=None):
        calls.append(req.full_url)
        return Response(json.dumps(_enabled_score()).encode())

    monkeypatch.setattr(scoring.urllib.request, "urlopen", fake)
    out = scoring.score_candidate_combined("candidate")
    assert calls == [f"{scoring.adapters.LENS_URL}/internal/lens/gx-score"]
    assert "token_assertion" not in out
    assert out["cx_energy"] == 2.0
