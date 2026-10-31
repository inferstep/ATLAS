"""Tests for V3 Failure Analysis (Feature 3A)."""

import json
from unittest.mock import Mock

import pytest

from stages.failure_analysis import (
    FAILURE_CATEGORIES,
    FailingCandidate,
    FailureAnalysis,
    FailureAnalysisConfig,
    FailureAnalysisEvent,
    FailureAnalyzer,
    format_candidates_with_errors,
    parse_common_pattern,
    parse_failure_categories,
    parse_new_constraints,
    parse_violated_constraints,
)

TWO_FAILING_CANDIDATES = [
    FailingCandidate(code="def solve(n):\n    return n * 2",
                     error_output="AssertionError: expected 3 got 4", index=0),
    FailingCandidate(code="def solve(n):\n    return n + 1",
                     error_output="AssertionError: expected 6 got 3", index=1),
]

# An LLM answer in the format the parsers expect.
STRUCTURED_ANSWER = """\
CATEGORY:
Solution 1: wrong_algorithm
Solution 2: implementation_bug

VIOLATED:
- Must handle negative numbers
- Must return sorted output

COMMON:
All solutions assume positive input. They fail when given negative numbers.

NEW_CONSTRAINTS:
- Handle negative input by taking absolute value first
- Validate input range before processing
"""


# --- FailureAnalyzer.analyze ------------------------------------------------------------
# An LLM callable returns (text, tokens used, generation time in ms).

@pytest.mark.parametrize("enabled, candidates", [
    pytest.param(False, TWO_FAILING_CANDIDATES, id="analyzer-disabled"),
    pytest.param(True, [], id="no-candidates"),
])
def test_analyze_returns_an_empty_analysis_and_calls_nothing(tmp_path, enabled, candidates):
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=enabled), telemetry_dir=tmp_path)
    llm = Mock(return_value=("should not be called", 100, 50.0))
    embed = Mock(return_value=[0.5])

    result = analyzer.analyze(problem="p", candidates=candidates, original_constraints=["must sort"],
                              llm_call=llm, embed_call=embed, task_id="t1")

    assert result == FailureAnalysis()  # every field empty
    llm.assert_not_called()
    embed.assert_not_called()
    assert not (tmp_path / "failure_analysis_events.jsonl").exists()


def test_analyze_reads_each_section_of_the_llm_answer():
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
    llm = Mock(return_value=(STRUCTURED_ANSWER, 100, 50.0))

    result = analyzer.analyze(problem="Find the sum of two numbers", candidates=TWO_FAILING_CANDIDATES,
                              original_constraints=["Must handle negative numbers"], llm_call=llm)

    llm.assert_called_once()
    _prompt, temperature, max_tokens, seed = llm.call_args.args
    assert (temperature, max_tokens, seed) == (0.3, 2048, 42)
    assert result.raw_analysis == STRUCTURED_ANSWER
    assert result.categories == {0: "wrong_algorithm", 1: "implementation_bug"}
    assert result.violated_constraints == ["Must handle negative numbers", "Must return sorted output"]
    assert result.common_pattern == (
        "All solutions assume positive input. They fail when given negative numbers.")
    assert result.new_constraints == ["Handle negative input by taking absolute value first",
                                      "Validate input range before processing"]
    assert result.analysis_time_ms > 0


def test_analyze_sends_at_most_max_candidates_to_the_llm():
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=True, max_candidates_to_analyze=2))
    ten_candidates = [FailingCandidate(code=f"code_{i}", error_output=f"err_{i}", index=i)
                      for i in range(10)]
    llm = Mock(return_value=("analysis", 100, 50.0))

    analyzer.analyze(problem="p", candidates=ten_candidates, original_constraints=[], llm_call=llm)

    prompt = llm.call_args.args[0]
    assert "code_0" in prompt
    assert "code_1" in prompt
    assert "code_2" not in prompt


@pytest.mark.parametrize("embed_call, expected_embeddings", [
    pytest.param(lambda code: [0.5] * 10, [[0.5] * 10, [0.5] * 10], id="one-vector-per-candidate"),
    pytest.param(Mock(side_effect=RuntimeError("embedding service down")), [], id="embedder-fails"),
])
def test_analyze_embeds_each_candidate(embed_call, expected_embeddings):
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=True))

    # No LLM is given: the analysis still runs, and its text stays empty.
    result = analyzer.analyze(problem="p", candidates=TWO_FAILING_CANDIDATES, original_constraints=[],
                              embed_call=embed_call)

    assert result.raw_analysis == ""
    assert result.failure_embeddings == expected_embeddings


