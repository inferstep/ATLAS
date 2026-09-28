"""CxGx allocation gate: C(x) picks the tier, G(x) escalates, k>=3 always.

The predecessor this replaces was C(x)-only, had no floor, and measured
+0.0 pp because it handed k=1 to tasks whose probe had just failed. Every
test here exists to hold one of the three properties that make this one
different: the floor, the escalation, and the live-path budget cap.
"""

from stages import cxgx_gate
from stages.cxgx_gate import (
    CANDIDATE_VERIFY_MS,
    DEFAULT_GX_SEVERE,
    FLOOR_TIER,
    K_FLOOR,
    MIN_CANDIDATE_MS,
    TIER_MIN_K,
    TIER_ORDER,
    allocate,
    base_tier,
    budget_capped_tier,
    estimate_candidate_ms,
    CANDIDATE_LLM_CALLS,
    estimate_escalation_ms,
    gx_escalation,
)
from stages.refinement_loop import ITERATION_LLM_CALLS, MIN_ITERATION_MS


# A calibrated lens that likes the probe: no escalation from G(x).
CLEAN_GX = {"gx_score": 0.9, "gx_available": True,
            "gx_verdict": "likely_correct"}


# --- C(x) base tier -----------------------------------------------------------

def test_cx_base_tier_walks_the_calibrated_energy_ladder():
    assert base_tier(0.05, True) == "nothink"
    assert base_tier(0.15, True) == "standard"
    assert base_tier(0.25, True) == "hard"
    assert base_tier(0.80, True) == "extreme"


def test_uncalibrated_cx_selects_the_floor_tier_not_a_table_row():
    # An uncalibrated normalized energy is not on any comparable scale —
    # routing on it is what made the predecessor track the normalization
    # convention instead of the task.
    assert base_tier(0.02, False) == FLOOR_TIER
    assert base_tier(0.99, False) == FLOOR_TIER


# --- The floor ----------------------------------------------------------------

def test_cx_only_low_energy_still_yields_the_floor():
    # C(x) says trivial (nothink => k=1 on the raw ladder), G(x) agrees.
    # The probe still failed verification, so k may not drop below 3.
    alloc = allocate(cx_normalized=0.01, cx_calibrated=True, **CLEAN_GX)
    assert alloc.base_tier == "nothink"
    assert alloc.gx_escalation == 0
    assert alloc.tier == FLOOR_TIER
    assert alloc.k == K_FLOOR


def test_floor_holds_across_the_whole_bottom_of_the_cx_range():
    for norm in (0.0, 0.001, 0.05, 0.0999):
        assert allocate(cx_normalized=norm, cx_calibrated=True,
                        **CLEAN_GX).k >= K_FLOOR


def test_no_allocation_is_ever_below_the_floor():
    for norm in (0.0, 0.1, 0.25, 0.5, 1.0):
        for gx in (0.0, 0.3, 0.5, 0.7, 1.0):
            for avail in (True, False):
                for cal in (True, False):
                    alloc = allocate(cx_normalized=norm, cx_calibrated=cal,
                                     gx_score=gx, gx_available=avail)
                    assert alloc.k >= K_FLOOR
                    assert TIER_MIN_K[alloc.tier] >= 1
                    assert alloc.tier != "nothink"


# --- G(x) escalation ----------------------------------------------------------

def test_gx_escalation_bands():
    assert gx_escalation(0.90, True) == 0
    assert gx_escalation(DEFAULT_GX_SEVERE, True) == 0        # at the boundary
    assert gx_escalation(0.55, True) == 1                     # below severe
    assert gx_escalation(0.30, True) == 2                     # well below


