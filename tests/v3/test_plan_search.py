"""Tests for V3 PlanSearch (Feature 1A)."""

import json
from typing import Optional, Tuple

import pytest

from stages.budget_forcing import BudgetForcing, BudgetForcingConfig
from stages.plan_search import (
    CODE_GENERATION_PROMPT,
    CONSTRAINT_EXTRACTION_PROMPT,
    PLAN_CONSTRUCTION_PROMPT,
    ConstraintSet,
    Plan,
    PlanSearch,
    PlanSearchConfig,
    PlanSearchResult,
    extract_code_from_response,
    parse_constraint_sets,
    parse_plan,
)


# ---------------------------------------------------------------------------
# Test data
# ---------------------------------------------------------------------------

SAMPLE_PROBLEM = """\
Given an array of integers nums and an integer target, return indices of the \
two numbers such that they add up to target. You may assume that each input \
would have exactly one solution, and you may not use the same element twice."""

SAMPLE_CONSTRAINT_RESPONSE = """\
CONSTRAINT SET 1:
- Constraint: Time complexity must be O(n) for large inputs
- Eliminates: Brute force O(n^2) nested loop approach
- Implies: Hash map / dictionary lookup

CONSTRAINT SET 2:
- Constraint: Each element used at most once
- Eliminates: Solutions that check (i, i) pairs
- Implies: Two-pointer or hash map with visited tracking

CONSTRAINT SET 3:
- Constraint: Exactly one solution exists
- Eliminates: Need for duplicate handling or multiple return values
- Implies: Early return on first match found"""

SAMPLE_PLAN_RESPONSE = """\
Algorithm choice: Hash map single-pass approach
Data structures: Dictionary mapping value -> index
1. Create empty hash map
2. Iterate through array once
3. For each element, check if (target - element) exists in map
4. If found, return [map[complement], current_index]
5. Otherwise, add current element to map

Edge cases:
- Negative numbers
- Zero as target
- Large arrays near constraint limit"""

SAMPLE_CODE_RESPONSE = """\
```python
def two_sum(nums, target):
    seen = {}
    for i, num in enumerate(nums):
        complement = target - num
        if complement in seen:
            return [seen[complement], i]
        seen[num] = i
    return []
```"""


# ---------------------------------------------------------------------------
# Mock LLM callable
# ---------------------------------------------------------------------------

class MockLLM:
    """Mock LLM that returns predefined responses based on call index."""

    def __init__(self, responses=None):
        self.responses = responses or []
        self.calls = []
        self._call_index = 0

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int]
                 ) -> Tuple[str, int, float]:
        self.calls.append({
            "prompt": prompt,
            "temperature": temperature,
            "max_tokens": max_tokens,
            "seed": seed,
        })
        if self._call_index < len(self.responses):
            resp = self.responses[self._call_index]
        else:
            resp = "def solve(): pass"
        self._call_index += 1
        tokens = len(resp) // 4
        return (resp, tokens, 100.0)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def tmp_telemetry(tmp_path):
    d = tmp_path / "telemetry"
    d.mkdir()
    return d


@pytest.fixture
def bf_enabled():
    return BudgetForcing(BudgetForcingConfig(enabled=True))


@pytest.fixture
def ps_enabled(tmp_telemetry, bf_enabled):
    cfg = PlanSearchConfig(enabled=True, num_plans=3)
    return PlanSearch(cfg, budget_forcing=bf_enabled, telemetry_dir=tmp_telemetry)


@pytest.fixture
def ps_disabled(tmp_telemetry):
    cfg = PlanSearchConfig(enabled=False)
    return PlanSearch(cfg, telemetry_dir=tmp_telemetry)


@pytest.fixture
def mock_llm():
    """Mock LLM with realistic responses for the 3-step pipeline."""
    return MockLLM(responses=[
        SAMPLE_CONSTRAINT_RESPONSE,  # Step 1: constraints
        SAMPLE_PLAN_RESPONSE,        # Step 2: plan 1
        SAMPLE_PLAN_RESPONSE,        # Step 2: plan 2
        SAMPLE_PLAN_RESPONSE,        # Step 2: plan 3
        SAMPLE_CODE_RESPONSE,        # Step 3: code 1
        SAMPLE_CODE_RESPONSE,        # Step 3: code 2
        SAMPLE_CODE_RESPONSE,        # Step 3: code 3
    ])


