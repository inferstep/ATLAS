"""The V3 pipeline orchestrator. V3PipelineService runs one task through its
phases (probe, allocation, generation, sandbox verification, selection,
repair). Also here: the diagnostic candidate allocation and the /v3/generate
problem builder. The modules beside this one hold the other parts:
run_steps.py holds the steps of a run and the candidate verifier,
candidate_cases.py holds the case wrappers and the CodeT consensus, and
pool_capture.py holds the benchmark-only pool capture. Stage telemetry, the
candidate records and the vetoes are also modules of their own."""

import json
import threading
import time
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

from stages.llm_client import extract_code_for_problem
from stages.budget_forcing import BudgetForcing, BudgetForcingConfig
from stages.plan_search import (
    PlanSearch, PlanSearchConfig, PlanSearchInfrastructureError)
from stages.div_sampling import DivSampling, DivSamplingConfig
from stages.failure_analysis import FailingCandidate
from stages.pr_cot import PRCoT, PRCoTConfig
from stages.refinement_loop import RefinementLoop, RefinementLoopConfig
from stages.self_test_gen import SelfTestGen, SelfTestGenConfig

import adapters
import scoring
from stage_telemetry import _progress_emitter, _resolve_telemetry_dir, _summarize_phases
from candidate_records import (_candidate_lens_payload, _ensure_delivered_evidence, _lens_view,
                               _note_lens, _task_identity)
from pool_capture import _PoolCapture
from candidate_cases import _evaluate_candidate, _task_input_file
from run_steps import (DEFAULT_WORKING_DIR, _RunContext, _RunPool, _allocate, _call_chain_context,
                       _check_incumbent, _check_repair, _client_check, _enriched_error,
                       _enter_repair, _evaluate_probe, _failing_candidates, _finish_with_best,
                       _leave_baseline, _new_llm, _new_result, _new_verifier, _out_of_budget,
                       _refinement_affordable, _score_and_test_probe, _seed_with_probe,
                       _smoke_language, _test_and_select, _with_file_context)

# Serializes pipeline-summary appends across the ThreadingHTTPServer's
# request threads. Stage JSONL appends need no lock: each event is one
# small O_APPEND write.
_SUMMARY_LOCK = threading.Lock()

_VETO_STAGES = frozenset(("lens_veto", "structural_veto", "call_graph_veto"))

BASE_TEMPERATURE = 0.6
DIVERSITY_TEMPERATURE = 0.8
MAX_TOKENS = 8192


# --- diagnostic candidate allocation -----------------------------------------
#
# `k` is the number of GENERATED candidates: the phase-zero probe takes one
# slot and `remaining_k = k - len(candidates)` fills the rest with generated
# alternatives. The incumbent is in the selectable pool but was not generated,
# so it takes no slot (`candidates` holds generated ones only). Consensus needs
# at least two generated alternatives to mean anything, and the allocator's
# budget cap can drive k to 1, so a measurement run needs a way to raise it.
#
# It raises and never lowers, it is capped small, it is an in-process
# argument the HTTP handler never supplies, and it reads no environment. No
# unauthenticated caller can reach it, and it is independent of the capture
# sink: neither turns the other on.
#
# Cost: at most 6 candidates. Each costs one generation call plus its
# sandbox runs; the existing out_of_budget() checks in the generation loops
# and the ATLAS_V3_TIMEOUT wall clock still apply unchanged, so the cap
# bounds the pool, not the clock.
DIAGNOSTIC_MAX_TOTAL_CANDIDATES = 6


def _diagnostic_total_candidates(value) -> int:
    """Validate the diagnostic pool floor. Absent is 0; anything else that
    is not a plain int in 1..cap is rejected rather than coerced."""
    if value is None or value == 0 and not isinstance(value, bool):
        if isinstance(value, float):
            raise TypeError("diagnostic candidate count must be an int")
        return 0
    if isinstance(value, bool) or not isinstance(value, int):
        raise TypeError("diagnostic candidate count must be an int")
    if value < 1:
        raise ValueError("diagnostic candidate count must be >= 1")
    if value > DIAGNOSTIC_MAX_TOTAL_CANDIDATES:
        raise ValueError(
            f"diagnostic candidate count exceeds the cap of "
            f"{DIAGNOSTIC_MAX_TOTAL_CANDIDATES}")
    return value


