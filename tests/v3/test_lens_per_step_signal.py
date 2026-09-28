"""Per-step lens scoring must not report a tie as a verdict.

`/internal/lens/score-per-step` answers 200 with an empty aggregate when
it cannot score — a pooling mode it can't read per-token vectors from, a
lens outage, an empty candidate. Filling that response's missing fields
with their defaults yields gx=0.500 for every candidate, which is
indistinguishable from a real verdict that happened to tie, and leaves
candidate selection with nothing to choose on while looking healthy.
Such an answer is reported as a typed failure and nothing else.
"""

import io
import json
from unittest.mock import patch

import scoring


def _response(payload):
    return io.BytesIO(json.dumps(payload).encode())


def _score(payload):
    with patch("urllib.request.urlopen") as urlopen:
        urlopen.return_value.__enter__.return_value = _response(payload)
        return scoring.score_candidate_per_step("def f():\n    return 1\n")


def test_zero_tokens_reports_no_signal_not_a_neutral_score():
    """The exact shape served under `--pooling mean`: enabled, 200 OK,
    n_tokens 0, empty aggregate. It carries the failure and no number."""
    out = _score({
        "enabled": True,
        "n_tokens": 0,
        "aggregate": {},
        "error": "ValueError: per-step evaluation failed",
    })
    assert set(out) == {"failure"}
    assert out["failure"]["kind"] == "lens_error"


def test_real_scores_survive():
    out = _score({
        "enabled": True,
        "n_tokens": 12,
        "gx_available": True,
        "latency_ms": 140.0,
        "aggregate": {
            "first_off_rails_idx": 4,
            "gx_score_min": 0.21,
            "gx_score_mean": 0.44,
            "cx_norm_max": 0.9,
            "cx_norm_mean": 0.5,
        },
    })
    assert out["n_tokens"] == 12
    assert out["gx_score_min"] == 0.21
    assert out["first_off_rails_idx"] == 4


def test_a_disabled_lens_is_unavailable():
    """The lens is required: switched off, it stops the run instead of
    reporting no signal."""
    import pytest
    with pytest.raises(scoring.LensUnavailable):
        _score({"enabled": False})
