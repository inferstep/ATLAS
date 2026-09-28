"""The bench's Lens client reports an unscored candidate as unscored.

`atlas bench` ranks its candidates by the same min-energy rule as the
product pipeline. A Lens answer that carries no score (the input exceeded
llama-server's physical batch, the Lens was unreachable) used to come back
as the neutral pair (0.0, 0.5), which the sort read as the best energy in
the pool.
"""

import pytest
import io
import json
import urllib.error

from atlas.bench import best_of_k


class _Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def _answer(monkeypatch, payload):
    monkeypatch.setattr(best_of_k.urllib.request, "urlopen",
                        lambda req, timeout=None: _Response(json.dumps(payload).encode()))


def test_typed_capacity_failure_is_unscored(monkeypatch):
    _answer(monkeypatch, {"enabled": True, "scored": False, "energy": None,
                          "normalized": None, "calibrated": False,
                          "failure": {"kind": "embed_capacity",
                                      "input_tokens": 2055,
                                      "capacity_tokens": 2048}})
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


def test_error_answer_without_typed_fields_is_unscored(monkeypatch):
    _answer(monkeypatch, {"energy": 0.0, "normalized": 0.5,
                          "error": "lens score-text failed (error_id=abc)"})
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


def test_transport_failure_is_unscored(monkeypatch):
    def refuse(req, timeout=None):
        raise urllib.error.URLError("connection refused")
    monkeypatch.setattr(best_of_k.urllib.request, "urlopen", refuse)
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


def test_real_energy_survives(monkeypatch):
    _answer(monkeypatch, {"energy": 9.1, "normalized": 0.42, "calibrated": True,
                          "enabled": True, "scored": True})
    assert best_of_k.score_candidate("text", "http://lens") == (9.1, 0.42)


def test_combined_typed_failure_carries_no_numbers(monkeypatch):
    _answer(monkeypatch, {"enabled": True, "scored": False, "cx_energy": None,
                          "cx_normalized": None, "cx_calibrated": False,
                          "gx_score": None, "gx_available": False,
                          "verdict": "unscored",
                          "failure": {"kind": "embed_capacity",
                                      "input_tokens": 2055,
                                      "capacity_tokens": 2048}})
    out = best_of_k.score_candidate_combined("text", "http://lens")
    assert out["cx_energy"] is None and out["gx_score"] is None
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == "embed_capacity"


def test_combined_untyped_error_answer_is_unscored(monkeypatch):
    _answer(monkeypatch, {"cx_energy": 0.0, "cx_normalized": 0.5,
                          "cx_calibrated": False, "gx_score": 0.5,
                          "verdict": "error", "enabled": True,
                          "gx_available": False, "error": "boom"})
    out = best_of_k.score_candidate_combined("text", "http://lens")
    assert out["cx_energy"] is None
    assert out["verdict"] == "unscored"


def test_combined_disabled_lens_stays_neutral(monkeypatch):
    _answer(monkeypatch, {"enabled": False})
    assert best_of_k.score_candidate_combined("text", "http://lens") == \
        best_of_k.NEUTRAL_COMBINED


# --- a non-finite number is not a score ------------------------------------------------

NON_FINITE = [float("nan"), float("inf"), float("-inf")]


@pytest.mark.parametrize("value", NON_FINITE)
def test_nonfinite_energy_is_unscored(monkeypatch, value):
    _answer(monkeypatch, {"energy": value, "normalized": 0.4, "calibrated": True,
                          "enabled": True, "scored": True})
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


@pytest.mark.parametrize("value", NON_FINITE + ["0.4"])
def test_nonfinite_normalized_energy_is_unscored(monkeypatch, value):
    _answer(monkeypatch, {"energy": 2.0, "normalized": value, "calibrated": True,
                          "enabled": True, "scored": True})
    assert best_of_k.score_candidate("text", "http://lens") == (None, None)


@pytest.mark.parametrize("field", ["cx_energy", "cx_normalized", "gx_score"])
@pytest.mark.parametrize("value", NON_FINITE)
def test_combined_nonfinite_field_is_unscored(monkeypatch, field, value):
    _answer(monkeypatch, {"enabled": True, "scored": True, "cx_energy": 3.0,
                          "cx_normalized": 0.3, "cx_calibrated": True,
                          "gx_score": 0.8, "gx_available": True,
                          "verdict": "likely_correct", field: value})
    out = best_of_k.score_candidate_combined("text", "http://lens")
    assert out["cx_energy"] is None and out["gx_score"] is None
    assert out["verdict"] == "unscored"
    assert out["failure"]["kind"] == "nonfinite_score"


def test_negative_and_zero_energies_survive(monkeypatch):
    for energy in (-4.5, 0.0):
        _answer(monkeypatch, {"energy": energy, "normalized": 0.1, "calibrated": True,
                              "enabled": True, "scored": True})
        assert best_of_k.score_candidate("text", "http://lens") == (energy, 0.1)
