"""V3 Failure Analysis (Feature 3A) — Why Did Candidates Fail?

Analyzes failing candidates to extract common failure patterns, categorize
failure types, and identify violated constraints. The structured analysis
feeds into constraint refinement (3B) and the full refinement loop (3E).

Config: [failure_analysis] in atlas.conf
Telemetry: telemetry/failure_analysis_events.jsonl

Failure Categories:
  - wrong_algorithm: Fundamentally wrong approach for problem constraints
  - implementation_bug: Correct approach but coding error
  - edge_case_miss: Fails on specific inputs
  - time_limit: Correct but too slow
  - format_error: Wrong output format
  - partial_correct: Passes most tests but fails specific subset
"""

import json
import re
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple


# Type alias for LLM callable
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]

# Type alias for embedding callable
# Signature: (text) -> List[float]
EmbedCallable = Callable[[str], List[float]]


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class FailureAnalysisConfig:
    """Configuration for Failure Analysis engine."""
    enabled: bool = False
    max_candidates_to_analyze: int = 5
    analysis_temperature: float = 0.3
    analysis_max_tokens: int = 2048


# ---------------------------------------------------------------------------
# Failure categories
# ---------------------------------------------------------------------------

FAILURE_CATEGORIES: Dict[str, str] = {
    "wrong_algorithm": "Solution uses fundamentally wrong approach for the problem constraints",
    "implementation_bug": "Correct approach but coding error (off-by-one, wrong operator, etc.)",
    "edge_case_miss": "Fails on specific inputs (empty, single element, large N, negative, etc.)",
    "time_limit": "Correct but too slow for the given constraints",
    "format_error": "Wrong output format, missing newlines, extra whitespace",
    "partial_correct": "Passes most test cases but fails on specific subset",
}


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class FailingCandidate:
    """A candidate that failed with its error information."""
    code: str
    error_output: str
    index: int = 0

    def to_dict(self) -> Dict:
        return {
            "index": self.index,
            "code_length": len(self.code),
            "error_preview": self.error_output[:200],
        }


@dataclass
class FailureAnalysis:
    """Structured result of failure analysis."""
    categories: Dict[int, str] = field(default_factory=dict)
    violated_constraints: List[str] = field(default_factory=list)
    common_pattern: str = ""
    new_constraints: List[str] = field(default_factory=list)
    failure_embeddings: List[List[float]] = field(default_factory=list)
    raw_analysis: str = ""
    analysis_time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "categories": self.categories,
            "violated_constraints": self.violated_constraints,
            "common_pattern": self.common_pattern,
            "new_constraints": self.new_constraints,
            "num_embeddings": len(self.failure_embeddings),
            "analysis_time_ms": self.analysis_time_ms,
        }


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class FailureAnalysisEvent:
    """Telemetry event for a failure analysis execution."""
    task_id: str
    num_candidates: int = 0
    categories_found: List[str] = field(default_factory=list)
    num_violated_constraints: int = 0
    num_new_constraints: int = 0
    analysis_time_ms: float = 0.0
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_candidates": self.num_candidates,
            "categories_found": self.categories_found,
            "num_violated_constraints": self.num_violated_constraints,
            "num_new_constraints": self.num_new_constraints,
            "analysis_time_ms": self.analysis_time_ms,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt template
# ---------------------------------------------------------------------------

FAILURE_ANALYSIS_PROMPT = """\
Problem: {problem}

Original constraints identified:
{constraints}

Failing solutions and their errors:
{candidates_with_errors}

Analyze these failures:
1. CATEGORY: What failure category does each solution fall into?
   Categories: {category_names}

2. VIOLATED: Which original constraints did each solution VIOLATE?

3. COMMON: What do ALL failures have in common? What assumption are they all making?

4. NEW_CONSTRAINTS: What NEW constraint(s) should be added that would prevent these failures?

Be specific and concrete. Don't say "edge cases" — say WHICH edge cases."""


# ---------------------------------------------------------------------------
# Parsing helpers
# ---------------------------------------------------------------------------

def parse_failure_categories(response: str,
                             num_candidates: int) -> Dict[int, str]:
    """Parse failure categories from analysis response.

    Looks for category assignments per candidate. Returns dict mapping
    candidate index to category name.
    """
    categories: Dict[int, str] = {}
    valid_cats = set(FAILURE_CATEGORIES.keys())

    # Look for "Solution N: category" or "Candidate N: category" patterns
    patterns = [
        r'(?:solution|candidate)\s*(\d+)\s*[:\-]\s*(\w+)',
        r'(\d+)\s*[.)]\s*(\w+)',
    ]

    for pat in patterns:
        for match in re.finditer(pat, response, re.IGNORECASE):
            try:
                idx = int(match.group(1)) - 1  # 1-indexed to 0-indexed
                cat = match.group(2).lower().strip()
                if cat in valid_cats and 0 <= idx < num_candidates:
                    categories[idx] = cat
            except (ValueError, IndexError):
                continue

    # If no structured matches, try to find any category mention
    if not categories:
        for cat in valid_cats:
            if cat in response.lower():
                for i in range(num_candidates):
                    if i not in categories:
                        categories[i] = cat
                        break

    return categories