def test_gx_escalation_raises_the_tier_and_k():
    base = allocate(cx_normalized=0.15, cx_calibrated=True, **CLEAN_GX)
    one = allocate(cx_normalized=0.15, cx_calibrated=True,
                   gx_score=0.55, gx_available=True,
                   gx_verdict="likely_incorrect")
    two = allocate(cx_normalized=0.15, cx_calibrated=True,
                   gx_score=0.30, gx_available=True,
                   gx_verdict="likely_incorrect")

    assert (base.tier, base.k) == ("standard", 3)
    assert (one.tier, one.k) == ("hard", 5)
    assert (two.tier, two.k) == ("extreme", 8)
    assert (base.gx_escalation, one.gx_escalation, two.gx_escalation) == (0, 1, 2)


def test_escalation_lifts_a_nothink_base_before_the_floor_applies():
    # C(x) called it trivial, G(x) says the probe is likely wrong: the
    # escalation runs from the nothink base, then the floor catches what
    # is left. +1 from nothink lands on standard (== the floor); +2 lands
    # on hard, which is strictly more than the floor.
    one = allocate(cx_normalized=0.01, cx_calibrated=True,
                   gx_score=0.55, gx_available=True)
    two = allocate(cx_normalized=0.01, cx_calibrated=True,
                   gx_score=0.20, gx_available=True)
    assert (one.tier, one.k) == ("standard", 3)
    assert (two.tier, two.k) == ("hard", 5)


def test_escalation_saturates_at_the_top_tier():
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.05, gx_available=True)
    assert alloc.base_tier == "extreme"
    assert alloc.tier == "extreme"
    assert alloc.k == TIER_MIN_K["extreme"]


def test_escalation_honors_a_per_model_severe_boundary():
    # A model whose G(x) scores cluster high: 0.8 is below ITS severe
    # boundary and must escalate, though it sits above the default 0.60.
    assert gx_escalation(0.80, True, gx_severe=0.90) == 1
    assert gx_escalation(0.80, True) == 0


# --- Fail-soft ----------------------------------------------------------------

def test_missing_lens_degrades_to_exactly_the_floor():
    # Every argument defaulted: the shape of "the lens told us nothing".
    alloc = allocate()
    assert (alloc.k, alloc.tier) == (K_FLOOR, FLOOR_TIER)
    assert alloc.reason == "uncalibrated"


def test_uncalibrated_gx_never_escalates():
    # G(x) loaded but with no thresholds for this model: its score scale
    # is unknown, so the severe-boundary bands mean nothing.
    for verdict in ("uncalibrated", "unavailable", "error"):
        assert gx_escalation(0.05, True, gx_verdict=verdict) == 0
    alloc = allocate(cx_normalized=0.15, cx_calibrated=True,
                     gx_score=0.05, gx_available=True,
                     gx_verdict="uncalibrated")
    assert (alloc.k, alloc.tier) == (K_FLOOR, FLOOR_TIER)


def test_unavailable_gx_leaves_cx_in_charge():
    alloc = allocate(cx_normalized=0.25, cx_calibrated=True,
                     gx_score=0.5, gx_available=False)
    assert alloc.gx_escalation == 0
    assert (alloc.tier, alloc.k) == ("hard", 5)


def test_allocate_never_raises_on_junk_input():
    for kwargs in (
        {"cx_normalized": float("nan"), "cx_calibrated": True},
        {"cx_normalized": -5.0, "cx_calibrated": True},
        {"cx_normalized": 12.0, "cx_calibrated": True},
        {"gx_score": float("nan"), "gx_available": True},
        {"gx_severe": 0.0, "gx_score": 0.0, "gx_available": True},
    ):
        assert allocate(**kwargs).k >= K_FLOOR


# --- Live-path budget cap -----------------------------------------------------

def test_no_cap_when_the_caller_has_no_budget():
    # The bench arm the gate was measured on: no wall-clock ceiling.
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.1, gx_available=True, remaining_ms=None)
    assert alloc.tier == "extreme"
    assert alloc.capped_from == ""
    assert alloc.reason == "gated"


def test_short_wall_clock_lowers_the_tier():
    # 20s observed per call. One iteration (3 calls) is reserved, leaving
    # too little for the 5 extra candidates `extreme` would add.
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.1, gx_available=True,
                     remaining_ms=150_000.0, observed_llm_call_ms=20_000.0)
    assert alloc.base_tier == "extreme"
    assert alloc.tier != "extreme"
    assert alloc.capped_from == "extreme"
    assert alloc.reason == "budget_capped"
    assert alloc.k < TIER_MIN_K["extreme"]


