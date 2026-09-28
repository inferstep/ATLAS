"""Tests for V3 Refinement Loop (Feature 3E)."""

import json
from typing import List, Optional, Tuple

import pytest

from stages.refinement_loop import (
    IterationResult,
    RefinementLoop,
    RefinementLoopConfig,
    RefinementLoopEvent,
    RefinementResult,
)
from stages.failure_analysis import (
    FailingCandidate,
    FailureAnalysisConfig,
    FailureAnalyzer,
)
from stages.constraint_refinement import (
    ConstraintRefinementConfig,
    ConstraintRefiner,
    RefinedHypothesis,
)


# ---------------------------------------------------------------------------
# Mock callables
# ---------------------------------------------------------------------------

class MockLLM:
    """Mock LLM that returns predefined responses for analysis and refinement."""

    def __init__(self):
        self.calls: list = []
        self._call_count = 0

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int]) -> Tuple[str, int, float]:
        self.calls.append({
            "prompt": prompt, "temperature": temperature,
            "max_tokens": max_tokens, "seed": seed,
        })
        self._call_count += 1

        # Return analysis-like response for failure analyzer calls
        if "Analyze these failures" in prompt or "expert debugger" in prompt:
            return (
                "CATEGORY:\nSolution 1: wrong_algorithm\n\n"
                "VIOLATED:\n- Must handle edge cases properly\n\n"
                "COMMON:\nAll solutions miss the zero case\n\n"
                "NEW_CONSTRAINTS:\n- Always check for zero input first\n"
            ), 80, 30.0

        # Return hypothesis response for constraint refiner calls
        if "REFINED constraint" in prompt or "constraint engineering" in prompt:
            return (
                "HYPOTHESIS 1:\n"
                "APPROACH: Check zero first then process\n"
                "RATIONALE: Prevents the common failure pattern\n"
                "CONSTRAINTS:\n"
                "- Must handle edge cases\n"
                "- NEW: Check for zero input at the start\n"
            ), 60, 25.0

        # Return code for code generation calls
        return "```python\ndef solve(n):\n    if n == 0: return 0\n    return n + 1\n```", 40, 20.0


class MockSandbox:
    """Mock sandbox that can be configured to pass/fail."""

    def __init__(self, pass_on_iteration: Optional[int] = None):
        self.calls: list = []
        self._call_count = 0
        self._pass_on = pass_on_iteration

    def __call__(self, code: str, test_input: str = "", **_) -> Tuple[bool, str, str]:
        self.calls.append({"code": code, "input": test_input})
        self._call_count += 1
        if self._pass_on is not None and self._call_count >= self._pass_on:
            return True, "correct", ""
        return False, "", "AssertionError: wrong answer"


class MockSandboxAlwaysPass:
    """Mock sandbox that always passes."""

    def __init__(self):
        self.calls: list = []

    def __call__(self, code: str, test_input: str = "", **_) -> Tuple[bool, str, str]:
        self.calls.append({"code": code, "input": test_input})
        return True, "correct", ""


class MockSandboxAlwaysFail:
    """Mock sandbox that always fails."""

    def __init__(self):
        self.calls: list = []

    def __call__(self, code: str, test_input: str = "", **_) -> Tuple[bool, str, str]:
        self.calls.append({"code": code, "input": test_input})
        return False, "", "AssertionError: wrong answer"


class MockEmbed:
    """Mock embedding callable."""

    def __init__(self, dim: int = 10):
        self.dim = dim
        self.calls: list = []

    def __call__(self, text: str) -> List[float]:
        self.calls.append(text)
        return [float(len(text) % 10) / 10.0] * self.dim


class MockCodeGen:
    """Mock code generation callable."""

    def __init__(self, code: str = "def solve(n): return n + 1"):
        self.code = code
        self.calls: list = []

    def __call__(self, problem: str, constraints: List[str],
                 seed: int) -> Tuple[str, int, float]:
        self.calls.append({
            "problem": problem, "constraints": constraints, "seed": seed,
        })
        return self.code, 50, 25.0


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def tmp_telemetry(tmp_path):
    """Provide a temporary telemetry directory."""
    d = tmp_path / "telemetry"
    d.mkdir()
    return d


