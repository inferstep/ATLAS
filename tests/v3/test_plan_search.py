"""Tests for V3 PlanSearch: constraint sets from the problem, then one plan and one
code candidate per constraint set."""

import json

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

TWO_SUM_CODE = """\
def two_sum(nums, target):
    seen = {}
    for i, num in enumerate(nums):
        complement = target - num
        if complement in seen:
            return [seen[complement], i]
        seen[num] = i
    return []
"""

SAMPLE_CODE_RESPONSE = "```python\n" + TWO_SUM_CODE + "```"

# The model's answers in a run with 3 plans: 1 constraint call, 3 plan calls, 3 code calls.
THREE_PLAN_RESPONSES = (
    [SAMPLE_CONSTRAINT_RESPONSE] + [SAMPLE_PLAN_RESPONSE] * 3 + [SAMPLE_CODE_RESPONSE] * 3
)


class MockLLM:
    """Answers the k-th call with responses[k] (tokens: a quarter of its length; time:
    100 ms) and records the prompt, temperature and seed of every call.

    The plan calls run in parallel, and so do the code calls, so their order within a
    step is not fixed: the tests give every call of one step the same response."""

    def __init__(self, responses=()):
        self.responses = list(responses)
        self.calls = []

    def __call__(self, prompt, temperature, max_tokens, seed):
        self.calls.append({"prompt": prompt, "temperature": temperature, "seed": seed})
        response = self.responses[len(self.calls) - 1]
        return response, len(response) // 4, 100.0


@pytest.fixture
def plan_search(tmp_path):
    """Enabled, 3 plans, with budget forcing; telemetry goes to tmp_path."""
    return PlanSearch(
        PlanSearchConfig(enabled=True, num_plans=3),
        budget_forcing=BudgetForcing(BudgetForcingConfig(enabled=True)),
        telemetry_dir=tmp_path,
    )


def test_disabled_makes_no_call_returns_nothing_and_logs_nothing(tmp_path):
    plan_search = PlanSearch(PlanSearchConfig(enabled=False), telemetry_dir=tmp_path)
    llm = MockLLM()
    result = plan_search.generate(SAMPLE_PROBLEM, "test_001", llm)
    assert llm.calls == []
    assert result.constraint_sets == []
    assert result.plans == []
    assert result.candidates == []
    assert not (tmp_path / "plan_search_events.jsonl").exists()


@pytest.mark.parametrize("response, expected_n, expected_first_constraints", [
    pytest.param(SAMPLE_CONSTRAINT_RESPONSE, 3,
                 ["Time complexity must be O(n) for large inputs",
                  "Each element used at most once",
                  "Exactly one solution exists"],
                 id="constraint-set-headers"),
    pytest.param(SAMPLE_CONSTRAINT_RESPONSE, 2,
                 ["Time complexity must be O(n) for large inputs",
                  "Each element used at most once"],
                 id="cut-to-expected-n"),
    pytest.param("CONSTRAINT SET 1:\n"
                 "- Constraint: Must handle negative numbers\n"
                 "- Implies: Need absolute value or signed comparison\n"
                 "\n"
                 "CONSTRAINT SET 2:\n"
                 "- Constraint: Output must be sorted", 2,
                 ["Must handle negative numbers", "Output must be sorted"],
                 id="headers-with-lines-missing"),
    pytest.param("1. Use O(n) time complexity — hash map approach\n"
                 "2. Use two pointers on sorted array — requires sorting first\n"
                 "3. Use brute force with early termination", 3,
                 ["Use O(n) time complexity — hash map approach",
                  "Use two pointers on sorted array — requires sorting first",
                  "Use brute force with early termination"],
                 id="numbered-lines"),
    pytest.param("Use dynamic programming with memoization", 1,
                 ["Use dynamic programming with memoization"],
                 id="one-plain-block"),
    pytest.param("", 3, [], id="empty"),
])
def test_parse_constraint_sets(response, expected_n, expected_first_constraints):
    sets = parse_constraint_sets(response, expected_n)
    assert [constraint_set.constraints[0] for constraint_set in sets] == expected_first_constraints


