"""Tests for V3 self-test generation: the model writes test cases from the problem
statement, and the parser reads them from the many forms a model writes."""

import json

import pytest

from stages.self_test_gen import (
    GeneratedTestCase,
    SelfTestGen,
    SelfTestGenConfig,
    _strip_think_tags,
)


class MockLLM:
    """Answers every call with the same response, 100 tokens and 50 ms, and counts the calls."""

    def __init__(self, response):
        self.response = response
        self.call_count = 0

    def __call__(self, prompt, temperature, max_tokens, seed):
        self.call_count += 1
        return self.response, 100, 50.0


SAMPLE_LLM_RESPONSE = """\
TEST CASE 1:
DESCRIPTION: Simple case with small input
INPUT: 3
OUTPUT: 6

TEST CASE 2:
DESCRIPTION: Edge case with zero
INPUT: 0
OUTPUT: 0

TEST CASE 3:
DESCRIPTION: Large input boundary
INPUT: 1000000
OUTPUT: 2000000
"""


def test_config_defaults():
    config = SelfTestGenConfig()
    assert config.enabled is False
    assert config.num_test_cases == 5
    assert config.majority_threshold == 0.6


def test_generated_test_case_to_dict():
    case = GeneratedTestCase(input_str="5", expected_output="10", description="simple doubling")
    assert case.to_dict() == {
        "input_str": "5",
        "expected_output": "10",
        "description": "simple doubling",
        "provenance": "generated",
    }


# Each row: a model response, and the (input, expected output) of every case parsed from it.
@pytest.mark.parametrize("response, expected", [
    # Nothing to parse.
    pytest.param("", [], id="empty"),
    pytest.param("no test cases here", [], id="no-test-cases"),
    pytest.param("1. Just some text\n2. More text\n", [], id="numbered-lines-without-input-or-output"),
    # The generator keeps at most num_test_cases (3) cases.
    pytest.param(SAMPLE_LLM_RESPONSE + "\nTEST CASE 4:\nDESCRIPTION: Extra\nINPUT: 99\nOUTPUT: 198\n"
                 "\nTEST CASE 5:\nDESCRIPTION: Extra2\nINPUT: 1\nOUTPUT: 2\n",
                 [("3", "6"), ("0", "0"), ("1000000", "2000000")], id="five-cases-cut-to-3"),
    # <think> blocks are not part of any case.
    pytest.param("<think>Let me think about this...</think>\nTEST CASE 1:\nINPUT: 5\nOUTPUT: 10",
                 [("5", "10")], id="closed-think-before-the-case"),
    pytest.param("TEST CASE 1:\nINPUT: 5\nOUTPUT: 10\n\n"
                 "<think>Now let me think about the next one but I ran out of tok",
                 [("5", "10")], id="unclosed-think-after-the-case"),
    # Case headers.
    pytest.param("Test Case 1:\nInput: 5\nOutput: 10\n\nTest Case 2:\nInput: 0\nOutput: 0\n",
                 [("5", "10"), ("0", "0")], id="header-title-case"),
    pytest.param("Test 1:\nInput: 5\nOutput: 10\n\nTest 2:\nInput: 0\nOutput: 0\n",
                 [("5", "10"), ("0", "0")], id="header-test-n"),
    pytest.param("test case 1:\ninput: 5\noutput: 10\n\ntest case 2:\ninput: 0\noutput: 0\n",
                 [("5", "10"), ("0", "0")], id="header-lowercase"),
    pytest.param("Test Case #1:\nInput: 42\nOutput: 84\n", [("42", "84")], id="header-hash-number"),
    pytest.param("Test Case 1 -\nInput - 5\nOutput - 10\n", [("5", "10")], id="header-dash-separator"),
    # Input and output labels.
    pytest.param("TEST CASE 1:\nINPUT: 5\nEXPECTED OUTPUT: 10\n", [("5", "10")], id="label-expected-output"),
    pytest.param("TEST CASE 1:\nINPUT: 5\nExpected: 10\n", [("5", "10")], id="label-expected"),
    pytest.param("TEST CASE 1:\nSTDIN: 5\nSTDOUT: 10\n", [("5", "10")], id="label-stdin-stdout"),
    pytest.param("Test Case 1:\nInput: hello\nExpected Output: world\n",
                 [("hello", "world")], id="label-mixed-case"),
    # Markdown around a value.
    pytest.param("TEST CASE 1:\nINPUT:\n```\n5\n```\nOUTPUT:\n```\n10\n```\n",
                 [("5", "10")], id="fenced-values"),
    pytest.param("TEST CASE 1:\nINPUT:\n```python\n5\n```\nOUTPUT:\n```python\n10\n```\n",
                 [("5", "10")], id="fenced-values-with-language"),
    pytest.param("TEST CASE 1:\nINPUT: `5`\nOUTPUT: `10`\n", [("5", "10")], id="backtick-values"),
    # No TEST CASE header: numbered blocks.
    pytest.param("1. Input: 5\nOutput: 10\n\n2. Input: 0\nOutput: 0\n",
                 [("5", "10"), ("0", "0")], id="numbered-dot"),
    pytest.param("1) Input: 5\nOutput: 10\n\n2) Input: 3\nOutput: 6\n",
                 [("5", "10"), ("3", "6")], id="numbered-paren"),
    pytest.param("1: Input: 5\nOutput: 10\n\n2: Input: 3\nOutput: 6\n",
                 [("5", "10"), ("3", "6")], id="numbered-colon"),
    # Values over several lines.
    pytest.param("TEST CASE 1:\nINPUT: 3\n1 2 3\nOUTPUT: 6\n", [("3\n1 2 3", "6")], id="multiline-input"),
    pytest.param("TEST CASE 1:\nINPUT: 3\nOUTPUT: 1\n2\n3\n", [("3", "1\n2\n3")], id="multiline-output"),
])
def test_parse_test_cases(response, expected):
    generator = SelfTestGen(SelfTestGenConfig(enabled=True, num_test_cases=3))
    cases = generator.parse_test_cases(response)
    assert [(case.input_str, case.expected_output) for case in cases] == expected


