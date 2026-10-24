"""The bench's Lens client reports an unscored candidate as unscored.

`atlas bench` ranks its candidates by the same min-energy rule as the
product pipeline. A Lens answer that carries no score (the input exceeded
llama-server's physical batch, the Lens was unreachable) used to come back
as the neutral pair (0.0, 0.5), which the sort read as the best energy in
the pool.
"""

import io
import json
import math
import urllib.error

import pytest

from atlas.bench import best_of_k


def _lens_answers(monkeypatch, payload):
    """Make every Lens request get `payload` back as JSON."""
    monkeypatch.setattr(
        best_of_k.urllib.request, "urlopen",
        lambda req, timeout=None: io.BytesIO(json.dumps(payload).encode()))


@pytest.mark.parametrize(
    ("answer", "expected"),
    [
        ({"enabled": True, "scored": False, "energy": None, "normalized": None,
          "calibrated": False,
          "failure": {"kind": "embed_capacity", "input_tokens": 2055,
                      "capacity_tokens": 2048}},
         (None, None)),
        # An error answer without the typed fields, neutral numbers attached.
        ({"energy": 0.0, "normalized": 0.5,
          "error": "lens score-text failed (error_id=abc)"},
         (None, None)),
        ({"energy": 9.1, "normalized": 0.42, "calibrated": True,
          "enabled": True, "scored": True},
         (9.1, 0.42)),
        ({"energy": -4.5, "normalized": 0.1, "calibrated": True,
          "enabled": True, "scored": True},
         (-4.5, 0.1)),
        ({"energy": 0.0, "normalized": 0.1, "calibrated": True,
          "enabled": True, "scored": True},
         (0.0, 0.1)),
    ],
    ids=["typed-capacity-failure", "untyped-error-answer", "real-energy",
         "negative-energy", "zero-energy"],
)
def test_score_candidate_returns_energy_only_when_scored(monkeypatch, answer,
                                                         expected):
    _lens_answers(monkeypatch, answer)
    assert best_of_k.score_candidate("text", "http://lens") == expected


@pytest.mark.parametrize("field", ["energy", "normalized"])
@pytest.mark.parametrize("value", [math.nan, math.inf, -math.inf, "0.4"],
                         ids=["nan", "inf", "-inf", "string"])
def test_score_candidate_non_number_is_unscored(monkeypatch, field, value):
    _lens_answers(monkeypatch, {"energy": 2.0, "normalized": 0.4,
                                "calibrated": True, "enabled": True,
                                "scored": True, field: value})
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


def test_transport_failure_is_unscored(monkeypatch):
    def refuse(req, timeout=None):
        raise urllib.error.URLError("connection refused")
    monkeypatch.setattr(best_of_k.urllib.request, "urlopen", refuse)
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


@pytest.mark.parametrize(
    ("answer", "failure_kind"),
    [
        ({"enabled": True, "scored": False, "cx_energy": None,
          "cx_normalized": None, "cx_calibrated": False,
          "gx_score": None, "gx_available": False, "verdict": "unscored",
          "failure": {"kind": "embed_capacity", "input_tokens": 2055,
                      "capacity_tokens": 2048}},
         "embed_capacity"),
        # An error answer without the typed fields, neutral numbers attached.
        ({"cx_energy": 0.0, "cx_normalized": 0.5, "cx_calibrated": False,
          "gx_score": 0.5, "verdict": "error", "enabled": True,
          "gx_available": False, "error": "boom"},
         "lens_error"),
    ],
    ids=["typed-capacity-failure", "untyped-error-answer"],
)
def test_combined_unscored_answer_carries_no_numbers(monkeypatch, answer,
                                                     failure_kind):
    _lens_answers(monkeypatch, answer)
    out = best_of_k.score_candidate_combined("text", "http://lens")
    assert out["cx_energy"] is None
    assert out["gx_score"] is None
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == failure_kind


@pytest.mark.parametrize("field", ["cx_energy", "cx_normalized", "gx_score"])
@pytest.mark.parametrize("value", [math.nan, math.inf, -math.inf],
                         ids=["nan", "inf", "-inf"])
def test_combined_nonfinite_field_is_unscored(monkeypatch, field, value):
    _lens_answers(monkeypatch, {"enabled": True, "scored": True,
                                "cx_energy": 3.0, "cx_normalized": 0.3,
                                "cx_calibrated": True, "gx_score": 0.8,
                                "gx_available": True,
                                "verdict": "likely_correct", field: value})
    out = best_of_k.score_candidate_combined("text", "http://lens")
    assert out["cx_energy"] is None
    assert out["gx_score"] is None
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == "nonfinite_score"


def test_combined_disabled_lens_stays_neutral(monkeypatch):
    _lens_answers(monkeypatch, {"enabled": False})
    assert best_of_k.score_candidate_combined("text", "http://lens") == \
        best_of_k.NEUTRAL_COMBINED