def test_parse_constraint_sets_takes_the_family_from_the_implies_line():
    sets = parse_constraint_sets(SAMPLE_CONSTRAINT_RESPONSE, 3)
    assert [constraint_set.algorithmic_family for constraint_set in sets] == [
        "Hash map / dictionary lookup",
        "Two-pointer or hash map with visited tracking",
        "Early return on first match found",
    ]


def test_parse_plan_reads_approach_steps_and_edge_cases():
    constraint_set = ConstraintSet(constraints=["Use hash map"])
    plan = parse_plan(SAMPLE_PLAN_RESPONSE, constraint_set)
    assert plan.constraint_set is constraint_set
    assert plan.approach == "Hash map single-pass approach"
    assert plan.steps == [
        "Create empty hash map",
        "Iterate through array once",
        "For each element, check if (target - element) exists in map",
        "If found, return [map[complement], current_index]",
        "Otherwise, add current element to map",
    ]
    assert plan.edge_cases == ["Negative numbers", "Zero as target", "Large arrays near constraint limit"]


def test_parse_plan_without_sections_keeps_the_whole_text_as_the_approach():
    constraint_set = ConstraintSet(constraints=["Simple approach"])
    plan = parse_plan("Just sort and return", constraint_set)
    assert plan.constraint_set is constraint_set
    assert plan.approach == "Just sort and return"


@pytest.mark.parametrize("response, expected", [
    pytest.param(SAMPLE_CODE_RESPONSE, TWO_SUM_CODE, id="python-fence"),
    pytest.param("```\ndef foo():\n    return 42\n```", "def foo():\n    return 42\n", id="plain-fence"),
    pytest.param("def foo():\n    return 42", "def foo():\n    return 42", id="raw-code"),
    pytest.param("<think>Let me think...</think>\n```python\ndef foo(): pass\n```",
                 "def foo(): pass\n", id="closed-think-then-fence"),
    # Everything after an unclosed <think> is reasoning, so no code is left.
    pytest.param("<think>Still thinking\ndef foo(): pass", "", id="unclosed-think"),
    pytest.param("```python\ndef helper(): pass\n```\nNow the main:\n```python\ndef solve(): return 1\n```",
                 "def solve(): return 1\n", id="last-of-several-fences"),
])
def test_extract_code_from_response(response, expected):
    assert extract_code_from_response(response) == expected


@pytest.mark.parametrize("budget_tier", ["standard", "nothink", "hard", "extreme"])
def test_pipeline_makes_7_concise_calls_and_3_candidates(plan_search, budget_tier):
    llm = MockLLM(THREE_PLAN_RESPONSES)
    result = plan_search.generate(SAMPLE_PROBLEM, "LCB_001", llm, budget_tier=budget_tier)

    # 1 constraint call, then 1 plan call and 1 code call per constraint set. Whatever
    # the tier, every call carries the plain, concise system prompt: the steps write
    # structured text or code from a finished plan, and thinking only spends tokens.
    assert len(llm.calls) == 7
    for call in llm.calls:
        assert "directly and concisely" in call["prompt"]
        assert "/nothink" not in call["prompt"]
    assert len(result.constraint_sets) == 3
    assert all(constraint_set.constraints for constraint_set in result.constraint_sets)
    assert len(result.plans) == 3
    assert result.candidates == [TWO_SUM_CODE] * 3
    assert result.total_tokens > 0
    assert result.total_time_ms > 0


def test_pipeline_uses_correct_temperatures(plan_search):
    llm = MockLLM(THREE_PLAN_RESPONSES)
    plan_search.generate(SAMPLE_PROBLEM, "test", llm)
    # constraints at 0.7, plans at 0.4, code at 0.2
    assert [call["temperature"] for call in llm.calls] == [0.7, 0.4, 0.4, 0.4, 0.2, 0.2, 0.2]


def test_custom_num_plans(plan_search):
    llm = MockLLM([SAMPLE_CONSTRAINT_RESPONSE] + [SAMPLE_PLAN_RESPONSE] * 2 + [SAMPLE_CODE_RESPONSE] * 2)
    result = plan_search.generate(SAMPLE_PROBLEM, "test", llm, num_plans=2)
    assert len(llm.calls) == 5  # 1 constraint call + 2 plan calls + 2 code calls
    assert len(result.candidates) == 2