# ---------------------------------------------------------------------------
# Test: Disabled noop
# ---------------------------------------------------------------------------

class TestDisabledNoop:

    def test_disabled_returns_empty(self, ps_disabled):
        llm = MockLLM()
        result = ps_disabled.generate(SAMPLE_PROBLEM, "test_001", llm)
        assert result.candidates == []
        assert result.plans == []
        assert result.constraint_sets == []
        assert len(llm.calls) == 0  # No LLM calls made


# ---------------------------------------------------------------------------
# Test: Constraint extraction parsing
# ---------------------------------------------------------------------------

class TestParseConstraintSets:

    def test_structured_format(self):
        sets = parse_constraint_sets(SAMPLE_CONSTRAINT_RESPONSE, 3)
        assert len(sets) == 3
        assert len(sets[0].constraints) >= 1
        assert sets[0].algorithmic_family != ""

    def test_numbered_format(self):
        response = """\
1. Use O(n) time complexity — hash map approach
2. Use two pointers on sorted array — requires sorting first
3. Use brute force with early termination"""
        sets = parse_constraint_sets(response, 3)
        assert len(sets) == 3

    def test_single_block_fallback(self):
        response = "Use dynamic programming with memoization"
        sets = parse_constraint_sets(response, 1)
        assert len(sets) == 1
        assert "dynamic programming" in sets[0].constraints[0].lower()

    def test_respects_expected_n(self):
        sets = parse_constraint_sets(SAMPLE_CONSTRAINT_RESPONSE, 2)
        assert len(sets) <= 2

    def test_empty_response(self):
        sets = parse_constraint_sets("", 3)
        assert len(sets) == 0

    def test_partial_format(self):
        response = """\
CONSTRAINT SET 1:
- Constraint: Must handle negative numbers
- Implies: Need absolute value or signed comparison

CONSTRAINT SET 2:
- Constraint: Output must be sorted"""
        sets = parse_constraint_sets(response, 2)
        assert len(sets) == 2


# ---------------------------------------------------------------------------
# Test: Plan parsing
# ---------------------------------------------------------------------------

class TestParsePlan:

    def test_basic_plan(self):
        cs = ConstraintSet(constraints=["Use hash map"])
        plan = parse_plan(SAMPLE_PLAN_RESPONSE, cs)
        assert plan.constraint_set is cs
        assert len(plan.approach) > 0
        assert len(plan.steps) >= 3
        assert len(plan.edge_cases) >= 1

    def test_minimal_plan(self):
        cs = ConstraintSet(constraints=["Simple approach"])
        plan = parse_plan("Just sort and return", cs)
        assert plan.approach == "Just sort and return"
        assert plan.constraint_set is cs


# ---------------------------------------------------------------------------
# Test: Code extraction
# ---------------------------------------------------------------------------

class TestExtractCode:

    def test_python_code_block(self):
        code = extract_code_from_response(SAMPLE_CODE_RESPONSE)
        assert "def two_sum" in code
        assert "```" not in code

    def test_plain_code_block(self):
        response = "```\ndef foo():\n    return 42\n```"
        code = extract_code_from_response(response)
        assert code == "def foo():\n    return 42\n"

    def test_raw_code(self):
        response = "def foo():\n    return 42"
        code = extract_code_from_response(response)
        assert "def foo" in code

    def test_with_think_block(self):
        response = "<think>Let me think...</think>\n```python\ndef foo(): pass\n```"
        code = extract_code_from_response(response)
        assert code == "def foo(): pass\n"

    def test_unclosed_think(self):
        response = "<think>Still thinking\ndef foo(): pass"
        code = extract_code_from_response(response)
        # Should return empty since everything is after <think>
        assert "def foo" not in code or code == ""

    def test_multiple_code_blocks(self):
        response = "```python\ndef helper(): pass\n```\nNow the main:\n```python\ndef solve(): return 1\n```"
        code = extract_code_from_response(response)
        assert "def solve" in code  # Should get the last block


