import io
import json
import sys
from pathlib import Path

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import main as v3main
import scoring


@pytest.mark.parametrize("thresholds", [
    pytest.param({"off_rails": 0.3, "low": 0.4, "severe": 0.2}, id="the selected model's thresholds"),
    pytest.param(None, id="no thresholds, none invented"),
])
def test_per_step_score_carries_the_lens_thresholds(monkeypatch, thresholds):
    answer = {
        "enabled": True,
        "gx_available": True,
        "n_tokens": 4,
        "latency_ms": 1.0,
        "thresholds": thresholds,
        "aggregate": {"first_off_rails_idx": 2, "gx_score_min": 0.21, "gx_score_mean": 0.42,
                      "cx_norm_max": 0.7, "cx_norm_mean": 0.4},
    }
    monkeypatch.setattr(scoring.urllib.request, "urlopen",
                        lambda *_args, **_kwargs: io.BytesIO(json.dumps(answer).encode()))

    assert v3main.score_candidate_per_step("code")["thresholds"] == thresholds
