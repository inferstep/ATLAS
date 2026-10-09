"""A non-finite number is not a score.

A cost field, a calibration, or an XGBoost forward can produce NaN or an
infinity (a degenerate weight, an overflowed exponent, a corrupted
artifact). Python arithmetic carries such a value silently, `json.dumps`
emits it as the non-standard tokens `NaN` / `Infinity`, and a consumer that
reads floats would rank on it. The service therefore treats a non-finite
raw energy, normalized energy, or G(x) score as a score that did not
happen: the answer is the typed unscored shape, never a number. A finite
negative or zero energy is a real score and stays one.
"""
import json
import math
import os
import sys

import numpy as np
import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "..")
sys.path.insert(0, ROOT)

from geometric_lens import embed_capacity as ec
from geometric_lens import embedding_extractor as ee
from geometric_lens import service

# The HTTP surface (no NaN / Infinity token ever leaves the service) is
# pinned beside the other endpoint tests in test_embed_capacity.py, which
# owns the one app client a process can boot.

DIM = 4
NON_FINITE = [float("nan"), float("inf"), float("-inf")]
CX_CFG = {"midpoint": 0.5, "steepness": 4.0}
THRESHOLDS = {"off_rails": 0.3, "low": 0.4, "severe": 0.2}


class _ConstField:
    """A C(x) that answers one value for every row, or a column of values."""

    def __init__(self, value):
        self.value = value

    def __call__(self, x):
        import torch
        if isinstance(self.value, list):
            return torch.tensor(self.value, dtype=torch.float32).reshape(-1, 1)
        return torch.full((x.shape[0], 1), float(self.value))

    def parameters(self):
        import torch
        return iter([torch.zeros(1, DIM)])


class _ConstGx:
    def __init__(self, p_correct):
        self.p = p_correct

    def predict_proba(self, x):
        p = np.full(x.shape[0], self.p, dtype=float)
        return np.stack([1.0 - p, p], axis=1)


def _weights(monkeypatch, field, gx=None):
    monkeypatch.setattr(
        service, "_snapshot_weights",
        lambda: (field, gx, np.eye(DIM, dtype=np.float32) if gx else None,
                 np.zeros(DIM, dtype=np.float32) if gx else None, None,
                 CX_CFG, THRESHOLDS if gx else None))


@pytest.fixture(autouse=True)
def _lens(monkeypatch):
    # No model server: both extractors are stubbed below.
    monkeypatch.setenv("LLAMA_URL", "http://127.0.0.1:9")
    monkeypatch.setenv("LLAMA_EMBED_URL", "http://127.0.0.1:9")
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: True)
    monkeypatch.setattr(ee, "extract_embedding", lambda text: [0.5] * DIM)
    monkeypatch.setattr(ee, "extract_per_token",
                        lambda text: ([[0.5] * DIM] * 3, DIM))
    _weights(monkeypatch, _ConstField(0.25), _ConstGx(0.8))
    ec.reset()
    yield
    ec.reset()


def _assert_unscored_combined(out):
    assert out["enabled"] is True
    assert out["scored"] is False
    assert out["failure"]["kind"] == ec.KIND_NONFINITE
    for key in ("cx_energy", "cx_normalized", "gx_score"):
        assert out[key] is None, key
    assert out["cx_calibrated"] is False and out["gx_available"] is False
    assert out["verdict"] == "unscored"
    json.dumps(out, allow_nan=False)


def _assert_unscored_per_step(out):
    assert out["scored"] is False
    assert out["failure"]["kind"] == ec.KIND_NONFINITE
    assert out["per_step"] == [] and out["aggregate"] == {}
    assert out["n_tokens"] == 0
    json.dumps(out, allow_nan=False)


# --- the service --------------------------------------------------------------------

@pytest.mark.parametrize("value", NON_FINITE)
def test_nonfinite_raw_energy_is_unscored(monkeypatch, value):
    _weights(monkeypatch, _ConstField(value), _ConstGx(0.8))
    out = service.evaluate_combined("def f(): pass")
    _assert_unscored_combined(out)
    assert out["failure"]["field"] == "cx_energy"


def test_nonfinite_normalized_energy_is_unscored(monkeypatch):
    # The raw energy is finite; the calibration step yields NaN.
    monkeypatch.setattr(service, "_normalize_cx_energy",
                        lambda energy, cfg=None, length=0: float("nan"))
    out = service.evaluate_combined("def f(): pass")
    _assert_unscored_combined(out)
    assert out["failure"]["field"] == "cx_normalized"


