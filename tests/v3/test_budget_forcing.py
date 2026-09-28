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
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def tmp_telemetry(tmp_path):
    """Provide a temporary telemetry directory."""
    d = tmp_path / "telemetry"
    d.mkdir()
    return d


@pytest.fixture
def bf_enabled(tmp_telemetry):
    """BudgetForcing instance with enabled=True."""
    cfg = BudgetForcingConfig(enabled=True)
    return BudgetForcing(cfg, telemetry_dir=tmp_telemetry)


@pytest.fixture
def bf_disabled(tmp_telemetry):
    """BudgetForcing instance with enabled=False."""
    cfg = BudgetForcingConfig(enabled=False)
    return BudgetForcing(cfg, telemetry_dir=tmp_telemetry)


# ---------------------------------------------------------------------------
# Test: Disabled noop behavior
# ---------------------------------------------------------------------------

class TestDisabledNoop:
    """When enabled=False, BudgetForcing should be a complete noop."""

    def test_select_tier_returns_nothink(self, bf_disabled):
        assert bf_disabled.select_tier(raw_energy=15.0) == "nothink"
        assert bf_disabled.select_tier(raw_energy=2.0) == "nothink"
        assert bf_disabled.select_tier() == "nothink"


# ---------------------------------------------------------------------------
# Test: Energy normalization
# ---------------------------------------------------------------------------

class TestNormalizeEnergy:
    """Test the sigmoid energy normalization function."""

    def test_pass_energy_is_low(self):
        """PASS mean energy (5.0) should normalize to low value."""
        norm = normalize_energy(5.0)
        assert norm < 0.10, f"PASS energy normalized to {norm}, expected <0.10"

    def test_fail_energy_is_high(self):
        """FAIL mean energy (14.0) should normalize to high value."""
        norm = normalize_energy(14.0)
        assert norm > 0.90, f"FAIL energy normalized to {norm}, expected >0.90"

    def test_midpoint_is_half(self):
        """Energy at midpoint (9.5) should normalize to exactly 0.5."""
        norm = normalize_energy(9.5)
        assert abs(norm - 0.5) < 1e-10

    def test_monotonically_increasing(self):
        """Higher energy should always give higher normalized value."""
        energies = [0, 3, 5, 7, 9.5, 12, 14, 20, 50]
        norms = [normalize_energy(e) for e in energies]
        for i in range(len(norms) - 1):
            assert norms[i] < norms[i + 1], (
                f"Not monotonic at {energies[i]}->{energies[i+1]}: "
                f"{norms[i]}->{norms[i+1]}"
            )

    def test_bounds(self):
        """Normalized energy should approach but not exceed [0, 1]."""
        assert 0 < normalize_energy(-100)
        assert normalize_energy(100) <= 1.0
        # Moderate values stay strictly in (0, 1)
        assert 0 < normalize_energy(-10) < 1
        assert 0 < normalize_energy(20) < 1

    def test_custom_midpoint_and_steepness(self):
        """Custom parameters should shift the sigmoid."""
        # With midpoint=5, energy=5 should be 0.5
        assert abs(normalize_energy(5.0, midpoint=5.0) - 0.5) < 1e-10
        # Steeper sigmoid → more extreme values
        steep = normalize_energy(12.0, steepness=2.0)
        gentle = normalize_energy(12.0, steepness=0.1)
        assert steep > gentle


# ---------------------------------------------------------------------------
# Test: Tier selection
# ---------------------------------------------------------------------------

