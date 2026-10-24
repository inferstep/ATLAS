"""train_gx must accept optional per-sample weights (in-the-loop labeling)
without breaking the unweighted bench path."""
import os
import sys

import numpy as np
import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

xgboost = pytest.importorskip("xgboost")
pytest.importorskip("sklearn")
from geometric_lens.training import train_gx


def _separable(seed=0):
    rng = np.random.default_rng(seed)
    emb = np.vstack([rng.normal(1.0, 0.3, (60, 16)),
                     rng.normal(-1.0, 0.3, (60, 16))]).tolist()
    labels = [1] * 60 + [0] * 60
    return emb, labels


def test_train_gx_accepts_weights():
    emb, labels = _separable()
    weights = [1.0] * 60 + [0.4] * 60
    res = train_gx({"embeddings": emb, "labels": labels, "weights": weights},
                   pca_dim=8, n_folds=3, max_rounds=40)
    assert res["cv_auc_mean"] > 0.8
    assert res["thresholds"]
    assert set(res["thresholds"]) == {"off_rails", "low", "severe"}


def test_train_gx_unweighted_still_works():
    emb, labels = _separable(seed=1)
    res = train_gx({"embeddings": emb, "labels": labels},
                   pca_dim=8, n_folds=3, max_rounds=40)
    assert res["cv_auc_mean"] > 0.8


def test_train_gx_ignores_mismatched_weights():
    # Wrong-length weights are ignored (uniform), not a crash.
    emb, labels = _separable(seed=2)
    res = train_gx({"embeddings": emb, "labels": labels, "weights": [1.0, 0.4]},
                   pca_dim=8, n_folds=3, max_rounds=40)
    assert res["cv_auc_mean"] > 0.8
