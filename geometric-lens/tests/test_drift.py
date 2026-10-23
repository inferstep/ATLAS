"""Drift-fingerprint validation, checking, and reproduction.

The reproduction test (test_fingerprint_reproduces_energies) is the
GPU-free acceptance check from the bug spec: a stored cost field + stored
reference embeddings reproduce the fingerprint energies without a live
llama-server. A drifted stack (wrong-scale embedding) fails the check.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from geometric_lens import drift


def _score_fixed(mapping):
    """A score_fn that returns a canned energy per reference text."""
    return lambda text: mapping.get(text, 0.0)


# --- validation ------------------------------------------------------------

def test_validate_fingerprint_shape_and_bounds():
    fp = drift.validate_fingerprint({
        "references": [{"text": "a", "expected_energy": 20.9}],
    })
    assert fp["tolerance_pct"] == drift.DEFAULT_TOLERANCE_PCT
    assert fp["references"][0]["expected_energy"] == 20.9
    for bad in ({"references": []},
                {"references": [{"text": "", "expected_energy": 1}]},
                {"references": [{"text": "a", "expected_energy": "x"}]},
                {"tolerance_pct": 0, "references": [{"text": "a",
                                                     "expected_energy": 1}]}):
        with pytest.raises(ValueError):
            drift.validate_fingerprint(bad)


# --- check_fingerprint -----------------------------------------------------

def test_absent_fingerprint_is_present_false_ok_true(tmp_path):
    present, ok, detail = drift.check_fingerprint(
        str(tmp_path), _score_fixed({}))
    assert present is False and ok is True and detail == ""


def test_in_tolerance_passes(tmp_path):
    drift.write_fingerprint(str(tmp_path), _score_fixed(
        dict(zip(drift.REFERENCE_TEXTS, (20.0, 30.0, 15.0)))), tolerance_pct=15)
    # Re-score slightly off but within 15%.
    present, ok, detail = drift.check_fingerprint(str(tmp_path), _score_fixed(
        dict(zip(drift.REFERENCE_TEXTS, (21.0, 29.0, 16.0)))))
    assert present is True and ok is True, detail


def test_drift_beyond_tolerance_fails_with_reason(tmp_path):
    drift.write_fingerprint(str(tmp_path), _score_fixed(
        dict(zip(drift.REFERENCE_TEXTS, (20.0, 30.0, 15.0)))), tolerance_pct=15)
    # The incident: energies balloon ~30x.
    present, ok, detail = drift.check_fingerprint(str(tmp_path), _score_fixed(
        dict(zip(drift.REFERENCE_TEXTS, (608.0, 700.0, 450.0)))))
    assert present is True and ok is False
    assert "608" in detail and "20" in detail


def test_unreadable_fingerprint_fails_closed(tmp_path):
    path = os.path.join(str(tmp_path), drift.FINGERPRINT_FILE)
    with open(path, "w") as fh:
        fh.write("{not json")
    present, ok, detail = drift.check_fingerprint(str(tmp_path),
                                                  _score_fixed({}))
    assert present is True and ok is False and "unreadable" in detail


# --- GPU-free reproduction (real cost field, stored embedding) -------------

def test_fingerprint_reproduces_energies(tmp_path):
    """Stored cost field + stored reference embedding reproduce the
    fingerprint energy without a live llama-server; a wrong-scale
    embedding (the drift signature) does not."""
    torch = pytest.importorskip("torch")
    from geometric_lens.cost_field import CostField

    dim = 16
    # Seed the init: CostField ends in Softplus, and for some random draws the
    # pre-activation sits far enough negative that both the unit and the scaled
    # embedding saturate to nearly the same energy. The scale-drift assertion
    # below then compares two values within tolerance and fails. Sampling 300
    # unseeded inits puts that at ~13%. Seed 3 separates the two energies by
    # ~155%, an order of magnitude above the 15% tolerance used here.
    torch.manual_seed(3)
    cf = CostField(input_dim=dim)
    cf.set_eval_mode()

    # A fixed unit-norm reference embedding (what a normalized server
    # would serve). score_fn runs the real MLP forward.
    ref_text = drift.REFERENCE_TEXTS[0]
    unit = [1.0 / (dim ** 0.5)] * dim

    def score(vec):
        with torch.no_grad():
            return float(cf(torch.tensor(vec, dtype=torch.float32)
                            .unsqueeze(0)).item())

    drift.write_fingerprint(str(tmp_path), lambda _t: score(unit),
                            texts=[ref_text], tolerance_pct=15)

    # Same embedding reproduces.
    present, ok, _ = drift.check_fingerprint(str(tmp_path),
                                             lambda _t: score(unit))
    assert present and ok

    # A 60x-scaled embedding (the unnormalized incident vector) diverges.
    drifted = [v * 60.0 for v in unit]
    present, ok, detail = drift.check_fingerprint(str(tmp_path),
                                                  lambda _t: score(drifted))
    assert present and not ok, "wrong-scale embedding should fail the check"