@pytest.fixture
def loop_enabled(tmp_telemetry):
    """RefinementLoop instance with enabled=True and sub-components enabled."""
    cfg = RefinementLoopConfig(enabled=True, max_iterations=5)
    fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
    cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
    return RefinementLoop(cfg, failure_analyzer=fa, constraint_refiner=cr,
                          telemetry_dir=tmp_telemetry)


@pytest.fixture
def loop_disabled(tmp_telemetry):
    """RefinementLoop instance with enabled=False."""
    cfg = RefinementLoopConfig(enabled=False)
    return RefinementLoop(cfg, telemetry_dir=tmp_telemetry)


@pytest.fixture
def sample_candidates():
    """A set of failing candidates for testing."""
    return [
        FailingCandidate(
            code="def solve(n): return n * 2",
            error_output="AssertionError: expected 3 got 4",
            index=0,
        ),
        FailingCandidate(
            code="def solve(n): return n + 1",
            error_output="AssertionError: expected 0 got 1",
            index=1,
        ),
    ]


# ---------------------------------------------------------------------------
# Test: Disabled noop behavior
# ---------------------------------------------------------------------------

class TestDisabledNoop:
    """When enabled=False, RefinementLoop should be a complete noop."""

    def test_returns_not_triggered(self, loop_disabled, sample_candidates):
        result = loop_disabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["Must sort"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
        )
        assert result.triggered is False
        assert result.solved is False
        assert result.reason == "disabled"

    def test_preserves_task_id(self, loop_disabled, sample_candidates):
        result = loop_disabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[], task_id="my_task",
        )
        assert result.task_id == "my_task"

    def test_no_telemetry_when_disabled(self, loop_disabled, sample_candidates, tmp_telemetry):
        loop_disabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[], llm_call=MockLLM(),
            sandbox_run=MockSandboxAlwaysPass(), task_id="t1",
        )
        events_file = tmp_telemetry / "refinement_loop_events.jsonl"
        assert not events_file.exists()

    def test_no_iterations_when_disabled(self, loop_disabled, sample_candidates):
        result = loop_disabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[],
        )
        assert result.iterations == []
        assert result.total_iterations == 0


# ---------------------------------------------------------------------------
# Test: RefinementLoop.run — stops on sandbox pass
# ---------------------------------------------------------------------------

class TestStopsOnPass:

    def test_stops_when_sandbox_passes(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        # Passes on first sandbox call
        mock_sandbox = MockSandboxAlwaysPass()
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["Must handle zero"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
            task_id="t1",
        )
        assert result.solved is True
        assert result.reason == "solved"
        assert len(result.winning_code) > 0

    def test_stops_on_second_iteration(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=5)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        mock_llm = MockLLM()
        # Fails first, passes second
        mock_sandbox = MockSandbox(pass_on_iteration=2)
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.solved is True
        assert result.total_iterations == 2

    def test_solved_result_has_winning_code(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert len(result.winning_code) > 0


# ---------------------------------------------------------------------------
# Test: RefinementLoop.run — stops at max_iterations
# ---------------------------------------------------------------------------

class TestStopsAtMaxIterations:

    def test_respects_max_iterations(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=3)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysFail()
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.solved is False
        assert result.total_iterations <= 3
        assert result.reason == "max_iterations_exhausted"

    def test_max_iterations_one(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=1)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysFail()
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.total_iterations <= 1


# ---------------------------------------------------------------------------
# Test: Time budget enforcement
# ---------------------------------------------------------------------------

class TestTimeBudget:

    def test_respects_time_budget(self, tmp_telemetry, sample_candidates):
        # Set a very short time budget (1ms) to guarantee it triggers
        cfg = RefinementLoopConfig(enabled=True, max_iterations=100, max_time_ms=1.0)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysFail()
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.reason == "time_budget_exceeded"
        assert result.total_iterations < 100

    def test_default_time_budget(self):
        cfg = RefinementLoopConfig()
        assert cfg.max_time_ms == 120000.0  # 2 minutes


# ---------------------------------------------------------------------------
# Test: Edge cases
# ---------------------------------------------------------------------------

class TestEdgeCases:

    def test_empty_candidates(self, loop_enabled):
        result = loop_enabled.run(
            problem="test", failing_candidates=[],
            original_constraints=[],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
        )
        assert result.triggered is False
        assert result.reason == "no_candidates"

    def test_missing_llm_callable(self, loop_enabled, sample_candidates):
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[],
            sandbox_run=MockSandboxAlwaysPass(),
        )
        assert result.triggered is False
        assert result.reason == "missing_callables"

    def test_missing_sandbox_callable(self, loop_enabled, sample_candidates):
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=[],
            llm_call=MockLLM(),
        )
        assert result.triggered is False
        assert result.reason == "missing_callables"

    def test_code_gen_callable_used(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        mock_codegen = MockCodeGen("def solve(n): return n")
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
            code_gen=mock_codegen,
        )
        assert result.solved is True
        assert len(mock_codegen.calls) > 0

    def test_embed_callable_used(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        mock_embed = MockEmbed(dim=5)
        # Run for the embed-call side effect; the result itself isn't asserted
        # here (the assertion is on the mock's call log below).
        _ = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
            embed_call=mock_embed,
        )
        assert len(mock_embed.calls) > 0

    def test_default_sub_components(self, tmp_telemetry, sample_candidates):
        """RefinementLoop creates default sub-components if not provided."""
        cfg = RefinementLoopConfig(enabled=True, max_iterations=1)
        loop = RefinementLoop(cfg, telemetry_dir=tmp_telemetry)
        assert loop.failure_analyzer is not None
        assert loop.constraint_refiner is not None

    def test_failed_iteration_adds_to_failure_set(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=2)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysFail()
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        # After failing iterations, each generates a new FailingCandidate
        assert result.total_iterations > 0
        assert result.solved is False


