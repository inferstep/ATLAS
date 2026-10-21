"""Tests for V3 PR-CoT (Feature 3C) — Multi-Perspective Repair."""

import json

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


class FakeLLM:
    """Answers the analysis prompt and the repair prompt of each perspective in turn,
    and records every prompt it is given."""

    def __init__(self, analysis="The code has an off-by-one error in the loop.",
                 repair="```python\ndef solve(n):\n    return n + 1\n```"):
        self.analysis = analysis
        self.repair = repair
        self.prompts = []

    def __call__(self, prompt, temperature, max_tokens, seed):
        self.prompts.append(prompt)
        is_analysis_call = len(self.prompts) % 2 == 1
        return (self.analysis if is_analysis_call else self.repair), 40, 10.0


def test_a_disabled_repair_returns_an_empty_result_and_does_nothing(tmp_path):
    llm = FakeLLM()
    pr = PRCoT(PRCoTConfig(enabled=False), telemetry_dir=tmp_path)
    result = pr.repair(problem="test", code="def f(): pass", error="error",
                       llm_call=llm, task_id="my_task")
    # An empty result: no perspectives, no repairs, no tokens, only the task id.
    assert result == PRCoTResult(task_id="my_task")
    assert llm.prompts == []
    assert not (tmp_path / "pr_cot_events.jsonl").exists()


def test_a_repair_without_an_llm_returns_an_empty_result():
    result = PRCoT(PRCoTConfig(enabled=True)).repair(
        problem="test", code="x", error="e", llm_call=None)
    assert result == PRCoTResult()


def test_there_are_four_named_perspectives_with_real_descriptions():
    assert set(PERSPECTIVES) == {
        "logical_consistency", "information_completeness",
        "biases", "alternative_solutions",
    }
    for name, description in PERSPECTIVES.items():
        assert len(description) > 20, f"Perspective '{name}' has too short a description"
    pr = PRCoT(PRCoTConfig(enabled=True))
    assert pr.perspectives == PERSPECTIVES
    assert pr.num_perspectives == 4


@pytest.mark.parametrize("response, expected", [
    pytest.param("Here is the fix:\n```python\ndef solve(n):\n    return n + 1\n```",
                 "def solve(n):\n    return n + 1\n", id="python-fence"),
    pytest.param("Fixed:\n```\ndef solve(n):\n    return n + 1\n```",
                 "def solve(n):\n    return n + 1\n", id="plain-fence"),
    pytest.param("def solve(n):\n    return n + 1",
                 "def solve(n):\n    return n + 1", id="raw-code-without-fence"),
    pytest.param("```python\ndef helper(): pass\n```\nThe full solution:\n"
                 "```python\ndef solve(n):\n    return n + 1\n```",
                 "def solve(n):\n    return n + 1\n", id="last-of-several-fences"),
    pytest.param("<think>Let me fix this.</think>\n```python\ndef solve(): pass\n```",
                 "def solve(): pass\n", id="think-block-dropped"),
    pytest.param("", "", id="empty-response"),
    pytest.param("The solution needs to handle edge cases properly.",
                 "The solution needs to handle edge cases properly.", id="prose-without-code"),
])
def test_extract_code_from_repair(response, expected):
    assert extract_code_from_repair(response) == expected


def test_repair_analyses_each_perspective_then_repairs_from_that_analysis():
    llm = FakeLLM()
    result = PRCoT(PRCoTConfig(enabled=True)).repair(
        problem="Find maximum subarray sum", code="def f(): pass", error="WA",
        llm_call=llm)
    assert len(llm.prompts) == 2 * len(result.perspectives)
    assert "Find maximum subarray sum" in llm.prompts[0]  # the analysis prompt
    assert "off-by-one" in llm.prompts[1]  # the repair prompt carries the analysis


def test_repair_returns_distinct_perspectives_with_their_repairs_tokens_and_times():
    # No telemetry dir: a task id must then be skipped for telemetry, not crash.
    pr = PRCoT(PRCoTConfig(enabled=True))
    result = pr.repair(problem="test", code="def f(): pass", error="err",
                       llm_call=FakeLLM(), task_id="t1")
    assert result.task_id == "t1"
    names = [p.perspective for p in result.perspectives]
    assert len(names) > 0
    assert len(names) == len(set(names)), "Duplicate perspectives found"
    assert set(names) <= set(PERSPECTIVES)
    assert all(p.analysis for p in result.perspectives)
    assert result.repairs
    assert all(isinstance(r, str) and r for r in result.repairs)
    assert result.total_tokens > 0
    assert 0 < result.total_time_ms < 5000
    assert all(p.time_ms > 0 for p in result.perspectives)


