"""V3 Refinement Loop (Feature 3E) — Iterative Constraint Refinement.

Orchestrates the full observe→hypothesize→validate→generate→test→learn cycle.
When Phase 1+2 candidates all fail, the loop:
  1. Analyzes failures (3A)
  2. Generates refined constraints (3B)
  3. Generates code from best hypothesis
  4. Tests in sandbox
  5. Learns from the result (fail → iterate)

When the loop exhausts its iterations unsolved, the caller (pipeline
orchestrator / bench runner) moves on to its next repair strategy.

Config: [refinement_loop] in atlas.conf
Telemetry: telemetry/refinement_loop_events.jsonl
"""

import json
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple

from .llm_client import extract_code_for_problem, strip_reasoning_leak

from .failure_analysis import (
    FailingCandidate,
    FailureAnalyzer,
    FailureAnalysisConfig,
)
from .constraint_refinement import (
    ConstraintRefiner,
    ConstraintRefinementConfig,
    RefinedHypothesis,
)


# Type alias for LLM callable
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]

# Type alias for sandbox execution
# Signature: (code, test_input) -> (passed, stdout, stderr)
SandboxCallable = Callable[[str, str], Tuple[bool, str, str]]

# Type alias for embedding callable
EmbedCallable = Callable[[str], List[float]]

# Type alias for code generation callable
# Signature: (problem, constraints, seed) -> (code, tokens, time_ms)
CodeGenCallable = Callable[[str, List[str], int], Tuple[str, int, float]]


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class RefinementLoopConfig:
    """Configuration for the Refinement Loop."""
    enabled: bool = False
    max_iterations: int = 2
    max_time_ms: float = 120000.0


# ---------------------------------------------------------------------------
# Orchestrator-side budget gate
# ---------------------------------------------------------------------------
# H200 join: 453/487 refinement entries exhausted their budget with ZERO
# completed iterations while burning ~6 minutes each — the loop was entered
# when it could never afford a single iteration at the observed serving
# speed. Both orchestrators gate on this BEFORE entering the loop and skip
# straight to their fallback when one iteration cannot fit.

# One iteration is ~3 sequential LLM calls: failure analysis, constraint
# refinement, code generation (the sandbox run is noise next to them).
ITERATION_LLM_CALLS = 3

# Conservative floor when no per-call latency has been observed yet. 120s
# matches RefinementLoopConfig.max_time_ms — a remaining budget below the
# loop's own per-run allowance cannot complete an iteration.
MIN_ITERATION_MS = 120_000.0


def estimate_iteration_ms(observed_llm_call_ms: float) -> float:
    """Estimated wall-clock cost of ONE refinement iteration.

    ``observed_llm_call_ms`` is the average per-call latency the caller has
    measured on this task so far (0 / falsy when nothing was observed —
    the conservative ``MIN_ITERATION_MS`` floor applies then).
    """
    if observed_llm_call_ms and observed_llm_call_ms > 0:
        return ITERATION_LLM_CALLS * observed_llm_call_ms
    return MIN_ITERATION_MS


def can_afford_iteration(remaining_ms: float,
                         estimated_iteration_ms: float) -> bool:
    """Budget gate: enter the loop only when the remaining wall-clock
    fits at least one full iteration."""
    return remaining_ms >= estimated_iteration_ms


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class IterationResult:
    """Result of a single refinement iteration."""
    iteration: int = 0
    hypothesis_used: Optional[RefinedHypothesis] = None
    code_generated: str = ""
    passed: bool = False
    error: str = ""
    tokens_used: int = 0
    time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "iteration": self.iteration,
            "has_hypothesis": self.hypothesis_used is not None,
            "passed": self.passed,
            "error_preview": self.error[:200] if self.error else "",
            "tokens_used": self.tokens_used,
            "time_ms": self.time_ms,
        }