class TestSelectTier:
    """Test energy-to-tier mapping."""

    def test_no_energy_returns_default(self):
        assert select_tier() == "standard"
        assert select_tier(default_tier="hard") == "hard"

    def test_very_easy_task(self):
        """Very low energy (easy) → nothink."""
        # Energy 2.0 normalizes to ~0.024
        assert select_tier(raw_energy=2.0) == "nothink"

    def test_easy_task(self):
        """Low energy → nothink."""
        # Energy 5.0 normalizes to ~0.095
        assert select_tier(raw_energy=5.0) == "nothink"

    def test_medium_task(self):
        """Medium energy → standard."""
        # Energy 6.0 normalizes to ~0.148 (in [0.10, 0.20) → standard)
        assert select_tier(raw_energy=6.0) == "standard"

    def test_hard_task(self):
        """Higher energy → hard."""
        # Energy 7.2 normalizes to ~0.241 (in [0.20, 0.30) → hard)
        assert select_tier(raw_energy=7.2) == "hard"

    def test_extreme_task(self):
        """High energy → extreme."""
        assert select_tier(raw_energy=14.0) == "extreme"

    def test_all_normalized_boundaries(self):
        """Test tier boundaries using pre-normalized energy values."""
        assert select_tier(normalized_energy=0.05) == "nothink"
        assert select_tier(normalized_energy=0.09) == "nothink"
        assert select_tier(normalized_energy=0.10) == "standard"
        assert select_tier(normalized_energy=0.19) == "standard"
        assert select_tier(normalized_energy=0.20) == "hard"
        assert select_tier(normalized_energy=0.29) == "hard"
        assert select_tier(normalized_energy=0.30) == "extreme"
        assert select_tier(normalized_energy=0.99) == "extreme"

    def test_normalized_takes_precedence(self):
        """If both raw and normalized provided, normalized is used."""
        # raw=2.0 would give nothink, but normalized=0.50 gives extreme
        assert select_tier(raw_energy=2.0, normalized_energy=0.50) == "extreme"


# ---------------------------------------------------------------------------
# Test: System prompt generation
# ---------------------------------------------------------------------------

class TestGetSystemPrompt:

    def test_nothink_prompt_is_plain_directive(self):
        # Thinking is disabled via the shared client's enable_thinking
        # kwarg, not an in-prompt token — the nothink prompt is plain
        # English with no model-specific directives.
        prompt = get_system_prompt("nothink")
        assert "/nothink" not in prompt
        assert "directly and concisely" in prompt

    def test_thinking_tiers_lack_nothink(self):
        for tier in ["light", "standard", "hard", "extreme"]:
            prompt = get_system_prompt(tier)
            assert "/nothink" not in prompt
            assert "think" in prompt.lower() or "step by step" in prompt.lower()

    @pytest.mark.parametrize("tier", ["nothink", "light", "standard", "hard", "extreme"])
    def test_every_generation_tier_preserves_exact_public_contracts(self, tier):
        prompt = get_system_prompt(tier)
        assert "public name and declaration as immutable" in prompt
        assert "spelling, capitalization" in prompt
        assert "parameter order and kinds (including / and * markers)" in prompt
        assert "defaults, and type annotations exactly" in prompt
        assert "do not substitute aliases" in prompt
        assert "laziness, stopping conditions, ordering" in prompt
        assert "complete, syntactically valid requested artifact" in prompt
        assert "never import that artifact from itself" in prompt
        assert "verify the artifact parses" in prompt
        assert "do not add demos, tests, or main blocks unless requested" in prompt


# ---------------------------------------------------------------------------
# Test: BudgetForcing class (integration)
# ---------------------------------------------------------------------------

