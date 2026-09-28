"""V3 PlanSearch (Feature 1A) — Constraint-Based Plan Generation.

Generates candidates representing fundamentally different algorithmic approaches
through constraint-based narrowing, not just temperature diversity.

Paper: Wang et al., ICLR 2025 Spotlight (arxiv:2409.03733)
Config: [plan_search] in atlas.conf
Telemetry: telemetry/plan_search_events.jsonl

Pipeline:
  Step 1: Extract N distinct constraint sets from the problem
  Step 2: Construct a solution plan per constraint set (thinking ON)
  Step 3: Generate code per plan (thinking OFF or budget-controlled)

Each constraint set narrows the solution space. Three constraints eliminating
70% each leave 0.3^3 = 2.7% of the original space — the model generates
within this massively narrowed space rather than searching blindly.
"""

import json
import re
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Dict, List, Optional, Tuple

from .llm_client import extract_code_for_problem, strip_reasoning_leak

from .budget_forcing import BudgetForcing, get_system_prompt


# ---------------------------------------------------------------------------
# Type alias for the LLM callable
# ---------------------------------------------------------------------------
# Signature: (chatml_prompt, temperature, max_tokens, seed) -> (text, tokens, time_ms)
LLMCallable = Callable[[str, float, int, Optional[int]], Tuple[str, int, float]]


# ---------------------------------------------------------------------------
# Request-context propagation and failure classification
# ---------------------------------------------------------------------------
# PlanSearch is the one V3 stage that dispatches inference from worker
# threads. Threads do not inherit ContextVars, so anything request-scoped
# that PlanSearch needs has to be handed to the worker explicitly. Two kinds
# of request state are involved and they travel differently:
#
#   * the outbound call's identity  -> on the request-scoped LLM adapter,
#     read by LLMAdapter._inference_headers with no ContextVar fallback;
#   * the log-correlation ID        -> captured here on the owning thread and
#     re-established in the worker, below.
#
# Measured 2026-08-23 on a 42-case acquisition: every one of 14 PlanSearch
# invocations returned 0 candidates because worker calls arrived without
# X-ATLAS-Request-ID and an attribution-enforcing upstream refused all 28 of
# them with HTTP 403. Nothing raised — the batch degraded to an empty
# candidate list and DivSampling filled the slots, so the stage read as
# alive and unproductive rather than as never having reached the model.

# HTTP statuses that mean the request never reached the model because it was
# not authorized to. Not retried away and not tolerated per-item: they say
# nothing about the problem being solved.
_AUTH_STATUSES = frozenset({401, 403})


def _current_identity():
    """(request_id, invocation_id) captured on the owning request thread."""
    try:
        from structured_log import current_identity
        return current_identity()
    except ImportError:
        return "", ""


def _bind_identity(identity) -> None:
    """Re-establish the owner's identity inside a worker thread."""
    try:
        from structured_log import bind_identity
        bind_identity(identity[0], identity[1])
    except ImportError:
        pass


class PlanSearchInfrastructureError(RuntimeError):
    """PlanSearch could not reach the model, as distinct from failing at it.

    Carries the partial result so the caller can still book the tokens the
    stage did spend — the cost ledger stays exact whether or not the stage
    produced anything.
    """

    def __init__(self, message: str, failures=(), result=None):
        super().__init__(message)
        self.failures = list(failures)
        self.result = result


def _is_infrastructure_failure(exc: BaseException) -> bool:
    """True when the call was refused or never wired, not answered badly."""
    # Imported here: adapters imports stages.llm_client, so a module-level
    # import back into adapters would close the cycle.
    try:
        from adapters import RequestIdentityMissing
        if isinstance(exc, RequestIdentityMissing):
            return True
    except ImportError:
        pass
    code = getattr(exc, "code", None)
    return isinstance(code, int) and code in _AUTH_STATUSES


def _raise_if_infrastructure(failures) -> None:
    if not failures:
        return
    idxs = ", ".join(str(i) for i, _ in failures)
    raise PlanSearchInfrastructureError(
        f"PlanSearch could not reach the model for item(s) {idxs}: "
        f"{failures[0][1]}", failures=failures)


# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

@dataclass
class PlanSearchConfig:
    """Configuration for PlanSearch constraint generation."""
    enabled: bool = False
    num_plans: int = 3
    max_plans: int = 7
    step1_temperature: float = 0.7
    step2_temperature: float = 0.4
    step3_temperature: float = 0.2


# ---------------------------------------------------------------------------
# Data structures
# ---------------------------------------------------------------------------

@dataclass
class ConstraintSet:
    """A set of constraints extracted from a problem."""
    constraints: List[str]
    algorithmic_family: str = ""

    def to_dict(self) -> Dict:
        return {
            "constraints": self.constraints,
            "algorithmic_family": self.algorithmic_family,
        }


@dataclass
class Plan:
    """A solution plan derived from a constraint set."""
    constraint_set: ConstraintSet
    approach: str = ""
    data_structures: str = ""
    steps: List[str] = field(default_factory=list)
    edge_cases: List[str] = field(default_factory=list)

    def to_dict(self) -> Dict:
        return {
            "constraint_set": self.constraint_set.to_dict(),
            "approach": self.approach,
            "data_structures": self.data_structures,
            "steps": self.steps,
            "edge_cases": self.edge_cases,
        }


@dataclass
class PlanSearchResult:
    """Complete result of a PlanSearch pipeline execution."""
    task_id: str
    constraint_sets: List[ConstraintSet] = field(default_factory=list)
    plans: List[Plan] = field(default_factory=list)
    candidates: List[str] = field(default_factory=list)
    raw_responses: Dict[str, List[str]] = field(default_factory=dict)
    total_tokens: int = 0
    total_time_ms: float = 0.0

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "constraint_sets": [cs.to_dict() for cs in self.constraint_sets],
            "plans": [p.to_dict() for p in self.plans],
            "num_candidates": len(self.candidates),
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
        }


# ---------------------------------------------------------------------------
# Telemetry event
# ---------------------------------------------------------------------------