def test_each_analysis_with_a_task_id_appends_one_event(tmp_path):
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=True), telemetry_dir=tmp_path)
    llm = Mock(return_value=(STRUCTURED_ANSWER, 100, 50.0))

    for task_id in ["LCB_001", "", "t2"]:  # "" is no task id: no event
        analyzer.analyze(problem="p", candidates=TWO_FAILING_CANDIDATES, original_constraints=[],
                         llm_call=llm, task_id=task_id)

    lines = (tmp_path / "failure_analysis_events.jsonl").read_text().splitlines()
    events = [json.loads(line) for line in lines]
    assert [event["task_id"] for event in events] == ["LCB_001", "t2"]
    assert events[0]["num_candidates"] == 2
    assert sorted(events[0]["categories_found"]) == ["implementation_bug", "wrong_algorithm"]
    assert "analysis_time_ms" in events[0]
    assert "timestamp" in events[0]


def test_an_analysis_without_a_telemetry_dir_still_runs():
    analyzer = FailureAnalyzer(FailureAnalysisConfig(enabled=True), telemetry_dir=None)

    result = analyzer.analyze(problem="p", candidates=TWO_FAILING_CANDIDATES, original_constraints=[],
                              llm_call=Mock(return_value=("analysis", 100, 50.0)), task_id="t1")

    assert result.raw_analysis == "analysis"


# --- the categories and the parsers ------------------------------------------------------

def test_there_are_six_failure_categories_with_descriptions():
    assert set(FAILURE_CATEGORIES) == {
        "wrong_algorithm", "implementation_bug", "edge_case_miss",
        "time_limit", "format_error", "partial_correct",
    }
    for name, description in FAILURE_CATEGORIES.items():
        assert len(description) > 10, f"{name!r} has too short a description: {description!r}"


def test_parse_failure_categories_knows_every_category():
    for name in FAILURE_CATEGORIES:
        assert parse_failure_categories(f"Solution 1: {name}", 1) == {0: name}


@pytest.mark.parametrize("answer, num_candidates, expected", [
    pytest.param("Solution 1: wrong_algorithm\nSolution 2: implementation_bug", 2,
                 {0: "wrong_algorithm", 1: "implementation_bug"}, id="solution-n"),
    pytest.param("Candidate 1: edge_case_miss\nCandidate 2: time_limit", 2,
                 {0: "edge_case_miss", 1: "time_limit"}, id="candidate-n"),
    pytest.param("1) wrong_algorithm\n2) format_error", 2,
                 {0: "wrong_algorithm", 1: "format_error"}, id="numbered"),
    pytest.param("SOLUTION 1: WRONG_ALGORITHM", 1, {0: "wrong_algorithm"}, id="upper-case"),
    pytest.param("The code has a clear implementation_bug in the loop.", 2,
                 {0: "implementation_bug"}, id="a-mention-goes-to-the-first-candidate"),
    pytest.param("Solution 10: wrong_algorithm", 2,
                 {0: "wrong_algorithm"}, id="index-out-of-range-falls-back-to-the-mention"),
    pytest.param("Solution 1: nonexistent_category", 1, {}, id="unknown-category-ignored"),
    pytest.param("", 2, {}, id="empty-answer"),
])
def test_parse_failure_categories(answer, num_candidates, expected):
    assert parse_failure_categories(answer, num_candidates) == expected


@pytest.mark.parametrize("answer, expected", [
    pytest.param("VIOLATED:\n* Constraint one is important\n- Constraint two matters\nCOMMON:",
                 ["Constraint one is important", "Constraint two matters"], id="bullets-stripped"),
    pytest.param("VIOLATED:\n- Yes\n- This is a real constraint\nCOMMON:",
                 ["This is a real constraint"], id="lines-of-5-chars-or-less-skipped"),
    pytest.param("This response mentions no relevant keywords.", [], id="no-section"),
])
def test_parse_violated_constraints(answer, expected):
    assert parse_violated_constraints(answer) == expected


