"""Tests for V3 Constraint Refinement (Feature 3B)."""

import json

import pytest

from stages.constraint_refinement import (
    ConstraintRefinementConfig,
    ConstraintRefinementEvent,
    ConstraintRefiner,
    RefinedHypothesis,
    RefinementResult,
    cosine_distance,
    filter_by_distance,
    parse_hypotheses,
)
from stages.failure_analysis import FailureAnalysis


class MockLLM:
    """Stands in for the model: returns a fixed answer and records the settings of each call."""

    def __init__(self, response):
        self.response = response
        self.calls = []

    def __call__(self, prompt, temperature, max_tokens, seed):
        self.calls.append({"temperature": temperature, "max_tokens": max_tokens, "seed": seed})
        return self.response, 100, 50.0


FAILURE_ANALYSIS = FailureAnalysis(
    categories={0: "wrong_algorithm", 1: "edge_case_miss"},
    violated_constraints=["Must handle empty input"],
    common_pattern="All solutions assume non-empty input",
    new_constraints=["Check for empty input before processing"],
)

# A model answer in the format the refinement prompt asks for.
TWO_HYPOTHESES = (
    "HYPOTHESIS 1:\n"
    "APPROACH: Use dynamic programming with memoization\n"
    "RATIONALE: Avoids the exponential blowup from recursive approach\n"
    "CONSTRAINTS:\n"
    "- Must handle empty input\n"
    "- NEW: Use bottom-up DP to avoid stack overflow\n"
    "\n"
    "HYPOTHESIS 2:\n"
    "APPROACH: Sort first, then use two pointers\n"
    "RATIONALE: Sorting reduces the problem to linear scan\n"
    "CONSTRAINTS:\n"
    "- Must handle empty input\n"
    "- NEW: Sort the array first for O(n log n) complexity\n"
    "- NEW: Handle duplicate elements in the sorted array\n"
)

# An embedding for each approach in TWO_HYPOTHESES; the two are orthogonal.
APPROACH_EMBEDDINGS = {
    "Use dynamic programming with memoization": [1.0, 0.0],
    "Sort first, then use two pointers": [0.0, 1.0],
}


@pytest.fixture
def refiner(tmp_path):
    """An enabled refiner that writes its telemetry to tmp_path."""
    return ConstraintRefiner(ConstraintRefinementConfig(enabled=True), telemetry_dir=tmp_path)


# ---------------------------------------------------------------------------
# cosine_distance
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("a, b, expected", [
    pytest.param([1.0, 2.0, 3.0], [1.0, 2.0, 3.0], 0.0, id="identical: 0"),
    pytest.param([1.0, 0.0], [0.0, 1.0], 1.0, id="orthogonal: 1"),
    pytest.param([1.0, 2.0, 3.0], [-1.0, -2.0, -3.0], 2.0, id="opposite: 2"),
    # Input that cannot be compared gives the default 1.0, the distance of orthogonal vectors.
    pytest.param([], [], 1.0, id="empty: default 1"),
    pytest.param([1.0, 2.0], [1.0, 2.0, 3.0], 1.0, id="different lengths: default 1"),
    pytest.param([0.0, 0.0, 0.0], [1.0, 2.0, 3.0], 1.0, id="zero vector: default 1"),
])
def test_cosine_distance(a, b, expected):
    assert cosine_distance(a, b) == pytest.approx(expected, abs=1e-10)


def test_cosine_distance_of_similar_vectors_is_small():
    assert cosine_distance([1.0, 2.0, 3.0], [1.1, 2.1, 3.1]) < 0.01


def test_cosine_distance_is_symmetric_and_between_0_and_2():
    a, b = [1.0, 3.0, 5.0], [2.0, 4.0, 6.0]
    assert 0.0 <= cosine_distance(a, b) <= 2.0
    assert cosine_distance(a, b) == pytest.approx(cosine_distance(b, a), abs=1e-10)


# ---------------------------------------------------------------------------
# parse_hypotheses
# ---------------------------------------------------------------------------

