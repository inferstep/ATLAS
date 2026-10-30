"""train_cost_field stops early and keeps the best epoch's weights.

It measures the test AUC after every epoch, stops once `patience` epochs pass
without a better one, and returns the model as it was at the best epoch.
"""
import os
import random
import re
import sys

import torch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from geometric_lens.training import train_cost_field


def test_training_stops_after_patience_epochs_and_keeps_the_best_weights(capsys):
    # Two clusters far apart: the test AUC soon reaches its best and cannot
    # improve on it, so training has to stop on its own.
    rng = random.Random(0)
    passing = [[rng.gauss(1.0, 0.1) for _ in range(4)] for _ in range(10)]
    failing = [[rng.gauss(-1.0, 0.1) for _ in range(4)] for _ in range(10)]
    data = {"embeddings": passing + failing, "labels": [1] * 10 + [0] * 10}

    result = train_cost_field(data, epochs=50, patience=2, seed=0)

    log = capsys.readouterr().out
    early_stop = re.search(r"Early stop at epoch (\d+):.*@ epoch (\d+)\)", log)
    assert early_stop, log
    stopped_at, best_epoch = int(early_stop[1]), int(early_stop[2])
    assert stopped_at < 50, log
    assert stopped_at == len(result["loss_history"]), log
    assert stopped_at - best_epoch == 2, log

    # Training again with the same seed for exactly the best epoch's count
    # gives the weights the model had at the best epoch.
    at_best_epoch = train_cost_field(data, epochs=best_epoch, patience=0, seed=0)
    kept = result["model"].state_dict()
    for name, weights in at_best_epoch["model"].state_dict().items():
        assert torch.equal(kept[name], weights), name
