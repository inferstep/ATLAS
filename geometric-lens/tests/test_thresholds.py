"""G(x) threshold validation.

The veto that consumes these thresholds reads the MEAN G(x) score. Its
cutoff is optional in the file and has to survive validation, which rebuilds
the required triple into a fresh dict.
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

def test_severe_mean_is_carried_through_validation():
    """The veto reads the MEAN G(x) score, so its cutoff has to survive
    validation — the required triple is rebuilt into a fresh dict and used
    to drop every other key.

    Measured 2026-08-04: gx_score_min is a minimum over every token and
    falls with length whatever the content (0.468 at 20 tokens to 0.305 at
    305 on one function repeated), so real code 0.325, a repetition loop
    0.320 and stub spam 0.286 sit on top of each other — which is why
    severe=0.28 never fired once across 56 sessions. gx_score_mean holds
    across the same length change and separates them: 0.594 / 0.485 / 0.467.
    """
    from geometric_lens.thresholds import validate_gx_thresholds

    base = {"off_rails": 0.34, "low": 0.34, "severe": 0.28}
    assert "severe_mean" not in validate_gx_thresholds(base)

    out = validate_gx_thresholds({**base, "severe_mean": 0.52})
    assert out["severe_mean"] == 0.52
    # The required triple is unaffected.
    assert out["severe"] == 0.28 and out["off_rails"] == 0.34

    for bad in (0, 1.5, -0.1, "0.5", True, float("nan")):
        assert "severe_mean" not in validate_gx_thresholds(
            {**base, "severe_mean": bad}), f"accepted {bad!r}"