# --- V3 Pipeline Orchestrator ------------------------------------------------


class V3PipelineService:
    """Full V3 pipeline for a single coding task, with streaming progress."""

    def __init__(self):
        # ALL V3 components enabled — same as benchmark runner with all phases
        # active. Stage telemetry mirrors the bench runner's telemetry/*.jsonl
        # into ATLAS_V3_TELEMETRY_DIR so live-orchestrator runs are measurable.
        self.telemetry_dir = _resolve_telemetry_dir()
        t = self.telemetry_dir
        self.budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True))
        self.plan_search = PlanSearch(PlanSearchConfig(enabled=True),
                                      telemetry_dir=t)
        self.div_sampling = DivSampling(DivSamplingConfig(enabled=True),
                                        telemetry_dir=t)
        self.pr_cot = PRCoT(PRCoTConfig(enabled=True), telemetry_dir=t)
        self.refinement_loop = RefinementLoop(RefinementLoopConfig(enabled=True),
                                              telemetry_dir=t)
        self.self_test_gen = SelfTestGen(SelfTestGenConfig(enabled=True),
                                         telemetry_dir=t)

    def run(self, problem: str, task_id: str = "cli",
            progress_callback=None, files: Dict[str, str] = None,
            file_path: str = "", build_command: str = "",
            working_dir: str = DEFAULT_WORKING_DIR,
            baseline_code: str = "",
            diagnostic_total_candidates=None,
            cancel_scope=None,
            trace_request_id: str = "",
            v3_invocation_id: str = "",
            budget_ms: Optional[float] = None) -> Dict[str, Any]:
        """Run the full V3 pipeline on a coding problem.

        Args:
            problem: Problem description
            task_id: Task identifier
            progress_callback: SSE progress emitter
            files: Dict of filename→content: the request's existing project files
            file_path: Target file path (used to detect the language for the
                smoke check — `.html` files use HTML parser, not Python compile,
                etc.)
            build_command: Optional project build command to run against an
                ephemeral candidate overlay after syntax/self-tests pass.
            working_dir: Container workspace root used by the sandbox overlay.
            baseline_code: The incumbent's EXACT bytes, as the request sent
                them, before prompt construction. A real candidate:
                it faces the same checks as a generated one, the lens ranks
                it with them, and every replacement must keep its top-level
                names. Interactive Python replacements are also compared
                against its observed import viability in the sandbox.
            budget_ms: The wall-clock cap the caller applies to this call.
                Every budget check plans against it when it is positive;
                otherwise ATLAS_V3_TIMEOUT applies, as for a bench caller.

        Writes one pipeline-summary telemetry line per task (fail-soft;
        see _write_pipeline_summary) around the actual pipeline body.
        """
        start = time.time()
        result: Optional[Dict[str, Any]] = None
        error = ""
        # Benchmark-only: inert unless ATLAS_V3_CAPTURE_POOL names a file.
        # Opened per run so a diagnostic can be turned on and off without
        # restarting the service, and closed here so the delivered artifact
        # and the selection summary are written from the finished result.
        # Validated before anything runs: a malformed diagnostic argument is
        # an error at the call, never a silently coerced allocation.
        _diag_floor = _diagnostic_total_candidates(diagnostic_total_candidates)
        capture = _PoolCapture.from_env()
        capture.bind(task_id)
        # Identity travels with the capture, not with the records' order.
        capture.identify(trace_request_id, v3_invocation_id)
        try:
            result = self._run_impl(
                problem, task_id=task_id, progress_callback=progress_callback,
                files=files, file_path=file_path, build_command=build_command,
                working_dir=working_dir, baseline_code=baseline_code,
                diagnostic_total_candidates=_diag_floor, _capture=capture,
                cancel_scope=cancel_scope, budget_ms=budget_ms,
                # Built here, where both halves of the identity are in scope,
                # and frozen: it reaches PlanSearch's worker threads on the
                # adapter, which is the only carrier that survives a thread
                # boundary.
                request_identity=adapters.RequestIdentity(
                    request_id=trace_request_id or "",
                    invocation_id=v3_invocation_id or ""))
            _ensure_delivered_evidence(result, file_path=file_path, problem=problem)
            return result
        except Exception as e:
            error = f"{type(e).__name__}: {e}"
            raise
        finally:
            capture.close(result)
            self._write_pipeline_summary(task_id, result, error, start)

    def _write_pipeline_summary(self, task_id: str,
                                result: Optional[Dict[str, Any]],
                                error: str, start: float) -> None:
        """Append one summary line to telemetry/pipeline_summary.jsonl.

        Carries the per-task shape the bench runner gets for free from its
        per-task JSON files: phases run (outcome + duration, folded from the
        run's progress events), veto events, and the final result fields.
        Fail-soft by construction — a telemetry error never reaches the
        caller, so it can never break generation.
        """
        if self.telemetry_dir is None:
            return
        try:
            events = (result or {}).get("events") or []
            line = {
                "schema": "v3_pipeline_summary_v1",
                "ts": datetime.now(timezone.utc).isoformat(),
                "task_id": task_id,
                "passed": bool((result or {}).get("passed")),
                "phase_solved": (result or {}).get("phase_solved", "none"),
                "task_type": (result or {}).get("task_type", ""),
                "candidates_generated": (result or {}).get("candidates_generated", 0),
                "total_tokens": (result or {}).get("total_tokens", 0),
                "total_time_ms": round(
                    (result or {}).get("total_time_ms")
                    or (time.time() - start) * 1000),
                "phases": _summarize_phases(events),
                "veto_events": [
                    {"stage": ev.get("stage", ""),
                     "index": (ev.get("data") or {}).get("index", -1),
                     "detail": str(ev.get("detail", ""))[:120]}
                    for ev in events if ev.get("stage") in _VETO_STAGES
                ],
            }
            if error:
                line["error"] = error[:300]
            with _SUMMARY_LOCK:
                with open(self.telemetry_dir / "pipeline_summary.jsonl", "a") as f:
                    f.write(json.dumps(line) + "\n")
        except Exception as e:
            print(f"  [telemetry] pipeline summary write failed (non-fatal): {e}",
                  flush=True)

    def _run_impl(self, problem: str, task_id: str = "cli",
                  progress_callback=None, files: Dict[str, str] = None,
                  file_path: str = "", build_command: str = "",
                  working_dir: str = DEFAULT_WORKING_DIR, baseline_code: str = "",
                  diagnostic_total_candidates: int = 0,
                  _capture: Optional["_PoolCapture"] = None,
                  cancel_scope=None,
                  request_identity=None,
                  budget_ms: Optional[float] = None) -> Dict[str, Any]:
        """The pipeline body — see run() for the argument contract.

        `_capture` is the benchmark-only pool sink run() owns; it observes
        and decides nothing, and defaults to an inert one.
        """
        start = time.time()
        events = []
        files = files or {}
        capture = _capture if _capture is not None else _PoolCapture.disabled()
        smoke_language = _smoke_language(file_path)
        problem = _with_file_context(problem, files)
        emit = _progress_emitter(events, start, progress_callback)
        check_client = _client_check(progress_callback, cancel_scope, task_id)
        llm = _new_llm(emit, start, budget_ms, cancel_scope,
                       request_identity=request_identity)
        # Ship the user's other project files into the sandbox so
        # multi-file imports resolve. `files` is the same Dict that V3
        # already prepends to the LLM prompt above; without it, a model
        # that writes `from utils import helper` is checked in a workspace
        # that contains only the candidate.
        sandbox = adapters.SandboxAdapter(project_files=files)
        # The contract verification must hold candidates to: the caller runs
        # the program with these files on disk and no stdin.
        task_input_file = _task_input_file(files)
        embed = adapters.EmbedAdapter()
        result = _new_result(task_id)
        ctx = _RunContext(problem=problem, task_id=task_id, files=files, file_path=file_path,
                          build_command=build_command, working_dir=working_dir,
                          baseline_code=baseline_code, budget_ms=budget_ms, start=start, events=events,
                          emit=emit, check_client=check_client, llm=llm, sandbox=sandbox, embed=embed,
                          task_input_file=task_input_file, smoke_language=smoke_language,
                          capture=capture, result=result)

        # ===== PHASE 0: PROBE =====
        probe_code, tokens = self._generate_probe(ctx)
        task_type, self_tests = self._classify_task(ctx)
        verified_sandbox = _new_verifier(ctx, task_type, self_tests)
        probe = _score_and_test_probe(ctx, probe_code, tokens, verified_sandbox)

        # Evidence strength decides whether phase 0 may close the pipeline.
        # A compile smoke on interactive code demonstrates only that the file
        # parses; returning on it would skip PlanSearch, DivSampling, consensus
        # and ranking for every interactive artifact.
        # Strength comes from the VERIFIER THAT RAN, not the file extension:
        # Pygame/Tkinter/Flask files get a compile smoke and nothing more, and
        # must not close the pipeline claiming behaviour nobody demonstrated.
        # The adapter is chosen from the artifact alone. The self-test cases
        # play no part in it: they are diagnostics (see verified_sandbox).
        _task = _task_identity(file_path, problem)

        incumbent, finished = _check_incumbent(ctx, verified_sandbox, _task, probe_code)
        if finished is not None:
            return finished
        probe_result, finished = _evaluate_probe(ctx, probe, _task)
        if finished is not None:
            return finished

        k, budget_tier, bf_tier = _allocate(ctx, probe, diagnostic_total_candidates)

        # ===== PHASE 1: CONSTRAINT-DIVERSE CANDIDATE GENERATION =====
        emit("phase1", f"Generating {k} diverse candidates...", k=k)
        candidates = []
        run_pool = _RunPool(incumbent, candidates, probe_code)
        try:
            _seed_with_probe(candidates, probe, probe_result)
            self._generate_from_plans(ctx, candidates, k, budget_tier)
            self._generate_by_div_sampling(ctx, candidates, k, bf_tier)
            result["candidates_generated"] = len(candidates)
            finished = _test_and_select(ctx, run_pool, verified_sandbox, self_tests, _task)
            if finished is not None:
                return finished
            finished = _enter_repair(ctx, run_pool, task_type)
            if finished is not None:
                return finished
            failing = _failing_candidates(run_pool)
            self_tests = self._regenerate_self_tests(ctx, task_type, self_tests)
            verified_sandbox.self_tests = self_tests
            return self._repair(ctx, run_pool, failing, verified_sandbox, _task)
        except adapters.BudgetExhausted as exc:
            # An anytime algorithm whose clock expires owes its caller the
            # best answer it has. Raised from the adapter rather than
            # checked at phase boundaries: every phase runs its own loop —
            # PR-CoT alone issues two calls — so a boundary check that
            # reserves one call is already wrong by the second.
            return _finish_with_best(ctx, run_pool, f"budget exhausted mid-pipeline ({exc})")

    def _generate_probe(self, ctx):
        """Phase 0's probe candidate: a light thinking budget first, then the
        standard one, then none. Returns its code and the last call's tokens."""
        problem, llm, emit = ctx.problem, ctx.llm, ctx.emit
        emit("probe", "Generating probe candidate...")
        # Light probe first (1024 thinking tokens), retry with standard if fails
        try:
            chatml = self.budget_forcing.format_chatml(problem, "light")
            response, tokens, t_ms = llm(chatml, BASE_TEMPERATURE, MAX_TOKENS, 42)
            probe_code = extract_code_for_problem(response, problem)
            if probe_code:
                emit("probe_light", f"Light probe: {len(probe_code)} chars, {tokens} tokens, {t_ms:.0f}ms")
        except Exception as e:
            emit("probe_error", str(e))
            probe_code = ""

        if not probe_code:
            emit("probe_retry", "Light probe failed — retrying with standard budget")
            try:
                chatml = self.budget_forcing.format_chatml(problem, "standard")
                response, tokens, t_ms = llm(chatml, BASE_TEMPERATURE, MAX_TOKENS, 42)
                probe_code = extract_code_for_problem(response, problem)
            except Exception as e:
                emit("probe_error", str(e))

        if not probe_code:
            emit("probe_failed", "No code extracted from probe")
            # Generate with the minimal reasoning budget
            chatml = self.budget_forcing.format_chatml(problem, "nothink")
            response, tokens, t_ms = llm(chatml, BASE_TEMPERATURE, MAX_TOKENS, 42)
            probe_code = extract_code_for_problem(response, problem)
        return probe_code, tokens

    def _classify_task(self, ctx):
        """The task's type and, for an algorithmic task, its generated
        self-tests (None when there are none)."""
        problem, llm, task_id, emit, result = ctx.problem, ctx.llm, ctx.task_id, ctx.emit, ctx.result
        # Classify task type. Interactive tasks (games, UIs, framework code)
        # skip synthetic I/O self-tests entirely: those tests would fail by
        # construction, falsely triggering PR-CoT/refinement on working code.
        task_type = scoring.classify_task_type(problem)
        emit("task_type", task_type)
        result["task_type"] = task_type

        # Generate self-tests (algorithmic tasks only) — used for sandbox verification
        self_tests = None
        if task_type == "algorithmic":
            emit("self_test_gen", "Generating verification tests...")
            try:
                self_tests = self.self_test_gen.generate(problem, llm, task_id)
                emit("self_test_done", f"{len(self_tests.test_cases)} test cases")
                result["total_tokens"] += self_tests.generation_tokens
            except Exception as e:
                emit("self_test_error", str(e)[:200])
        else:
            emit("self_test_skip", "Interactive task — using compile smoke-test")
        return task_type, self_tests

    def _generate_from_plans(self, ctx, candidates, k, budget_tier):
        """Step 1A: PlanSearch writes a candidate for each open slot; each is
        Lens-scored as it arrives and appended to `candidates`."""
        problem, task_id, llm, capture, emit, result = (
            ctx.problem, ctx.task_id, ctx.llm, ctx.capture, ctx.emit, ctx.result)
        remaining_k = max(0, k - len(candidates))

        # Step 1A: PlanSearch
        if remaining_k > 0:
            emit("plansearch", f"Generating {remaining_k} plans...",
                 plans=remaining_k)
            try:
                ps_result = self.plan_search.generate(
                    problem, task_id, llm, num_plans=remaining_k,
                    # The allocator's tier, not the signature default
                    # ("standard"), so PlanSearch thinks at the depth the
                    # CxGx gate decided.
                    budget_tier=budget_tier,
                )
                # PlanSearch reports one aggregate for the whole batch, so
                # a per-candidate split would be invented. It is recorded as
                # named shared overhead instead: the ledger stays exact and
                # says which phase the cost belongs to.
                capture.note_cost(code=None, phase="plansearch",
                                  tokens=getattr(ps_result, "total_tokens", 0),
                                  latency_ms=getattr(ps_result, "total_time_ms", 0.0))
                for i, code in enumerate(ps_result.candidates):
                    if code:
                        candidates.append({
                            "index": len(candidates), "code": code,
                            "passed": False, "stdout": "", "stderr": "",
                            **_lens_view(code),
                        })
                        capture.note_lens_candidate(
                            role="generated", index=candidates[-1]["index"],
                            code=code, phase="plansearch_scored",
                            lens=_candidate_lens_payload(candidates[-1]))
                        _note_lens(emit, candidates[-1], "plansearch")
                result["total_tokens"] += ps_result.total_tokens
                emit("plansearch_done",
                     f"{len(ps_result.candidates)} candidates from PlanSearch",
                     candidates=len(ps_result.candidates),
                     tokens=ps_result.total_tokens)
            except PlanSearchInfrastructureError as e:
                # PlanSearch never reached the model. DivSampling still
                # fills the slots below — that is unchanged — but the
                # backfill must not be the only trace of what happened:
                # an empty PlanSearch batch and a refused one produce the
                # same candidate count, and only one of them is a result.
                partial = getattr(e, "result", None)
                if partial is not None:
                    capture.note_cost(
                        code=None, phase="plansearch",
                        tokens=getattr(partial, "total_tokens", 0),
                        latency_ms=getattr(partial, "total_time_ms", 0.0))
                    result["total_tokens"] += getattr(partial, "total_tokens", 0)
                emit("plansearch_error", str(e)[:200],
                     kind="infrastructure",
                     infrastructure_failures=len(getattr(e, "failures", ())))
            except scoring.LensUnavailable:
                raise
            except Exception as e:
                emit("plansearch_error", str(e)[:200], kind="stage")

    def _generate_by_div_sampling(self, ctx, candidates, k, bf_tier):
        """Step 1B: DivSampling fills the slots PlanSearch left, one call at a
        time, until they are full or the budget is spent."""
        emit, check_client = ctx.emit, ctx.check_client
        # Step 1B: DivSampling to fill remaining slots
        remaining_k = max(0, k - len(candidates))
        if remaining_k > 0:
            emit("divsampling", f"Filling {remaining_k} slots with diverse sampling...",
                 slots=remaining_k)
            for idx in range(remaining_k):
                check_client()
                if _out_of_budget(ctx):
                    emit("divsampling_stop",
                         f"budget spent after {len(candidates)} candidate(s)")
                    break
                self._div_sample(ctx, candidates, idx, bf_tier)
            emit("divsampling_done", f"{len(candidates)} total candidates",
                 total=len(candidates))

    def _div_sample(self, ctx, candidates, idx, bf_tier):
        """One DivSampling call: a perturbed problem, one generation, and the
        candidate it yields, Lens-scored and appended to `candidates`."""
        problem, task_id, llm, capture, emit, result = (
            ctx.problem, ctx.task_id, ctx.llm, ctx.capture, ctx.emit, ctx.result)
        try:
            perturbed = self.div_sampling.apply(problem, len(candidates) + idx, task_id)
            chatml = self.budget_forcing.format_chatml(perturbed, bf_tier)
            response, tokens, t_ms = llm(
                chatml, DIVERSITY_TEMPERATURE,
                self.budget_forcing.get_max_tokens(bf_tier),
                42 + len(candidates) + idx,
            )
            code = extract_code_for_problem(response, problem)
            # Attribute at the site that owns the bytes: this call
            # produced this candidate, so no sibling thread can be
            # charged for it. A call that yielded nothing usable is
            # shared overhead, not a free candidate.
            capture.note_cost(code=code or None, phase="divsampling",
                              tokens=tokens, latency_ms=t_ms)
            if code:
                candidates.append({
                    "index": len(candidates), "code": code,
                    "passed": False, "stdout": "", "stderr": "",
                    **_lens_view(code),
                })
                capture.note_lens_candidate(
                    role="generated", index=candidates[-1]["index"],
                    code=code, phase="divsampling_scored",
                    lens=_candidate_lens_payload(candidates[-1]))
                _note_lens(emit, candidates[-1], "divsampling")
            result["total_tokens"] += tokens
        except scoring.LensUnavailable:
            raise
        except Exception as e:
            emit("divsampling_error", str(e)[:200])

    def _regenerate_self_tests(self, ctx, task_type, self_tests):
        """Phase 3 generates self-tests only when phase 0 produced none; a
        failed retry keeps what there was. Returns the run's self-tests."""
        problem, llm, task_id, emit, result = ctx.problem, ctx.llm, ctx.task_id, ctx.emit, ctx.result
        # Repair runs the SAME self-tests phase 0 generated —
        # verified_sandbox closes over them and records each score as a
        # diagnostic. Regenerate only when phase 0 produced none (e.g. a
        # transient LLM failure); a failed retry here must not downgrade an
        # existing good set to None. Interactive tasks repair against
        # compile-smoke.
        if task_type == "algorithmic" and not (self_tests and self_tests.test_cases):
            emit("self_test_gen", "Generating self-tests...")
            try:
                self_tests = self.self_test_gen.generate(problem, llm, task_id)
                emit("self_test_done", f"{len(self_tests.test_cases)} test cases generated")
                result["total_tokens"] += self_tests.generation_tokens
            except Exception as e:
                emit("self_test_error", str(e)[:200])
        return self_tests

    def _repair(self, ctx, run_pool, failing, verified_sandbox, _task):
        """Phase 3's two strategies, PR-CoT then the refinement loop, each
        given the failing function's call-chain context. With nothing
        verified the caller keeps its baseline. Returns the result."""
        chain_context_block = _call_chain_context(ctx, failing)
        finished = self._repair_with_pr_cot(ctx, run_pool, failing, verified_sandbox, _task, chain_context_block)
        if finished is not None:
            return finished
        run_refinement = _refinement_affordable(ctx, failing)
        if run_refinement:
            finished = self._refine(ctx, failing, verified_sandbox, _task, chain_context_block)
            if finished is not None:
                return finished
        return _leave_baseline(ctx, run_pool)

    def _repair_with_pr_cot(self, ctx, run_pool, failing, verified_sandbox, _task, chain_context_block):
        """Strategy 1: PR-CoT repairs the easiest failing candidate, and the
        first repair that verifies is the finished run, returned. Else None.
        With the budget spent it does not start: the best so far is returned."""
        problem, task_id, llm, emit, result, start, events = (
            ctx.problem, ctx.task_id, ctx.llm, ctx.emit, ctx.result, ctx.start, ctx.events)
        # Strategy 1: PR-CoT Quick Repair
        if failing and _out_of_budget(ctx):
            return _finish_with_best(ctx, run_pool, "budget spent before PR-CoT repair")
        if failing:
            emit("pr_cot", "Attempting PR-CoT repair...",
                 strategy="pr_cot", failing=len(failing))
            best_failing = failing[0]
            try:
                pr_result = self.pr_cot.repair(
                    problem=problem,
                    code=best_failing.code,
                    error=_enriched_error(best_failing.error_output, chain_context_block),
                    llm_call=llm,
                    task_id=task_id,
                )
                result["total_tokens"] += pr_result.total_tokens
                for repair_code in pr_result.repairs:
                    passed, repair_evidence = _check_repair(ctx, failing, pr_result, repair_code, verified_sandbox, _task)
                    if passed:
                        emit("pr_cot_pass", "PR-CoT repair succeeded!",
                             strategy="pr_cot", tokens=pr_result.total_tokens)
                        result["passed"] = True
                        result["code"] = repair_code
                        result["phase_solved"] = "pr_cot"
                        result["total_time_ms"] = (time.time() - start) * 1000
                        result["verification_evidence"] = repair_evidence
                        result["events"] = events
                        return result
                emit("pr_cot_failed", "PR-CoT repair did not produce passing code")
            except scoring.LensUnavailable:
                raise
            except Exception as e:
                emit("pr_cot_error", str(e)[:200])
        return None

    def _refine(self, ctx, failing, verified_sandbox, _task, chain_context_block):
        """Strategy 2: the refinement loop over every failing candidate, each
        error given the call-chain context. A winner that verifies is the
        finished run, returned. Else None."""
        problem, task_id, llm, sandbox, embed, check_client = (
            ctx.problem, ctx.task_id, ctx.llm, ctx.sandbox, ctx.embed, ctx.check_client)
        file_path, emit, capture, result, start, events = (
            ctx.file_path, ctx.emit, ctx.capture, ctx.result, ctx.start, ctx.events)
        check_client()
        emit("refinement", "Starting refinement loop...",
             strategy="refinement", failing=len(failing))
        # Enrich each failing candidate's error_output
        # with call-chain context so the refinement loop sees it on
        # every iteration. Cheap (chain_context_block is built once
        # above and reused).
        failing_for_refinement = failing
        if chain_context_block:
            failing_for_refinement = [
                FailingCandidate(
                    index=c.index,
                    code=c.code,
                    error_output=_enriched_error(c.error_output, chain_context_block),
                )
                for c in failing
            ]
        try:
            ref_result = self.refinement_loop.run(
                problem=problem,
                failing_candidates=failing_for_refinement,
                original_constraints=[],
                llm_call=llm,
                sandbox_run=sandbox,
                embed_call=embed,
                task_id=task_id,
            )
            result["total_tokens"] += ref_result.total_tokens
            if ref_result.solved:
                passed, _, stderr, refinement_evidence = verified_sandbox(ref_result.winning_code)
                # Refinement winners are late-generated candidates too;
                # apply the same capture-only Lens observation as the
                # PR-CoT repair path above.
                refinement_lens = (_lens_view(ref_result.winning_code)
                                   if capture.enabled else None)
                # A refinement winner is a candidate too, and a candidate
                # with no record is one nothing can say anything about.
                capture.note_candidate(
                    role="refinement", index=None,
                    code=ref_result.winning_code, accepted=passed,
                    record=_evaluate_candidate(
                        file_path, ref_result.winning_code, passed,
                        emit, task=_task),
                    phase="refinement", lens=refinement_lens)
                if passed:
                    emit("refinement_pass",
                         f"Refinement solved in {ref_result.total_iterations} iterations!",
                         strategy="refinement",
                         iterations=ref_result.total_iterations,
                         tokens=ref_result.total_tokens)
                    result["passed"] = True
                    result["code"] = ref_result.winning_code
                    result["phase_solved"] = "refinement"
                    result["total_time_ms"] = (time.time() - start) * 1000
                    result["verification_evidence"] = refinement_evidence
                    result["events"] = events
                    return result
                emit("refinement_verify_failed", (stderr or "")[:200])
            emit("refinement_failed", f"Exhausted {ref_result.total_iterations} iterations")
        except scoring.LensUnavailable:
            raise
        except Exception as e:
            emit("refinement_error", str(e)[:200])
        return None