@pytest.mark.parametrize("config, max_rounds", [
    pytest.param(PRCoTConfig(enabled=True, max_repair_rounds=1), 1, id="one-round"),
    pytest.param(PRCoTConfig(enabled=True, max_repair_rounds=2), 2, id="two-rounds"),
    pytest.param(PRCoTConfig(enabled=True), 3, id="default-three-rounds"),
])
def test_repair_stops_after_max_repair_rounds_repairs(config, max_rounds):
    # Every repair of the fake LLM differs from the original, so each
    # perspective adds one repair until the limit is reached.
    result = PRCoT(config).repair(problem="test", code="def f(): pass", error="err",
                                  llm_call=FakeLLM())
    assert len(result.repairs) == max_rounds
    # The loop stops: no perspective is analysed after the limit.
    assert len(result.perspectives) == max_rounds


def test_a_repair_identical_to_the_original_is_not_kept():
    result = PRCoT(PRCoTConfig(enabled=True)).repair(
        problem="test", code="def f(): pass", error="err",
        llm_call=FakeLLM(repair="def f(): pass"))
    assert result.repairs == []


def test_each_repair_with_a_task_id_appends_one_telemetry_event(tmp_path):
    pr = PRCoT(PRCoTConfig(enabled=True), telemetry_dir=tmp_path)
    for task_id in ("LCB_001", "LCB_002"):
        pr.repair(problem="test", code="def f(): pass", error="err",
                  llm_call=FakeLLM(), task_id=task_id)
    lines = (tmp_path / "pr_cot_events.jsonl").read_text().splitlines()
    assert len(lines) == 2
    event = json.loads(lines[0])
    assert event["task_id"] == "LCB_001"
    assert event["num_perspectives"] > 0
    assert event["num_repairs"] >= 0
    assert "perspectives_used" in event
    assert "total_tokens" in event
    assert "timestamp" in event


def test_a_repair_without_a_task_id_writes_no_telemetry(tmp_path):
    PRCoT(PRCoTConfig(enabled=True), telemetry_dir=tmp_path).repair(
        problem="test", code="def f(): pass", error="err", llm_call=FakeLLM())
    assert not (tmp_path / "pr_cot_events.jsonl").exists()


def test_perspective_result_to_dict():
    with_repair = PerspectiveResult(
        perspective="logical_consistency", analysis="Found an off-by-one error",
        repair_code="def f(): return 1", tokens_used=80, time_ms=50.0)
    assert with_repair.to_dict() == {
        "perspective": "logical_consistency",
        "analysis_length": len("Found an off-by-one error"),
        "has_repair": True,
        "tokens_used": 80,
        "time_ms": 50.0,
    }
    without_repair = PerspectiveResult(perspective="biases", analysis="analysis", repair_code="")
    assert without_repair.to_dict()["has_repair"] is False


def test_prcot_result_to_dict():
    d = PRCoTResult(
        task_id="t1",
        perspectives=[PerspectiveResult(perspective="biases", analysis="a", tokens_used=10)],
        repairs=["def solve(): pass"],
        total_tokens=100,
        total_time_ms=200.0,
    ).to_dict()
    assert d["task_id"] == "t1"
    assert d["num_perspectives"] == 1
    assert d["num_repairs"] == 1
    assert d["total_tokens"] == 100
    assert len(d["perspective_details"]) == 1


def test_event_to_dict():
    d = PRCoTEvent(
        task_id="t1", num_perspectives=4, num_repairs=3,
        perspectives_used=["logical_consistency", "biases"],
        total_tokens=200, total_time_ms=500.0,
    ).to_dict()
    assert d["task_id"] == "t1"
    assert d["num_perspectives"] == 4
    assert d["num_repairs"] == 3
    assert "timestamp" in d


def test_config_defaults():
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