def test_parse_hypotheses_reads_approach_rationale_and_constraints():
    hypotheses = parse_hypotheses(TWO_HYPOTHESES, ["Must handle empty input"])

    assert len(hypotheses) == 2
    first, second = hypotheses
    assert first.approach == "Use dynamic programming with memoization"
    assert first.rationale == "Avoids the exponential blowup from recursive approach"
    # Each hypothesis keeps the original constraints first, then adds its NEW ones.
    assert first.new_constraints == ["Use bottom-up DP to avoid stack overflow"]
    assert first.constraints == [
        "Must handle empty input",
        "Use bottom-up DP to avoid stack overflow",
    ]
    assert second.new_constraints == [
        "Sort the array first for O(n log n) complexity",
        "Handle duplicate elements in the sorted array",
    ]
    assert second.constraints == [
        "Must handle empty input",
        "Sort the array first for O(n log n) complexity",
        "Handle duplicate elements in the sorted array",
    ]


@pytest.mark.parametrize("response, approaches", [
    pytest.param("", [], id="empty answer: none"),
    pytest.param("Just some random text.", [], id="no HYPOTHESIS header: none"),
    pytest.param(
        "HYPOTHESIS 1:\n"
        "APPROACH: Brute force\n"
        "RATIONALE: Simple and correct\n"
        "CONSTRAINTS:\n"
        "- NEW: Check all pairs\n",
        ["Brute force"],
        id="one block: one hypothesis",
    ),
    pytest.param(
        "HYPOTHESIS 1:\n"
        "APPROACH: Brute force\n"
        "CONSTRAINTS:\n"
        "- NEW: Check all pairs\n",
        ["Brute force"],
        id="block without RATIONALE: one hypothesis",
    ),
])
def test_parse_hypotheses_finds_one_hypothesis_per_block(response, approaches):
    assert [h.approach for h in parse_hypotheses(response, [])] == approaches


# ---------------------------------------------------------------------------
# filter_by_distance
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("embedding, failed_embeddings, min_distance, kept", [
    pytest.param([1.0, 0.001, 0.0], [[1.0, 0.0, 0.0]], 0.15, 0, id="close to a failure: dropped"),
    pytest.param([0.0, 1.0, 0.0], [[1.0, 0.0, 0.0]], 0.15, 1, id="far from a failure: kept"),
    pytest.param([0.9, 0.4], [[1.0, 0.0]], 0.01, 1, id="distance 0.086, low minimum: kept"),
    pytest.param([0.9, 0.4], [[1.0, 0.0]], 0.99, 0, id="distance 0.086, high minimum: dropped"),
    pytest.param([], [[1.0, 0.0]], 0.15, 1, id="no embedding: kept"),
    pytest.param([1.0, 0.0], [], 0.15, 1, id="no failures to compare with: kept"),
])
def test_filter_by_distance(embedding, failed_embeddings, min_distance, kept):
    hypothesis = RefinedHypothesis(approach="a", embedding=embedding)
    assert len(filter_by_distance([hypothesis], failed_embeddings, min_distance)) == kept


# ---------------------------------------------------------------------------
# ConstraintRefiner.refine
# ---------------------------------------------------------------------------

def test_disabled_refiner_does_nothing(tmp_path):
    refiner = ConstraintRefiner(ConstraintRefinementConfig(enabled=False), telemetry_dir=tmp_path)
    llm = MockLLM(TWO_HYPOTHESES)

    result = refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=["Must sort"], llm_call=llm, task_id="t1",
    )

    assert result.hypotheses == []
    assert result.filtered_count == 0
    assert result.total_generated == 0
    assert llm.calls == []
    assert not (tmp_path / "constraint_refinement_events.jsonl").exists()


def test_refine_without_an_llm_returns_no_hypotheses(refiner):
    result = refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS, original_constraints=[],
    )
    assert result.hypotheses == []


def test_refine_turns_the_model_answer_into_hypotheses(refiner):
    llm = MockLLM(TWO_HYPOTHESES)

    result = refiner.refine(
        problem="Find two sum", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=["Must handle empty input"], llm_call=llm,
    )

    # One call, with no seed so that repeated refinements give different hypotheses.
    assert llm.calls == [{"temperature": 0.5, "max_tokens": 2048, "seed": None}]
    assert result.total_generated == 2
    assert result.refinement_time_ms > 0
    assert len(result.hypotheses) == 2
    for hypothesis in result.hypotheses:
        assert "Must handle empty input" in hypothesis.constraints
        assert hypothesis.new_constraints
        assert hypothesis.approach
        assert hypothesis.rationale