# ---------------------------------------------------------------------------
# Test: AC-3E-1 — Orchestrates analyze->refine->generate->test cycle
# ---------------------------------------------------------------------------

class TestAC3E1OrchestrationCycle:
    """AC-3E-1: Loop orchestrates the full analyze->refine->generate->test cycle."""

    def test_triggered_on_failing_candidates(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.triggered is True

    def test_iterations_recorded(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert len(result.iterations) > 0
        for it in result.iterations:
            assert isinstance(it, IterationResult)
            assert it.time_ms > 0


# ---------------------------------------------------------------------------
# Test: AC-3E-2 — Stops when sandbox passes
# ---------------------------------------------------------------------------

class TestAC3E2StopsOnPass:
    """AC-3E-2: Loop terminates immediately when a solution passes."""

    def test_first_pass_terminates(self, loop_enabled, sample_candidates):
        mock_llm = MockLLM()
        mock_sandbox = MockSandboxAlwaysPass()
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=mock_llm, sandbox_run=mock_sandbox,
        )
        assert result.solved is True
        assert result.total_iterations == 1


# ---------------------------------------------------------------------------
# Test: AC-3E-3 — Stops at max_iterations
# ---------------------------------------------------------------------------

class TestAC3E3MaxIterations:
    """AC-3E-3: Loop respects max_iterations limit."""

    def test_exhausts_max_iterations(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=2)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysFail(),
        )
        assert result.total_iterations <= 2
        assert result.reason == "max_iterations_exhausted"


# ---------------------------------------------------------------------------
# Test: AC-3E-4 — Time budget enforcement
# ---------------------------------------------------------------------------

class TestAC3E4TimeBudget:
    """AC-3E-4: Loop respects time budget."""

    def test_time_budget_stops_loop(self, tmp_telemetry, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=100, max_time_ms=1.0)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=tmp_telemetry)

        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysFail(),
        )
        assert result.reason == "time_budget_exceeded"


# ---------------------------------------------------------------------------
# Test: AC-3E-5 — Tracks total tokens and time
# ---------------------------------------------------------------------------

class TestAC3E5Tracking:
    """AC-3E-5: Loop tracks total tokens, time, and iteration details."""

    def test_total_tokens_tracked(self, loop_enabled, sample_candidates):
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
            code_gen=MockCodeGen(),
        )
        assert result.total_tokens > 0

    def test_total_time_tracked(self, loop_enabled, sample_candidates):
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
        )
        assert result.total_time_ms > 0

    def test_iteration_details_tracked(self, loop_enabled, sample_candidates):
        result = loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
        )
        for it in result.iterations:
            assert it.iteration >= 0
            assert it.time_ms > 0


