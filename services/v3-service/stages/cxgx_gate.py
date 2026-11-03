"""CxGx candidate allocation — the probe's lens scores choose k.

One probe candidate is generated and scored by the Geometric Lens in a
single embedding extraction (`/internal/lens/gx-score`). Two signals come
back and both are used:

  C(x) normalized energy  picks a BASE tier on the lens's own per-model
                          calibrated scale (lower energy = easier task),
                          through budget_forcing.select_tier.
  G(x) quality score      ESCALATES that tier by +1 or +2 when the XGBoost
                          correctness classifier disagrees with C(x) —
                          i.e. when the probe looks cheap but wrong.

The tier then sets k through `TIER_MIN_K`, under a hard ``k >= 3`` floor.

Why the floor is the whole design. The C(x)-only predecessor had no floor
and measured +0.0 pp: its k tracked the lens's normalization convention
rather than the task, and it handed k=1 to 60% of tasks whose probe had
just FAILED the sandbox — it starved generation on exactly the tasks that
needed candidates. With the floor, the allocator can only ADD candidates
to today's pinned k=3, never remove them, so its worst case equals current
behavior. The G(x) escalation is meant to put the added compute on the
right tasks. That is unmeasured: an earlier comparison (Qwen3.5-9B, a
patched out-of-tree runner, thinking enabled on escalation, on tasks the
G(x) head was trained on) found the gated, fixed-k=3 and shuffled arms
within noise of each other at n=175/arm.

Both orchestrators call `allocate()`: the bench runner (no wall-clock cap,
`remaining_ms=None`) and the live V3 service (capped by ATLAS_V3_TIMEOUT,
which passes its remaining budget so an escalation can never buy more
candidates than the clock can generate).
"""

from dataclasses import dataclass
from typing import Optional, Tuple

from .budget_forcing import get_max_tokens, select_tier
from .refinement_loop import (
    ITERATION_LLM_CALLS,
    MIN_ITERATION_MS,
    can_afford_iteration,
    estimate_iteration_ms,
)


# ---------------------------------------------------------------------------
# The ladder
# ---------------------------------------------------------------------------

# Ascending compute order. `light` is a probe-only tier and is not part of
# the allocation ladder.
TIER_ORDER: Tuple[str, ...] = ("nothink", "standard", "hard", "extreme")

# Candidates generated at each tier.
TIER_MIN_K = {"nothink": 1, "standard": 3, "hard": 5, "extreme": 8}

# The floor. `k=3` is what the pipeline allocates today for every task whose
# probe failed; the gate never goes below it, so `standard` is the lowest
# reachable tier even when C(x) calls the task trivial.
K_FLOOR = 3
FLOOR_TIER = "standard"
FLOOR_TIER_IDX = TIER_ORDER.index(FLOOR_TIER)

# G(x) escalation is anchored to the model's calibrated severe boundary
# (`gx_thresholds.json`), the score below which a passing candidate is rare
# enough to act on. 0.60 is the benched model's shipped value and the
# default when a caller has no per-model figure; the +2 band sits at 0.75x
# it (0.45 on that model), which is where the triangulation ran. Anchoring
# to the boundary rather than to fixed cutoffs is deliberate: hardcoded
# G(x) cutoffs calibrated on one model's score scale silently never fire on
# a model whose scores cluster higher — that defect is why the lens's own
# thresholds became per-model in the first place.
DEFAULT_GX_SEVERE = 0.60
GX_DEEP_RATIO = 0.75

# Verdicts that mean G(x) has no calibration for the served model — its
# score scale is unknown, so no escalation may be inferred from it.
# "unscored" is the probe the Lens could not score at all (its input
# exceeded the embedding server's physical batch, the Lens was unreachable):
# there is no score to read, calibrated or not.
_UNCALIBRATED_VERDICTS = frozenset(("uncalibrated", "unavailable", "error",
                                    "unscored"))


# ---------------------------------------------------------------------------
# Live-path budget model
# ---------------------------------------------------------------------------
# The bench had no outer wall-clock cap; the live path does — the proxy's V3
# bridge abandons the call after ATLAS_V3_TIMEOUT (default 180s). An
# unbounded escalation to k=8 there is not "more accuracy for more tokens",
# it is a timeout: the user gets the fallback instead of the k=3 answer the
# clock could have produced. Same failure the refinement budget gate fixed
# for phase 3, so the cap is built from the same helpers.