def test_refine_embeds_the_approach_of_each_hypothesis(refiner):
    result = refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=["c1"], llm_call=MockLLM(TWO_HYPOTHESES),
        embed_call=lambda approach: APPROACH_EMBEDDINGS[approach],
    )

    assert [h.embedding for h in result.hypotheses] == [[1.0, 0.0], [0.0, 1.0]]


def test_refine_drops_hypotheses_close_to_a_failed_solution(refiner):
    result = refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=[], llm_call=MockLLM(TWO_HYPOTHESES),
        embed_call=lambda approach: APPROACH_EMBEDDINGS[approach],
        # A failed solution with the same embedding as the dynamic programming approach.
        failed_embeddings=[[1.0, 0.0]],
    )

    assert [h.approach for h in result.hypotheses] == ["Sort first, then use two pointers"]
    assert result.filtered_count == 1
    assert result.total_generated == 2


# ---------------------------------------------------------------------------
# Telemetry
# ---------------------------------------------------------------------------

def test_refine_with_a_task_id_appends_one_event_per_call(refiner, tmp_path):
    for task_id in ["LCB_001", "LCB_002"]:
        refiner.refine(
            problem="test", failure_analysis=FAILURE_ANALYSIS,
            original_constraints=["c1"], llm_call=MockLLM(TWO_HYPOTHESES), task_id=task_id,
        )

    lines = (tmp_path / "constraint_refinement_events.jsonl").read_text().splitlines()
    events = [json.loads(line) for line in lines]
    assert [event["task_id"] for event in events] == ["LCB_001", "LCB_002"]
    assert events[0]["num_hypotheses_generated"] == 2
    assert events[0]["num_hypotheses_viable"] == 2
    assert events[0]["num_filtered_by_distance"] == 0
    assert "refinement_time_ms" in events[0]
    assert "timestamp" in events[0]


def test_refine_without_a_task_id_writes_no_event(refiner, tmp_path):
    refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=[], llm_call=MockLLM(TWO_HYPOTHESES),
    )
    assert not (tmp_path / "constraint_refinement_events.jsonl").exists()


def test_refine_without_a_telemetry_dir_still_works():
    refiner = ConstraintRefiner(ConstraintRefinementConfig(enabled=True), telemetry_dir=None)
    result = refiner.refine(
        problem="test", failure_analysis=FAILURE_ANALYSIS,
        original_constraints=[], llm_call=MockLLM(TWO_HYPOTHESES), task_id="t1",
    )
    assert len(result.hypotheses) == 2


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

def test_refined_hypothesis_to_dict():
    hypothesis = RefinedHypothesis(
        constraints=["c1", "c2", "new_c"],
        new_constraints=["new_c"],
        approach="Dynamic programming",
        rationale="Avoids exponential blowup",
        embedding=[1.0, 2.0, 3.0],
    )
    assert hypothesis.to_dict() == {
        "num_constraints": 3,
        "num_new_constraints": 1,
        "approach": "Dynamic programming",
        "rationale": "Avoids exponential blowup",
        "has_embedding": True,
    }
    assert RefinedHypothesis(approach="test").to_dict()["has_embedding"] is False


def test_refinement_result_to_dict():
    result = RefinementResult(
        hypotheses=[RefinedHypothesis(approach="a")],
        filtered_count=1, total_generated=2, refinement_time_ms=100.0,
    )
    assert result.to_dict() == {
        "num_hypotheses": 1,
        "filtered_count": 1,
        "total_generated": 2,
        "refinement_time_ms": 100.0,
    }


def test_event_to_dict():
    event = ConstraintRefinementEvent(
        task_id="t1", num_hypotheses_generated=3,
        num_hypotheses_viable=2, num_filtered_by_distance=1,
        refinement_time_ms=200.0,
    )
    as_dict = event.to_dict()
    assert as_dict["task_id"] == "t1"
    assert as_dict["num_hypotheses_generated"] == 3
    assert as_dict["num_filtered_by_distance"] == 1
    assert "timestamp" in as_dict


def test_config_defaults():
    config = ConstraintRefinementConfig()
    assert config.enabled is False
    assert config.num_hypotheses == 3
    assert config.min_cosine_distance == 0.15
    assert config.refinement_temperature == 0.5
    assert config.refinement_max_tokens == 2048