@dataclass
class RefinementResult:
    """Complete result of the refinement loop."""
    task_id: str = ""
    triggered: bool = False
    solved: bool = False
    winning_code: str = ""
    iterations: List[IterationResult] = field(default_factory=list)
    total_iterations: int = 0
    total_tokens: int = 0
    total_time_ms: float = 0.0
    reason: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "triggered": self.triggered,
            "solved": self.solved,
            "total_iterations": self.total_iterations,
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
            "reason": self.reason,
            "iterations": [it.to_dict() for it in self.iterations],
        }


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class RefinementLoopEvent:
    """Telemetry event for a refinement loop execution."""
    task_id: str
    triggered: bool = False
    solved: bool = False
    total_iterations: int = 0
    total_tokens: int = 0
    total_time_ms: float = 0.0
    reason: str = ""
    timestamp: str = ""

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "triggered": self.triggered,
            "solved": self.solved,
            "total_iterations": self.total_iterations,
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
            "reason": self.reason,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class RefinementLoop:
    """Iterative constraint refinement loop.

    When enabled, orchestrates the failure analysis → constraint refinement
    → code generation → sandbox testing → learning cycle. Iterates up to
    max_iterations times or until a solution passes.

    When disabled, does nothing (noop).

    Args:
        config: RefinementLoopConfig instance.
        failure_analyzer: FailureAnalyzer instance.
        constraint_refiner: ConstraintRefiner instance.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: RefinementLoopConfig,
                 failure_analyzer: Optional[FailureAnalyzer] = None,
                 constraint_refiner: Optional[ConstraintRefiner] = None,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.failure_analyzer = failure_analyzer or FailureAnalyzer(
            FailureAnalysisConfig(enabled=config.enabled)
        )
        self.constraint_refiner = constraint_refiner or ConstraintRefiner(
            ConstraintRefinementConfig(enabled=config.enabled)
        )
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "refinement_loop_events.jsonl"

    def run(self, problem: str,
            failing_candidates: List[FailingCandidate],
            original_constraints: List[str],
            llm_call: Optional[LLMCallable] = None,
            sandbox_run: Optional[SandboxCallable] = None,
            embed_call: Optional[EmbedCallable] = None,
            code_gen: Optional[CodeGenCallable] = None,
            task_id: str = "") -> RefinementResult:
        """Execute the refinement loop.

        Args:
            problem: Original problem description.
            failing_candidates: Candidates that failed Phase 1+2.
            original_constraints: Constraints from Phase 1.
            llm_call: LLM callable for analysis and refinement.
            sandbox_run: Sandbox callable for testing.
            embed_call: Embedding callable for distance checking.
            code_gen: Code generation callable.
            task_id: Task identifier for telemetry.

        Returns:
            RefinementResult with iteration details and outcome.
        """
        if not self.config.enabled:
            return RefinementResult(task_id=task_id, reason="disabled")

        if not failing_candidates:
            return RefinementResult(task_id=task_id, reason="no_candidates")

        if llm_call is None or sandbox_run is None:
            return RefinementResult(task_id=task_id, reason="missing_callables")

        start_time = time.time()
        result = RefinementResult(task_id=task_id, triggered=True)
        total_tokens = 0

        # Track all failed embeddings for distance checking
        all_failed_embeddings: List[List[float]] = []
        if embed_call is not None:
            for c in failing_candidates:
                try:
                    emb = embed_call(c.code)
                    all_failed_embeddings.append(emb)
                except Exception:
                    # best-effort: swallow on failure (caller continues)
                    pass

        # Current failure set (mutable across iterations)
        current_failures = list(failing_candidates)

        for iteration in range(self.config.max_iterations):
            iter_start = time.time()

            # Check time budget
            elapsed_so_far = (time.time() - start_time) * 1000
            if elapsed_so_far > self.config.max_time_ms:
                result.reason = "time_budget_exceeded"
                break

            # Step 1: Analyze failures
            analysis = self.failure_analyzer.analyze(
                problem, current_failures, original_constraints,
                llm_call, embed_call, task_id=task_id,
            )

            # Step 2: Refine constraints
            refinement = self.constraint_refiner.refine(
                problem, analysis, original_constraints,
                all_failed_embeddings,
                llm_call, embed_call, task_id=task_id,
            )

            if not refinement.hypotheses:
                iter_result = IterationResult(
                    iteration=iteration,
                    passed=False,
                    error="no_viable_hypotheses",
                    time_ms=(time.time() - iter_start) * 1000,
                )
                result.iterations.append(iter_result)
                continue

            # Step 3: Generate code from best hypothesis
            best = refinement.hypotheses[0]
            code = ""
            gen_tokens = 0

            if code_gen is not None:
                code, gen_tokens, _ = code_gen(
                    problem, best.constraints, 42 + iteration
                )
            elif llm_call is not None:
                # Fallback: use LLM directly
                code_prompt = self._build_code_prompt(
                    problem, best.constraints, best.approach
                )
                code_response, gen_tokens, _ = llm_call(
                    code_prompt, 0.2, 4096, 42 + iteration
                )
                code = extract_code_for_problem(
                    code_response, problem, fallback="last"
                )

            total_tokens += gen_tokens

            # Step 4: Test in sandbox
            passed, stdout, stderr = sandbox_run(code, "")
            error_output = stderr if not passed else ""

            iter_time = (time.time() - iter_start) * 1000
            iter_result = IterationResult(
                iteration=iteration,
                hypothesis_used=best,
                code_generated=code,
                passed=passed,
                error=error_output,
                tokens_used=gen_tokens,
                time_ms=iter_time,
            )
            result.iterations.append(iter_result)

            if passed:
                # Success!
                result.solved = True
                result.winning_code = code
                result.reason = "solved"
                break
            else:
                # Add to failure set
                current_failures.append(FailingCandidate(
                    code=code, error_output=error_output,
                    index=len(current_failures),
                ))
                if embed_call is not None:
                    try:
                        emb = embed_call(code)
                        all_failed_embeddings.append(emb)
                    except Exception:
                        # best-effort: swallow on failure (caller continues)
                        pass

        if not result.solved and not result.reason:
            result.reason = "max_iterations_exhausted"

        total_time = (time.time() - start_time) * 1000
        result.total_iterations = len(result.iterations)
        result.total_tokens = total_tokens
        result.total_time_ms = total_time

        # Log telemetry
        if task_id:
            self._log_event(RefinementLoopEvent(
                task_id=task_id,
                triggered=True,
                solved=result.solved,
                total_iterations=result.total_iterations,
                total_tokens=total_tokens,
                total_time_ms=total_time,
                reason=result.reason,
            ))

        return result

    # -- Helpers ------------------------------------------------------------

    def _build_code_prompt(self, problem: str,
                           constraints: List[str],
                           approach: str) -> str:
        """Build a code generation prompt from refined constraints."""
        constraints_text = '\n'.join(f"- {c}" for c in constraints)
        user_content = (
            f"Implement this solution as Python code:\n\n"
            f"Problem: {problem}\n\n"
            f"Approach: {approach}\n\n"
            f"These constraints MUST be satisfied:\n{constraints_text}\n\n"
            f"Treat every explicitly requested public name, exact signature, "
            f"entry point, and input/output behavior as a hard contract.\n"
            f"Preserve existing public interfaces unless the problem explicitly "
            f"requires changing them.\n"
            f"Copy parameter order and kinds (including / and * markers), defaults, "
            f"and type annotations exactly.\n"
            f"Implement behavioral contracts such as laziness, stopping conditions, "
            f"ordering, and error behavior literally.\n"
            f"Write one complete, syntactically valid Python source file only; "
            f"never import that file or requested artifact from itself. Before "
            f"answering, mentally compile it and verify every requested declaration "
            f"and contract is implemented."
        )
        system = "You are an expert programmer. Think through the approach carefully, then write correct code."
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _extract_code(self, response: str) -> str:
        """Extract code from LLM response."""
        import re
        # Safety net: client returns clean content, but strip any leaked
        # reasoning block model-agnostically before code extraction.
        response = strip_reasoning_leak(response)
        py_blocks = re.findall(r'```python\s*\n(.*?)```', response, re.DOTALL)
        if py_blocks:
            return py_blocks[-1].strip()
        code_blocks = re.findall(r'```\s*\n(.*?)```', response, re.DOTALL)
        if code_blocks:
            return code_blocks[-1].strip()
        return response.strip()

    def _log_event(self, event: RefinementLoopEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
