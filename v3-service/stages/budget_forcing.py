"""V3 Budget Forcing (Feature 1C) — Extended Reasoning via Token Budget Control.

Controls model thinking depth by tier: each tier maps to a system prompt
(direct vs. think-step-by-step) and a max-token budget.

Paper: Muennighoff et al., Stanford (arxiv:2501.19393)
Config: [budget_forcing] in atlas.conf
Telemetry: telemetry/budget_forcing_events.jsonl

Budget Tiers:
  nothink  — 0 thinking tokens (thinking disabled via the shared client)
  light    — up to 1024 thinking tokens
  standard — up to 2048 thinking tokens
  hard     — up to 4096 thinking tokens
  extreme  — up to 8192 thinking tokens

Energy-to-tier mapping uses normalized Lens energy (sigmoid scale 0-1):
  energy < 0.10 → nothink  (easy, don't waste tokens)
  energy < 0.20 → standard
  energy < 0.30 → hard
  energy >= 0.30 → extreme  (very hard, maximum reasoning)

If no energy available (first generation, no Lens score), uses default_tier.
"""

import json
import math
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, Optional


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class BudgetForcingConfig:
    """Configuration for Budget Forcing."""
    enabled: bool = False
    default_tier: str = "standard"
    energy_midpoint: float = 9.5
    energy_steepness: float = 0.5


# ---------------------------------------------------------------------------
# Budget tier definitions
# ---------------------------------------------------------------------------
# inject_wait / wait_threshold are currently unread — nothing performs Wait
# injection. Retained pending the tier-table review.

BUDGET_TIERS: Dict[str, Dict] = {
    "nothink": {
        "max_thinking": 0,
        "inject_wait": False,
        "wait_threshold": 0,
    },
    "light": {
        "max_thinking": 1024,
        "inject_wait": False,
        "wait_threshold": 0,
    },
    "standard": {
        "max_thinking": 2048,
        "inject_wait": True,
        "wait_threshold": 512,
    },
    "hard": {
        "max_thinking": 4096,
        "inject_wait": True,
        "wait_threshold": 1024,
    },
    "extreme": {
        "max_thinking": 8192,
        "inject_wait": True,
        "wait_threshold": 2048,
    },
}

VALID_TIERS = frozenset(BUDGET_TIERS.keys())

# System prompts per tier category. Thinking is controlled model-agnostically
# by the shared client (`enable_thinking` chat-template kwarg) rather than by an
# in-prompt directive, so no `/nothink` token here — these are plain English.
# COUPLING: v3_runner's LLMAdapter derives enable_thinking from think-language
# in the system prompt ("Think step by step" / "Think carefully" / "Think
# through") because the LLMCallable contract has no tier parameter. Reword
# these prompts and the adapter's marker list together.
_CODE_CONTRACT_SUFFIX = (
    " When the user asks for code, treat every explicitly requested public "
    "name and declaration as immutable. Copy spelling, capitalization, "
    "parameter order and kinds (including / and * markers), defaults, and "
    "type annotations exactly; do not substitute aliases. Implement "
    "behavioral contracts such as laziness, stopping conditions, ordering, "
    "and error behavior literally. Return one complete, syntactically valid "
    "requested artifact; never import that artifact from itself, and do not "
    "add demos, tests, or main blocks unless requested. Before answering, "
    "verify the artifact parses and every requested declaration still matches."
)

_SYSTEM_PROMPT_NOTHINK = (
    "You are an expert programmer. Respond directly and concisely."
    + _CODE_CONTRACT_SUFFIX
)
_SYSTEM_PROMPT_THINK = (
    "You are an expert programmer. Think step by step about the problem "
    "before writing code."
    + _CODE_CONTRACT_SUFFIX
)


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class BudgetForcingEvent:
    """A single Budget Forcing telemetry event."""
    task_id: str
    tier: str
    raw_energy: Optional[float] = None
    normalized_energy: Optional[float] = None
    thinking_tokens: int = 0
    wait_injections: int = 0
    thinking_extended: bool = False
    total_tokens: int = 0
    timestamp: str = ""

    def to_dict(self) -> Dict:
        d = {
            "task_id": self.task_id,
            "tier": self.tier,
            "thinking_tokens": self.thinking_tokens,
            "wait_injections": self.wait_injections,
            "thinking_extended": self.thinking_extended,
            "total_tokens": self.total_tokens,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }
        if self.raw_energy is not None:
            d["raw_energy"] = self.raw_energy
        if self.normalized_energy is not None:
            d["normalized_energy"] = self.normalized_energy
        return d


# ---------------------------------------------------------------------------
# Core functions (stateless, usable without class)
# ---------------------------------------------------------------------------