# One extra candidate costs its generation LLM calls plus the per-candidate
# verification tail the pipeline runs on everything it generates: per-step
# lens scoring (~3-7s on this hardware tier) and one sandbox execution.
CANDIDATE_VERIFY_MS = 10_000.0

# PlanSearch, the generator this k feeds, spends TWO calls per candidate: one
# to construct the plan from its constraint set, one to turn that plan into
# code. Pricing a candidate at a single call halved the estimate and is why
# the budget cap never fired. Measured across 43 pipeline runs: a median of 7
# LLM calls per session, which is exactly PlanSearch's 1 + 2k at k=3, against
# a model that predicted 3.
CANDIDATE_LLM_CALLS = 2

# Conservative floor when no per-call latency has been observed: one call's
# share of the refinement loop's own no-observation floor.
MIN_CANDIDATE_MS = MIN_ITERATION_MS / ITERATION_LLM_CALLS


def tier_token_ratio(tier: str) -> float:
    """Per-call cost of `tier` relative to the floor tier.

    Escalation buys more candidates AND a deeper thinking budget for each
    one, so the budget model reads the ratio off the single tier table in
    budget_forcing rather than restating it.
    """
    base = get_max_tokens(FLOOR_TIER)
    if base <= 0:
        return 1.0
    return get_max_tokens(tier) / base


def estimate_candidate_ms(observed_llm_call_ms: float) -> float:
    """Estimated wall-clock cost of ONE additional candidate at the floor
    tier. ``observed_llm_call_ms`` is the average per-call latency measured
    on this task so far (0 / falsy before any observation)."""
    if observed_llm_call_ms and observed_llm_call_ms > 0:
        return (CANDIDATE_LLM_CALLS * observed_llm_call_ms
                + CANDIDATE_VERIFY_MS)
    return MIN_CANDIDATE_MS


def estimate_escalation_ms(tier: str, observed_llm_call_ms: float) -> float:
    """Wall-clock the escalation to `tier` ADDS beyond the k>=3 floor."""
    extra = TIER_MIN_K.get(tier, K_FLOOR) - K_FLOOR
    if extra <= 0:
        return 0.0
    return (extra * estimate_candidate_ms(observed_llm_call_ms)
            * tier_token_ratio(tier))


# ---------------------------------------------------------------------------
# Decision
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class Allocation:
    """One allocation decision, with the reasoning that produced it."""
    k: int
    tier: str
    base_tier: str
    gx_escalation: int
    capped_from: str = ""   # tier before the budget cap ("" = not capped)
    reason: str = "gated"   # gated | uncalibrated | budget_capped

    def to_dict(self) -> dict:
        return {
            "k": self.k,
            "tier": self.tier,
            "base_tier": self.base_tier,
            "gx_escalation": self.gx_escalation,
            "capped_from": self.capped_from,
            "reason": self.reason,
        }


def base_tier(cx_normalized: float, cx_calibrated: bool) -> str:
    """Base tier from the probe's C(x) normalized energy.

    Delegates to budget_forcing.select_tier, which already owns the
    normalized-energy tier boundaries this pipeline uses everywhere else.
    An uncalibrated C(x) carries no comparable scale, so it selects the
    floor tier instead of a table row.
    """
    if not cx_calibrated or cx_normalized is None:
        return FLOOR_TIER
    return select_tier(normalized_energy=cx_normalized,
                       default_tier=FLOOR_TIER)


def gx_escalation(gx_score: float, gx_available: bool,
                  gx_verdict: str = "",
                  gx_severe: Optional[float] = None) -> int:
    """Tiers to escalate because G(x) contradicts C(x).

    +1 below the model's severe boundary, +2 well below it, 0 otherwise —
    and 0 whenever G(x) is missing or uncalibrated for this model, since
    the boundary the bands are measured against would not exist.
    """
    if not gx_available or gx_score is None:
        return 0
    if gx_verdict and gx_verdict in _UNCALIBRATED_VERDICTS:
        return 0
    severe = DEFAULT_GX_SEVERE if gx_severe is None else gx_severe
    if gx_score < GX_DEEP_RATIO * severe:
        return 2
    if gx_score < severe:
        return 1
    return 0


def estimate_tier_ms(tier: str, observed_llm_call_ms: float) -> float:
    """Wall-clock ALL of a tier's candidates cost, not just the surcharge.

    estimate_escalation_ms prices only what a tier adds beyond k=3, so it
    returns 0.0 at the floor and the budget check could never see the cost
    of the floor itself. That is the largest single cost in the pipeline:
    at the observed ~22s per call, k=3 is six calls and 132s of a 180s
    budget, before the probe and the self-test that precede it.
    """
    k = TIER_MIN_K.get(tier, K_FLOOR)
    return k * estimate_candidate_ms(observed_llm_call_ms) * tier_token_ratio(tier)


