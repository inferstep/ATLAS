"""get_model_info reports which lens models and calibrations are loaded.

/health copies these fields, and the proxy reads `gx_loaded` from /health to
decide whether the lens can score. The other tests replace get_model_info
with a stub, so this file calls the real one.
"""
import os
import sys

import pytest
import torch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from geometric_lens import service

CX_NORMALIZATION = {"midpoint": 20.0, "steepness": 0.2}
GX_THRESHOLDS = {"severe": 0.2, "off_rails": 0.3, "low": 0.4}


# object() stands in for the XGBoost model: get_model_info only checks that
# one is there. The calibrations are loaded in the other row, so each field
# must follow its own input.
@pytest.mark.parametrize("gx_model, cx_normalization, gx_thresholds, expected", [
    pytest.param(object(), None, None,
                 {"cx_calibrated": False, "gx_loaded": True, "gx_calibrated": False,
                  "gx_thresholds": None, "gx_type": "xgboost"},
                 id="gx-model-without-calibrations"),
    pytest.param(None, CX_NORMALIZATION, GX_THRESHOLDS,
                 {"cx_calibrated": True, "gx_loaded": False, "gx_calibrated": True,
                  "gx_thresholds": GX_THRESHOLDS, "gx_type": "none"},
                 id="calibrations-without-a-gx-model"),
])
def test_model_info_reports_what_is_loaded(
        monkeypatch, gx_model, cx_normalization, gx_thresholds, expected):
    monkeypatch.setattr(service, "_models_loaded", True)
    monkeypatch.setattr(service, "_cost_field", torch.nn.Linear(4, 1))
    monkeypatch.setattr(service, "_gx_xgboost", gx_model)
    monkeypatch.setattr(service, "_cx_normalization", cx_normalization)
    monkeypatch.setattr(service, "_gx_thresholds", gx_thresholds)

    info = service.get_model_info()

    assert {field: info[field] for field in expected} == expected
