"""Unit tests for per-model G(x) threshold derivation (derive_gx_thresholds).

The thresholds must adapt to the selected model's score scale rather than
borrowing fixed operating points from another model.
"""
import os
import sys

import numpy as np
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from geometric_lens.thresholds import (
    derive_gx_thresholds,
    validate_gx_thresholds,
)


@pytest.mark.parametrize("seed, mean, sd", [
    pytest.param(0, 0.46, 0.05, id="mid-scale-pass-scores"),
    pytest.param(1, 0.85, 0.18, id="high-pass-scores-with-a-broad-tail"),
])
def test_derived_thresholds_sit_on_the_models_pass_scale(seed, mean, sd):
    pass_scores = np.clip(np.random.default_rng(seed).normal(mean, sd, 500), 0, 1)
    t = derive_gx_thresholds(pass_scores)
    assert t["low"] > 0.25, f"low too low for a model centered at {mean}: {t}"
    assert t["severe"] < t["low"], f"severe must be strictest: {t}"
    assert t["severe"] <= t["off_rails"] <= t["low"], f"ordering wrong: {t}"
    # All within the clamp band.
    assert all(0.02 <= t[k] <= 0.6 for k in t), t


@pytest.mark.parametrize("pass_scores", [
    pytest.param(np.array([0.4, 0.5, 0.6]), id="three-scores"),
    pytest.param(None, id="no-scores"),
])
def test_too_few_pass_scores_refuse_to_fabricate_calibration(pass_scores):
    with pytest.raises(ValueError, match="at least 20 PASS"):
        derive_gx_thresholds(pass_scores)


@pytest.mark.parametrize("value", [
    pytest.param({}, id="empty"),
    pytest.param({"severe": True, "off_rails": 0.2, "low": 0.3}, id="a-bool-threshold"),
    pytest.param({"severe": 0.4, "off_rails": 0.3, "low": 0.2}, id="reversed-order"),
    pytest.param({"severe": np.nan, "off_rails": 0.2, "low": 0.3}, id="a-nan-threshold"),
])
def test_invalid_threshold_objects_are_rejected(value):
    with pytest.raises(ValueError):
        validate_gx_thresholds(value)
