"""A Lens answer whose score is not a finite number is unscored.

`json.loads` accepts the non-standard tokens `NaN`, `Infinity` and
`-Infinity`, and reads an overflowing literal such as `1e999` as an
infinity. An older or external Lens that serialized a degenerate forward
therefore hands V3 a float that sorts, ties, or poisons every comparison.
Such a value is a score that did not validly happen: the client reports
it as a typed failure, and nothing downstream ranks on it.
"""

import io
import json
import math
import sys
from pathlib import Path

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import scoring
from stages.candidate_selection import NONFINITE_SCORE

NON_FINITE = [float("nan"), float("inf"), float("-inf")]

SCORED = {"enabled": True, "scored": True, "cx_energy": 3.0,
          "cx_normalized": 0.3, "cx_calibrated": True,
          "gx_score": 0.8, "gx_available": True, "verdict": "likely_correct"}

PER_STEP = {"enabled": True, "scored": True, "gx_available": True,
            "n_tokens": 12, "latency_ms": 1.0,
            "thresholds": {"off_rails": 0.3, "low": 0.4, "severe": 0.2},
            "aggregate": {"first_off_rails_idx": -1, "gx_score_min": 0.6,
                          "gx_score_mean": 0.7, "cx_norm_max": 0.4,
                          "cx_norm_mean": 0.3}}


class _Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def _answer(monkeypatch, payload):
    """Serve `payload`; a dict is serialized the way an older Lens would
    (json.dumps emits NaN / Infinity), a str is served verbatim."""
    body = payload if isinstance(payload, str) else json.dumps(payload)
    monkeypatch.setattr(scoring.urllib.request, "urlopen",
                        lambda req, timeout=None: _Response(body.encode()))


def _assert_unscored(out):
    for key in ("cx_energy", "cx_normalized", "gx_score"):
        assert out[key] is None, key
    assert out["verdict"] == "unscored"
    assert out["cx_calibrated"] is False and out["gx_available"] is False
    assert out["failure"]["kind"] == NONFINITE_SCORE


@pytest.mark.parametrize("field", ["cx_energy", "cx_normalized", "gx_score"])
@pytest.mark.parametrize("value", NON_FINITE)
def test_nonfinite_score_field_is_unscored(monkeypatch, field, value):
    _answer(monkeypatch, {**SCORED, field: value})
    out = scoring.score_candidate_combined("code")
    _assert_unscored(out)
    assert out["failure"]["field"] == field
    scored = scoring.score_candidate("code")
    assert scored == (None, None, False)
    assert scored.failure["kind"] == NONFINITE_SCORE


def test_overflowing_literal_is_unscored(monkeypatch):
    _answer(monkeypatch, '{"enabled": true, "scored": true, "cx_energy": 1e999, '
                         '"cx_normalized": 0.5, "cx_calibrated": true, '
                         '"gx_score": 0.5, "gx_available": false, '
                         '"verdict": "unavailable"}')
    _assert_unscored(scoring.score_candidate_combined("code"))


def test_nonnumeric_normalized_energy_is_unscored(monkeypatch):
    _answer(monkeypatch, {**SCORED, "cx_normalized": "0.3"})
    _assert_unscored(scoring.score_candidate_combined("code"))


@pytest.mark.parametrize("value", NON_FINITE)
def test_nonfinite_per_step_aggregate_is_unscored(monkeypatch, value):
    _answer(monkeypatch, {**PER_STEP,
                          "aggregate": {**PER_STEP["aggregate"], "gx_score_min": value}})
    out = scoring.score_candidate_per_step("code")
    assert set(out) == {"failure"}
    assert out["failure"]["kind"] == NONFINITE_SCORE


@pytest.mark.parametrize("energy", [-3.25, 0.0, 1e300])
def test_finite_energies_including_negative_and_zero_survive(monkeypatch, energy):
    _answer(monkeypatch, {**SCORED, "cx_energy": energy})
    out = scoring.score_candidate_combined("code")
    assert out["cx_energy"] == energy and out["verdict"] == "likely_correct"
    assert "failure" not in out
    assert scoring.score_candidate("code") == (energy, 0.3, True)


def test_a_disabled_lens_is_unavailable_not_neutral(monkeypatch):
    """The lens is required: a lens that is switched off stops the run
    rather than answering neutral scores, whatever numbers it attached."""
    _answer(monkeypatch, {"enabled": False, "cx_energy": float("nan")})
    with pytest.raises(scoring.LensUnavailable):
        scoring.score_candidate_combined("code")


def test_per_step_finite_answer_survives(monkeypatch):
    _answer(monkeypatch, PER_STEP)
    out = scoring.score_candidate_per_step("code")
    assert out["gx_score_min"] == 0.6 and math.isfinite(out["cx_norm_max"])
