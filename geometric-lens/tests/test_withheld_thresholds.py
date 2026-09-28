"""A threshold is only returned beside the score it is for.

Without a G(x) model the per-step G(x) scores are 0.5 placeholders. The
thresholds used to be returned beside them all the same, and a consumer
comparing 0.5 with severe_mean 0.52 read every candidate as severe: V3's
veto would have vetoed every sandbox-passing candidate.
"""
import os
import sys

import numpy as np

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, ".."))

from geometric_lens import embed_capacity as ec  # noqa: E402
from geometric_lens import embedding_extractor as ee  # noqa: E402
from geometric_lens import service  # noqa: E402

DIM = 4
THRESHOLDS = {"off_rails": 0.3, "low": 0.4, "severe": 0.2, "severe_mean": 0.52}


class _Field:
    def __call__(self, x):
        import torch
        return torch.full((x.shape[0], 1), 0.25)

    def parameters(self):
        import torch
        return iter([torch.zeros(1, DIM)])


def _stub(monkeypatch, gx_loaded):
    monkeypatch.setenv("LLAMA_URL", "http://127.0.0.1:9")
    monkeypatch.setenv("LLAMA_EMBED_URL", "http://127.0.0.1:9")
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: True)
    monkeypatch.setattr(ee, "extract_per_token", lambda text: ([[0.5] * DIM] * 3, DIM))

    class _Gx:
        def predict_proba(self, x):
            p = np.full(x.shape[0], 0.8, dtype=float)
            return np.stack([1.0 - p, p], axis=1)
    gx = _Gx() if gx_loaded else None
    monkeypatch.setattr(
        service, "_snapshot_weights",
        lambda: (_Field(), gx, np.eye(DIM, dtype=np.float32) if gx else None,
                 np.zeros(DIM, dtype=np.float32) if gx else None, None,
                 {"midpoint": 0.5, "steepness": 4.0}, dict(THRESHOLDS)))
    ec.reset()


def test_no_thresholds_beside_placeholder_gx_scores(monkeypatch):
    _stub(monkeypatch, gx_loaded=False)
    out = service.evaluate_per_step("def f(): pass")
    assert out["scored"] is True and out["gx_available"] is False
    assert out["thresholds"] is None


def test_thresholds_are_returned_beside_real_gx_scores(monkeypatch):
    _stub(monkeypatch, gx_loaded=True)
    out = service.evaluate_per_step("def f(): pass")
    assert out["gx_available"] is True
    assert out["thresholds"] == THRESHOLDS