def normalize_energy(raw_energy: float, midpoint: float = 9.5,
                     steepness: float = 0.5) -> float:
    """Normalize raw Lens energy to 0-1 scale via sigmoid.

    V2 measured: PASS mean = 5.00, FAIL mean = 14.04.
    Midpoint at (5.00 + 14.04) / 2 ≈ 9.5.

    Args:
        raw_energy: Raw C(x) energy from Geometric Lens.
        midpoint: Center of sigmoid transition.
        steepness: Controls transition sharpness.

    Returns:
        Normalized energy in [0, 1]. Low = easy, high = hard.
    """
    return 1.0 / (1.0 + math.exp(-steepness * (raw_energy - midpoint)))


def select_tier(raw_energy: Optional[float] = None,
                normalized_energy: Optional[float] = None,
                default_tier: str = "standard",
                midpoint: float = 9.5,
                steepness: float = 0.5) -> str:
    """Select budget tier based on Lens energy.

    If no energy is provided, returns default_tier.

    Args:
        raw_energy: Raw C(x) energy (will be normalized).
        normalized_energy: Pre-normalized energy in [0, 1].
        default_tier: Fallback tier when no energy available.
        midpoint: Normalization midpoint.
        steepness: Normalization steepness.

    Returns:
        Tier name: one of "nothink", "light", "standard", "hard", "extreme".
    """
    if normalized_energy is None and raw_energy is None:
        return default_tier

    if normalized_energy is None:
        normalized_energy = normalize_energy(raw_energy, midpoint, steepness)

    if normalized_energy < 0.10:
        return "nothink"
    elif normalized_energy < 0.20:
        return "standard"
    elif normalized_energy < 0.30:
        return "hard"
    else:
        return "extreme"


def get_max_tokens(tier: str) -> int:
    """Return the max token budget for a tier (thinking + output).

    Adds a 4096-token buffer for code output on top of the thinking budget.
    """
    tier_config = BUDGET_TIERS.get(tier, BUDGET_TIERS["standard"])
    if tier == "nothink":
        return 4096  # Code output only, no thinking overhead
    return tier_config["max_thinking"] + 4096


def get_system_prompt(tier: str) -> str:
    """Return the system prompt for a given budget tier."""
    if tier == "nothink":
        return _SYSTEM_PROMPT_NOTHINK
    return _SYSTEM_PROMPT_THINK


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class BudgetForcing:
    """Budget Forcing controller for V3 extended reasoning.

    Manages thinking budget tier selection, prompt formatting, and
    telemetry logging.

    Args:
        config: BudgetForcingConfig instance.
        telemetry_dir: Directory for JSONL event logs. None disables logging.
    """

    def __init__(self, config: BudgetForcingConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "budget_forcing_events.jsonl"

    # -- Public API ---------------------------------------------------------

    def select_tier(self, raw_energy: Optional[float] = None,
                    normalized_energy: Optional[float] = None) -> str:
        """Select budget tier. Delegates to module-level select_tier()."""
        if not self.config.enabled:
            return "nothink"
        return select_tier(
            raw_energy=raw_energy,
            normalized_energy=normalized_energy,
            default_tier=self.config.default_tier,
            midpoint=self.config.energy_midpoint,
            steepness=self.config.energy_steepness,
        )


    def format_chatml(self, user_content: str, tier: str) -> str:
        """Format a ChatML prompt with the appropriate system prompt for the tier.

        The returned ChatML string is normalized by the shared client
        (`chatml_to_messages` -> `/v1/chat/completions`), so the model's OWN
        template applies. Thinking on/off is handled model-agnostically by the
        client's `enable_thinking` kwarg, NOT by a pre-filled `<think>` block
        (that pre-fill was Qwen-specific and breaks other templates). We
        therefore emit a clean trailing assistant cue regardless of tier; the
        normalizer drops the empty assistant turn.

        Args:
            user_content: The user's message content.
            tier: Budget tier name.

        Returns:
            Full ChatML-formatted prompt string.
        """
        system = get_system_prompt(tier)
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def get_max_tokens(self, tier: str) -> int:
        """Return the max token budget for a tier. Delegates to the
        module-level get_max_tokens()."""
        return get_max_tokens(tier)

    def log_event(self, task_id: str, tier: str,
                  raw_energy: Optional[float] = None,
                  normalized_energy: Optional[float] = None,
                  thinking_tokens: int = 0,
                  wait_injections: int = 0,
                  thinking_extended: bool = False,
                  total_tokens: int = 0) -> BudgetForcingEvent:
        """Log a Budget Forcing telemetry event.

        Returns the event object for inspection/testing.
        """
        event = BudgetForcingEvent(
            task_id=task_id,
            tier=tier,
            raw_energy=raw_energy,
            normalized_energy=normalized_energy,
            thinking_tokens=thinking_tokens,
            wait_injections=wait_injections,
            thinking_extended=thinking_extended,
            total_tokens=total_tokens,
            timestamp=datetime.now(timezone.utc).isoformat(),
        )
        self._write_event(event)
        return event

    # -- Private helpers ----------------------------------------------------

    def _write_event(self, event: BudgetForcingEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            pass  # Telemetry failure should not crash the benchmark
