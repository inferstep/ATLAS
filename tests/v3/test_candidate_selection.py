"""Tests for V3 candidate selection strategies."""

import pytest

from stages.candidate_selection import (
    CandidateInfo,
    select_candidate,
    select_lens,
    select_logprob,
    select_oracle,
    select_random,
)

# Candidate 2 fails, yet it has the lowest energy and the highest mean logprob:
# a selector that does not skip failing candidates picks it.
CANDIDATES = [
    CandidateInfo(index=0, code="def f(): return 1", energy=5.0, passed=True,
                  logprobs=[-0.1, -0.2, -0.3]),  # mean -0.2
    CandidateInfo(index=1, code="def f(): return 2", energy=3.0, passed=True,
                  logprobs=[-0.5, -0.6, -0.7]),  # mean -0.6
    CandidateInfo(index=2, code="def f(): return 3", energy=1.0, passed=False,
                  logprobs=[-0.01, -0.02]),  # mean -0.015
    CandidateInfo(index=3, code="def f(): return 4", energy=12.0, passed=True,
                  logprobs=[-1.0, -1.5, -2.0]),  # mean -1.5
]


def test_lens_picks_the_passing_candidate_with_the_lowest_energy():
    assert select_lens(CANDIDATES).index == 1


def test_logprob_picks_the_passing_candidate_with_the_highest_mean_logprob():
    assert select_logprob(CANDIDATES).index == 0


def test_logprob_falls_back_to_lens_without_logprobs():
    candidates = [
        CandidateInfo(index=0, code="x", energy=5.0, passed=True, logprobs=None),
        CandidateInfo(index=1, code="y", energy=3.0, passed=True, logprobs=None),
    ]
    assert select_logprob(candidates).index == 1  # the lowest energy


def test_oracle_picks_a_passing_candidate():
    assert select_oracle(CANDIDATES).passed


def test_random_picks_a_passing_candidate_and_the_same_seed_picks_the_same_one():
    for seed in range(10):
        assert select_random(CANDIDATES, seed=seed).passed, f"seed {seed}"
    assert select_random(CANDIDATES, seed=42).index == select_random(CANDIDATES, seed=42).index


@pytest.mark.parametrize("selector", [
    pytest.param(select_lens, id="lens"),
    pytest.param(select_random, id="random"),
    pytest.param(select_logprob, id="logprob"),
    pytest.param(select_oracle, id="oracle"),
])
def test_no_passing_candidate_selects_none(selector):
    assert selector([CandidateInfo(index=0, code="x", energy=1.0, passed=False)]) is None
    assert selector([]) is None


def test_select_candidate_runs_the_named_strategy():
    assert select_candidate(CANDIDATES, strategy="lens").index == 1
    assert select_candidate(CANDIDATES, strategy="logprob").index == 0
    assert select_candidate(CANDIDATES, strategy="oracle").passed
    assert select_candidate(CANDIDATES, strategy="random", seed=42).passed


def test_select_candidate_rejects_an_unknown_strategy():
    with pytest.raises(ValueError, match="invalid"):
        select_candidate(CANDIDATES, strategy="invalid")
