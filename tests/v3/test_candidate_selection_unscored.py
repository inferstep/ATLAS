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


def _c(index, energy, passed=True):
    return CandidateInfo(index=index, code=f"code-{index}", energy=energy,
                         passed=passed)


def test_unscored_never_beats_scored():
    picked = select_lens([_c(0, None), _c(1, 9.0), _c(2, 4.0)])
    assert picked.index == 2


def test_unscored_is_selected_only_when_nothing_else_passed():
    picked = select_lens([_c(0, None), _c(1, 2.0, passed=False)])
    assert picked.index == 0


def test_all_unscored_picks_the_first_index():
    assert select_lens([_c(3, None), _c(1, None), _c(2, None)]).index == 1


def test_strategy_dispatch_uses_the_same_rule():
    picked = select_candidate([_c(0, None), _c(1, 5.0)], strategy="lens")
    assert picked.index == 1


def test_rank_key_orders_scored_before_unscored():
    rows = [{"index": 0, "energy": None}, {"index": 1, "energy": 7.0},
            {"index": 2, "energy": 1.0}]
    assert [r["index"] for r in sorted(rows, key=energy_rank_key)] == [2, 1, 0]


def test_rank_key_tolerates_a_missing_energy():
    assert energy_rank_key({"index": 0}) == energy_rank_key({"index": 0, "energy": None})


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


def test_rank_key_orders_unscored_by_index_deterministically():
    rows = [{"index": 3, "energy": float("nan")}, {"index": 1, "energy": None},
            {"index": 2, "energy": float("inf")}, {"index": 0, "energy": 5.0},
            {"index": 4, "energy": -2.0}, {"index": 5}]
    order = [r["index"] for r in sorted(rows, key=energy_rank_key)]
    assert order == [4, 0, 1, 2, 3, 5]
    assert order == [r["index"] for r in sorted(reversed(rows), key=energy_rank_key)]


def test_lens_selector_never_prefers_a_nan_energy():
    picked = select_lens([_c(0, float("nan")), _c(1, float("-inf")), _c(2, 9.0)])
    assert picked.index == 2
    assert select_lens([_c(2, float("nan")), _c(1, float("inf"))]).index == 1


def test_rank_key_tolerates_a_missing_or_odd_index():
    assert energy_rank_key({"energy": float("nan")}) == (1, 0.0)
    assert energy_rank_key({"index": "x", "energy": None}) == (1, 0.0)
    assert energy_rank_key(object()) == (1, 0.0)