def budget_capped_tier(tier: str, remaining_ms: Optional[float],
                       observed_llm_call_ms: float = 0.0) -> str:
    """Lower `tier` until its candidates fit the remaining wall-clock.
    Returns `tier` unchanged when there is no cap.

    The affordability test reserves one refinement iteration: generating
    candidates must not consume the budget the phase-3 repair gate needs to
    enter the loop at all. Buying breadth by making repair impossible is not
    a trade this gate is allowed to make.

    Measured across 43 pipeline runs: that reservation never once held. The
    cap priced tiers with estimate_escalation_ms, which is 0.0 at the floor,
    so the first affordability test always passed and the walk stopped
    immediately — capped_from was empty in all 33 allocations while sessions
    spent a median 207s of a 180s budget on generation alone. Refinement was
    then reached with 7-9s left and skipped 19 times.
    The floor's purpose is to stop the LENS lowering quality below the old
    fixed k=3; `allocate` still applies it before calling this. It was never
    meant to stop the clock, so the walk here runs to the bottom of the
    ladder, where `nothink` already provides k=1.
    """
    if remaining_ms is None:
        return tier
    idx = TIER_ORDER.index(tier) if tier in TIER_ORDER else FLOOR_TIER_IDX
    usable_ms = remaining_ms - estimate_iteration_ms(observed_llm_call_ms)
    while idx > 0:
        cost = estimate_tier_ms(TIER_ORDER[idx], observed_llm_call_ms)
        if can_afford_iteration(usable_ms, cost):
            break
        idx -= 1
    return TIER_ORDER[idx]


def allocate(cx_normalized: float = 0.5,
             cx_calibrated: bool = False,
             gx_score: float = 0.5,
             gx_available: bool = False,
             gx_verdict: str = "",
             gx_severe: Optional[float] = None,
             remaining_ms: Optional[float] = None,
             observed_llm_call_ms: float = 0.0) -> Allocation:
    """Allocate candidates for a task whose probe failed verification.

    Callers pass the probe's lens scores; every argument has a fail-soft
    default, so a missing, unreachable, or uncalibrated lens produces
    exactly ``k=3`` at the ``standard`` tier — today's pinned behavior —
    rather than an exception or a starved allocation.

    `remaining_ms` is the caller's remaining wall-clock budget; pass None
    when the caller has no cap (the bench runner) to skip the budget cap.
    """
    base = base_tier(cx_normalized, cx_calibrated)
    escalation = gx_escalation(gx_score, gx_available, gx_verdict, gx_severe)

    base_idx = TIER_ORDER.index(base) if base in TIER_ORDER else FLOOR_TIER_IDX
    tier = TIER_ORDER[min(len(TIER_ORDER) - 1, base_idx + escalation)]
    # Floor AFTER escalation: a `nothink` base that G(x) escalates still
    # escalates from where C(x) put it, but the result can never sit below
    # the tier that yields k=3.
    if TIER_ORDER.index(tier) < FLOOR_TIER_IDX:
        tier = FLOOR_TIER

    capped = budget_capped_tier(tier, remaining_ms, observed_llm_call_ms)
    capped_from = tier if capped != tier else ""

    if capped_from:
        reason = "budget_capped"
    elif not cx_calibrated and not escalation:
        # An unscored probe is named as such: the floor was allocated
        # because there was no score, not because the score had no scale.
        reason = "unscored" if gx_verdict == "unscored" else "uncalibrated"
    else:
        reason = "gated"

    # K_FLOOR stops the LENS starving generation: its C(x)-only predecessor
    # handed k=1 to 60% of tasks whose probe had just failed, which is what
    # the floor exists to prevent. It must not also override the CLOCK. When
    # the budget cap lowered the tier, the smaller k IS the finding — holding
    # k at 3 there spends the whole budget on candidates and leaves the
    # phase-3 repair the reservation was made for unable to run at all,
    # measured as 19 refinement skips with 7-9s remaining of 180s.
    capped_k = TIER_MIN_K[capped]
    return Allocation(
        k=capped_k if capped_from else max(K_FLOOR, capped_k),
        tier=capped,
        base_tier=base,
        gx_escalation=escalation,
        capped_from=capped_from,
        reason=reason,
    )