# --- Problem Builder for /v3/generate ----------------------------------------

def _build_problem_from_request(
    file_path: str, baseline_code: str, project_context: Dict[str, str],
    framework: str, build_command: str, constraints: List[str],
    user_message: str = "",
) -> str:
    """Build a problem description for the V3 pipeline from a generate request.

    Here the incumbent is prose: the reference the generated candidates work
    from. It is ALSO a candidate in its own right, with its exact bytes -- the
    pipeline checks, scores and ranks it beside them -- and pool index
    0 is still the phase-zero probe, a fresh generation, not the incumbent.

    The user's own request leads, when the caller sends one. Without it the
    pipeline saw only "Create the file X", the project context and the
    baseline, under an instruction to improve on the baseline "preserving all
    functionality" — so every candidate could only mimic a draft whose
    requirement it had never been shown, and a baseline that misread the task
    was reproduced rather than corrected.

    Measured on the AoC tasks, whose prompt states "reads input.txt": 9 of the
    12 solutions ATLAS produced read stdin instead, and the caller runs
    `python solve.py` with no stdin. The same model given the task directly
    wrote file readers 12 times out of 12.
    """
    parts = []

    if user_message.strip():
        parts.append("## The request\n\n")
        parts.append(user_message.strip() + "\n\n")

    parts.append(f"Create the file `{file_path}`")
    if framework:
        parts.append(f" for a {framework} project")
    parts.append(".\n\n")

    # Project context
    if project_context:
        parts.append("## Existing project files:\n\n")
        for path, content in project_context.items():
            if len(content) < 500:
                parts.append(f"### {path}\n```\n{content}\n```\n\n")
            else:
                parts.append(f"### {path} (truncated)\n```\n{content[:300]}\n...\n```\n\n")

    # Constraints
    if constraints:
        parts.append("## Requirements:\n")
        for c in constraints:
            parts.append(f"- {c}\n")
        parts.append("\n")

    # Build command
    if build_command:
        parts.append(f"## Build verification:\nThe file must pass: `{build_command}`\n\n")

    # Baseline as reference
    if baseline_code:
        parts.append("## Reference implementation:\n")
        parts.append("Improve upon this baseline if possible, preserving all functionality.\n\n")
        parts.append(f"```\n{baseline_code}\n```\n")

    return "".join(parts)