@dataclass
class PlanSearchEvent:
    """Telemetry event for a PlanSearch execution."""
    task_id: str
    num_constraint_sets: int = 0
    num_plans: int = 0
    num_candidates: int = 0
    total_tokens: int = 0
    total_time_ms: float = 0.0
    step1_tokens: int = 0
    step2_tokens: int = 0
    step3_tokens: int = 0
    budget_tier: str = ""
    timestamp: str = ""
    # num_candidates counts step-3 slots, including ones that came back
    # empty. These two say what actually happened in them, and are read by
    # the acquisition liveness gate: it has to tell a stage that could not
    # reach the model from a model that could not solve the problem, and
    # both show num_candidates == 0.
    num_usable_candidates: int = 0
    infrastructure_failures: int = 0
    item_failures: int = 0

    def to_dict(self) -> Dict:
        return {
            "task_id": self.task_id,
            "num_constraint_sets": self.num_constraint_sets,
            "num_plans": self.num_plans,
            "num_candidates": self.num_candidates,
            "num_usable_candidates": self.num_usable_candidates,
            "infrastructure_failures": self.infrastructure_failures,
            "item_failures": self.item_failures,
            "total_tokens": self.total_tokens,
            "total_time_ms": self.total_time_ms,
            "step1_tokens": self.step1_tokens,
            "step2_tokens": self.step2_tokens,
            "step3_tokens": self.step3_tokens,
            "budget_tier": self.budget_tier,
            "timestamp": self.timestamp or datetime.now(timezone.utc).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt templates
# ---------------------------------------------------------------------------

CONSTRAINT_EXTRACTION_PROMPT = """\
Read this problem carefully. Identify {n} distinct CONSTRAINTS that any valid \
solution must satisfy. For each constraint, state:
1. What the constraint is (e.g., "time complexity must be O(n log n)")
2. What it ELIMINATES (e.g., "brute force O(n^2) approaches won't pass")
3. What algorithmic approach it IMPLIES (e.g., "requires sorting or divide-and-conquer")

Each constraint should point toward a DIFFERENT algorithmic family.

Output format — use exactly this structure for each constraint set:

CONSTRAINT SET 1:
- Constraint: <the constraint>
- Eliminates: <what it rules out>
- Implies: <what approach it suggests>

CONSTRAINT SET 2:
- Constraint: <the constraint>
- Eliminates: <what it rules out>
- Implies: <what approach it suggests>

(Continue for all {n} sets)

Problem:
{problem}"""

PLAN_CONSTRUCTION_PROMPT = """\
Based on these constraints about the problem:
{constraints}

Non-negotiable implementation contract:
- Preserve every explicitly requested public name, exact signature, entry point, and input/output behavior.
- Preserve existing public interfaces unless the problem explicitly requires changing them.
- The eventual answer must be a complete, syntactically valid source file.

Design a solution plan that satisfies ALL of them:
1. Algorithm choice (justified by the constraints)
2. Data structures needed
3. Key implementation steps (numbered)
4. Edge cases identified from the constraints

Do NOT write code yet. Focus on the design.

Problem:
{problem}"""

CODE_GENERATION_PROMPT = """\
Implement this plan as Python code:

Plan:
{plan}

These constraints MUST be satisfied:
{constraints}

Treat every explicitly requested public name, exact signature, entry point, and input/output behavior as a hard contract.
Preserve existing public interfaces unless the problem explicitly requires changing them.
Copy parameter order and kinds (including / and * markers), defaults, and type annotations exactly.
Implement behavioral contracts such as laziness, stopping conditions, ordering, and error behavior literally.
Write one complete, syntactically valid source file only; never import that file or requested artifact from itself.
Before answering, mentally compile it and verify every requested declaration and contract is implemented.

Problem:
{problem}"""


# ---------------------------------------------------------------------------
# Parsing helpers
# ---------------------------------------------------------------------------

def parse_constraint_sets(response: str, expected_n: int) -> List[ConstraintSet]:
    """Parse constraint sets from LLM response.

    Looks for "CONSTRAINT SET N:" headers and extracts structured content.
    Falls back to paragraph splitting if structured format not found.
    """
    sets: List[ConstraintSet] = []

    # Try structured parsing first: CONSTRAINT SET N:
    pattern = r'CONSTRAINT\s+SET\s+(\d+)\s*:(.*?)(?=CONSTRAINT\s+SET\s+\d+\s*:|$)'
    matches = re.findall(pattern, response, re.DOTALL | re.IGNORECASE)

    if matches:
        for _num, block in matches:
            constraints = []
            family = ""
            for line in block.strip().split('\n'):
                line = line.strip().lstrip('-').strip()
                if not line:
                    continue
                if line.lower().startswith('constraint:'):
                    constraints.append(line.split(':', 1)[1].strip())
                elif line.lower().startswith('eliminates:'):
                    constraints.append(f"Eliminates: {line.split(':', 1)[1].strip()}")
                elif line.lower().startswith('implies:'):
                    family = line.split(':', 1)[1].strip()
                    constraints.append(f"Implies: {family}")
                elif line:
                    constraints.append(line)
            if constraints:
                sets.append(ConstraintSet(constraints=constraints,
                                          algorithmic_family=family))
        return sets[:expected_n]

    # Fallback: split by numbered headers (1., 2., 3.)
    numbered = re.split(r'\n\s*\d+[.)]\s+', '\n' + response)
    for block in numbered[1:]:  # skip empty first split
        block = block.strip()
        if block:
            lines = [l.strip() for l in block.split('\n') if l.strip()]
            if lines:
                sets.append(ConstraintSet(constraints=lines))
    if sets:
        return sets[:expected_n]

    # Last resort: treat whole response as one constraint set
    lines = [l.strip() for l in response.strip().split('\n') if l.strip()]
    if lines:
        sets.append(ConstraintSet(constraints=lines))
    return sets[:expected_n]


def parse_plan(response: str, constraint_set: ConstraintSet) -> Plan:
    """Parse a plan from LLM response."""
    plan = Plan(constraint_set=constraint_set)
    plan.approach = response.strip()

    # Try to extract structured sections
    sections = {
        'algorithm': r'(?:algorithm|approach)\s*(?:choice)?[:\-]?\s*(.*?)(?=\n\s*\d|data struct|key impl|edge case|$)',
        'data_structures': r'data\s+struct\w*[:\-]?\s*(.*?)(?=\n\s*\d|key impl|edge case|$)',
    }
    for key, pat in sections.items():
        match = re.search(pat, response, re.DOTALL | re.IGNORECASE)
        if match:
            if key == 'algorithm':
                plan.approach = match.group(1).strip()
            elif key == 'data_structures':
                plan.data_structures = match.group(1).strip()

    # Extract numbered steps
    step_matches = re.findall(r'\d+[.)]\s+(.+)', response)
    if step_matches:
        plan.steps = [s.strip() for s in step_matches]

    # Extract edge cases
    edge_section = re.search(r'edge\s+cases?[:\-]?\s*(.*?)$', response,
                             re.DOTALL | re.IGNORECASE)
    if edge_section:
        edges = re.findall(r'[-*]\s+(.+)', edge_section.group(1))
        plan.edge_cases = [e.strip() for e in edges]

    return plan


def extract_code_from_response(response: str) -> str:
    """Extract Python code from an LLM response.

    Handles: ```python blocks, ``` blocks, <think> blocks, raw code.
    """
    # Strip any reasoning leak via the shared, model-agnostic helper (handles
    # both closed <think>...</think> blocks and orphaned closing tags).
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

    # Return as-is (may be raw code)
    return response.lstrip()


# ---------------------------------------------------------------------------
# Main class
# ---------------------------------------------------------------------------

class PlanSearch:
    """PlanSearch constraint-based plan generation pipeline.

    When enabled, replaces temperature-only diversity with constraint-driven
    generation. Each constraint set narrows the solution space, producing
    fundamentally different algorithmic approaches.

    When disabled, returns an empty result (noop).

    Args:
        config: PlanSearchConfig instance.
        budget_forcing: BudgetForcing instance for thinking mode control.
        telemetry_dir: Directory for JSONL event logs.
    """

    def __init__(self, config: PlanSearchConfig,
                 budget_forcing: Optional[BudgetForcing] = None,
                 telemetry_dir: Optional[Path] = None):
        self.config = config
        self.budget_forcing = budget_forcing
        self.telemetry_dir = telemetry_dir
        self._events_file: Optional[Path] = None
        if telemetry_dir is not None:
            telemetry_dir.mkdir(parents=True, exist_ok=True)
            self._events_file = telemetry_dir / "plan_search_events.jsonl"

    # -- Public API ---------------------------------------------------------

    def generate(self, problem: str, task_id: str,
                 llm_call: LLMCallable,
                 num_plans: Optional[int] = None,
                 budget_tier: str = "standard",
                 base_seed: int = 42) -> PlanSearchResult:
        """Execute the full PlanSearch pipeline.

        Args:
            problem: The coding problem description.
            task_id: Unique task identifier for telemetry.
            llm_call: Callable matching LLMCallable signature.
            num_plans: Number of plans to generate (overrides config).
            budget_tier: Budget tier for thinking mode control.
            base_seed: Base seed for reproducibility.

        Returns:
            PlanSearchResult with constraint sets, plans, and code candidates.
        """
        if not self.config.enabled:
            return PlanSearchResult(task_id=task_id, constraint_sets=[],
                                    plans=[], candidates=[])

        n = min(num_plans or self.config.num_plans, self.config.max_plans)
        result = PlanSearchResult(task_id=task_id, raw_responses={})
        total_start = time.time()
        step1_tokens = 0
        step2_tokens = 0
        step3_tokens = 0

        # Step 1: Constraint Extraction. The step-1 token count is returned
        # through locals (not stored on self) so concurrent generate() calls
        # on a shared instance never cross-attribute each other's telemetry.
        #
        # Step 1 is a single call on this thread, not a fan-out, but the
        # classification is a property of the failure rather than of the
        # branch that hit it: a refused step 1 is the stage never reaching
        # the model just as surely as a refused step 3.
        try:
            constraint_sets, step1_tokens = self._step1_extract_constraints(
                problem, n, llm_call, budget_tier, base_seed
            )
        except Exception as exc:  # noqa: BLE001 — re-raised either way
            if not _is_infrastructure_failure(exc):
                raise
            result.total_time_ms = (time.time() - total_start) * 1000
            self._log_event(self._event_for(
                task_id, [], [], [], result, 0, 0, 0, budget_tier,
                [(0, exc, True)]))
            raise PlanSearchInfrastructureError(
                f"PlanSearch could not reach the model for constraint "
                f"extraction: {exc}", failures=[(0, exc)], result=result) from exc
        result.constraint_sets = constraint_sets

        # Ensure we have at least one constraint set
        if not constraint_sets:
            constraint_sets = [ConstraintSet(
                constraints=["Solve the problem correctly"],
                algorithmic_family="general",
            )]
            result.constraint_sets = constraint_sets

        # Steps 2 and 3 fan out. Each plan is built from its own constraint
        # set, and each candidate from its own plan and seed, so nothing in
        # either step reads another's result — this is the parallel-sampling
        # shape best-of-N assumes, and it was being run end to end.
        #
        # The comment here used to say a pool "added threads without
        # parallelism" because the service adapter serialized every call
        # behind a class-level lock. That lock existed to stop concurrent
        # REQUESTS from oversubscribing llama.cpp and is now a semaphore
        # bounded by the slot count, so independent calls share the slots
        # the server already had.
        #
        # Measured 2026-08-03: 4 candidates at ~22s each, strictly serial,
        # inside a pipeline that totalled 166s against the proxy's 180s cap
        # while three of four slots idled.
        plans: List[Plan] = [None] * len(constraint_sets)
        failures: List[Tuple[int, Exception, bool]] = []
        candidates: List[str] = []
        try:
            step2_results = self._fan_out(
                [(i, cs) for i, cs in enumerate(constraint_sets)],
                lambda i, cs: self._step2_construct_plan(
                    problem, cs, llm_call, budget_tier, seed=base_seed + i + 100),
                failures=failures,
            )
            for i, (plan, tokens, _t) in step2_results:
                plans[i] = plan
                step2_tokens += tokens
            plans = [p for p in plans if p is not None]
            result.plans = plans

            # Step 3: Code Generation
            candidates = [None] * len(plans)
            step3_results = self._fan_out(
                [(i, p) for i, p in enumerate(plans)],
                lambda i, plan: self._step3_generate_code(
                    problem, plan, llm_call, budget_tier, seed=base_seed + i + 200),
                failures=failures,
            )
            for i, (code, tokens, _t) in step3_results:
                candidates[i] = code
                step3_tokens += tokens
            result.candidates = [c for c in candidates if c is not None]
        except PlanSearchInfrastructureError as exc:
            # The tokens already spent are real and stay on the ledger, and
            # the telemetry line says why the stage stopped. Re-raised so the
            # caller cannot mistake "refused" for "produced nothing".
            result.plans = [p for p in plans if p is not None]
            result.total_tokens = step1_tokens + step2_tokens + step3_tokens
            result.total_time_ms = (time.time() - total_start) * 1000
            self._log_event(self._event_for(
                task_id, constraint_sets, result.plans, result.candidates,
                result, step1_tokens, step2_tokens, step3_tokens, budget_tier,
                failures))
            exc.result = result
            raise

        total_time = (time.time() - total_start) * 1000
        result.total_tokens = step1_tokens + step2_tokens + step3_tokens
        result.total_time_ms = total_time

        # Log telemetry
        self._log_event(self._event_for(
            task_id, constraint_sets, plans, candidates, result,
            step1_tokens, step2_tokens, step3_tokens, budget_tier, failures))

        return result

    @staticmethod
    def _event_for(task_id, constraint_sets, plans, candidates, result,
                   step1_tokens, step2_tokens, step3_tokens, budget_tier,
                   failures):
        """One telemetry line, with the two failure classes counted apart.

        A liveness gate reads infrastructure_failures: it is the difference
        between the stage being unable to reach the model and the model being
        unable to solve the problem, and num_candidates alone shows zero for
        both.
        """
        return PlanSearchEvent(
            task_id=task_id,
            num_constraint_sets=len(constraint_sets),
            num_plans=len(plans),
            num_candidates=len(candidates),
            total_tokens=result.total_tokens,
            total_time_ms=result.total_time_ms,
            step1_tokens=step1_tokens,
            step2_tokens=step2_tokens,
            step3_tokens=step3_tokens,
            budget_tier=budget_tier,
            num_usable_candidates=len(result.candidates),
            infrastructure_failures=sum(1 for _, _, infra in failures if infra),
            item_failures=sum(1 for _, _, infra in failures if not infra),
        )

    # -- Pipeline steps -----------------------------------------------------

    def _fan_out(self, items, fn, failures=None):
        """Run `fn(index, item)` over independent items, results in order.

        Concurrency is bounded downstream by the adapter's slot semaphore, so
        the pool only has to be wide enough to keep those slots fed; the
        backend decides how many actually run at once.

        One item raising drops that item and leaves the rest — a candidate
        that fails to generate is a smaller loss than the batch, and the
        caller already tolerates a short candidate list. That tolerance is
        for the model failing to produce a usable plan or candidate. It is
        NOT for the call never reaching the model: an infrastructure or
        authentication failure is raised to the caller, because the
        difference between "the model could not solve it" and "the request
        was refused" is invisible in a short candidate list, and the
        DivSampling backfill downstream would close over the gap.

        Worker threads do not inherit the request thread's ContextVars, so
        the request ID used for log correlation is captured here, on the
        owning thread, and re-established inside each worker. The identity
        that authorizes the outbound call does not travel this way at all —
        it lives on the request-scoped LLM adapter (see
        LLMAdapter._inference_headers), so no worker depends on a ContextVar
        being set to send an attributed request.
        """
        infra: List[Tuple[int, Exception]] = []
        seen = failures if failures is not None else []

        def _record(idx, exc):
            print(f"  [plansearch] item {idx} failed: {exc}", flush=True)
            is_infra = _is_infrastructure_failure(exc)
            seen.append((idx, exc, is_infra))
            if is_infra:
                infra.append((idx, exc))

        if len(items) <= 1:
            out = []
            for idx, item in items:
                try:
                    out.append((idx, fn(idx, item)))
                except Exception as exc:  # noqa: BLE001 — one item, not the batch
                    _record(idx, exc)
            _raise_if_infrastructure(infra)
            return out

        # Captured on the owning request thread; a worker that reads the
        # ContextVars directly would read the defaults, not these. Both
        # identities travel, so a worker's log line joins to the same
        # invocation as the request thread's.
        owner_identity = _current_identity()

        def _in_worker(idx, item):
            _bind_identity(owner_identity)
            return fn(idx, item)

        results = []
        with ThreadPoolExecutor(max_workers=len(items)) as pool:
            futures = {pool.submit(_in_worker, idx, item): idx
                       for idx, item in items}
            for fut, idx in futures.items():
                try:
                    results.append((idx, fut.result()))
                except Exception as exc:  # noqa: BLE001
                    _record(idx, exc)
        _raise_if_infrastructure(infra)
        # Ordered by index so seeds, plans and candidates stay aligned with
        # the constraint sets they came from — selection reports winners by
        # index, and a reordered list would rename them.
        return sorted(results, key=lambda r: r[0])

    def _step1_extract_constraints(
        self, problem: str, n: int,
        llm_call: LLMCallable, budget_tier: str,
        seed: int
    ) -> Tuple[List[ConstraintSet], int]:
        """Step 1: Extract N constraint sets from the problem.

        Returns the parsed constraint sets and the token count for this
        step (threaded back to the caller via locals rather than instance
        state, so parallel requests keep independent telemetry).
        """
        user_content = CONSTRAINT_EXTRACTION_PROMPT.format(
            n=n, problem=problem
        )
        # Constraint extraction is structured output — thinking wastes tokens
        # and can consume the entire budget, leaving no room for constraints.
        # base=1024: most constraint responses are <500 tokens of structured text.
        prompt = self._format_prompt(user_content, "nothink")
        max_tokens = self._get_max_tokens("nothink", base=1024)

        response, tokens, time_ms = llm_call(
            prompt, self.config.step1_temperature, max_tokens, seed
        )

        return parse_constraint_sets(response, n), tokens

    def _step2_construct_plan(
        self, problem: str, constraint_set: ConstraintSet,
        llm_call: LLMCallable, budget_tier: str,
        seed: int
    ) -> Tuple[Plan, int, float]:
        """Step 2: Construct a solution plan for a constraint set."""
        constraints_text = '\n'.join(
            f"- {c}" for c in constraint_set.constraints
        )
        user_content = PLAN_CONSTRUCTION_PROMPT.format(
            constraints=constraints_text, problem=problem
        )
        # Plan construction is structured output — thinking wastes tokens.
        # base=1024: plan descriptions are typically <500 tokens.
        prompt = self._format_prompt(user_content, "nothink")
        max_tokens = self._get_max_tokens("nothink", base=1024)

        response, tokens, time_ms = llm_call(
            prompt, self.config.step2_temperature, max_tokens, seed
        )
        plan = parse_plan(response, constraint_set)
        return plan, tokens, time_ms

    def _step3_generate_code(
        self, problem: str, plan: Plan,
        llm_call: LLMCallable, budget_tier: str,
        seed: int
    ) -> Tuple[str, int, float]:
        """Step 3: Generate code implementing a plan."""
        constraints_text = '\n'.join(
            f"- {c}" for c in plan.constraint_set.constraints
        )
        user_content = CODE_GENERATION_PROMPT.format(
            plan=plan.approach,
            constraints=constraints_text,
            problem=problem,
        )
        # Code generation always uses nothink — the plan already specifies the
        # approach, so thinking wastes tokens and can cause empty candidates
        # when <think> blocks consume the entire token budget.
        code_tier = "nothink"
        prompt = self._format_prompt(user_content, code_tier)
        max_tokens = 4096  # Code output doesn't need huge budget

        response, tokens, time_ms = llm_call(
            prompt, self.config.step3_temperature, max_tokens, seed
        )
        code = extract_code_for_problem(response, problem, fallback="last")
        return code, tokens, time_ms

    # -- Helpers ------------------------------------------------------------

    def _format_prompt(self, user_content: str, tier: str) -> str:
        """Format ChatML prompt, using BudgetForcing if available."""
        if self.budget_forcing is not None:
            return self.budget_forcing.format_chatml(user_content, tier)
        # Fallback: basic ChatML with nothink
        system = get_system_prompt(tier)
        return (
            f"<|im_start|>system\n{system}<|im_end|>\n"
            f"<|im_start|>user\n{user_content}<|im_end|>\n"
            f"<|im_start|>assistant\n"
        )

    def _get_max_tokens(self, tier: str, base: int = 2048) -> int:
        """Get max tokens for a generation step.

        For nothink tier, respects the base parameter (steps 1/2 produce
        structured text, not code — they don't need the full 4096 budget).
        """
        if self.budget_forcing is not None:
            bf_max = self.budget_forcing.get_max_tokens(tier)
            if tier == "nothink":
                return min(base, bf_max)
            return bf_max
        return base + 4096

    def _log_event(self, event: PlanSearchEvent) -> None:
        """Append event to JSONL telemetry file."""
        if self._events_file is None:
            return
        try:
            with open(self._events_file, "a") as f:
                f.write(json.dumps(event.to_dict()) + "\n")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