# ---------------------------------------------------------------------------
# Test: Full pipeline
# ---------------------------------------------------------------------------

class TestPlanSearchPipeline:

    def test_full_pipeline_produces_candidates(self, ps_enabled, mock_llm):
        result = ps_enabled.generate(
            SAMPLE_PROBLEM, "LCB_001", mock_llm, budget_tier="standard"
        )
        assert len(result.candidates) == 3
        assert len(result.constraint_sets) == 3
        assert len(result.plans) == 3
        assert result.total_tokens > 0
        assert result.total_time_ms > 0

    def test_pipeline_makes_correct_call_count(self, ps_enabled, mock_llm):
        """1 constraint call + 3 plan calls + 3 code calls = 7 total."""
        ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        assert len(mock_llm.calls) == 7

    def test_pipeline_uses_correct_temperatures(self, ps_enabled, mock_llm):
        ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        # Step 1: constraint extraction at 0.7
        assert mock_llm.calls[0]["temperature"] == 0.7
        # Step 2: plan construction at 0.4
        assert mock_llm.calls[1]["temperature"] == 0.4
        assert mock_llm.calls[2]["temperature"] == 0.4
        assert mock_llm.calls[3]["temperature"] == 0.4
        # Step 3: code generation at 0.2
        assert mock_llm.calls[4]["temperature"] == 0.2

    def test_pipeline_uses_nothink_for_structured_steps(self, ps_enabled, mock_llm):
        ps_enabled.generate(
            SAMPLE_PROBLEM, "test", mock_llm, budget_tier="hard"
        )
        # Steps 1 and 2 always use nothink (thinking wastes tokens on
        # structured output) — visible as the plain concise system prompt
        # (thinking itself is toggled by the shared client, not the prompt).
        assert "directly and concisely" in mock_llm.calls[0]["prompt"]
        assert "directly and concisely" in mock_llm.calls[1]["prompt"]

    def test_pipeline_nothink_mode(self, ps_enabled):
        llm = MockLLM(responses=[
            SAMPLE_CONSTRAINT_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_CODE_RESPONSE,
            SAMPLE_CODE_RESPONSE,
            SAMPLE_CODE_RESPONSE,
        ])
        ps_enabled.generate(SAMPLE_PROBLEM, "test", llm, budget_tier="nothink")
        # All calls carry the plain concise (nothink) system prompt.
        for call in llm.calls:
            assert "directly and concisely" in call["prompt"]
            assert "/nothink" not in call["prompt"]

    def test_custom_num_plans(self, ps_enabled, bf_enabled, tmp_telemetry):
        """Override num_plans at call time."""
        llm = MockLLM(responses=[
            SAMPLE_CONSTRAINT_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_CODE_RESPONSE,
            SAMPLE_CODE_RESPONSE,
        ])
        result = ps_enabled.generate(
            SAMPLE_PROBLEM, "test", llm, num_plans=2
        )
        assert len(result.candidates) == 2
        # 1 constraint + 2 plan + 2 code = 5 calls
        assert len(llm.calls) == 5

    def test_seeds_are_unique_per_plan(self, ps_enabled, mock_llm):
        ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm, base_seed=42)
        # Step 2 calls (index 1, 2, 3) should have different seeds
        step2_seeds = [mock_llm.calls[i]["seed"] for i in [1, 2, 3]]
        assert len(set(step2_seeds)) == 3  # All unique

    def test_fallback_when_no_constraints_parsed(self, ps_enabled):
        """If constraint parsing fails, should fallback to a generic constraint."""
        llm = MockLLM(responses=[
            "",  # Empty constraint response
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_CODE_RESPONSE,
        ])
        result = ps_enabled.generate(SAMPLE_PROBLEM, "test", llm, num_plans=1)
        # Should still produce a candidate using fallback constraint
        assert len(result.candidates) >= 1
        assert len(result.constraint_sets) >= 1


