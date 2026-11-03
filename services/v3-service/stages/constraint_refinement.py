"""V3 Constraint Refinement (Feature 3B) — Generate New Constraints from Failures.

Given failure analysis results (3A), generates refined constraint sets that
address the identified failure patterns. Each refined set includes ALL original
constraints plus new ones derived from failures. Enforces geometric distance
from prior failures to prevent hypothesis repetition.

Config: [constraint_refinement] in atlas.conf
Telemetry: telemetry/constraint_refinement_events.jsonl
"""

import json
import math
import re
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple

from .llm_client import strip_reasoning_leak

from .failure_analysis import FailureAnalysis


# Type alias for LLM callable
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]

# Type alias for embedding callable
EmbedCallable = Callable[[str], List[float]]


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class ConstraintRefinementConfig:
    """Configuration for Constraint Refinement."""
    enabled: bool = False
    num_hypotheses: int = 3
    min_cosine_distance: float = 0.15
    refinement_temperature: float = 0.5
    refinement_max_tokens: int = 2048


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class RefinedHypothesis:
    """A refined constraint set derived from failure analysis."""
    constraints: List[str] = field(default_factory=list)
    new_constraints: List[str] = field(default_factory=list)
    approach: str = ""
    rationale: str = ""
    embedding: List[float] = field(default_factory=list)

    def to_dict(self) -> Dict:
        return {
            "num_constraints": len(self.constraints),
            "num_new_constraints": len(self.new_constraints),
            "approach": self.approach[:200],
            "rationale": self.rationale[:200],
            "has_embedding": bool(self.embedding),
        }


@dataclass
class RefinementResult:
    """Result of constraint refinement."""
    hypotheses: List[RefinedHypothesis] = field(default_factory=list)
    filtered_count: int = 0
    total_generated: int = 0
    refinement_time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "num_hypotheses": len(self.hypotheses),
            "filtered_count": self.filtered_count,
            "total_generated": self.total_generated,
            "refinement_time_ms": self.refinement_time_ms,
        }


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class ConstraintRefinementEvent:
    """Telemetry event for a constraint refinement execution."""
    task_id: str
    num_hypotheses_generated: int = 0
    num_hypotheses_viable: int = 0
    num_filtered_by_distance: int = 0
    refinement_time_ms: float = 0.0
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_hypotheses_generated": self.num_hypotheses_generated,
            "num_hypotheses_viable": self.num_hypotheses_viable,
            "num_filtered_by_distance": self.num_filtered_by_distance,
            "refinement_time_ms": self.refinement_time_ms,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt template
# ---------------------------------------------------------------------------

REFINEMENT_PROMPT = """\
Problem: {problem}

Original constraints:
{original_constraints}

Failure analysis:
- Common pattern: {common_pattern}
- Violated constraints: {violated_constraints}
- Suggested new constraints: {new_constraints}

Generate {n} REFINED constraint sets. Each must:
1. Include ALL original constraints
2. Add at least 1 NEW constraint that specifically prevents the identified failure pattern
3. Suggest a different algorithmic approach than the failing solutions

For each constraint set, use this format:

HYPOTHESIS 1:
APPROACH: <the algorithmic approach>
RATIONALE: <why this avoids the failure>
CONSTRAINTS:
- <original constraint 1>
- <original constraint 2>
- NEW: <new constraint from failure analysis>"""


# ---------------------------------------------------------------------------
# Core functions
# ---------------------------------------------------------------------------

def cosine_distance(a: List[float], b: List[float]) -> float:
    """Compute cosine distance (1 - cosine_similarity) between two vectors.

    Returns value in [0, 2]. 0 = identical, 1 = orthogonal, 2 = opposite.
    """
    if not a or not b or len(a) != len(b):
        return 1.0  # Default to orthogonal for invalid inputs

    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(x * x for x in b))

    if norm_a == 0 or norm_b == 0:
        return 1.0

    similarity = dot / (norm_a * norm_b)
    # Clamp to [-1, 1] for numerical stability
    similarity = max(-1.0, min(1.0, similarity))
    return 1.0 - similarity


def parse_hypotheses(response: str,
                     original_constraints: List[str]) -> List[RefinedHypothesis]:
    """Parse refined hypotheses from LLM response.

    Looks for "HYPOTHESIS N:" headers with APPROACH, RATIONALE, CONSTRAINTS.
    """
    # Safety net: remove any leaked <think> reasoning before structured parsing
    # (model-agnostic, single source of truth in stages.llm_client).
    response = strip_reasoning_leak(response)
    hypotheses: List[RefinedHypothesis] = []

    # Split by HYPOTHESIS N:
    pattern = r'HYPOTHESIS\s+\d+\s*:(.*?)(?=HYPOTHESIS\s+\d+\s*:|$)'
    matches = re.findall(pattern, response, re.DOTALL | re.IGNORECASE)

    for block in matches:
        h = RefinedHypothesis()
        h.constraints = list(original_constraints)  # Start with originals

        # Parse APPROACH
        approach_match = re.search(r'APPROACH[:\s]*(.*?)(?=RATIONALE|CONSTRAINTS|$)',
                                    block, re.DOTALL | re.IGNORECASE)
        if approach_match:
            h.approach = approach_match.group(1).strip()

        # Parse RATIONALE
        rationale_match = re.search(r'RATIONALE[:\s]*(.*?)(?=CONSTRAINTS|$)',
                                     block, re.DOTALL | re.IGNORECASE)
        if rationale_match:
            h.rationale = rationale_match.group(1).strip()

        # Parse CONSTRAINTS
        constraints_match = re.search(r'CONSTRAINTS[:\s]*(.*?)$',
                                       block, re.DOTALL | re.IGNORECASE)
        if constraints_match:
            for line in constraints_match.group(1).strip().split('\n'):
                line = line.strip().lstrip('-*').strip()
                if not line:
                    continue
                if line.upper().startswith('NEW:'):
                    new_c = line[4:].strip()
                    if new_c:
                        h.new_constraints.append(new_c)
                        h.constraints.append(new_c)
                elif line and line not in h.constraints:
                    h.constraints.append(line)

        if h.approach or h.new_constraints:
            hypotheses.append(h)

    return hypotheses


