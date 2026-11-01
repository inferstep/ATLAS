"""Tests for the V3 refinement loop (Feature 3E): analyze why the candidates
failed, refine the constraints, generate code, test it in the sandbox, and
repeat until a candidate passes or the loop's budget is spent."""

import json

import pytest

from stages.constraint_refinement import RefinedHypothesis
from stages.failure_analysis import (
    FailingCandidate,
    FailureAnalysisConfig,
    FailureAnalyzer,
)
from stages.refinement_loop import (
    IterationResult,
    RefinementLoop,
    RefinementLoopConfig,
    RefinementResult,
)


# The input of every run below: two candidates that failed phases 1 and 2.
FAILING_CANDIDATES = [
    FailingCandidate(code="def solve(n): return n * 2",
                     error_output="AssertionError: expected 3 got 4", index=0),
    FailingCandidate(code="def solve(n): return n + 1",
                     error_output="AssertionError: expected 0 got 1", index=1),
]

# The code fake_llm writes inside its fenced answer.
LLM_CODE = "def solve(n):\n    if n == 0: return 0\n    return n + 1\n"


def fake_llm(prompt, temperature, max_tokens, seed):
    """Answers the three calls of one iteration in the formats the loop
    parses: the failure analysis, one refined hypothesis, then the code."""
    if "Analyze these failures" in prompt:
        return ("CATEGORY:\nSolution 1: wrong_algorithm\n\n"
                "VIOLATED:\n- Must handle edge cases properly\n\n"
                "COMMON:\nAll solutions miss the zero case\n\n"
                "NEW_CONSTRAINTS:\n- Always check for zero input first\n"), 80, 30.0
    if "REFINED constraint" in prompt:
        return ("HYPOTHESIS 1:\n"
                "APPROACH: Check zero first then process\n"
                "RATIONALE: Prevents the common failure pattern\n"
                "CONSTRAINTS:\n"
                "- Must handle edge cases\n"
                "- NEW: Check for zero input at the start\n"), 60, 25.0
    return f"```python\n{LLM_CODE}```", 40, 20.0


def sandbox_passes(code, test_input):
    return True, "correct", ""


def sandbox_fails(code, test_input):
    return False, "", "AssertionError: wrong answer"


@pytest.mark.parametrize("enabled, candidates, llm_call, sandbox_run, reason", [
    pytest.param(False, FAILING_CANDIDATES, fake_llm, sandbox_passes, "disabled", id="disabled"),
    pytest.param(True, [], fake_llm, sandbox_passes, "no_candidates", id="no failing candidates"),
    pytest.param(True, FAILING_CANDIDATES, None, sandbox_passes, "missing_callables", id="no LLM"),
    pytest.param(True, FAILING_CANDIDATES, fake_llm, None, "missing_callables", id="no sandbox"),
])
def test_a_loop_that_cannot_start_returns_an_empty_result(
        tmp_path, enabled, candidates, llm_call, sandbox_run, reason):
    loop = RefinementLoop(RefinementLoopConfig(enabled=enabled), telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=candidates, original_constraints=["c1"],
                      llm_call=llm_call, sandbox_run=sandbox_run, task_id="t1")
    # Not triggered, not solved, no iterations: only the task id and the reason.
    assert result == RefinementResult(task_id="t1", reason=reason)
    assert not (tmp_path / "refinement_loop_events.jsonl").exists()


def test_a_passing_candidate_ends_the_loop_after_one_iteration(tmp_path):
    # Built without an analyzer or a refiner: the loop makes enabled ones itself.
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=5),
                          telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes)
    assert result.triggered is True
    assert result.solved is True
    assert result.reason == "solved"
    assert result.winning_code == LLM_CODE
    assert result.total_iterations == 1
    assert result.iterations[0].iteration == 0
    assert result.iterations[0].time_ms > 0
    assert result.total_time_ms > 0


def test_the_loop_runs_on_until_a_candidate_passes(tmp_path):
    # The sandbox fails the first new candidate and passes the second.
    verdicts = iter([(False, "", "AssertionError: wrong answer"), (True, "correct", "")])
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=5),
                          telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm,
                      sandbox_run=lambda code, test_input: next(verdicts))
    assert result.solved is True
    assert result.total_iterations == 2


def test_a_loop_that_never_passes_stops_after_max_iterations(tmp_path):
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=3),
                          telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_fails)
    assert result.solved is False
    assert result.winning_code == ""
    assert result.total_iterations == 3
    assert result.reason == "max_iterations_exhausted"


def test_each_failed_attempt_joins_the_failures_the_next_analysis_sees(tmp_path):
    analysed = []

    class RecordingAnalyzer(FailureAnalyzer):
        def analyze(self, problem, candidates, *args, **kwargs):
            analysed.append([c.code for c in candidates])
            return super().analyze(problem, candidates, *args, **kwargs)

    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=2),
                          failure_analyzer=RecordingAnalyzer(FailureAnalysisConfig(enabled=True)),
                          telemetry_dir=tmp_path)
    loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
             original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_fails)
    original = [c.code for c in FAILING_CANDIDATES]
    assert analysed == [original, original + [LLM_CODE]]