@pytest.mark.parametrize("answer, expected", [
    pytest.param("COMMON:\nLine one.\nLine two.\nLine three.\nNEW_CONSTRAINTS:",
                 "Line one. Line two. Line three.", id="lines-joined"),
    pytest.param("COMMON:\n\nNEW_CONSTRAINTS:", "", id="empty-section"),
    pytest.param("Just some analysis with no relevant keywords here.", "", id="no-section"),
])
def test_parse_common_pattern(answer, expected):
    assert parse_common_pattern(answer) == expected


@pytest.mark.parametrize("answer, expected", [
    pytest.param("NEW_CONSTRAINTS:\n1. Always validate boundary conditions\n2. Handle the zero case explicitly",
                 ["Always validate boundary conditions", "Handle the zero case explicitly"],
                 id="numbers-stripped"),
    pytest.param("NEW_CONSTRAINTS:\n- Short\n- This is a sufficiently long constraint",
                 ["This is a sufficiently long constraint"], id="lines-of-10-chars-or-less-skipped"),
    pytest.param("NEW_CONSTRAINT:\n- Validate all edge cases before processing",
                 ["Validate all edge cases before processing"], id="singular-header"),
    pytest.param("Some analysis without new constraints section.", [], id="no-section"),
])
def test_parse_new_constraints(answer, expected):
    assert parse_new_constraints(answer) == expected


@pytest.mark.parametrize("candidates, expected", [
    pytest.param([], "", id="none"),
    pytest.param([FailingCandidate(code="def f(): pass", error_output="TypeError", index=0)],
                 "Solution 1:\n```python\ndef f(): pass\n```\nError: TypeError", id="one"),
    pytest.param([FailingCandidate(code="code_a", error_output="error_a", index=0),
                  FailingCandidate(code="code_b", error_output="error_b", index=1)],
                 "Solution 1:\n```python\ncode_a\n```\nError: error_a\n\n"
                 "Solution 2:\n```python\ncode_b\n```\nError: error_b", id="two"),
])
def test_format_candidates_with_errors(candidates, expected):
    assert format_candidates_with_errors(candidates) == expected


# --- the data structures ------------------------------------------------------------------

@pytest.mark.parametrize("candidate, expected", [
    pytest.param(FailingCandidate(code="def solve(): return 42",
                                  error_output="AssertionError: got 42 expected 43", index=3),
                 {"index": 3, "code_length": len("def solve(): return 42"),
                  "error_preview": "AssertionError: got 42 expected 43"}, id="short-error"),
    pytest.param(FailingCandidate(code="x", error_output="E" * 500, index=0),
                 {"index": 0, "code_length": 1, "error_preview": "E" * 200}, id="error-cut-to-200-chars"),
])
def test_failing_candidate_to_dict(candidate, expected):
    assert candidate.to_dict() == expected


def test_failure_analysis_to_dict_counts_the_embeddings():
    analysis = FailureAnalysis(
        categories={0: "wrong_algorithm", 1: "edge_case_miss"},
        violated_constraints=["Must sort", "Must handle empty"],
        common_pattern="All assume sorted input",
        new_constraints=["Validate input order"],
        failure_embeddings=[[1.0, 2.0], [3.0, 4.0]],
        analysis_time_ms=150.0,
    )
    assert analysis.to_dict() == {
        "categories": {0: "wrong_algorithm", 1: "edge_case_miss"},
        "violated_constraints": ["Must sort", "Must handle empty"],
        "common_pattern": "All assume sorted input",
        "new_constraints": ["Validate input order"],
        "num_embeddings": 2,
        "analysis_time_ms": 150.0,
    }


def test_event_to_dict_fills_in_the_timestamp():
    event = FailureAnalysisEvent(
        task_id="t1", num_candidates=3,
        categories_found=["wrong_algorithm", "edge_case_miss"],
        num_violated_constraints=2, num_new_constraints=1, analysis_time_ms=100.0,
    )
    as_dict = event.to_dict()
    assert as_dict["task_id"] == "t1"
    assert as_dict["num_candidates"] == 3
    assert as_dict["categories_found"] == ["wrong_algorithm", "edge_case_miss"]
    assert as_dict["timestamp"]  # none was given, so the current time is filled in


def test_config_defaults():
    config = FailureAnalysisConfig()
    assert config.enabled is False
    assert config.max_candidates_to_analyze == 5
    assert config.analysis_temperature == 0.3
    assert config.analysis_max_tokens == 2048
