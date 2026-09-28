"""V3 PR-CoT (Feature 3C) — Multi-Perspective Repair.

When all candidates fail, analyzes the best candidate from 4 perspectives
and generates repair attempts informed by each perspective. Runs BEFORE
the full refinement loop as a lightweight repair attempt.

Paper: Soarez et al. (arxiv:2601.07780, Jan 2026).
Config: [pr_cot] in atlas.conf
Telemetry: telemetry/pr_cot_events.jsonl

Perspectives:
  - logical_consistency: Check for logic errors
  - information_completeness: Check for missing cases
  - biases: Check for unstated assumptions
  - alternative_solutions: Consider different approaches
"""

import json
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple

from .llm_client import extract_code_for_problem, strip_reasoning_leak


# Type alias for LLM callable
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class PRCoTConfig:
    """Configuration for PR-CoT multi-perspective repair."""
    enabled: bool = False
    max_repair_rounds: int = 3
    analysis_temperature: float = 0.3
    repair_temperature: float = 0.4
    analysis_max_tokens: int = 2048
    repair_max_tokens: int = 4096


# ---------------------------------------------------------------------------
# Perspectives
# ---------------------------------------------------------------------------

PERSPECTIVES: Dict[str, str] = {
    "logical_consistency":
        "Check this code for logical errors: incorrect loop bounds, wrong conditionals, "
        "off-by-one errors, incorrect operator usage.",

    "information_completeness":
        "Does this code handle ALL cases mentioned in the problem? Check for: "
        "missing edge cases, unhandled input ranges, ignored constraints.",

    "biases":
        "Is this code making assumptions not stated in the problem? Check for: "
        "assumed input ordering, assumed positive numbers, assumed connected graphs.",

    "alternative_solutions":
        "Is there a fundamentally different algorithmic approach? Consider: "
        "different data structures, different traversal orders, mathematical shortcuts.",
}


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class PerspectiveResult:
    """Result of analyzing from one perspective."""
    perspective: str
    analysis: str
    repair_code: str = ""
    tokens_used: int = 0
    time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "perspective": self.perspective,
            "analysis_length": len(self.analysis),
            "has_repair": bool(self.repair_code),
            "tokens_used": self.tokens_used,
            "time_ms": self.time_ms,
        }


@dataclass
class PRCoTResult:
    """Complete result of PR-CoT repair attempt."""
    task_id: str = ""
    perspectives: List[PerspectiveResult] = field(default_factory=list)
    repairs: List[str] = field(default_factory=list)
    total_tokens: int = 0
    total_time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_perspectives": len(self.perspectives),
            "num_repairs": len(self.repairs),
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
            "perspective_details": [p.to_dict() for p in self.perspectives],
        }


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class PRCoTEvent:
    """Telemetry event for a PR-CoT repair attempt."""
    task_id: str
    num_perspectives: int = 0
    num_repairs: int = 0
    perspectives_used: List[str] = field(default_factory=list)
    total_tokens: int = 0
    total_time_ms: float = 0.0
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_perspectives": self.num_perspectives,
            "num_repairs": self.num_repairs,
            "perspectives_used": self.perspectives_used,
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt templates
# ---------------------------------------------------------------------------

ANALYSIS_PROMPT = """\
Problem: {problem}

Code that FAILED:
```python
{code}
```

Error output: {error}

Analyze from this perspective: {perspective}

Be specific about what's wrong. Point to exact lines or logic issues."""

REPAIR_PROMPT = """\
Problem: {problem}

Analysis of the failing code:
{analysis}

Original failing code:
```python
{code}
```

Treat every explicitly requested public name, exact signature, entry point, and input/output behavior as a hard contract.
Preserve existing public interfaces unless the problem explicitly requires changing them.
Copy parameter order and kinds (including / and * markers), defaults, and type annotations exactly.
Implement behavioral contracts such as laziness, stopping conditions, ordering, and error behavior literally.
Think step by step about what needs to change, then write one complete, syntactically valid fixed Python source file; never import that file or requested artifact from itself. Before answering, mentally compile it and verify every requested declaration and contract is implemented."""


# ---------------------------------------------------------------------------
# Core functions
# ---------------------------------------------------------------------------

