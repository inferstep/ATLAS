"""Model-specific calibration helpers for Geometric Lens scores."""

import json
import math
import os


def _is_number(value) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def derive_cx_normalization(pass_energy_mean: float,
                            fail_energy_mean: float) -> dict:
    """Derive a sigmoid calibration from one model's C(x) distribution."""
    pass_mean = float(pass_energy_mean)
    fail_mean = float(fail_energy_mean)
    if not math.isfinite(pass_mean) or not math.isfinite(fail_mean):
        raise ValueError("C(x) energy means must be finite")
    separation = fail_mean - pass_mean
    if separation <= 0.0:
        raise ValueError("C(x) calibration requires FAIL energy > PASS energy")
    return {
        "midpoint": (pass_mean + fail_mean) / 2.0,
        "steepness": 4.0 / max(separation, 0.1),
        "pass_energy_mean": pass_mean,
        "fail_energy_mean": fail_mean,
    }


def validate_cx_normalization(value) -> dict:
    """Validate and normalize a deserialized C(x) calibration object."""
    if not isinstance(value, dict):
        raise ValueError("expected a JSON object")
    required = ("midpoint", "steepness")
    if not all(_is_number(value.get(k)) for k in required):
        raise ValueError("expected numeric midpoint and steepness")
    calibrated = {k: float(value[k]) for k in required}
    if not all(math.isfinite(v) for v in calibrated.values()):
        raise ValueError("midpoint and steepness must be finite")
    if calibrated["steepness"] <= 0.0:
        raise ValueError("steepness must be positive")
    # Optional length baseline. C(x) rises with token count whatever the
    # content — measured 2026-08-04, the same function repeated scored 1.84
    # at 20 tokens and 13.91 at 305 — so a fixed midpoint reads long output
    # as bad and short output as good. Fitted on clean repo code:
    # C(x) = intercept + log_slope*ln(tokens), residual sd ~1.2, which
    # separates what the raw energy cannot (real code z=-0.09, a repetition
    # loop z=+2.92, stub spam z=+11.89, where the first two differ by 0.4
    # in raw C(x)).
    baseline = value.get("length_baseline")
    if isinstance(baseline, dict):
        keys = ("intercept", "log_slope", "residual_sd")
        if all(_is_number(baseline.get(k)) for k in keys):
            fitted = {k: float(baseline[k]) for k in keys}
            if (all(math.isfinite(v) for v in fitted.values())
                    and fitted["residual_sd"] > 0.0):
                calibrated["length_baseline"] = fitted

    for key in ("pass_energy_mean", "fail_energy_mean"):
        if key in value:
            if not _is_number(value[key]):
                raise ValueError(f"{key} must be numeric")
            number = float(value[key])
            if not math.isfinite(number):
                raise ValueError(f"{key} must be finite")
            calibrated[key] = number
    if all(key in calibrated for key in ("pass_energy_mean",
                                          "fail_energy_mean")):
        if calibrated["fail_energy_mean"] <= calibrated["pass_energy_mean"]:
            raise ValueError("FAIL energy mean must be greater than PASS energy mean")
    return calibrated


# Residual (in clean-code standard deviations) that maps to 0.5 once a
# length baseline is in play. Real code sits near 0, a repetition loop at
# ~+2.9 and stub spam at ~+11.9, so 2.0 puts the midpoint between healthy
# output and the mildest pathology measured.
_Z_MIDPOINT = 2.0


def normalize_cx_energy(energy: float, calibration,
                        length: int = 0) -> float:
    """Normalize C(x) energy, returning a neutral score if uncalibrated.

    `length` is the text's character count, which is what the baseline
    was fitted on. With both present the returned score reflects
    how far the energy sits above what clean code of that length scores.
    Without either it falls back to the fixed midpoint/steepness sigmoid,
    so calibrations predating the baseline behave exactly as before.
    """
    if calibration is None:
        return 0.5
    calibrated = validate_cx_normalization(calibration)
    baseline = calibrated.get("length_baseline")
    if baseline and length and length > 0:
        # Score the residual from the length-matched norm, not the raw
        # energy: 0.5 sits at the expected value for this length, and the
        # scale is standard deviations of clean code.
        expected = (baseline["intercept"]
                    + baseline["log_slope"] * math.log(length))
        z = (float(energy) - expected) / baseline["residual_sd"] - _Z_MIDPOINT
    else:
        z = calibrated["steepness"] * (float(energy) - calibrated["midpoint"])
    if z >= 0:
        return 1.0 / (1.0 + math.exp(-min(z, 709.0)))
    exp_z = math.exp(max(z, -709.0))
    return exp_z / (1.0 + exp_z)


def save_cx_normalization(save_dir: str, calibration: dict) -> str:
    """Write the selected model's C(x) calibration beside its weights."""
    calibrated = validate_cx_normalization(calibration)
    os.makedirs(save_dir, exist_ok=True)
    path = os.path.join(save_dir, "cx_normalization.json")
    with open(path, "w") as fh:
        json.dump(calibrated, fh, indent=2)
        fh.write("\n")
    return path
