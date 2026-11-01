"""CxGx allocation gate: C(x) picks the tier, G(x) escalates, k>=3 always.

The predecessor this replaces was C(x)-only, had no floor, and measured
+0.0 pp because it handed k=1 to tasks whose probe had just failed. Every
test here exists to hold one of the three properties that make this one
different: the floor, the escalation, and the live-path budget cap.
"""

import pytest

from stages import cxgx_gate
from stages.cxgx_gate import (
    CANDIDATE_LLM_CALLS,
    CANDIDATE_VERIFY_MS,
    DEFAULT_GX_SEVERE,
    FLOOR_TIER,
    K_FLOOR,
    MIN_CANDIDATE_MS,
    TIER_MIN_K,
    TIER_ORDER,
    Allocation,
    allocate,
    base_tier,
    budget_capped_tier,
    estimate_candidate_ms,
    estimate_escalation_ms,
    gx_escalation,
)
from stages.refinement_loop import ITERATION_LLM_CALLS, MIN_ITERATION_MS


# A calibrated lens that likes the probe: no escalation from G(x).
CLEAN_GX = {"gx_score": 0.9, "gx_available": True,
            "gx_verdict": "likely_correct"}

# C(x) says extreme and G(x) says the probe is far off: the deepest
# allocation there is, before any budget cap.
EXTREME_PROBE = {"cx_normalized": 0.9, "cx_calibrated": True,
                 "gx_score": 0.1, "gx_available": True}


# --- C(x) base tier and G(x) escalation ---------------------------------------

@pytest.mark.parametrize("cx_normalized, cx_calibrated, tier", [
    pytest.param(0.05, True, "nothink", id="calibrated 0.05"),
    pytest.param(0.15, True, "standard", id="calibrated 0.15"),
    pytest.param(0.25, True, "hard", id="calibrated 0.25"),
    pytest.param(0.80, True, "extreme", id="calibrated 0.80"),
    # An uncalibrated normalized energy is not on any comparable scale —
    # routing on it is what made the predecessor track the normalization
    # convention instead of the task.
    pytest.param(0.02, False, FLOOR_TIER, id="uncalibrated 0.02 gets the floor tier"),
    pytest.param(0.99, False, FLOOR_TIER, id="uncalibrated 0.99 gets the floor tier"),
])
def test_cx_base_tier_walks_the_calibrated_energy_ladder(cx_normalized, cx_calibrated, tier):
    assert base_tier(cx_normalized, cx_calibrated) == tier


@pytest.mark.parametrize("gx_score, gx_verdict, gx_severe, escalation", [
    pytest.param(DEFAULT_GX_SEVERE, "", None, 0, id="at the severe boundary"),
    pytest.param(0.55, "", None, 1, id="below the severe boundary"),
    pytest.param(0.30, "", None, 2, id="well below the severe boundary"),
    # A model whose G(x) scores cluster high: 0.8 is below ITS severe
    # boundary and must escalate, though it sits above the default 0.60.
    pytest.param(0.80, "", 0.90, 1, id="below a per-model severe boundary"),
    pytest.param(0.80, "", None, 0, id="above the default severe boundary"),
    # G(x) loaded but with no thresholds for this model: its score scale
    # is unknown, so the severe-boundary bands mean nothing.
    pytest.param(0.05, "uncalibrated", None, 0, id="uncalibrated verdict"),
    pytest.param(0.05, "unavailable", None, 0, id="unavailable verdict"),
    pytest.param(0.05, "error", None, 0, id="error verdict"),
])
def test_gx_escalation_bands(gx_score, gx_verdict, gx_severe, escalation):
    assert gx_escalation(gx_score, True, gx_verdict=gx_verdict, gx_severe=gx_severe) == escalation


# --- The allocation and its floor ---------------------------------------------

@pytest.mark.parametrize("probe, expected", [
    # C(x) says trivial (nothink => k=1 on the raw ladder), G(x) agrees.
    # The probe still failed verification, so k may not drop below 3.
    pytest.param(dict(cx_normalized=0.01, cx_calibrated=True, **CLEAN_GX),
                 Allocation(k=3, tier="standard", base_tier="nothink", gx_escalation=0),
                 id="trivial C(x) and a clean G(x) get the floor"),
    pytest.param(dict(cx_normalized=0.15, cx_calibrated=True, **CLEAN_GX),
                 Allocation(k=3, tier="standard", base_tier="standard", gx_escalation=0),
                 id="standard C(x) and a clean G(x)"),
    pytest.param(dict(cx_normalized=0.15, cx_calibrated=True, gx_score=0.55,
                      gx_available=True, gx_verdict="likely_incorrect"),
                 Allocation(k=5, tier="hard", base_tier="standard", gx_escalation=1),
                 id="G(x) below the severe boundary raises one tier"),
    pytest.param(dict(cx_normalized=0.15, cx_calibrated=True, gx_score=0.30,
                      gx_available=True, gx_verdict="likely_incorrect"),
                 Allocation(k=8, tier="extreme", base_tier="standard", gx_escalation=2),
                 id="G(x) well below the severe boundary raises two tiers"),
    # The escalation runs from the nothink base, then the floor catches what
    # is left: +1 lands on standard (the floor), +2 on hard (above it).
    pytest.param(dict(cx_normalized=0.01, cx_calibrated=True, gx_score=0.55, gx_available=True),
                 Allocation(k=3, tier="standard", base_tier="nothink", gx_escalation=1),
                 id="trivial C(x) raised one tier lands on the floor"),
    pytest.param(dict(cx_normalized=0.01, cx_calibrated=True, gx_score=0.20, gx_available=True),
                 Allocation(k=5, tier="hard", base_tier="nothink", gx_escalation=2),
                 id="trivial C(x) raised two tiers lands above the floor"),
    pytest.param(dict(cx_normalized=0.9, cx_calibrated=True, gx_score=0.05, gx_available=True),
                 Allocation(k=8, tier="extreme", base_tier="extreme", gx_escalation=2),
                 id="the escalation saturates at the top tier"),
    pytest.param(dict(cx_normalized=0.15, cx_calibrated=True, gx_score=0.05,
                      gx_available=True, gx_verdict="uncalibrated"),
                 Allocation(k=3, tier="standard", base_tier="standard", gx_escalation=0),
                 id="an uncalibrated G(x) never escalates"),
    pytest.param(dict(cx_normalized=0.25, cx_calibrated=True, gx_score=0.5, gx_available=False),
                 Allocation(k=5, tier="hard", base_tier="hard", gx_escalation=0),
                 id="an unavailable G(x) leaves C(x) in charge"),
    # Every argument defaulted: the shape of "the lens told us nothing".
    pytest.param({},
                 Allocation(k=3, tier="standard", base_tier="standard", gx_escalation=0,
                            reason="uncalibrated"),
                 id="no lens at all gets exactly the floor"),
])
def test_allocation_from_the_probe_lens_scores(probe, expected):
    assert allocate(**probe) == expected