@pytest.mark.parametrize("response, expected", [
    pytest.param(SAMPLE_LLM_RESPONSE, [
        ("Simple case with small input", "3", "6"),
        ("Edge case with zero", "0", "0"),
        ("Large input boundary", "1000000", "2000000"),
    ], id="description-label"),
    pytest.param("TEST CASE 1:\nExplanation: tests basic case\nINPUT: 5\nOUTPUT: 10\n",
                 [("tests basic case", "5", "10")], id="explanation-label"),
])
def test_parse_test_cases_reads_the_description(response, expected):
    generator = SelfTestGen(SelfTestGenConfig(enabled=True, num_test_cases=3))
    cases = generator.parse_test_cases(response)
    assert [(case.description, case.input_str, case.expected_output) for case in cases] == expected


def test_strip_think_tags_direct():
    assert _strip_think_tags("<think>stuff</think>hello") == "hello"
    assert _strip_think_tags("<think>stuff without close") == ""
    assert _strip_think_tags("hello<think>mid</think>world") == "helloworld"
    assert _strip_think_tags("before<think>unclosed") == "before"


def test_disabled_returns_empty():
    generator = SelfTestGen(SelfTestGenConfig(enabled=False))
    llm = MockLLM(SAMPLE_LLM_RESPONSE)
    result = generator.generate("double the number", llm_call=llm)
    assert result.test_cases == []
    assert llm.call_count == 0


def test_enabled_generates_tests():
    generator = SelfTestGen(SelfTestGenConfig(enabled=True, num_test_cases=3))
    llm = MockLLM(SAMPLE_LLM_RESPONSE)
    result = generator.generate("Given n, return 2*n", llm_call=llm, task_id="test_1")
    assert llm.call_count == 1
    assert len(result.test_cases) == 3
    assert result.generation_tokens == 100


def test_no_llm_returns_empty():
    generator = SelfTestGen(SelfTestGenConfig(enabled=True, num_test_cases=3))
    result = generator.generate("problem", llm_call=None)
    assert result.test_cases == []
    assert result.reason == "missing_callable"


def test_telemetry_logged(tmp_path):
    generator = SelfTestGen(SelfTestGenConfig(enabled=True, num_test_cases=3), telemetry_dir=tmp_path)
    generator.generate("problem", llm_call=MockLLM(SAMPLE_LLM_RESPONSE), task_id="t1")
    lines = (tmp_path / "self_test_gen_events.jsonl").read_text().splitlines()
    assert len(lines) == 1
    event = json.loads(lines[0])
    assert event["task_id"] == "t1"
    assert event["num_cases_generated"] == 3
