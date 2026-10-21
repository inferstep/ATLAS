"""Tests for V3 Budget Forcing (Feature 1C)."""

import json

import pytest

from stages.budget_forcing import (
    BUDGET_TIERS,
    BudgetForcing,
    BudgetForcingConfig,
    get_system_prompt,
    normalize_energy,
    select_tier,
)


# ---------------------------------------------------------------------------
# Energy normalization
# ---------------------------------------------------------------------------

def test_normalize_energy_puts_pass_energy_low_and_fail_energy_high():
    # V2 measured a mean Lens energy of 5.0 for passing code and 14.0 for
    # failing code; the default midpoint 9.5 lies halfway between them.
    assert normalize_energy(5.0) < 0.10
    assert normalize_energy(9.5) == pytest.approx(0.5, abs=1e-10)
    assert normalize_energy(14.0) > 0.90


def test_normalize_energy_rises_with_energy():
    energies = [0, 3, 5, 7, 9.5, 12, 14, 20, 50]
    normalized = [normalize_energy(energy) for energy in energies]
    for lower, higher in zip(normalized, normalized[1:]):
        assert lower < higher


def test_normalize_energy_stays_between_0_and_1():
    assert 0 < normalize_energy(-100)
    assert normalize_energy(100) <= 1.0
    assert 0 < normalize_energy(-10) < 1
    assert 0 < normalize_energy(20) < 1


def test_normalize_energy_with_custom_midpoint_and_steepness():
    assert normalize_energy(5.0, midpoint=5.0) == pytest.approx(0.5, abs=1e-10)
    # A steeper curve gives a more extreme value for the same energy.
    assert normalize_energy(12.0, steepness=2.0) > normalize_energy(12.0, steepness=0.1)


# ---------------------------------------------------------------------------
# Tier selection
# ---------------------------------------------------------------------------

def test_select_tier_without_energy_returns_the_default_tier():
    assert select_tier() == "standard"
    assert select_tier(default_tier="hard") == "hard"


@pytest.mark.parametrize("raw_energy, normalized_energy, tier", [
    # Raw Lens energy is normalized first; the normalized value is in brackets.
    pytest.param(2.0, None, "nothink", id="raw 2.0 (0.023): nothink"),
    pytest.param(5.0, None, "nothink", id="raw 5.0 (0.095): nothink"),
    pytest.param(6.0, None, "standard", id="raw 6.0 (0.148): standard"),
    pytest.param(7.2, None, "hard", id="raw 7.2 (0.240): hard"),
    pytest.param(14.0, None, "extreme", id="raw 14.0 (0.905): extreme"),
    # The tier boundaries are at normalized 0.10, 0.20 and 0.30.
    pytest.param(None, 0.05, "nothink", id="normalized 0.05: nothink"),
    pytest.param(None, 0.09, "nothink", id="normalized 0.09: nothink"),
    pytest.param(None, 0.10, "standard", id="normalized 0.10: standard"),
    pytest.param(None, 0.19, "standard", id="normalized 0.19: standard"),
    pytest.param(None, 0.20, "hard", id="normalized 0.20: hard"),
    pytest.param(None, 0.29, "hard", id="normalized 0.29: hard"),
    pytest.param(None, 0.30, "extreme", id="normalized 0.30: extreme"),
    pytest.param(None, 0.99, "extreme", id="normalized 0.99: extreme"),
    pytest.param(2.0, 0.50, "extreme", id="both given: normalized wins"),
])
def test_select_tier(raw_energy, normalized_energy, tier):
    assert select_tier(raw_energy=raw_energy, normalized_energy=normalized_energy) == tier


@pytest.mark.parametrize("enabled, raw_energy, tier", [
    pytest.param(False, 15.0, "nothink", id="off, energy 15.0: nothink"),
    pytest.param(False, 2.0, "nothink", id="off, energy 2.0: nothink"),
    pytest.param(False, None, "nothink", id="off, no energy: nothink"),
    pytest.param(True, None, "standard", id="on, no energy: default tier"),
    pytest.param(True, 2.0, "nothink", id="on, energy 2.0: nothink"),
    pytest.param(True, 3.0, "nothink", id="on, energy 3.0: nothink"),
    pytest.param(True, 5.0, "nothink", id="on, energy 5.0: nothink"),
    pytest.param(True, 14.0, "extreme", id="on, energy 14.0: extreme"),
])
def test_budget_forcing_select_tier(enabled, raw_energy, tier):
    budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=enabled))
    assert budget_forcing.select_tier(raw_energy=raw_energy) == tier