def parse_violated_constraints(response: str) -> List[str]:
    """Parse violated constraints from analysis response."""
    violated: List[str] = []

    # Look for VIOLATED section
    violated_section = re.search(
        r'VIOLATED[:\s]*(.*?)(?=COMMON|NEW_CONSTRAINT|$)',
        response, re.DOTALL | re.IGNORECASE
    )
    if violated_section:
        text = violated_section.group(1)
        for line in text.strip().split('\n'):
            line = line.strip().lstrip('-*').strip()
            if line and len(line) > 5:
                violated.append(line)

    return violated


def parse_common_pattern(response: str) -> str:
    """Parse common failure pattern from analysis response."""
    common_section = re.search(
        r'COMMON[:\s]*(.*?)(?=NEW_CONSTRAINT|$)',
        response, re.DOTALL | re.IGNORECASE
    )
    if common_section:
        text = common_section.group(1).strip()
        # Take first meaningful paragraph
        lines = [l.strip() for l in text.split('\n') if l.strip()]
        if lines:
            return ' '.join(lines[:3])
    return ""


def parse_new_constraints(response: str) -> List[str]:
    """Parse new constraint suggestions from analysis response."""
    constraints: List[str] = []

    new_section = re.search(
        r'NEW_CONSTRAINT[S]?[:\s]*(.*?)$',
        response, re.DOTALL | re.IGNORECASE
    )
    if new_section:
        text = new_section.group(1)
        for line in text.strip().split('\n'):
            line = line.strip().lstrip('-*0123456789.)').strip()
            if line and len(line) > 10:
                constraints.append(line)

    return constraints


def format_candidates_with_errors(candidates: List[FailingCandidate]) -> str:
    """Format failing candidates with their errors for the prompt."""
    parts = []
    for i, c in enumerate(candidates):
        parts.append(f"Solution {i + 1}:\n```python\n{c.code}\n```\nError: {c.error_output}")
    return "\n\n".join(parts)


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class FailureAnalyzer:
    """Failure Analysis engine for identifying why candidates failed.

    When enabled, analyzes failing candidates to extract failure patterns,
    violated constraints, and suggests new constraints to prevent failures.

    When disabled, returns an empty analysis (noop).

    Args:
        config: FailureAnalysisConfig instance.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: FailureAnalysisConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "failure_analysis_events.jsonl"

    def analyze(self, problem: str,
                candidates: List[FailingCandidate],
                original_constraints: List[str],
                llm_call: Optional[LLMCallable] = None,
                embed_call: Optional[EmbedCallable] = None,
                task_id: str = "") -> FailureAnalysis:
        """Analyze why all candidates failed.

        Args:
            problem: Original problem description.
            candidates: List of failing candidates with errors.
            original_constraints: Constraints from Phase 1.
            llm_call: LLM callable for analysis generation.
            embed_call: Embedding callable for failure embeddings.
            task_id: Task identifier for telemetry.

        Returns:
            FailureAnalysis with categories, violated constraints, etc.
        """
        if not self.config.enabled:
            return FailureAnalysis()

        if not candidates:
            return FailureAnalysis()

        start_time = time.time()

        # Limit candidates analyzed
        to_analyze = candidates[:self.config.max_candidates_to_analyze]

        # Build analysis prompt
        analysis = FailureAnalysis()

        if llm_call is not None:
            prompt = self._build_prompt(problem, to_analyze, original_constraints)
            response, tokens, gen_time = llm_call(
                prompt,
                self.config.analysis_temperature,
                self.config.analysis_max_tokens,
                42,  # fixed seed for reproducibility
            )
            analysis.raw_analysis = response

            # Parse structured output
            analysis.categories = parse_failure_categories(
                response, len(to_analyze)
            )
            analysis.violated_constraints = parse_violated_constraints(response)
            analysis.common_pattern = parse_common_pattern(response)
            analysis.new_constraints = parse_new_constraints(response)

        # Compute failure embeddings if embed callable provided
        if embed_call is not None:
            for c in to_analyze:
                try:
                    emb = embed_call(c.code)
                    analysis.failure_embeddings.append(emb)
                except Exception:  # noqa: S110
                    # best-effort: swallow on failure (caller continues)
                    pass

        elapsed = (time.time() - start_time) * 1000
        analysis.analysis_time_ms = elapsed

        # Log telemetry
        if task_id:
            cats = list(set(analysis.categories.values()))
            self._log_event(FailureAnalysisEvent(
                task_id=task_id,
                num_candidates=len(to_analyze),
                categories_found=cats,
                num_violated_constraints=len(analysis.violated_constraints),
                num_new_constraints=len(analysis.new_constraints),
                analysis_time_ms=elapsed,
            ))

        return analysis

    def get_category_descriptions(self) -> Dict[str, str]:
        """Return failure category descriptions."""
        return dict(FAILURE_CATEGORIES)

    # -- Helpers ------------------------------------------------------------

    def _build_prompt(self, problem: str,
                      candidates: List[FailingCandidate],
                      original_constraints: List[str]) -> str:
        """Build the failure analysis prompt."""
        constraints_text = '\n'.join(
            f"- {c}" for c in original_constraints
        ) if original_constraints else "(none identified)"

        user_content = FAILURE_ANALYSIS_PROMPT.format(
            problem=problem,
            constraints=constraints_text,
            candidates_with_errors=format_candidates_with_errors(candidates),
            category_names=', '.join(FAILURE_CATEGORIES.keys()),
        )

        system = ("You are an expert debugger. Analyze code failures precisely. "
                   "Be specific about what went wrong and why.")
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _log_event(self, event: FailureAnalysisEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
