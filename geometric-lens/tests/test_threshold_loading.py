import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from geometric_lens import service


def test_missing_cx_calibration_returns_neutral_score(tmp_path, monkeypatch):
    monkeypatch.setattr(service, "_cx_normalization", {"midpoint": 100.0, "steepness": 1.0})
    service._load_cx_normalization(str(tmp_path))
    assert service._cx_normalization is None
    assert service._normalize_cx_energy(-500.0) == 0.5


def test_valid_cx_calibration_is_loaded_for_selected_model(tmp_path):
    expected = {"midpoint": 12.0, "steepness": 0.4}
    (tmp_path / "cx_normalization.json").write_text(json.dumps(expected))
    service._load_cx_normalization(str(tmp_path))
    assert service._cx_normalization == expected


def test_invalid_cx_calibration_is_disabled(tmp_path):
    (tmp_path / "cx_normalization.json").write_text(
        json.dumps({"midpoint": 12.0, "steepness": 0.0})
    )
    service._load_cx_normalization(str(tmp_path))
    assert service._cx_normalization is None


def test_a_pickled_gx_model_is_not_loaded_and_the_log_says_how_to_rebuild(tmp_path, monkeypatch, caplog):
    for name in ("_gx_xgboost", "_gx_pca_components", "_gx_pca_mean", "_gx_top_dims"):
        monkeypatch.setattr(service, name, getattr(service, name))
    (tmp_path / "gx_xgboost.pkl").write_bytes(b"never read")
    (tmp_path / "gx_weights.json").write_text("{}")

    service._load_gx_models(str(tmp_path))

    assert service._gx_xgboost is None
    assert "atlas lens build" in caplog.text


def test_missing_threshold_file_disables_calibrated_verdicts(tmp_path, monkeypatch):
    monkeypatch.setattr(service, "_gx_thresholds", {"off_rails": 0.9, "low": 0.9, "severe": 0.9})
    service._load_gx_thresholds(str(tmp_path))
    assert service._gx_thresholds is None


def test_valid_threshold_file_loads_selected_model_calibration(tmp_path):
    expected = {"off_rails": 0.31, "low": 0.42, "severe": 0.18}
    (tmp_path / "gx_thresholds.json").write_text(json.dumps(expected))
    service._load_gx_thresholds(str(tmp_path))
    assert service._gx_thresholds == expected


def test_invalid_threshold_file_disables_interventions(tmp_path):
    invalid = {"off_rails": 0.4, "low": 0.2, "severe": 0.3}
    (tmp_path / "gx_thresholds.json").write_text(json.dumps(invalid))
    service._load_gx_thresholds(str(tmp_path))
    assert service._gx_thresholds is None
