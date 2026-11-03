"""V3 Candidate Selection Strategies — baseline selectors for ablation.

Provides four selection strategies for choosing among passing candidates:
- lens: Select by lowest C(x) energy (current V3 default)
- random: Uniform random selection (baseline)
- logprob: Select by highest mean token log-probability (baseline)
- oracle: Always select a passing candidate if one exists (ceiling)

Strategy is chosen by the caller (bench: --selection-strategy; product: lens)
"""

import math
import random
from dataclasses import dataclass
from typing import List, Optional, Tuple

# The failure kind V3 records when a Lens answer carries a score that is
# not a finite number (NaN, an infinity, a string, a bool): the same name
# the Lens service uses when its own forward produces one.
NONFINITE_SCORE = "nonfinite_score"


def finite_score(value) -> Optional[float]:
    """`value` as a float when it is a finite number, else None.

    The one rule for what counts as a score on the consumer side: a bool
    is not one, a string is not one, NaN and the infinities are not one.
    `json.loads` accepts the tokens NaN / Infinity and reads 1e999 as an
    infinity, so a Lens answer must pass through here before any field of
    it is ranked, allocated on, or reported as a number.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    out = float(value)
    return out if math.isfinite(out) else None


@dataclass
class CandidateInfo:
    """Minimal candidate info needed for selection.

    `energy` is None for a candidate the Lens did not score (the input
    exceeded the embedding server's physical batch, the Lens was
    unreachable). It is not a low energy and never ranks as one.
    """
    index: int
    code: str
    energy: Optional[float]
    passed: bool
    logprobs: Optional[List[float]] = None


def energy_rank_key(candidate) -> Tuple[int, float]:
    """Sort key for the min-energy rule: scored candidates by ascending C(x)
    energy, then every unscored candidate by index. Accepts a candidate
    dict or a CandidateInfo.

    Unscored is anything that is not a finite number: None, absent, NaN,
    an infinity, a string, a bool. The typed Lens boundary
    (scoring.score_candidate_combined) is where such a value becomes a
    recorded failure; this key is the last line, so that whatever reaches
    a sort cannot poison the comparison or land first. The order among
    unscored candidates is by index, so a sort is deterministic.
    """
    if isinstance(candidate, dict):
        energy, index = candidate.get("energy"), candidate.get("index")
    else:
        energy = getattr(candidate, "energy", None)
        index = getattr(candidate, "index", None)
    scored = finite_score(energy)
    if scored is None:
        return (1, finite_score(index) or 0.0)
    return (0, scored)


def select_lens(candidates: List[CandidateInfo]) -> Optional[CandidateInfo]:
    """Select the passing candidate with lowest C(x) energy.

    This is the default V3 strategy — the Geometric Lens picks the
    candidate it believes is most likely correct. A passing candidate the
    Lens did not score is chosen only when no scored candidate passed.
    """
    passing = [c for c in candidates if c.passed]
    if not passing:
        return None
    return min(passing, key=energy_rank_key)


def select_random(candidates: List[CandidateInfo],
                  seed: Optional[int] = None) -> Optional[CandidateInfo]:
    """Select a passing candidate uniformly at random.

    Baseline: proves whether structured selection (Lens) outperforms
    naive uniform selection from k candidates.
    """
    passing = [c for c in candidates if c.passed]
    if not passing:
        return None
    rng = random.Random(seed)
    return rng.choice(passing)


def select_logprob(candidates: List[CandidateInfo]) -> Optional[CandidateInfo]:
    """Select the passing candidate with highest mean token log-probability.

    Baseline: model confidence (generation probability) as a selection
    signal. If logprobs are unavailable, falls back to lens selection.
    """
    passing = [c for c in candidates if c.passed]
    if not passing:
        return None

    # Filter to candidates with logprobs
    with_logprobs = [c for c in passing if c.logprobs]
    if not with_logprobs:
        return select_lens(candidates)

    def mean_logprob(c):
        return sum(c.logprobs) / len(c.logprobs) if c.logprobs else float('-inf')

    return max(with_logprobs, key=mean_logprob)


def select_oracle(candidates: List[CandidateInfo]) -> Optional[CandidateInfo]:
    """Select any passing candidate (oracle ceiling measurement).

    This represents the theoretical ceiling: if you could always identify
    a correct candidate from the pool, what would pass@1 be? Equivalent
    to pass@k but reported as pass@1 for comparison.
    """
    passing = [c for c in candidates if c.passed]
    if not passing:
        return None
    return passing[0]


# Strategy registry
STRATEGIES = {
    "lens": select_lens,
    "random": select_random,
    "logprob": select_logprob,
    "oracle": select_oracle,
}


def select_candidate(candidates: List[CandidateInfo],
                     strategy: str = "lens",
                     seed: Optional[int] = None) -> Optional[CandidateInfo]:
    """Select a candidate using the specified strategy.

    Args:
        candidates: List of candidate info objects.
        strategy: One of "lens", "random", "logprob", "oracle".
        seed: Random seed (only used by "random" strategy).

    Returns:
        Selected candidate, or None if no passing candidates.
    """
    if strategy == "random":
        return select_random(candidates, seed=seed)

    selector = STRATEGIES.get(strategy)
    if selector is None:
        raise ValueError(f"Unknown selection strategy: {strategy}. "
                         f"Choose from: {list(STRATEGIES.keys())}")
    return selector(candidates)