def extract_code_from_repair(response: str) -> str:
    """Extract Python code from a repair response."""
    import re

    # Safety net: client returns clean content, but strip any leaked
    # reasoning block model-agnostically before code extraction.
    response = strip_reasoning_leak(response)

    # The block comes back verbatim: the fence is framing, the bytes inside
    # it are the artifact, final newline and trailing blank lines included.
    # The candidate hash downstream is computed from exactly this value.
    # Try ```python blocks
    py_blocks = re.findall(r'```python\s*\n(.*?)```', response, re.DOTALL)
    if py_blocks:
        return py_blocks[-1]

    # Try plain ``` blocks
    code_blocks = re.findall(r'```\s*\n(.*?)```', response, re.DOTALL)
    if code_blocks:
        return code_blocks[-1]

    return response.lstrip()


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class PRCoT:
    """PR-CoT multi-perspective repair controller.

    When enabled, analyzes a failing candidate from 4 perspectives and
    generates repair attempts based on each perspective's findings.

    When disabled, returns an empty result (noop).

    Args:
        config: PRCoTConfig instance.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: PRCoTConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "pr_cot_events.jsonl"

    @property
    def perspectives(self) -> Dict[str, str]:
        """The available analysis perspectives."""
        return dict(PERSPECTIVES)

    @property
    def num_perspectives(self) -> int:
        return len(PERSPECTIVES)

    def repair(self, problem: str,
               code: str,
               error: str,
               llm_call: Optional[LLMCallable] = None,
               task_id: str = "") -> PRCoTResult:
        """Attempt multi-perspective repair on a failing candidate.

        Args:
            problem: Original problem description.
            code: The failing code to repair.
            error: The error output from sandbox execution.
            llm_call: LLM callable for analysis and repair generation.
            task_id: Task identifier for telemetry.

        Returns:
            PRCoTResult with perspective analyses and repair candidates.
        """
        if not self.config.enabled:
            return PRCoTResult(task_id=task_id)

        if llm_call is None:
            return PRCoTResult(task_id=task_id)

        start_time = time.time()
        result = PRCoTResult(task_id=task_id)
        total_tokens = 0

        for name, perspective_desc in PERSPECTIVES.items():
            if len(result.repairs) >= self.config.max_repair_rounds:
                break

            perspective_start = time.time()

            # Phase 1: Analyze from this perspective
            analysis_prompt = self._build_analysis_prompt(
                problem, code, error, perspective_desc
            )
            analysis_response, analysis_tokens, _ = llm_call(
                analysis_prompt,
                self.config.analysis_temperature,
                self.config.analysis_max_tokens,
                42,
            )
            total_tokens += analysis_tokens

            # Phase 2: Generate repair based on analysis
            repair_prompt = self._build_repair_prompt(
                problem, code, analysis_response
            )
            repair_response, repair_tokens, _ = llm_call(
                repair_prompt,
                self.config.repair_temperature,
                self.config.repair_max_tokens,
                None,  # no seed for diverse repairs
            )
            total_tokens += repair_tokens

            repair_code = extract_code_for_problem(
                repair_response, problem, fallback="last"
            )
            perspective_time = (time.time() - perspective_start) * 1000

            perspective_result = PerspectiveResult(
                perspective=name,
                analysis=analysis_response,
                repair_code=repair_code,
                tokens_used=analysis_tokens + repair_tokens,
                time_ms=perspective_time,
            )
            result.perspectives.append(perspective_result)

            if repair_code and repair_code != code:
                result.repairs.append(repair_code)

        total_time = (time.time() - start_time) * 1000
        result.total_tokens = total_tokens
        result.total_time_ms = total_time

        # Log telemetry
        if task_id:
            self._log_event(PRCoTEvent(
                task_id=task_id,
                num_perspectives=len(result.perspectives),
                num_repairs=len(result.repairs),
                perspectives_used=[p.perspective for p in result.perspectives],
                total_tokens=total_tokens,
                total_time_ms=total_time,
            ))

        return result

    # -- Helpers ------------------------------------------------------------

    def _build_analysis_prompt(self, problem: str, code: str,
                                error: str, perspective: str) -> str:
        user_content = ANALYSIS_PROMPT.format(
            problem=problem, code=code,
            error=error, perspective=perspective,
        )
        system = "You are an expert code reviewer. Analyze code failures precisely. Identify the root cause, not just symptoms."
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _build_repair_prompt(self, problem: str, code: str,
                              analysis: str) -> str:
        user_content = REPAIR_PROMPT.format(
            problem=problem, code=code, analysis=analysis,
        )
        system = "You are an expert programmer. Think carefully about the root cause, then write a complete corrected solution."
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _log_event(self, event: PRCoTEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