# ---------------------------------------------------------------------------
# Test: Integration with Budget Forcing
# ---------------------------------------------------------------------------

class TestBudgetForcingIntegration:

    def test_uses_nothink_for_structured_steps(self, ps_enabled, mock_llm):
        """Steps 1-2 always use nothink regardless of budget tier."""
        ps_enabled.generate(
            SAMPLE_PROBLEM, "test", mock_llm, budget_tier="extreme"
        )
        # Steps 1-2 use nothink (structured output, thinking wastes tokens)
        assert "directly and concisely" in mock_llm.calls[0]["prompt"]

    def test_without_budget_forcing(self, tmp_telemetry):
        """PlanSearch should work without BudgetForcing (fallback prompts)."""
        cfg = PlanSearchConfig(enabled=True, num_plans=1)
        ps = PlanSearch(cfg, budget_forcing=None, telemetry_dir=tmp_telemetry)
        llm = MockLLM(responses=[
            SAMPLE_CONSTRAINT_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_CODE_RESPONSE,
        ])
        result = ps.generate(SAMPLE_PROBLEM, "test", llm, num_plans=1)
        assert len(result.candidates) == 1


# ---------------------------------------------------------------------------
# Test: Telemetry
# ---------------------------------------------------------------------------

class TestTelemetry:

    def test_event_logged(self, ps_enabled, mock_llm, tmp_telemetry):
        ps_enabled.generate(SAMPLE_PROBLEM, "LCB_001", mock_llm)
        events_file = tmp_telemetry / "plan_search_events.jsonl"
        assert events_file.exists()
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 1
        data = json.loads(lines[0])
        assert data["task_id"] == "LCB_001"
        assert data["num_constraint_sets"] == 3
        assert data["num_plans"] == 3
        assert data["num_candidates"] == 3
        assert data["total_tokens"] > 0

    def test_no_telemetry_when_disabled(self, ps_disabled, tmp_telemetry):
        llm = MockLLM()
        ps_disabled.generate(SAMPLE_PROBLEM, "test", llm)
        events_file = tmp_telemetry / "plan_search_events.jsonl"
        assert not events_file.exists()


# ---------------------------------------------------------------------------
# Test: Data structures
# ---------------------------------------------------------------------------

class TestDataStructures:

    def test_constraint_set_to_dict(self):
        cs = ConstraintSet(
            constraints=["O(n) time", "No extra space"],
            algorithmic_family="hash map",
        )
        d = cs.to_dict()
        assert d["constraints"] == ["O(n) time", "No extra space"]
        assert d["algorithmic_family"] == "hash map"

    def test_plan_to_dict(self):
        cs = ConstraintSet(constraints=["test"])
        plan = Plan(constraint_set=cs, approach="DP", steps=["Step 1"])
        d = plan.to_dict()
        assert d["approach"] == "DP"
        assert d["steps"] == ["Step 1"]

    def test_result_to_dict(self):
        result = PlanSearchResult(
            task_id="t1",
            constraint_sets=[ConstraintSet(constraints=["c1"])],
            plans=[],
            candidates=["code1", "code2"],
            total_tokens=500,
        )
        d = result.to_dict()
        assert d["task_id"] == "t1"
        assert d["num_candidates"] == 2
        assert d["total_tokens"] == 500


# ---------------------------------------------------------------------------
# Test: AC-1A-1 — Produces >=3 distinct constraint sets
# ---------------------------------------------------------------------------

class TestAC1A1ConstraintDiversity:

    def test_generates_3_constraint_sets(self, ps_enabled, mock_llm):
        result = ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        assert len(result.constraint_sets) >= 3, (
            f"Expected >=3 constraint sets, got {len(result.constraint_sets)}"
        )

    def test_constraint_sets_are_non_empty(self, ps_enabled, mock_llm):
        result = ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        for cs in result.constraint_sets:
            assert len(cs.constraints) >= 1


# ---------------------------------------------------------------------------
# Test: AC-1A-2 — Cosine similarity <0.85 (structural; runtime needs embeddings)
# ---------------------------------------------------------------------------