def test_no_allocation_is_ever_below_the_floor():
    for norm in (0.0, 0.001, 0.05, 0.0999, 0.1, 0.25, 0.5, 1.0):
        for gx in (0.0, 0.3, 0.5, 0.7, 1.0):
            for avail in (True, False):
                for cal in (True, False):
                    alloc = allocate(cx_normalized=norm, cx_calibrated=cal,
                                     gx_score=gx, gx_available=avail)
                    assert alloc.k >= K_FLOOR
                    assert TIER_MIN_K[alloc.tier] >= 1
                    assert alloc.tier != "nothink"


@pytest.mark.parametrize("junk", [
    pytest.param({"cx_normalized": float("nan"), "cx_calibrated": True}, id="NaN C(x)"),
    pytest.param({"cx_normalized": -5.0, "cx_calibrated": True}, id="negative C(x)"),
    pytest.param({"cx_normalized": 12.0, "cx_calibrated": True}, id="C(x) above 1"),
    pytest.param({"gx_score": float("nan"), "gx_available": True}, id="NaN G(x)"),
    pytest.param({"gx_severe": 0.0, "gx_score": 0.0, "gx_available": True},
                 id="a severe boundary of 0"),
])
def test_allocate_never_raises_on_junk_input(junk):
    assert allocate(**junk).k >= K_FLOOR


# --- Live-path budget cap -----------------------------------------------------

@pytest.mark.parametrize("remaining_ms, observed_llm_call_ms, expected", [
    # The bench arm the gate was measured on: no wall-clock ceiling.
    pytest.param(None, 0.0,
                 Allocation(k=8, tier="extreme", base_tier="extreme", gx_escalation=2),
                 id="no budget means no cap"),
    pytest.param(3_600_000.0, 5_000.0,
                 Allocation(k=8, tier="extreme", base_tier="extreme", gx_escalation=2),
                 id="an hour left leaves the escalation intact"),
    # The floor stops the LENS starving generation, not the clock. Holding k
    # at 3 when the clock cannot afford 3 spends the whole budget on
    # candidates and leaves phase-3 repair, which the cap reserves for,
    # unable to run. Measured across 43 pipeline runs: capped_from was empty
    # in all 33 allocations, sessions spent a median 207s of a 180s budget
    # on generation alone, and refinement was skipped 19 times with 7-9s left.
    pytest.param(1_000.0, 60_000.0,
                 Allocation(k=1, tier="nothink", base_tier="extreme", gx_escalation=2,
                            capped_from="extreme", reason="budget_capped"),
                 id="one second left caps below the floor"),
    pytest.param(-10_000.0, 5_000.0,
                 Allocation(k=1, tier="nothink", base_tier="extreme", gx_escalation=2,
                            capped_from="extreme", reason="budget_capped"),
                 id="a negative budget gets the smallest allocation"),
])
def test_budget_cap_on_the_deepest_allocation(remaining_ms, observed_llm_call_ms, expected):
    alloc = allocate(**EXTREME_PROBE, remaining_ms=remaining_ms,
                     observed_llm_call_ms=observed_llm_call_ms)
    assert alloc == expected


def test_short_wall_clock_lowers_the_tier():
    # 20s observed per call. One iteration (3 calls) is reserved, leaving
    # too little for all the candidates `extreme` would generate.
    alloc = allocate(**EXTREME_PROBE, remaining_ms=150_000.0, observed_llm_call_ms=20_000.0)
    assert alloc.base_tier == "extreme"
    assert alloc.tier != "extreme"
    assert alloc.capped_from == "extreme"
    assert alloc.reason == "budget_capped"
    assert alloc.k < TIER_MIN_K["extreme"]


def test_cap_is_monotonic_in_the_remaining_budget():
    ks = [allocate(**EXTREME_PROBE, remaining_ms=float(ms), observed_llm_call_ms=10_000.0).k
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


def test_escalation_is_free_up_to_the_floor_and_costs_more_for_each_deeper_tier():
    cost = {tier: estimate_escalation_ms(tier, 10_000.0)
            for tier in ("nothink", "standard", "hard", "extreme")}
    assert cost["nothink"] == cost["standard"] == 0.0
    assert cost["standard"] < cost["hard"] < cost["extreme"]


def test_tier_token_ratio_tracks_the_budget_forcing_table():
    assert cxgx_gate.tier_token_ratio(FLOOR_TIER) == 1.0
    assert cxgx_gate.tier_token_ratio("extreme") > 1.0