def test_the_loop_stops_when_its_time_budget_is_spent(tmp_path):
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=100, max_time_ms=1.0),
                          telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_fails)
    assert result.reason == "time_budget_exceeded"
    assert result.total_iterations < 100


def test_a_code_gen_callable_writes_the_code_in_place_of_the_llm(tmp_path):
    def code_gen(problem, constraints, seed):
        return "def solve(n): return n", 50, 25.0

    loop = RefinementLoop(RefinementLoopConfig(enabled=True), telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes,
                      code_gen=code_gen)
    assert result.solved is True
    assert result.winning_code == "def solve(n): return n"
    assert result.total_tokens == 50


def test_a_hypothesis_embedded_on_top_of_a_failure_is_dropped(tmp_path):
    # Every text embeds to the same point, so the refined hypothesis is at
    # distance 0 from each failure: it is dropped and no code is written,
    # though this sandbox would pass anything.
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=2),
                          telemetry_dir=tmp_path)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes,
                      embed_call=lambda text: [1.0, 1.0])
    assert result.solved is False
    assert [it.error for it in result.iterations] == ["no_viable_hypotheses"] * 2


def test_each_run_with_a_task_id_appends_one_event(tmp_path):
    loop = RefinementLoop(RefinementLoopConfig(enabled=True), telemetry_dir=tmp_path)
    for task_id in ("LCB_001", "LCB_002"):
        loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                 original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes,
                 task_id=task_id)
    lines = (tmp_path / "refinement_loop_events.jsonl").read_text().splitlines()
    assert [json.loads(line)["task_id"] for line in lines] == ["LCB_001", "LCB_002"]
    event = json.loads(lines[0])
    assert set(event) == {"task_id", "triggered", "solved", "total_iterations",
                          "total_tokens", "total_time_ms", "reason", "timestamp"}
    assert event["triggered"] is True
    assert event["solved"] is True
    assert event["total_iterations"] == 1
    assert event["reason"] == "solved"
    assert event["timestamp"] != ""


def test_a_run_without_a_task_id_writes_no_event(tmp_path):
    loop = RefinementLoop(RefinementLoopConfig(enabled=True), telemetry_dir=tmp_path)
    loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
             original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes)
    assert not (tmp_path / "refinement_loop_events.jsonl").exists()


def test_a_loop_without_a_telemetry_dir_still_runs():
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=1), telemetry_dir=None)
    result = loop.run(problem="test", failing_candidates=FAILING_CANDIDATES,
                      original_constraints=["c1"], llm_call=fake_llm, sandbox_run=sandbox_passes,
                      task_id="t1")
    assert result.solved is True


@pytest.mark.parametrize("iteration, expected", [
    pytest.param(IterationResult(iteration=2, hypothesis_used=RefinedHypothesis(approach="DP"),
                                 code_generated="def solve(): pass", passed=True, error="",
                                 tokens_used=50, time_ms=200.0),
                 {"iteration": 2, "has_hypothesis": True, "passed": True,
                  "error_preview": "", "tokens_used": 50, "time_ms": 200.0},
                 id="a passing iteration"),
    pytest.param(IterationResult(iteration=0),
                 {"iteration": 0, "has_hypothesis": False, "passed": False,
                  "error_preview": "", "tokens_used": 0, "time_ms": 0.0},
                 id="no hypothesis"),
    pytest.param(IterationResult(error="E" * 500),
                 {"iteration": 0, "has_hypothesis": False, "passed": False,
                  "error_preview": "E" * 200, "tokens_used": 0, "time_ms": 0.0},
                 id="a long error cut to 200 characters"),
])
def test_iteration_result_to_dict(iteration, expected):
    assert iteration.to_dict() == expected


def test_refinement_result_to_dict():
    iteration = IterationResult(iteration=0, passed=True)
    result = RefinementResult(task_id="t1", triggered=True, solved=True,
                              winning_code="def solve(): pass", iterations=[iteration],
                              total_iterations=1, total_tokens=200, total_time_ms=500.0,
                              reason="solved")
    assert result.to_dict() == {
        "task_id": "t1", "triggered": True, "solved": True, "total_iterations": 1,
        "total_tokens": 200, "total_time_ms": 500.0, "reason": "solved",
        "iterations": [iteration.to_dict()],
    }


def test_config_defaults():
    cfg = RefinementLoopConfig()
    assert cfg.enabled is False
    assert cfg.max_iterations == 2
    assert cfg.max_time_ms == 120000.0  # 2 minutes


def test_refinement_prompt_preserves_the_declared_interface_contract():
    prompt = RefinementLoop._build_code_prompt(
        object(), "Implement the requested API", ["handle errors"], "iterate")
    assert "exact signature" in prompt
    assert "Preserve existing public interfaces" in prompt
    assert "parameter order and kinds (including / and * markers)" in prompt
    assert "laziness, stopping conditions, ordering" in prompt
    assert "never import that file or requested artifact from itself" in prompt
    assert "mentally compile" in prompt
    assert "complete, syntactically valid Python source file only" in prompt