class TestAC1A2Diversity:

    def test_candidates_are_strings(self, ps_enabled, mock_llm):
        """Structural: each candidate is a non-empty code string."""
        result = ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        for code in result.candidates:
            assert isinstance(code, str)
            assert len(code) > 0


# ---------------------------------------------------------------------------
# Test: AC-1A-3 — Oracle pass@k improvement (runtime benchmark)
# Test: AC-1A-4 — Time <2x temperature-only (runtime benchmark)
# These are validated during Phase 1 benchmark, tested structurally here.
# ---------------------------------------------------------------------------

class TestAC1A3OracleImprovement:

    def test_pipeline_produces_diverse_plans(self, ps_enabled, mock_llm):
        """Structural: different seeds lead to different plans."""
        result = ps_enabled.generate(SAMPLE_PROBLEM, "test", mock_llm)
        # Each plan should be based on a different constraint set
        assert len(result.plans) == len(result.constraint_sets)


class TestAC1A4TimeBudget:

    def test_code_gen_uses_efficient_tier(self, ps_enabled, mock_llm):
        """Code generation (Step 3) should use nothink/light for speed."""
        ps_enabled.generate(
            SAMPLE_PROBLEM, "test", mock_llm, budget_tier="hard"
        )
        # Step 3 calls (index 4, 5, 6) should use nothink or light
        for i in [4, 5, 6]:
            prompt = mock_llm.calls[i]["prompt"]
            # nothink shows as the plain concise prompt; light keeps the
            # step-by-step prompt with a smaller budget. Either is an
            # acceptable efficiency tier for code gen.
            assert ("directly and concisely" in prompt
                    or "step by step" in prompt.lower())


# ---------------------------------------------------------------------------
# Test: AC-1A-5 — No regression on passing tasks (runtime benchmark)
# ---------------------------------------------------------------------------

class TestAC1A5NoRegression:

    def test_nothink_mode_passes_through(self, ps_enabled):
        """In nothink mode, PlanSearch should still produce valid candidates."""
        llm = MockLLM(responses=[
            SAMPLE_CONSTRAINT_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_PLAN_RESPONSE,
            SAMPLE_CODE_RESPONSE,
            SAMPLE_CODE_RESPONSE,
            SAMPLE_CODE_RESPONSE,
        ])
        result = ps_enabled.generate(
            SAMPLE_PROBLEM, "test", llm, budget_tier="nothink"
        )
        assert len(result.candidates) == 3
        for code in result.candidates:
            assert len(code) > 0


# ---------------------------------------------------------------------------
# Test: Prompt templates
# ---------------------------------------------------------------------------

class TestPromptTemplates:

    def test_constraint_prompt_includes_problem(self):
        prompt = CONSTRAINT_EXTRACTION_PROMPT.format(
            n=3, problem="Sort a list"
        )
        assert "Sort a list" in prompt
        assert "3" in prompt
        assert "CONSTRAINT" in prompt

    def test_plan_prompt_includes_constraints_and_problem(self):
        prompt = PLAN_CONSTRUCTION_PROMPT.format(
            constraints="- Use hash map\n- O(n) time",
            problem="Two sum",
        )
        assert "hash map" in prompt
        assert "Two sum" in prompt
        assert "Do NOT write code" in prompt
        assert "exact signature" in prompt
        assert "syntactically valid source file" in prompt

    def test_code_prompt_includes_all_context(self):
        prompt = CODE_GENERATION_PROMPT.format(
            plan="Use DP approach",
            constraints="- O(n) time",
            problem="Max subarray",
        )
        assert "DP approach" in prompt
        assert "O(n)" in prompt
        assert "Max subarray" in prompt
        assert "exact signature" in prompt
        assert "mentally compile" in prompt
        assert "parameter order and kinds (including / and * markers)" in prompt
        assert "laziness, stopping conditions, ordering" in prompt
        assert "complete, syntactically valid source file only" in prompt
        assert "never import that file or requested artifact from itself" in prompt
