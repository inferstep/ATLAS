"""Tests for V3 PR-CoT (Feature 3C) — Multi-Perspective Repair."""

import json
from typing import Optional, Tuple

import pytest

from stages.pr_cot import (
    PERSPECTIVES,
    PRCoT,
    PRCoTConfig,
    PRCoTEvent,
    PRCoTResult,
    PerspectiveResult,
    extract_code_from_repair,
)


# ---------------------------------------------------------------------------
# Mock callables
# ---------------------------------------------------------------------------

class MockLLM:
    """Mock LLM that returns predefined responses."""

    def __init__(self, response: str = ""):
        self.response = response
        self.calls: list = []

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int]) -> Tuple[str, int, float]:
        self.calls.append({
            "prompt": prompt, "temperature": temperature,
            "max_tokens": max_tokens, "seed": seed,
        })
        return self.response, 50, 25.0


class MockLLMAlternating:
    """Mock LLM that alternates between analysis and repair responses."""

    def __init__(self):
        self.calls: list = []
        self._call_count = 0

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int]) -> Tuple[str, int, float]:
        self.calls.append({"prompt": prompt, "seed": seed})
        self._call_count += 1
        if self._call_count % 2 == 1:
            # Analysis response
            return "The code has an off-by-one error in the loop.", 30, 10.0
        else:
            # Repair response with code
            return "```python\ndef solve(n):\n    return n + 1\n```", 40, 15.0


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
def pr_enabled(tmp_telemetry):
    """PRCoT instance with enabled=True."""
    cfg = PRCoTConfig(enabled=True)
    return PRCoT(cfg, telemetry_dir=tmp_telemetry)


@pytest.fixture
def pr_disabled(tmp_telemetry):
    """PRCoT instance with enabled=False."""
    cfg = PRCoTConfig(enabled=False)
    return PRCoT(cfg, telemetry_dir=tmp_telemetry)


# ---------------------------------------------------------------------------
# Test: Disabled noop behavior
# ---------------------------------------------------------------------------

class TestDisabledNoop:
    """When enabled=False, PRCoT should be a complete noop."""

    def test_returns_empty_result(self, pr_disabled):
        result = pr_disabled.repair(
            problem="test", code="def f(): pass", error="error",
            llm_call=MockLLM("should not run"), task_id="t1",
        )
        assert result.perspectives == []
        assert result.repairs == []
        assert result.total_tokens == 0

    def test_preserves_task_id(self, pr_disabled):
        result = pr_disabled.repair(
            problem="test", code="x", error="e", task_id="my_task",
        )
        assert result.task_id == "my_task"

    def test_no_telemetry_when_disabled(self, pr_disabled, tmp_telemetry):
        pr_disabled.repair(
            problem="test", code="x", error="e",
            llm_call=MockLLM("x"), task_id="t1",
        )
        events_file = tmp_telemetry / "pr_cot_events.jsonl"
        assert not events_file.exists()

    def test_does_not_call_llm_when_disabled(self, pr_disabled):
        mock_llm = MockLLM("should not run")
        pr_disabled.repair(
            problem="test", code="x", error="e", llm_call=mock_llm,
        )
        assert len(mock_llm.calls) == 0


# ---------------------------------------------------------------------------
# Test: PERSPECTIVES
# ---------------------------------------------------------------------------

class TestPerspectives:

    def test_has_four_perspectives(self):
        assert len(PERSPECTIVES) == 4

    def test_expected_perspectives(self):
        expected = {
            "logical_consistency", "information_completeness",
            "biases", "alternative_solutions",
        }
        assert set(PERSPECTIVES.keys()) == expected

    def test_descriptions_are_nonempty(self):
        for name, desc in PERSPECTIVES.items():
            assert len(desc) > 20, f"Perspective '{name}' has too short a description"

    def test_instance_perspectives_property(self, pr_enabled):
        assert pr_enabled.perspectives == PERSPECTIVES
        assert pr_enabled.num_perspectives == 4