def filter_by_distance(hypotheses: List[RefinedHypothesis],
                       failed_embeddings: List[List[float]],
                       min_distance: float = 0.15) -> List[RefinedHypothesis]:
    """Filter hypotheses by geometric distance from prior failures.

    Removes hypotheses whose embeddings are too close to any failed solution.
    """
    if not failed_embeddings:
        return hypotheses  # No failures to compare against

    viable = []
    for h in hypotheses:
        if not h.embedding:
            viable.append(h)  # No embedding, can't filter
            continue

        is_distant = all(
            cosine_distance(h.embedding, fe) > min_distance
            for fe in failed_embeddings
        )
        if is_distant:
            viable.append(h)

    return viable


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class ConstraintRefiner:
    """Constraint Refinement engine for generating improved constraint sets.

    When enabled, uses failure analysis to generate refined constraint sets
    that specifically address identified failure patterns.

    When disabled, returns an empty result (noop).

    Args:
        config: ConstraintRefinementConfig instance.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: ConstraintRefinementConfig,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "constraint_refinement_events.jsonl"

    def refine(self, problem: str,
               failure_analysis: FailureAnalysis,
               original_constraints: List[str],
               failed_embeddings: Optional[List[List[float]]] = None,
               llm_call: Optional[LLMCallable] = None,
               embed_call: Optional[EmbedCallable] = None,
               task_id: str = "") -> RefinementResult:
        """Generate refined constraint sets from failure analysis.

        Args:
            problem: Original problem description.
            failure_analysis: Analysis from 3A.
            original_constraints: Constraints from Phase 1.
            failed_embeddings: Embeddings of failed solutions.
            llm_call: LLM callable for hypothesis generation.
            embed_call: Embedding callable for distance checking.
            task_id: Task identifier for telemetry.

        Returns:
            RefinementResult with viable hypotheses.
        """
        if not self.config.enabled:
            return RefinementResult()

        if llm_call is None:
            return RefinementResult()

        start_time = time.time()

        # Build refinement prompt
        prompt = self._build_prompt(
            problem, failure_analysis, original_constraints,
        )

        response, tokens, gen_time = llm_call(
            prompt,
            self.config.refinement_temperature,
            self.config.refinement_max_tokens,
            None,  # no seed for diverse hypotheses
        )

        # Parse hypotheses
        hypotheses = parse_hypotheses(response, original_constraints)
        total_generated = len(hypotheses)

        # Compute embeddings for each hypothesis
        if embed_call is not None:
            for h in hypotheses:
                try:
                    h.embedding = embed_call(h.approach)
                except Exception:  # noqa: S110
                    # best-effort: swallow on failure (caller continues)
                    pass

        # Filter by distance from failures
        viable = filter_by_distance(
            hypotheses,
            failed_embeddings or [],
            self.config.min_cosine_distance,
        )
        filtered_count = total_generated - len(viable)

        elapsed = (time.time() - start_time) * 1000

        result = RefinementResult(
            hypotheses=viable,
            filtered_count=filtered_count,
            total_generated=total_generated,
            refinement_time_ms=elapsed,
        )

        # Log telemetry
        if task_id:
            self._log_event(ConstraintRefinementEvent(
                task_id=task_id,
                num_hypotheses_generated=total_generated,
                num_hypotheses_viable=len(viable),
                num_filtered_by_distance=filtered_count,
                refinement_time_ms=elapsed,
            ))

        return result

    # -- Helpers ------------------------------------------------------------

    def _build_prompt(self, problem: str,
                      failure_analysis: FailureAnalysis,
                      original_constraints: List[str]) -> str:
        """Build the refinement prompt."""
        original_text = '\n'.join(f"- {c}" for c in original_constraints) \
            if original_constraints else "(none)"
        violated_text = '\n'.join(f"- {c}" for c in failure_analysis.violated_constraints) \
            if failure_analysis.violated_constraints else "(none identified)"
        new_text = '\n'.join(f"- {c}" for c in failure_analysis.new_constraints) \
            if failure_analysis.new_constraints else "(none)"

        user_content = REFINEMENT_PROMPT.format(
            problem=problem,
            original_constraints=original_text,
            common_pattern=failure_analysis.common_pattern or "(none identified)",
            violated_constraints=violated_text,
            new_constraints=new_text,
            n=self.config.num_hypotheses,
        )

        system = ("You are a constraint engineering expert. Generate refined "
                   "constraint sets that prevent identified failure patterns.")
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _log_event(self, event: ConstraintRefinementEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