def test_an_exhausted_wall_clock_caps_below_the_floor():
    """The floor stops the LENS starving generation, not the clock.

    Holding k at 3 when the clock cannot afford 3 spends the whole budget on
    candidates and leaves phase-3 repair — which the cap explicitly reserves
    for — unable to run. Measured across 43 pipeline runs: capped_from was
    empty in all 33 allocations, sessions spent a median 207s of a 180s
    budget on generation alone, and refinement was skipped 19 times with
    7-9s left.
    """
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.1, gx_available=True,
                     remaining_ms=1_000.0, observed_llm_call_ms=60_000.0)
    assert alloc.k < K_FLOOR, "a budget that affords nothing must not buy k=3"
    assert alloc.tier == TIER_ORDER[0]
    assert alloc.capped_from == "extreme"
    assert alloc.reason == "budget_capped"


def test_negative_remaining_budget_yields_the_smallest_allocation():
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.1, gx_available=True,
                     remaining_ms=-10_000.0, observed_llm_call_ms=5_000.0)
    assert (alloc.k, alloc.tier) == (TIER_MIN_K[TIER_ORDER[0]], TIER_ORDER[0])


def test_generous_wall_clock_leaves_the_escalation_intact():
    alloc = allocate(cx_normalized=0.9, cx_calibrated=True,
                     gx_score=0.1, gx_available=True,
                     remaining_ms=3_600_000.0, observed_llm_call_ms=5_000.0)
    assert alloc.tier == "extreme"
    assert alloc.capped_from == ""


def test_cap_is_monotonic_in_the_remaining_budget():
    ks = [allocate(cx_normalized=0.9, cx_calibrated=True, gx_score=0.1,
                   gx_available=True, remaining_ms=float(ms),
                   observed_llm_call_ms=10_000.0).k
          for ms in range(0, 600_000, 20_000)]
    assert ks == sorted(ks), "more budget must never buy fewer candidates"
    assert min(ks) == TIER_MIN_K[TIER_ORDER[0]]
    assert max(ks) == TIER_MIN_K["extreme"]


def test_budget_cap_never_promotes_a_tier():
    assert budget_capped_tier(FLOOR_TIER, 10_000_000.0, 1.0) == FLOOR_TIER


# --- Cost model ---------------------------------------------------------------

def test_candidate_estimate_scales_with_observed_latency():
    # PlanSearch spends two calls per candidate (plan, then code), which is
    # what the estimate has to price: a one-call model halved it and is why
    # the budget cap never fired.
    assert estimate_candidate_ms(10_000.0) == (
        CANDIDATE_LLM_CALLS * 10_000.0 + CANDIDATE_VERIFY_MS)


def test_candidate_estimate_falls_back_to_a_conservative_floor():
    assert estimate_candidate_ms(0.0) == MIN_CANDIDATE_MS
    assert estimate_candidate_ms(-1.0) == MIN_CANDIDATE_MS
    # One call's share of the refinement loop's own no-observation floor.
    assert MIN_CANDIDATE_MS == MIN_ITERATION_MS / ITERATION_LLM_CALLS


def test_floor_tier_escalation_is_free():
    assert estimate_escalation_ms(FLOOR_TIER, 10_000.0) == 0.0
    assert estimate_escalation_ms("nothink", 10_000.0) == 0.0


def test_deeper_tiers_cost_strictly_more():
    costs = [estimate_escalation_ms(t, 10_000.0)
             for t in ("standard", "hard", "extreme")]
    assert costs[0] < costs[1] < costs[2]


def test_tier_token_ratio_tracks_the_budget_forcing_table():
    assert cxgx_gate.tier_token_ratio(FLOOR_TIER) == 1.0
    assert cxgx_gate.tier_token_ratio("extreme") > 1.0