# ---------------------------------------------------------------------------
# Test: extract_code_from_repair
# ---------------------------------------------------------------------------

class TestExtractCodeFromRepair:

    def test_python_block(self):
        response = "Here is the fix:\n```python\ndef solve(n):\n    return n + 1\n```"
        code = extract_code_from_repair(response)
        assert code == "def solve(n):\n    return n + 1\n"

    def test_plain_code_block(self):
        response = "Fixed:\n```\ndef solve(n):\n    return n + 1\n```"
        code = extract_code_from_repair(response)
        assert code == "def solve(n):\n    return n + 1\n"

    def test_raw_code_fallback(self):
        response = "def solve(n):\n    return n + 1"
        code = extract_code_from_repair(response)
        assert code == "def solve(n):\n    return n + 1"

    def test_multiple_blocks_takes_last(self):
        response = (
            "```python\ndef helper(): pass\n```\n"
            "The full solution:\n"
            "```python\ndef solve(n):\n    return n + 1\n```"
        )
        code = extract_code_from_repair(response)
        assert code == "def solve(n):\n    return n + 1\n"

    def test_strips_thinking_blocks(self):
        response = "<think>Let me fix this.</think>\n```python\ndef solve(): pass\n```"
        code = extract_code_from_repair(response)
        assert code == "def solve(): pass\n"

    def test_empty_response(self):
        code = extract_code_from_repair("")
        assert code == ""

    def test_no_code_returns_stripped_response(self):
        response = "The solution needs to handle edge cases properly."
        code = extract_code_from_repair(response)
        assert code == response


# ---------------------------------------------------------------------------
# Test: PRCoT.repair (enabled, with mock LLM)
# ---------------------------------------------------------------------------