def test_seeds_are_unique_per_plan(plan_search):
    llm = MockLLM(THREE_PLAN_RESPONSES)
    plan_search.generate(SAMPLE_PROBLEM, "test", llm, base_seed=42)
    plan_call_seeds = {call["seed"] for call in llm.calls[1:4]}
    assert len(plan_call_seeds) == 3


def test_fallback_when_no_constraints_parsed(plan_search):
    llm = MockLLM(["", SAMPLE_PLAN_RESPONSE, SAMPLE_CODE_RESPONSE])
    result = plan_search.generate(SAMPLE_PROBLEM, "test", llm, num_plans=1)
    assert [constraint_set.constraints for constraint_set in result.constraint_sets] == [
        ["Solve the problem correctly"]
    ]
    assert result.candidates == [TWO_SUM_CODE]


def test_without_budget_forcing(tmp_path):
    plan_search = PlanSearch(PlanSearchConfig(enabled=True, num_plans=1),
                             budget_forcing=None, telemetry_dir=tmp_path)
    llm = MockLLM([SAMPLE_CONSTRAINT_RESPONSE, SAMPLE_PLAN_RESPONSE, SAMPLE_CODE_RESPONSE])
    result = plan_search.generate(SAMPLE_PROBLEM, "test", llm, num_plans=1)
    assert result.candidates == [TWO_SUM_CODE]


def test_event_logged(plan_search, tmp_path):
    plan_search.generate(SAMPLE_PROBLEM, "LCB_001", MockLLM(THREE_PLAN_RESPONSES))
    lines = (tmp_path / "plan_search_events.jsonl").read_text().splitlines()
    assert len(lines) == 1
    event = json.loads(lines[0])
    assert event["task_id"] == "LCB_001"
    assert event["num_constraint_sets"] == 3
    assert event["num_plans"] == 3
    assert event["num_candidates"] == 3
    assert event["total_tokens"] > 0


@pytest.mark.parametrize("record, expected", [
    pytest.param(ConstraintSet(constraints=["O(n) time", "No extra space"], algorithmic_family="hash map"),
                 {"constraints": ["O(n) time", "No extra space"], "algorithmic_family": "hash map"},
                 id="constraint-set"),
    pytest.param(Plan(constraint_set=ConstraintSet(constraints=["test"]), approach="DP", steps=["Step 1"]),
                 {"constraint_set": {"constraints": ["test"], "algorithmic_family": ""},
                  "approach": "DP", "data_structures": "", "steps": ["Step 1"], "edge_cases": []},
                 id="plan"),
    pytest.param(PlanSearchResult(task_id="t1", constraint_sets=[ConstraintSet(constraints=["c1"])],
                                  candidates=["code1", "code2"], total_tokens=500),
                 {"task_id": "t1", "constraint_sets": [{"constraints": ["c1"], "algorithmic_family": ""}],
                  "plans": [], "num_candidates": 2, "total_tokens": 500, "total_time_ms": 0.0},
                 id="result"),
])
def test_to_dict(record, expected):
    assert record.to_dict() == expected


@pytest.mark.parametrize("template, fields, expected_parts", [
    pytest.param(CONSTRAINT_EXTRACTION_PROMPT, {"n": 3, "problem": "Sort a list"},
                 ["Sort a list", "Identify 3 distinct CONSTRAINTS"],
                 id="constraint-extraction"),
    pytest.param(PLAN_CONSTRUCTION_PROMPT,
                 {"constraints": "- Use hash map\n- O(n) time", "problem": "Two sum"},
                 ["hash map", "Two sum", "Do NOT write code", "exact signature",
                  "syntactically valid source file"],
                 id="plan-construction"),
    pytest.param(CODE_GENERATION_PROMPT,
                 {"plan": "Use DP approach", "constraints": "- O(n) time", "problem": "Max subarray"},
                 ["DP approach", "O(n)", "Max subarray", "exact signature", "mentally compile",
                  "parameter order and kinds (including / and * markers)",
                  "laziness, stopping conditions, ordering",
                  "complete, syntactically valid source file only",
                  "never import that file or requested artifact from itself"],
                 id="code-generation"),
])
def test_prompt_template_carries_its_inputs_and_rules(template, fields, expected_parts):
    prompt = template.format(**fields)
    for part in expected_parts:
        assert part in prompt