class TestBudgetForcingClass:

    def test_format_chatml_nothink(self, bf_enabled):
        prompt = bf_enabled.format_chatml("Solve this", "nothink")
        # No in-prompt directive and no Qwen-specific think prefill —
        # the shared client handles thinking via enable_thinking.
        assert "/nothink" not in prompt
        assert "<think>" not in prompt
        assert "<|im_start|>system" in prompt
        assert "<|im_start|>user" in prompt
        assert "Solve this" in prompt

    def test_format_chatml_thinking(self, bf_enabled):
        prompt = bf_enabled.format_chatml("Solve this", "hard")
        assert "/nothink" not in prompt
        assert "step by step" in prompt.lower()

    def test_get_max_tokens_nothink(self, bf_enabled):
        assert bf_enabled.get_max_tokens("nothink") == 4096

    def test_get_max_tokens_tiers(self, bf_enabled):
        assert bf_enabled.get_max_tokens("light") == 1024 + 4096
        assert bf_enabled.get_max_tokens("standard") == 2048 + 4096
        assert bf_enabled.get_max_tokens("hard") == 4096 + 4096
        assert bf_enabled.get_max_tokens("extreme") == 8192 + 4096

    def test_select_tier_enabled(self, bf_enabled):
        """Enabled BudgetForcing selects by energy."""
        assert bf_enabled.select_tier(raw_energy=2.0) == "nothink"
        assert bf_enabled.select_tier(raw_energy=14.0) == "extreme"

    def test_select_tier_no_energy_uses_default(self, bf_enabled):
        assert bf_enabled.select_tier() == "standard"


# ---------------------------------------------------------------------------
# Test: Telemetry logging
# ---------------------------------------------------------------------------

class TestTelemetry:

    def test_event_written_to_jsonl(self, bf_enabled, tmp_telemetry):
        bf_enabled.log_event(
            task_id="LCB_001",
            tier="hard",
            raw_energy=12.5,
            normalized_energy=0.82,
            thinking_tokens=2000,
            wait_injections=1,
            thinking_extended=True,
            total_tokens=5000,
        )
        events_file = tmp_telemetry / "budget_forcing_events.jsonl"
        assert events_file.exists()
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 1
        data = json.loads(lines[0])
        assert data["task_id"] == "LCB_001"
        assert data["tier"] == "hard"
        assert data["raw_energy"] == 12.5
        assert data["thinking_tokens"] == 2000
        assert data["wait_injections"] == 1
        assert data["thinking_extended"] is True

    def test_multiple_events_appended(self, bf_enabled, tmp_telemetry):
        for i in range(5):
            bf_enabled.log_event(task_id=f"task_{i}", tier="standard")
        events_file = tmp_telemetry / "budget_forcing_events.jsonl"
        lines = events_file.read_text().strip().split("\n")
        assert len(lines) == 5

    def test_event_without_energy(self, bf_enabled, tmp_telemetry):
        event = bf_enabled.log_event(task_id="test", tier="nothink")
        data = event.to_dict()
        assert "raw_energy" not in data
        assert "normalized_energy" not in data

    def test_no_telemetry_dir_no_crash(self):
        """BudgetForcing without telemetry_dir should not crash on log."""
        cfg = BudgetForcingConfig(enabled=True)
        bf = BudgetForcing(cfg, telemetry_dir=None)
        event = bf.log_event(task_id="test", tier="standard")
        assert event.task_id == "test"


# ---------------------------------------------------------------------------
# Test: Budget tier definitions
# ---------------------------------------------------------------------------

class TestBudgetTierDefinitions:

    def test_all_five_tiers_exist(self):
        expected = {"nothink", "light", "standard", "hard", "extreme"}
        assert set(BUDGET_TIERS.keys()) == expected

    def test_thinking_budgets_increase(self):
        order = ["nothink", "light", "standard", "hard", "extreme"]
        for i in range(len(order) - 1):
            assert (
                BUDGET_TIERS[order[i]]["max_thinking"]
                < BUDGET_TIERS[order[i + 1]]["max_thinking"]
            )

    def test_nothink_no_injection(self):
        assert BUDGET_TIERS["nothink"]["inject_wait"] is False
        assert BUDGET_TIERS["nothink"]["max_thinking"] == 0

    def test_light_no_injection(self):
        assert BUDGET_TIERS["light"]["inject_wait"] is False

    def test_standard_hard_extreme_inject(self):
        for tier in ["standard", "hard", "extreme"]:
            assert BUDGET_TIERS[tier]["inject_wait"] is True
            assert BUDGET_TIERS[tier]["wait_threshold"] > 0


