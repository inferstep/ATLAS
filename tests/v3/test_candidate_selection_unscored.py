"""The lens selector ranks scored candidates before unscored ones.

An unscored candidate carries `energy=None`. It is never preferred over a
candidate the Lens scored, and among unscored candidates the earliest
index wins so the choice is deterministic. Nothing here invents an
energy for it.
"""

import pytest

from stages.candidate_selection import (
    CandidateInfo, energy_rank_key, select_candidate, select_lens,
)


def candidate(index, energy, passed=True):
    return CandidateInfo(index=index, code=f"code-{index}", energy=energy, passed=passed)


@pytest.mark.parametrize("candidates, expected_index", [
    pytest.param([candidate(0, None), candidate(1, 9.0), candidate(2, 4.0)], 2,
                 id="unscored-never-beats-scored"),
    pytest.param([candidate(0, float("nan")), candidate(1, float("-inf")), candidate(2, 9.0)], 2,
                 id="nan-and-minus-inf-never-beat-scored"),
    pytest.param([candidate(0, None), candidate(1, 2.0, passed=False)], 0,
                 id="unscored-picked-only-when-no-scored-one-passed"),
    pytest.param([candidate(3, None), candidate(1, None), candidate(2, None)], 1,
                 id="all-unscored-picks-the-first-index"),
    pytest.param([candidate(2, float("nan")), candidate(1, float("inf"))], 1,
                 id="nan-and-inf-are-unscored-and-ranked-by-index"),
])
def test_lens_selector(candidates, expected_index):
    assert select_lens(candidates).index == expected_index


def test_strategy_dispatch_uses_the_same_rule():
    picked = select_candidate([candidate(0, None), candidate(1, 5.0)], strategy="lens")
    assert picked.index == 1


@pytest.mark.parametrize("energy", [0.0, -1.0, 1e9])
def test_rank_key_keeps_real_energies_ordered(energy):
    assert energy_rank_key({"energy": energy}) < energy_rank_key({"energy": None})


# --- nothing that is not a finite number ranks as an energy ---------------------------
#
# The typed Lens boundary (scoring.score_candidate_combined) is the owner
# that turns a non-finite or nonnumeric score into an unscored failure.
# The rank key is the last line: whatever reaches it that is not a finite
# number ranks after every scored candidate, by index, so a sort is
# deterministic and a NaN can never poison the comparison.

NOT_AN_ENERGY = [None, float("nan"), float("inf"), float("-inf"),
                 "3.0", True, False, [], {}]


@pytest.mark.parametrize("energy", NOT_AN_ENERGY)
def test_rank_key_ranks_anything_but_a_finite_number_as_unscored(energy):
    assert energy_rank_key({"index": 4, "energy": energy}) == \
        energy_rank_key({"index": 4, "energy": None})
    assert energy_rank_key({"index": 4, "energy": -1e9}) < \
        energy_rank_key({"index": 0, "energy": energy})


def test_rank_key_orders_scored_by_energy_then_unscored_by_index():
    rows = [{"index": 3, "energy": float("nan")}, {"index": 1, "energy": None},
            {"index": 2, "energy": float("inf")}, {"index": 0, "energy": 5.0},
            {"index": 4, "energy": -2.0}, {"index": 5}]
    order = [r["index"] for r in sorted(rows, key=energy_rank_key)]
    assert order == [4, 0, 1, 2, 3, 5]
    assert order == [r["index"] for r in sorted(reversed(rows), key=energy_rank_key)]


def test_rank_key_tolerates_a_missing_energy_or_an_odd_index():
    assert energy_rank_key({"index": 0}) == energy_rank_key({"index": 0, "energy": None})
    assert energy_rank_key({"energy": float("nan")}) == (1, 0.0)
    assert energy_rank_key({"index": "x", "energy": None}) == (1, 0.0)
    assert energy_rank_key(object()) == (1, 0.0)
