"""V3 DivSampling (Feature 1B) — Perturbation Diversity for Candidate Generation.

Maintains a library of distinct role/instruction/style perturbations that are
prepended to generation prompts. Each candidate gets a different perturbation,
increasing solution diversity when combined with PlanSearch.

Paper: Wang et al. (arxiv:2502.11027, Dec 2025)
Config: [div_sampling] in atlas.conf
Telemetry: telemetry/div_sampling_events.jsonl

Perturbation Categories (12 total: 4 + 4 + 4):
  - Role assignments (4): different expert personas
  - Instruction rephrasing (4): different thinking strategies
  - Style variations (4): functional, pythonic, optimize_iteratively, structured
"""

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Dict, List, Optional


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class DivSamplingConfig:
    """Configuration for DivSampling perturbation diversity."""
    enabled: bool = False
    custom_perturbations: List[str] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Perturbation library
# ---------------------------------------------------------------------------

@dataclass
class Perturbation:
    """A single perturbation with metadata."""
    text: str
    category: str  # "role", "instruction", or "style"
    label: str     # Short identifier

    def to_dict(self) -> Dict:
        return {
            "text": self.text,
            "category": self.category,
            "label": self.label,
        }


# The default perturbation library — >=10 perturbations across 3 categories
DEFAULT_PERTURBATIONS: List[Perturbation] = [
    # Role assignments (4)
    Perturbation(
        text="You are an expert competitive programmer who has solved "
             "thousands of contest problems on Codeforces and AtCoder.",
        category="role",
        label="competitive_programmer",
    ),
    Perturbation(
        text="You are a systems engineer who prioritizes clean, readable, "
             "and maintainable code with clear variable names.",
        category="role",
        label="systems_engineer",
    ),
    Perturbation(
        text="You are a mathematician who approaches coding problems with "
             "formal rigor, proving correctness before implementing.",
        category="role",
        label="mathematician",
    ),
    Perturbation(
        text="You are a pragmatic developer who writes the simplest correct "
             "solution first, optimizing only when necessary.",
        category="role",
        label="pragmatist",
    ),
    # Instruction rephrasing (4)
    Perturbation(
        text="Solve this step by step, verifying each logical step before "
             "proceeding to the next.",
        category="instruction",
        label="step_by_step",
    ),
    Perturbation(
        text="Think about what could go wrong first, then write code that "
             "handles all edge cases explicitly.",
        category="instruction",
        label="edge_case_first",
    ),
    Perturbation(
        text="Consider the computational complexity before choosing your "
             "approach. The solution must be efficient.",
        category="instruction",
        label="complexity_aware",
    ),
    Perturbation(
        text="Analyze the input constraints carefully. Use them to determine "
             "the optimal algorithm and data structures.",
        category="instruction",
        label="constraint_driven",
    ),
    # Style variations (4)
    Perturbation(
        text="Write your solution using functional programming style where "
             "possible, leveraging map, filter, and comprehensions.",
        category="style",
        label="functional",
    ),
    Perturbation(
        text="Focus on writing the most Pythonic solution using standard "
             "library features like collections, itertools, and bisect.",
        category="style",
        label="pythonic",
    ),
    Perturbation(
        text="Start with the brute force approach, then optimize step by "
             "step until you reach an efficient solution.",
        category="style",
        label="optimize_iteratively",
    ),
    Perturbation(
        text="Implement this using object-oriented design with clear class "
             "structure if appropriate, otherwise use clean functions.",
        category="style",
        label="structured",
    ),
]


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class DivSamplingEvent:
    """Telemetry event for a DivSampling perturbation application."""
    task_id: str
    candidate_index: int
    perturbation_label: str
    perturbation_category: str
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "candidate_index": self.candidate_index,
            "perturbation_label": self.perturbation_label,
            "perturbation_category": self.perturbation_category,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Core functions
# ---------------------------------------------------------------------------

def get_perturbation_library(config: Optional[DivSamplingConfig] = None
                             ) -> List[Perturbation]:
    """Return the full perturbation library including any custom entries.

    Custom perturbations from config are appended as "custom" category.
    """
    library = list(DEFAULT_PERTURBATIONS)
    if config and config.custom_perturbations:
        for i, text in enumerate(config.custom_perturbations):
            library.append(Perturbation(
                text=text,
                category="custom",
                label=f"custom_{i}",
            ))
    return library


def select_perturbation(candidate_index: int,
                        library: List[Perturbation]) -> Perturbation:
    """Select a perturbation for a given candidate index.

    Uses modular indexing to cycle through the library.

    Args:
        candidate_index: 0-based candidate index.
        library: List of available perturbations.

    Returns:
        The selected Perturbation.
    """
    if not library:
        return Perturbation(text="", category="none", label="empty")
    return library[candidate_index % len(library)]


def apply_perturbation(prompt: str, perturbation: Perturbation) -> str:
    """Prepend a perturbation to a generation prompt.

    The perturbation is prepended as a separate paragraph before the
    main prompt content, without modifying the core task description.

    Args:
        prompt: The original generation prompt.
        perturbation: The perturbation to apply.

    Returns:
        Modified prompt with perturbation prepended.
    """
    if not perturbation.text:
        return prompt
    return f"{perturbation.text}\n\n{prompt}"


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class DivSampling:
    """DivSampling perturbation diversity controller.

    When enabled, applies a different perturbation to each candidate's
    generation prompt, increasing solution diversity.

    When disabled, returns prompts unmodified (noop).

    Args:
        config: DivSamplingConfig instance.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: DivSamplingConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self._library = get_perturbation_library(config)
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "div_sampling_events.jsonl"

    @property
    def library(self) -> List[Perturbation]:
        """The current perturbation library."""
        return self._library

    def apply(self, prompt: str, candidate_index: int,
              task_id: str = "") -> str:
        """Apply perturbation to a prompt for a given candidate.

        Args:
            prompt: Original generation prompt.
            candidate_index: 0-based candidate index.
            task_id: Task identifier for telemetry.

        Returns:
            Prompt with perturbation prepended (or unmodified if disabled).
        """
        if not self.config.enabled:
            return prompt

        perturbation = select_perturbation(candidate_index, self._library)
        result = apply_perturbation(prompt, perturbation)

        # Log telemetry
        if task_id:
            self._log_event(DivSamplingEvent(
                task_id=task_id,
                candidate_index=candidate_index,
                perturbation_label=perturbation.label,
                perturbation_category=perturbation.category,
            ))

        return result


    # -- Private helpers ----------------------------------------------------

    def _log_event(self, event: DivSamplingEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
