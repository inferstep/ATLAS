"""The plan JSON is read from the model's raw text: the first {...} object,
found by matching braces outside strings."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

import planning


@pytest.mark.parametrize("raw, plan", [
    pytest.param('{"steps": []}', {"steps": []}, id="plain-json"),
    pytest.param('Here is the plan: {"steps": []} Good luck.', {"steps": []}, id="prose-around-it"),
    pytest.param('A plan for the 12" screen: {"steps": []}', {"steps": []}, id="a-quote-in-the-prose-before-it"),
    pytest.param('{"why": "use {x}", "steps": []}', {"why": "use {x}", "steps": []}, id="braces-in-a-string"),
    pytest.param('{"why": "say \\"{\\" here", "steps": []}', {"why": 'say "{" here', "steps": []},
                 id="an-escaped-quote-in-a-string"),
    pytest.param('{"a": {"b": 1}} and a stray }', {"a": {"b": 1}}, id="nested-then-a-stray-brace"),
    pytest.param('Plan {v2}:\n```json\n{"steps": []}\n```', {"steps": []}, id="a-fenced-plan-after-braces-in-prose"),
    pytest.param('{"steps": [', None, id="never-closed"),
    pytest.param('{steps: []}', None, id="balanced-braces-but-not-json"),
])
def test_the_plan_is_the_first_whole_object(raw, plan):
    assert planning._parse_plan_json(raw) == plan