# ---------------------------------------------------------------------------
# Test: Telemetry
# ---------------------------------------------------------------------------

class TestTelemetry:

    def test_event_written_to_jsonl(self, loop_enabled, sample_candidates, tmp_telemetry):
        loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
            task_id="LCB_001",
        )
        events_file = tmp_telemetry / "refinement_loop_events.jsonl"
        assert events_file.exists()
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 1
        data = json.loads(lines[0])
        assert data["task_id"] == "LCB_001"
        assert data["triggered"] is True
        assert "solved" in data
        assert "total_iterations" in data
        assert "total_tokens" in data
        assert "total_time_ms" in data
        assert "reason" in data
        assert "timestamp" in data

    def test_multiple_events_appended(self, loop_enabled, sample_candidates, tmp_telemetry):
        for i in range(3):
            loop_enabled.run(
                problem="test", failing_candidates=sample_candidates,
                original_constraints=["c1"],
                llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
                task_id=f"t{i}",
            )
        events_file = tmp_telemetry / "refinement_loop_events.jsonl"
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 3

    def test_no_telemetry_without_task_id(self, loop_enabled, sample_candidates, tmp_telemetry):
        loop_enabled.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
        )
        events_file = tmp_telemetry / "refinement_loop_events.jsonl"
        assert not events_file.exists()

    def test_no_crash_without_telemetry_dir(self, sample_candidates):
        cfg = RefinementLoopConfig(enabled=True, max_iterations=1)
        fa = FailureAnalyzer(FailureAnalysisConfig(enabled=True))
        cr = ConstraintRefiner(ConstraintRefinementConfig(enabled=True))
        loop = RefinementLoop(cfg, fa, cr, telemetry_dir=None)
        result = loop.run(
            problem="test", failing_candidates=sample_candidates,
            original_constraints=["c1"],
            llm_call=MockLLM(), sandbox_run=MockSandboxAlwaysPass(),
            task_id="t1",
        )
        assert isinstance(result, RefinementResult)


# ---------------------------------------------------------------------------
# Test: Data structures
# ---------------------------------------------------------------------------

class TestDataStructures:

    def test_iteration_result_to_dict(self):
        it = IterationResult(
            iteration=2,
            hypothesis_used=RefinedHypothesis(approach="DP"),
            code_generated="def solve(): pass",
            passed=True, error="",
            tokens_used=50, time_ms=200.0,
        )
        d = it.to_dict()
        assert d["iteration"] == 2
        assert d["has_hypothesis"] is True
        assert d["passed"] is True
        assert d["error_preview"] == ""
        assert d["tokens_used"] == 50
        assert d["time_ms"] == 200.0

    def test_iteration_result_no_hypothesis(self):
        it = IterationResult(iteration=0)
        d = it.to_dict()
        assert d["has_hypothesis"] is False

    def test_iteration_result_error_truncated(self):
        it = IterationResult(error="E" * 500)
        d = it.to_dict()
        assert len(d["error_preview"]) == 200

    def test_refinement_result_to_dict(self):
        r = RefinementResult(
            task_id="t1", triggered=True, solved=True,
            winning_code="def solve(): pass",
            iterations=[IterationResult(iteration=0, passed=True)],
            total_iterations=1, total_tokens=200,
            total_time_ms=500.0, reason="solved",
        )
        d = r.to_dict()
        assert d["task_id"] == "t1"
        assert d["triggered"] is True
        assert d["solved"] is True
        assert d["total_iterations"] == 1
        assert d["reason"] == "solved"
        assert len(d["iterations"]) == 1

    def test_event_to_dict(self):
        e = RefinementLoopEvent(
            task_id="t1", triggered=True, solved=False,
            total_iterations=3, total_tokens=500,
            total_time_ms=2000.0, reason="max_iterations_exhausted",
        )
        d = e.to_dict()
        assert d["task_id"] == "t1"
        assert d["triggered"] is True
        assert d["solved"] is False
        assert d["total_iterations"] == 3
        assert d["reason"] == "max_iterations_exhausted"
        assert "timestamp" in d

    def test_config_defaults(self):
        cfg = RefinementLoopConfig()
        assert cfg.enabled is False
        assert cfg.max_iterations == 2
        assert cfg.max_time_ms == 120000.0


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
