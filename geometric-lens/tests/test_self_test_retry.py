"""A self-test that fails because llama-server is not answering yet retries.

The lens is required (docs/adr/0011): a lens whose self-test failed refuses
every request until the self-test passes. /ready re-runs a failed self-test
only when the failure was retryable, so a passing model-server hiccup at boot
must be classified as retryable, and must not read as drift.

Three ways it was not:
  - evaluate_energy turned any error into (0.0, 0.0), which the self-test
    reported as "C(x) evaluation returned zeros", not retryable;
  - check_fingerprint turned a scoring error into a drift verdict;
  - the retry rule named urllib's HTTPError, and the transport now raises
    ModelServerHTTPError, so a 503 while llama loads never retried.
"""
import importlib
import os
import sys
import urllib.error

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "..")
sys.path.insert(0, ROOT)

from geometric_lens import drift
from geometric_lens import embedding_extractor as ee
from geometric_lens import service
from geometric_lens.model_transport import ModelServerHTTPError


@pytest.fixture(scope="module")
def main_module(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("lens-self-test")
    os.environ["ATLAS_SERVICE_TOKEN_FILE"] = str(tmp / "no-token")
    for name in ("main", "config"):
        mod = sys.modules.get(name)
        if mod is not None and not str(getattr(mod, "__file__", "")).startswith(
                os.path.abspath(ROOT)):
            del sys.modules[name]
    sys.path.insert(0, os.path.abspath(ROOT))
    cwd = os.getcwd()
    os.chdir(ROOT)
    try:
        return importlib.import_module("main")
    finally:
        os.chdir(cwd)


def _loaded(monkeypatch, energy, models_dir=None):
    """A lens with its models loaded; `energy` is evaluate_energy."""
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: True)
    monkeypatch.setattr(service, "get_model_info", lambda: {
        "loaded": True, "gx_loaded": True, "gx_type": "xgboost",
        "cx_calibrated": True, "gx_calibrated": True, "artifact_model": "m"})
    monkeypatch.setattr(service, "_cost_field", None)
    monkeypatch.setattr(ee, "extract_embedding", lambda text: [0.1, 0.2, 0.3, 0.4])
    monkeypatch.setattr(service, "evaluate_energy", energy)
    monkeypatch.setattr(service, "active_models_dir", lambda: models_dir)


def _raise(exc):
    def f(*_a, **_k):
        raise exc
    return f


def test_evaluate_energy_raises_instead_of_returning_zeros(monkeypatch):
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: True)
    monkeypatch.setattr(ee, "extract_embedding",
                        _raise(urllib.error.URLError("connection refused")))
    monkeypatch.setattr(service, "_snapshot_weights",
                        lambda: (None, None, None, None, None, {}, None))
    with pytest.raises(urllib.error.URLError):
        service.evaluate_energy("def f(): pass")


def test_a_reference_that_cannot_be_scored_is_not_drift(tmp_path):
    drift.write_fingerprint(str(tmp_path), lambda _t: 20.0)
    with pytest.raises(urllib.error.URLError):
        drift.check_fingerprint(str(tmp_path),
                                _raise(urllib.error.URLError("connection reset")))
    present, ok, _ = drift.check_fingerprint(str(tmp_path), lambda _t: 20.0)
    assert present and ok


@pytest.mark.parametrize("exc, retryable", [
    (urllib.error.URLError("connection refused"), True),
    (ConnectionResetError("reset by peer"), True),
    (TimeoutError("timed out"), True),
    (ModelServerHTTPError(503, "Loading model", "http://llama/embedding"), True),
    (ModelServerHTTPError(400, "embeddings disabled", "http://llama/embedding"), False),
    (ValueError("dimension mismatch"), False),
])
def test_the_self_test_retries_only_a_model_server_that_is_not_answering(
        main_module, monkeypatch, exc, retryable):
    _loaded(monkeypatch, _raise(exc))
    main_module._run_lens_self_test()
    state = main_module._BOOT_STATE
    assert state["self_test_pass"] is False
    assert state["self_test_retryable"] is retryable, state["self_test_error"]


def test_a_fingerprint_scoring_failure_retries_and_is_not_drift(
        main_module, monkeypatch, tmp_path):
    drift.write_fingerprint(str(tmp_path), lambda _t: 20.0)
    calls = {"n": 0}

    def energy(_text):
        calls["n"] += 1
        if calls["n"] == 1:
            return (20.0, 0.5)  # the self-test's own evaluation
        raise urllib.error.URLError("connection reset")  # then a fingerprint reference

    _loaded(monkeypatch, energy, models_dir=str(tmp_path))
    main_module._run_lens_self_test()
    state = main_module._BOOT_STATE
    assert state["self_test_pass"] is False
    assert state["self_test_retryable"] is True
    assert state["fingerprint_ok"] is not False, "a failed measurement read as drift"


def test_a_healthy_lens_passes(main_module, monkeypatch, tmp_path):
    drift.write_fingerprint(str(tmp_path), lambda _t: 20.0)
    _loaded(monkeypatch, lambda _t: (20.0, 0.5), models_dir=str(tmp_path))
    main_module._run_lens_self_test()
    state = main_module._BOOT_STATE
    assert state["self_test_pass"] is True, state["self_test_error"]
    assert state["fingerprint_ok"] is True
