import json
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import main as v3main
import scoring


class _Response:
    def __init__(self, payload):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self):
        return json.dumps(self.payload).encode()


def _payload(thresholds):
    return {
        "enabled": True,
        "gx_available": True,
        "n_tokens": 4,
        "latency_ms": 1.0,
        "thresholds": thresholds,
        "aggregate": {
            "first_off_rails_idx": 2,
            "gx_score_min": 0.21,
            "gx_score_mean": 0.42,
            "cx_norm_max": 0.7,
            "cx_norm_mean": 0.4,
        },
    }


def test_per_step_score_carries_selected_models_thresholds(monkeypatch):
    expected = {"off_rails": 0.3, "low": 0.4, "severe": 0.2}
    monkeypatch.setattr(
        scoring.urllib.request,
        "urlopen",
        lambda *_args, **_kwargs: _Response(_payload(expected)),
    )
    assert v3main.score_candidate_per_step("code")["thresholds"] == expected


def test_per_step_score_does_not_invent_missing_thresholds(monkeypatch):
    monkeypatch.setattr(
        scoring.urllib.request,
        "urlopen",
        lambda *_args, **_kwargs: _Response(_payload(None)),
    )
    assert v3main.score_candidate_per_step("code")["thresholds"] is None
