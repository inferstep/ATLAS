"""Tests for V3 DivSampling (Feature 1B)."""

import json

import pytest

from stages.div_sampling import (
    DEFAULT_PERTURBATIONS,
    DivSampling,
    DivSamplingConfig,
    DivSamplingEvent,
    Perturbation,
    apply_perturbation,
    get_perturbation_library,
    select_perturbation,
)


def test_disabled_div_sampling_leaves_the_prompt_alone_and_logs_nothing(tmp_path):
    ds = DivSampling(DivSamplingConfig(enabled=False), telemetry_dir=tmp_path)
    assert ds.apply("Solve this problem", candidate_index=0, task_id="t1") == "Solve this problem"
    perturbation = ds.get_perturbation(0)
    assert perturbation.label == "disabled"
    assert perturbation.text == ""
    assert not (tmp_path / "div_sampling_events.jsonl").exists()


def test_the_default_library_has_at_least_four_perturbations_in_each_category():
    # AC-1B-1 asks for at least 10: three categories of four or more give at least 12.
    counts = DivSampling(DivSamplingConfig(enabled=True)).get_category_counts()
    assert {"role", "instruction", "style"} <= set(counts)
    for category, count in counts.items():
        assert count >= 4, f"Category {category} has only {count} perturbations"


def test_every_default_perturbation_is_constructive_prose_without_code():
    # AC-1B-3: guidance that could cause a regression has no place in the library.
    destructive_patterns = [
        "don't write", "never use", "avoid all", "ignore the",
        "skip testing", "bad code", "terrible",
    ]
    for p in DEFAULT_PERTURBATIONS:
        assert len(p.text) > 10, f"Perturbation {p.label} has too-short text"
        for pattern in destructive_patterns:
            assert pattern not in p.text.lower(), f"{p.label} contains {pattern!r}"
        assert "\ndef " not in p.text and not p.text.startswith("def ")
        assert "\nimport " not in p.text and not p.text.startswith("import ")
        assert "```" not in p.text


def test_candidates_cycle_through_every_perturbation_once_per_round():
    ds = DivSampling(DivSamplingConfig(enabled=True))
    n = ds.library_size
    first_round = [ds.get_perturbation(i).label for i in range(n)]
    second_round = [ds.get_perturbation(n + i).label for i in range(n)]
    # Each candidate of a round gets a different perturbation, and every one is reached.
    assert len(set(first_round)) == n
    assert second_round == first_round


def test_apply_puts_a_different_perturbation_before_the_intact_prompt_for_each_candidate():
    # No telemetry dir: a task id must then be skipped for telemetry, not crash.
    ds = DivSampling(DivSamplingConfig(enabled=True))
    base = "Given an array of integers, return the maximum sum subarray."
    prompts = [ds.apply(base, i, task_id="t1") for i in range(3)]
    assert len(set(prompts)) == 3
    for prompt in prompts:
        assert prompt.index(base) > 0  # the task text is whole, with the perturbation before it


@pytest.mark.parametrize("text, prompt, expected", [
    pytest.param("Be careful.", "Solve X", "Be careful.\n\nSolve X", id="text-then-blank-line"),
    pytest.param("", "Solve X", "Solve X", id="empty-text-changes-nothing"),
    pytest.param("Think hard.", "Line 1\nLine 2\nLine 3",
                 "Think hard.\n\nLine 1\nLine 2\nLine 3", id="multiline-prompt"),
])
def test_apply_perturbation(text, prompt, expected):
    perturbation = Perturbation(text=text, category="instruction", label="test")
    assert apply_perturbation(prompt, perturbation) == expected


def test_select_from_an_empty_library_returns_an_empty_perturbation():
    p = select_perturbation(0, [])
    assert p.label == "empty"
    assert p.text == ""


@pytest.mark.parametrize("custom, appended", [
    pytest.param([], [], id="no-custom-perturbations"),
    pytest.param(["Use recursion.", "Avoid global state."], [
        Perturbation(text="Use recursion.", category="custom", label="custom_0"),
        Perturbation(text="Avoid global state.", category="custom", label="custom_1"),
    ], id="two-custom-perturbations"),
])
def test_custom_perturbations_are_appended_to_the_defaults(custom, appended):
    config = DivSamplingConfig(enabled=True, custom_perturbations=custom)
    assert get_perturbation_library(config) == DEFAULT_PERTURBATIONS + appended


def test_each_apply_with_a_task_id_appends_one_telemetry_event(tmp_path):
    ds = DivSampling(DivSamplingConfig(enabled=True), telemetry_dir=tmp_path)
    ds.apply("Solve X", 0, task_id="LCB_001")
    ds.apply("Solve X", 1, task_id="LCB_001")
    lines = (tmp_path / "div_sampling_events.jsonl").read_text().splitlines()
    assert len(lines) == 2
    event = json.loads(lines[0])
    assert event["task_id"] == "LCB_001"
    assert event["candidate_index"] == 0
    assert event["perturbation_label"] == DEFAULT_PERTURBATIONS[0].label
    assert event["perturbation_category"] == DEFAULT_PERTURBATIONS[0].category


def test_an_apply_without_a_task_id_writes_no_telemetry(tmp_path):
    DivSampling(DivSamplingConfig(enabled=True), telemetry_dir=tmp_path).apply("prompt", 0)
    assert not (tmp_path / "div_sampling_events.jsonl").exists()


def test_perturbation_to_dict():
    p = Perturbation(text="Be fast.", category="style", label="fast")
    assert p.to_dict() == {"text": "Be fast.", "category": "style", "label": "fast"}


def test_event_to_dict():
    d = DivSamplingEvent(
        task_id="t1", candidate_index=2,
        perturbation_label="mathematician", perturbation_category="role",
    ).to_dict()
    assert d["task_id"] == "t1"
    assert d["candidate_index"] == 2
    assert "timestamp" in d
