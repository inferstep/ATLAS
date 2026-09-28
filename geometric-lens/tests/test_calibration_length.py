"""C(x) is scored against a length-matched baseline.

C(x) rises with token count whatever the content — measured 2026-08-04, the
same function repeated scored 1.84 at 20 tokens and 13.91 at 305. A fixed
midpoint therefore reads long output as bad and short output as good, which
is why cx_norm_max read exactly 1.000 on every sample across three live
runs and the signal was unusable.

Fitted on clean repo code, C(x) = -6.965 + 2.530*ln(characters) with residual
sd 1.222. Characters, not tokens: the two do not track each other, and
mixing the units puts a sample in the wrong baseline. That separates what the raw energy cannot: real code z=-0.09, a
repetition loop z=+2.92, stub spam z=+11.89 — and the first two differ by
only 0.4 in raw C(x).
"""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from geometric_lens.calibration import (
    normalize_cx_energy, validate_cx_normalization,
)

BASE = {"midpoint": 10.53, "steepness": 1.565}
FITTED = {**BASE, "length_baseline": {
    "intercept": -6.965, "log_slope": 2.530, "residual_sd": 1.222}}


def test_baseline_survives_validation_and_is_optional():
    assert "length_baseline" not in validate_cx_normalization(BASE)
    out = validate_cx_normalization(FITTED)
    assert out["length_baseline"]["log_slope"] == 2.530
    # Malformed or degenerate baselines are dropped, not fatal.
    for bad in ({"intercept": 1.0}, {"intercept": 1.0, "log_slope": 2.0,
                                     "residual_sd": 0.0}, "nope", None):
        assert "length_baseline" not in validate_cx_normalization(
            {**BASE, "length_baseline": bad})


def test_clean_code_scores_alike_at_any_length():
    """The defect: raw C(x) on clean repo code rose 8.53 -> 14.80 across
    length bands, so a fixed midpoint called long files bad.

    Measured band means, scored against the baseline they were fitted on,
    must land together and near neutral. (Repeating one function to make a
    file longer is NOT the test — that makes the content genuinely
    repetitive, which the lens is supposed to flag; the earlier
    demonstration of the length effect conflated the two.)
    """
    short = normalize_cx_energy(8.53, FITTED, length=400)
    long_ = normalize_cx_energy(14.80, FITTED, length=5000)
    assert abs(short - long_) < 0.15, (
        f"length still dominates: {short:.2f} vs {long_:.2f}")
    assert short < 0.5 and long_ < 0.5, "clean code must sit below neutral"


def test_pathology_separates_from_real_code():
    real = normalize_cx_energy(12.61, FITTED, length=2400)
    repetition = normalize_cx_energy(12.22, FITTED, length=480)
    stub = normalize_cx_energy(24.71, FITTED, length=880)
    assert real < 0.5 < repetition < stub
    # Raw energies say the opposite — real code scores HIGHER than the loop.
    assert 12.61 > 12.22


def test_without_a_baseline_nothing_changes():
    """Calibrations predating the baseline keep the fixed-midpoint sigmoid."""
    assert (normalize_cx_energy(12.0, BASE, length=500)
            == normalize_cx_energy(12.0, BASE))
    assert normalize_cx_energy(12.0, None) == 0.5