class TestRepairEnabled:

    def test_basic_repair(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="Find two sum",
            code="def solve(nums, target): pass",
            error="Wrong answer",
            llm_call=mock_llm,
            task_id="t1",
        )
        assert isinstance(result, PRCoTResult)
        assert result.task_id == "t1"
        assert len(result.perspectives) > 0
        assert result.total_tokens > 0
        assert result.total_time_ms > 0

    def test_generates_perspective_analyses(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert len(result.perspectives) > 0
        for p in result.perspectives:
            assert p.perspective in PERSPECTIVES
            assert len(p.analysis) > 0

    def test_generates_repair_candidates(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert len(result.repairs) > 0
        for r in result.repairs:
            assert isinstance(r, str)
            assert len(r) > 0

    def test_two_calls_per_perspective(self, pr_enabled):
        """Each perspective should produce one analysis call and one repair call."""
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        num_perspectives = len(result.perspectives)
        assert len(mock_llm.calls) == num_perspectives * 2

    def test_no_llm_returns_empty(self, pr_enabled):
        result = pr_enabled.repair(
            problem="test", code="x", error="e", llm_call=None,
        )
        assert result.perspectives == []
        assert result.repairs == []

    def test_analysis_prompt_contains_problem(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        pr_enabled.repair(
            problem="Find maximum subarray sum",
            code="def solve(): pass", error="WA",
            llm_call=mock_llm,
        )
        # First call should be analysis containing the problem
        first_call = mock_llm.calls[0]
        assert "Find maximum subarray sum" in first_call["prompt"]

    def test_repair_prompt_contains_analysis(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        # Second call (repair) should contain the analysis text
        second_call = mock_llm.calls[1]
        assert "off-by-one" in second_call["prompt"]

    def test_identical_repair_not_added(self, pr_enabled):
        """If repair code is identical to original, it should not be added."""
        mock_llm = MockLLM("def f(): pass")  # Returns same code
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert len(result.repairs) == 0


# ---------------------------------------------------------------------------
# Test: max_repair_rounds respected
# ---------------------------------------------------------------------------

class TestMaxRepairRounds:

    def test_respects_max_repair_rounds(self, tmp_telemetry):
        cfg = PRCoTConfig(enabled=True, max_repair_rounds=2)
        pr = PRCoT(cfg, telemetry_dir=tmp_telemetry)
        mock_llm = MockLLMAlternating()
        result = pr.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert len(result.repairs) <= 2

    def test_max_rounds_one(self, tmp_telemetry):
        cfg = PRCoTConfig(enabled=True, max_repair_rounds=1)
        pr = PRCoT(cfg, telemetry_dir=tmp_telemetry)
        mock_llm = MockLLMAlternating()
        result = pr.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert len(result.repairs) <= 1

    def test_default_max_rounds_is_three(self):
        cfg = PRCoTConfig()
        assert cfg.max_repair_rounds == 3

    def test_stops_early_when_max_repairs_reached(self, tmp_telemetry):
        """When max_repair_rounds repairs are generated, loop stops."""
        cfg = PRCoTConfig(enabled=True, max_repair_rounds=2)
        pr = PRCoT(cfg, telemetry_dir=tmp_telemetry)
        mock_llm = MockLLMAlternating()
        result = pr.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        # With max_repair_rounds=2 and 4 perspectives, should stop after
        # generating 2 successful repairs
        assert len(result.repairs) <= 2
        # But might have more perspectives analyzed if repairs == code
        assert len(result.perspectives) <= 4


# ---------------------------------------------------------------------------
# Test: AC-3C-1 — Analyzes from 4 perspectives
# ---------------------------------------------------------------------------

class TestAC3C1Perspectives:
    """AC-3C-1: PR-CoT analyzes from 4 distinct perspectives."""

    def test_four_perspectives_available(self):
        assert len(PERSPECTIVES) == 4

    def test_all_perspectives_used_when_enough_rounds(self, pr_enabled):
        """With default max_repair_rounds=3, at least 3 perspectives used."""
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        used = {p.perspective for p in result.perspectives}
        assert len(used) >= 3

    def test_perspectives_are_distinct(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        perspectives = [p.perspective for p in result.perspectives]
        assert len(perspectives) == len(set(perspectives)), "Duplicate perspectives found"


# ---------------------------------------------------------------------------
# Test: AC-3C-2 — Generates repair candidates
# ---------------------------------------------------------------------------

class TestAC3C2RepairCandidates:
    """AC-3C-2: PR-CoT generates repair candidates from perspective analyses."""

    def test_repairs_are_code(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        for r in result.repairs:
            assert isinstance(r, str)
            assert "def " in r or "return" in r or len(r) > 0

    def test_repairs_differ_from_original(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        for r in result.repairs:
            assert r != "def f(): pass"


# ---------------------------------------------------------------------------
# Test: AC-3C-3 — Runs before refinement loop (structural)
# ---------------------------------------------------------------------------

class TestAC3C3BeforeRefinement:
    """AC-3C-3: Structural — PR-CoT is lightweight and fast."""

    def test_repair_is_fast(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        # With mock LLM, should complete in well under 5 seconds
        assert result.total_time_ms < 5000

    def test_result_has_time_tracking(self, pr_enabled):
        mock_llm = MockLLMAlternating()
        result = pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        assert result.total_time_ms > 0
        for p in result.perspectives:
            assert p.time_ms > 0


# ---------------------------------------------------------------------------
# Test: Telemetry
# ---------------------------------------------------------------------------

class TestTelemetry:

    def test_event_written_to_jsonl(self, pr_enabled, tmp_telemetry):
        mock_llm = MockLLMAlternating()
        pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm, task_id="LCB_001",
        )
        events_file = tmp_telemetry / "pr_cot_events.jsonl"
        assert events_file.exists()
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 1
        data = json.loads(lines[0])
        assert data["task_id"] == "LCB_001"
        assert data["num_perspectives"] > 0
        assert data["num_repairs"] >= 0
        assert "perspectives_used" in data
        assert "total_tokens" in data
        assert "timestamp" in data

    def test_multiple_events_appended(self, pr_enabled, tmp_telemetry):
        mock_llm = MockLLMAlternating()
        for i in range(3):
            pr_enabled.repair(
                problem="test", code="def f(): pass", error="err",
                llm_call=mock_llm, task_id=f"t{i}",
            )
        events_file = tmp_telemetry / "pr_cot_events.jsonl"
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 3

    def test_no_telemetry_without_task_id(self, pr_enabled, tmp_telemetry):
        mock_llm = MockLLMAlternating()
        pr_enabled.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm,
        )
        events_file = tmp_telemetry / "pr_cot_events.jsonl"
        assert not events_file.exists()

    def test_no_crash_without_telemetry_dir(self):
        cfg = PRCoTConfig(enabled=True)
        pr = PRCoT(cfg, telemetry_dir=None)
        mock_llm = MockLLMAlternating()
        result = pr.repair(
            problem="test", code="def f(): pass", error="err",
            llm_call=mock_llm, task_id="t1",
        )
        assert isinstance(result, PRCoTResult)


# ---------------------------------------------------------------------------
# Test: Data structures
# ---------------------------------------------------------------------------

class TestDataStructures:

    def test_perspective_result_to_dict(self):
        p = PerspectiveResult(
            perspective="logical_consistency",
            analysis="Found an off-by-one error",
            repair_code="def f(): return 1",
            tokens_used=80,
            time_ms=50.0,
        )
        d = p.to_dict()
        assert d["perspective"] == "logical_consistency"
        assert d["analysis_length"] == len("Found an off-by-one error")
        assert d["has_repair"] is True
        assert d["tokens_used"] == 80
        assert d["time_ms"] == 50.0

    def test_perspective_result_no_repair(self):
        p = PerspectiveResult(perspective="biases", analysis="analysis", repair_code="")
        d = p.to_dict()
        assert d["has_repair"] is False

    def test_prcot_result_to_dict(self):
        r = PRCoTResult(
            task_id="t1",
            perspectives=[
                PerspectiveResult(perspective="biases", analysis="a", tokens_used=10),
            ],
            repairs=["def solve(): pass"],
            total_tokens=100,
            total_time_ms=200.0,
        )
        d = r.to_dict()
        assert d["task_id"] == "t1"
        assert d["num_perspectives"] == 1
        assert d["num_repairs"] == 1
        assert d["total_tokens"] == 100
        assert len(d["perspective_details"]) == 1

    def test_event_to_dict(self):
        e = PRCoTEvent(
            task_id="t1", num_perspectives=4, num_repairs=3,
            perspectives_used=["logical_consistency", "biases"],
            total_tokens=200, total_time_ms=500.0,
        )
        d = e.to_dict()
        assert d["task_id"] == "t1"
        assert d["num_perspectives"] == 4
        assert d["num_repairs"] == 3
        assert "timestamp" in d

    def test_config_defaults(self):
        cfg = PRCoTConfig()
        assert cfg.enabled is False
        assert cfg.max_repair_rounds == 3
        assert cfg.analysis_temperature == 0.3
        assert cfg.repair_temperature == 0.4
        assert cfg.analysis_max_tokens == 2048
        assert cfg.repair_max_tokens == 4096


def test_repair_prompt_preserves_the_declared_interface_contract():
    prompt = PRCoT(PRCoTConfig(enabled=True))._build_repair_prompt(
        "Implement the requested API", "def old(): pass", "wrong interface")
    assert "exact signature" in prompt
    assert "Preserve existing public interfaces" in prompt
    assert "parameter order and kinds (including / and * markers)" in prompt
    assert "laziness, stopping conditions, ordering" in prompt
    assert "never import that file or requested artifact from itself" in prompt
    assert "mentally compile" in prompt
    assert "complete, syntactically valid fixed Python source file" in prompt