# ---------------------------------------------------------------------------
# Test: AC-1C-2 — HARD_PATH tasks >=5% improvement
# (Offline validation — measured during benchmark, tested structurally here)
# ---------------------------------------------------------------------------

class TestAC1C2HardPathImprovement:
    """AC-1C-2: Structural test that hard tier gets appropriate budget."""

    def test_hard_tier_gets_4096_budget(self, bf_enabled):
        """Hard tier should have 4096 thinking tokens — sufficient for complex reasoning."""
        assert BUDGET_TIERS["hard"]["max_thinking"] == 4096

    def test_extreme_tier_gets_8192_budget(self, bf_enabled):
        """Extreme tier for hardest tasks gets maximum budget."""
        assert BUDGET_TIERS["extreme"]["max_thinking"] == 8192

    def test_high_energy_selects_extreme(self, bf_enabled):
        """Tasks with FAIL-like energy should get maximum reasoning."""
        tier = bf_enabled.select_tier(raw_energy=14.0)
        assert tier == "extreme"


# ---------------------------------------------------------------------------
# Test: AC-1C-4 — Total thinking tokens within budget ±10%
# ---------------------------------------------------------------------------

class TestAC1C4TokenBudgetCompliance:
    """AC-1C-4: Token budgets are correctly configured."""

    def test_max_tokens_includes_output_buffer(self, bf_enabled):
        """get_max_tokens adds 4096 output buffer to thinking budget."""
        for tier_name, tier_config in BUDGET_TIERS.items():
            max_tok = bf_enabled.get_max_tokens(tier_name)
            if tier_name == "nothink":
                assert max_tok == 4096  # V3.1: code-only, no thinking
            else:
                assert max_tok == tier_config["max_thinking"] + 4096

    def test_wait_threshold_below_max(self):
        """Wait threshold must be strictly below max_thinking."""
        for name, cfg in BUDGET_TIERS.items():
            if cfg["inject_wait"]:
                assert cfg["wait_threshold"] < cfg["max_thinking"], (
                    f"{name}: wait_threshold ({cfg['wait_threshold']}) >= "
                    f"max_thinking ({cfg['max_thinking']})"
                )


# ---------------------------------------------------------------------------
# Test: AC-1C-5 — FAST_PATH (/nothink) no regression
# ---------------------------------------------------------------------------

class TestAC1C5NothinkNoRegression:
    """AC-1C-5: /nothink keeps direct generation semantics."""

    def test_nothink_system_prompt_retains_direct_prefix(self):
        """The nothink prompt keeps its direct wording and adds only the
        model-agnostic artifact contract."""
        prompt = get_system_prompt("nothink")
        assert prompt.startswith(
            "You are an expert programmer. Respond directly and concisely.")
        assert "explicitly requested public name and declaration" in prompt

    def test_nothink_chatml_has_no_prefill(self, bf_enabled):
        """The closed-think-block prefill was Qwen-specific and breaks
        other templates; nothink ChatML ends with a clean assistant cue."""
        user_content = "Write a function to sort a list"
        chatml = bf_enabled.format_chatml(user_content, "nothink")
        assert "/nothink" not in chatml
        assert "<think>" not in chatml
        assert chatml.endswith("<|im_start|>assistant\n")

    def test_nothink_max_tokens_v31(self, bf_enabled):
        """V3.1: Nothink max tokens reduced to 4096 (code-only)."""
        assert bf_enabled.get_max_tokens("nothink") == 4096

    def test_easy_tasks_use_nothink(self, bf_enabled):
        """Easy tasks (low energy) should use nothink — same as V2."""
        assert bf_enabled.select_tier(raw_energy=3.0) == "nothink"
        assert bf_enabled.select_tier(raw_energy=5.0) == "nothink"