# ---------------------------------------------------------------------------
# Tier budgets
# ---------------------------------------------------------------------------

def test_there_are_five_tiers():
    assert set(BUDGET_TIERS) == {"nothink", "light", "standard", "hard", "extreme"}


@pytest.mark.parametrize("tier, max_thinking, inject_wait, max_tokens", [
    # max_tokens is the thinking budget plus 4096 tokens for the code itself.
    pytest.param("nothink", 0, False, 4096, id="nothink"),
    pytest.param("light", 1024, False, 1024 + 4096, id="light"),
    pytest.param("standard", 2048, True, 2048 + 4096, id="standard"),
    pytest.param("hard", 4096, True, 4096 + 4096, id="hard"),
    pytest.param("extreme", 8192, True, 8192 + 4096, id="extreme"),
])
def test_tier_budget(tier, max_thinking, inject_wait, max_tokens):
    budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True))
    assert BUDGET_TIERS[tier]["max_thinking"] == max_thinking
    assert BUDGET_TIERS[tier]["inject_wait"] is inject_wait
    assert budget_forcing.get_max_tokens(tier) == max_tokens


def test_tiers_that_inject_wait_do_it_before_the_thinking_budget_ends():
    for tier in ["standard", "hard", "extreme"]:
        assert 0 < BUDGET_TIERS[tier]["wait_threshold"] < BUDGET_TIERS[tier]["max_thinking"]


# ---------------------------------------------------------------------------
# System prompt and ChatML
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("tier", ["nothink", "light", "standard", "hard", "extreme"])
def test_system_prompt_of_every_tier_carries_the_code_contract(tier):
    prompt = get_system_prompt(tier)
    # Thinking is switched by the shared client's enable_thinking, not by a token in the prompt.
    assert "/nothink" not in prompt
    assert "explicitly requested public name and declaration as immutable" in prompt
    assert "spelling, capitalization" in prompt
    assert "parameter order and kinds (including / and * markers)" in prompt
    assert "defaults, and type annotations exactly" in prompt
    assert "do not substitute aliases" in prompt
    assert "laziness, stopping conditions, ordering" in prompt
    assert "complete, syntactically valid requested artifact" in prompt
    assert "never import that artifact from itself" in prompt
    assert "verify the artifact parses" in prompt
    assert "do not add demos, tests, or main blocks unless requested" in prompt


def test_nothink_system_prompt_asks_for_a_direct_answer():
    assert get_system_prompt("nothink").startswith(
        "You are an expert programmer. Respond directly and concisely.")


def test_thinking_tiers_ask_the_model_to_think_step_by_step():
    for tier in ["light", "standard", "hard", "extreme"]:
        assert "step by step" in get_system_prompt(tier).lower()


@pytest.mark.parametrize("tier", ["nothink", "hard"])
def test_format_chatml(tier):
    budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True))

    chatml = budget_forcing.format_chatml("Solve this", tier)

    assert chatml == (
        f"<|im_start|>system\n{get_system_prompt(tier)}<|im_end|>\n"
        "<|im_start|>user\nSolve this<|im_end|>\n"
        "<|im_start|>assistant\n"
    )
    # No /nothink directive and no <think> prefill: both were specific to one model family.
    assert "/nothink" not in chatml
    assert "<think>" not in chatml


# ---------------------------------------------------------------------------
# Telemetry
# ---------------------------------------------------------------------------

def test_log_event_appends_one_json_line_per_event(tmp_path):
    budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True), telemetry_dir=tmp_path)

    budget_forcing.log_event(
        task_id="LCB_001", tier="hard", raw_energy=12.5, normalized_energy=0.82,
        thinking_tokens=2000, wait_injections=1, thinking_extended=True, total_tokens=5000,
    )
    budget_forcing.log_event(task_id="LCB_002", tier="nothink")

    lines = (tmp_path / "budget_forcing_events.jsonl").read_text().splitlines()
    assert len(lines) == 2
    first, second = json.loads(lines[0]), json.loads(lines[1])
    assert first["task_id"] == "LCB_001"
    assert first["tier"] == "hard"
    assert first["raw_energy"] == 12.5
    assert first["thinking_tokens"] == 2000
    assert first["wait_injections"] == 1
    assert first["thinking_extended"] is True
    # An event without energy leaves the energy fields out.
    assert second["task_id"] == "LCB_002"
    assert "raw_energy" not in second
    assert "normalized_energy" not in second


def test_log_event_without_a_telemetry_dir_still_returns_the_event():
    budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True), telemetry_dir=None)
    event = budget_forcing.log_event(task_id="test", tier="standard")
    assert event.task_id == "test"
