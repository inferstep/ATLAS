"""Model-specific G(x) operating-threshold calibration."""

import math


def validate_gx_thresholds(value) -> dict:
    """Validate and normalize a deserialized G(x) threshold object."""
    if not isinstance(value, dict):
        raise ValueError("expected a JSON object")
    required = ("off_rails", "low", "severe")
    if not all(isinstance(value.get(key), (int, float))
               and not isinstance(value.get(key), bool)
               for key in required):
        raise ValueError("expected numeric off_rails, low, and severe")
    calibrated = {key: float(value[key]) for key in required}
    if not all(math.isfinite(number) for number in calibrated.values()):
        raise ValueError("thresholds must be finite")
    if not all(0.0 < calibrated[key] <= 1.0 for key in required):
        raise ValueError("thresholds must be in (0, 1]")
    if not (calibrated["severe"] <= calibrated["off_rails"]
            <= calibrated["low"]):
        raise ValueError("expected severe <= off_rails <= low")
    # Optional: the cutoff for the MEAN G(x) score.
    #
    # The three required thresholds above are read against gx_score_min,
    # which is a minimum over every token and so falls with length whatever
    # the content — measured 2026-08-04 on one function repeated, 0.468 at
    # 20 tokens down to 0.305 at 305. Real code (0.325), a repetition loop
    # (0.320) and stub spam (0.286) land on top of each other there.
    # gx_score_mean holds across the same 15x length change and separates
    # them (0.594 / 0.485 / 0.467), so a mean cutoff is the one that can
    # actually fire. Artifacts predating it keep the min-based behaviour.
    mean_cut = value.get("severe_mean")
    if (isinstance(mean_cut, (int, float)) and not isinstance(mean_cut, bool)
            and math.isfinite(float(mean_cut))
            and 0.0 < float(mean_cut) <= 1.0):
        calibrated["severe_mean"] = float(mean_cut)
    return calibrated


def derive_gx_thresholds(pass_scores) -> dict:
    """Derive {off_rails, low, severe} from one model's PASS scores.

    At least 20 positive samples are required. Returning another model's
    historical defaults would make interventions silently model-dependent.
    """
    import numpy as np

    if pass_scores is None or len(pass_scores) < 20:
        raise ValueError(
            "at least 20 PASS scores are required to calibrate G(x) thresholds"
        )

    def clamp(value):
        return float(min(0.6, max(0.02, value)))

    severe = clamp(float(np.percentile(pass_scores, 5)))
    off_rails = clamp(float(np.percentile(pass_scores, 10)))
    low = clamp(float(np.percentile(pass_scores, 20)))
    off_rails = max(off_rails, severe)
    low = max(low, off_rails)
    return validate_gx_thresholds({
        "off_rails": round(off_rails, 3),
        "low": round(low, 3),
        "severe": round(severe, 3),
    })