@pytest.mark.parametrize("value", NON_FINITE)
def test_nonfinite_gx_score_is_unscored(monkeypatch, value):
    _weights(monkeypatch, _ConstField(0.25), _ConstGx(value))
    out = service.evaluate_combined("def f(): pass")
    _assert_unscored_combined(out)
    assert out["failure"]["field"] == "gx_score"


@pytest.mark.parametrize("value", NON_FINITE)
def test_one_nonfinite_token_unscores_the_per_step_answer(monkeypatch, value):
    _weights(monkeypatch, _ConstField([0.1, value, 0.3]), _ConstGx(0.8))
    _assert_unscored_per_step(service.evaluate_per_step("def f(): pass"))


def test_nonfinite_per_step_gx_is_unscored(monkeypatch):
    _weights(monkeypatch, _ConstField(0.25), _ConstGx(float("nan")))
    _assert_unscored_per_step(service.evaluate_per_step("def f(): pass"))


@pytest.mark.parametrize("value", [np.float16(0.125), np.float32(0.625), np.float64(-0.25)])
def test_finite_numpy_scalars_are_builtin_scores(value):
    out = ec.finite(value, "gx_score")
    assert type(out) is float
    assert out == pytest.approx(float(value))


def test_numpy_float32_xgboost_probability_stays_scored(monkeypatch):
    class _Float32Gx(_ConstGx):
        def predict_proba(self, x):
            p = np.full(x.shape[0], self.p, dtype=np.float32)
            return np.stack([1.0 - p, p], axis=1)

    _weights(monkeypatch, _ConstField(0.25), _Float32Gx(0.8))
    out = service.evaluate_combined("def f(): pass")
    assert out["scored"] is True
    assert type(out["gx_score"]) is float
    assert out["gx_score"] == pytest.approx(0.8)


@pytest.mark.parametrize(
    "value",
    [np.float16(float("nan")), np.float32(float("inf")), np.float64(float("-inf"))],
)
def test_nonfinite_numpy_scalars_are_rejected(value):
    with pytest.raises(ec.NonFiniteScoreError) as exc:
        ec.finite(value, "gx_score")
    assert exc.value.field == "gx_score"


@pytest.mark.parametrize("value", [True, "0.5", None])
def test_bool_and_nonnumeric_values_are_not_scores(value):
    with pytest.raises(ec.NonFiniteScoreError):
        ec.finite(value, "gx_score")


@pytest.mark.parametrize("value", [-2.5, 0.0, -0.0, 1e30])
def test_finite_energies_including_negative_and_zero_stay_scores(monkeypatch, value):
    _weights(monkeypatch, _ConstField(value), _ConstGx(0.8))
    out = service.evaluate_combined("def f(): pass")
    assert out["scored"] is True and out.get("failure") is None
    assert out["cx_energy"] == pytest.approx(value, rel=1e-6)  # float32 forward
    assert math.isfinite(out["cx_normalized"])
    per_step = service.evaluate_per_step("def f(): pass")
    assert per_step["scored"] is True and per_step["n_tokens"] == 3
    assert per_step["aggregate"]["cx_energy_min"] == pytest.approx(value, rel=1e-6)


def test_the_lens_has_no_off_switch(monkeypatch):
    """GEOMETRIC_LENS_ENABLED is removed (ADR 0011): a leftover `false` in an
    old .env changes nothing, and the lens still scores."""
    monkeypatch.setenv("GEOMETRIC_LENS_ENABLED", "false")
    out = service.evaluate_combined("def f(): pass")
    assert out["enabled"] is True and out["scored"] is True
    per_step = service.evaluate_per_step("def f(): pass")
    assert per_step["scored"] is True and per_step["n_tokens"] == 3


def test_a_lens_without_models_is_unchanged(monkeypatch):
    """No model loaded: the answer says so (enabled false), with no failure
    and no computed number. The lens has no off switch (ADR 0011); this is
    the only state that answers enabled false."""
    _weights(monkeypatch, _ConstField(float("nan")))
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: False)
    out = service.evaluate_combined("def f(): pass")
    assert out["enabled"] is False and out["cx_energy"] == 0.0
    assert "failure" not in out
    per_step = service.evaluate_per_step("def f(): pass")
    assert per_step["enabled"] is False and per_step["n_tokens"] == 0


def test_nonfinite_failure_is_its_own_kind():
    failure = ec.failure_from_exception(ec.NonFiniteScoreError("gx_score", float("nan")))
    assert failure["kind"] == ec.KIND_NONFINITE
    assert failure["field"] == "gx_score"
    assert failure["detail"] == "nan"
    json.dumps(failure, allow_nan=False)
