"""The V3 pipeline orchestrator. V3PipelineService runs one task through its
phases (probe, allocation, generation, sandbox verification, selection,
repair) as the steps defined here. Also here: the benchmark-only pool
capture, the self-test and output-probe case wrappers with the CodeT
consensus over them, the candidate verifier, and the /v3/generate problem
builder. Stage telemetry, the candidate records and the vetoes are modules
of their own beside this one."""

import dataclasses
import json
import os
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, NamedTuple, Optional

from stages.llm_client import extract_code_for_problem
from stages.budget_forcing import BudgetForcing, BudgetForcingConfig
from stages import cxgx_gate
from stages.plan_search import (
    PlanSearch, PlanSearchConfig, PlanSearchInfrastructureError)
from stages.div_sampling import DivSampling, DivSamplingConfig
from stages.failure_analysis import FailingCandidate
from stages.pr_cot import PRCoT, PRCoTConfig
from stages.refinement_loop import (
    RefinementLoop, RefinementLoopConfig,
    can_afford_iteration, estimate_iteration_ms,
)
from stages.self_test_gen import (SelfTestGen, SelfTestGenConfig,
                                  PROVENANCE_GENERATED)
from stages.candidate_selection import (CandidateInfo, energy_rank_key,
                                        select_candidate)

import adapters
import scoring
import symbols
from runtime_verification import PythonImportComparison
from stage_telemetry import _progress_emitter, _resolve_telemetry_dir, _summarize_phases
from candidate_records import (INCUMBENT_INDEX, _announce_selection, _candidate_lens_payload,
                               _ensure_delivered_evidence, _lens_view, _note_lens, _record_closes,
                               _select_by_contract, _task_identity, _without_retypings)
from vetoes import _call_graph_veto, _lens_veto, _structural_veto
from pool_capture import _PoolCapture, _capture_case, _capture_pool_member
from candidate_cases import (_consensus_winners, _evaluate_candidate, _failed_a_project_check,
                             _make_self_test, _task_input_file, _trusted_oracle)

# Serializes pipeline-summary appends across the ThreadingHTTPServer's
# request threads. Stage JSONL appends need no lock: each event is one
# small O_APPEND write.
_SUMMARY_LOCK = threading.Lock()

_VETO_STAGES = frozenset(("lens_veto", "structural_veto", "call_graph_veto"))

BASE_TEMPERATURE = 0.6
DIVERSITY_TEMPERATURE = 0.8
MAX_TOKENS = 8192
DEFAULT_WORKING_DIR = "/workspace"


def _remaining_budget_ms(start: float, cap_ms: Optional[float] = None) -> Optional[float]:
    """Remaining wall-clock (ms) in this run's budget.

    The proxy's V3 bridge abandons a live pipeline call at its cap: at most
    ``ATLAS_V3_TIMEOUT`` seconds (default 300; 0 disables the cap), and at
    most half of the session's remaining time. It sends that cap as
    ``budget_ms``, passed here as ``cap_ms``, so late phases skip work the
    bridge would abandon mid-flight. Without one (an offline or bench
    caller), the service reads ``ATLAS_V3_TIMEOUT`` itself. Returns None
    when the cap is disabled.
    """
    if cap_ms is not None and cap_ms > 0:
        return cap_ms - (time.time() - start) * 1000.0
    raw = os.environ.get("ATLAS_V3_TIMEOUT", "").strip()
    try:
        seconds = int(raw) if raw else 300
    except ValueError:
        seconds = 300
    if seconds <= 0:
        return None
    return seconds * 1000.0 - (time.time() - start) * 1000.0


# --- V3 Pipeline Orchestrator ------------------------------------------------


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


# --- The steps of a pipeline run ---------------------------------------------
#
# V3PipelineService._run_impl reads top to bottom as these steps.


@dataclasses.dataclass(frozen=True)
class _RunContext:
    """What every step of one run reads and none reassigns.

    The request's own values, the clock, the run's emit and check_client,
    its adapters, the capture sink, and the result dict the steps fill in.
    _run_impl builds it once its setup is done; a step unpacks the fields it
    uses.
    """
    problem: str
    task_id: str
    files: Dict[str, str]
    file_path: str
    build_command: str
    working_dir: str
    baseline_code: str
    budget_ms: Optional[float]
    start: float
    events: List[Dict[str, Any]]
    emit: Any
    check_client: Any
    llm: Any
    sandbox: Any
    embed: Any
    task_input_file: str
    smoke_language: str
    capture: Any
    result: Dict[str, Any]


class _Probe(NamedTuple):
    """The phase-zero probe as the later phases read it: its code, its
    combined Lens scores and its sandbox verdict. With no probe code it is
    unscored and not passed, and `evidence` is None."""
    code: str
    scores: Dict[str, Any]
    energy_raw: Any
    energy_norm: Any
    cx_calibrated: bool
    passed: bool
    stdout: str
    stderr: str
    evidence: Any


def _score_and_test_probe(ctx, probe_code, tokens, verified_sandbox):
    """Score the probe with the combined C(x)+G(x) call and verify it in the
    sandbox; its tokens count toward the run once it produced code."""
    emit, result = ctx.emit, ctx.result
    # Score and test probe with self-generated tests. The probe is the
    # only candidate the CxGx gate below can see, so it is scored with
    # the combined C(x)+G(x) call — one embedding extraction, both
    # models — rather than C(x) alone.
    probe_scores = dict(scoring.NEUTRAL_COMBINED)
    probe_energy_raw, probe_energy_norm = 0.0, 0.5
    probe_cx_calibrated = False
    probe_passed = False
    probe_stdout = probe_stderr = ""
    probe_evidence = None
    if probe_code:
        probe_scores = scoring.score_candidate_combined(probe_code)
        probe_energy_raw = probe_scores["cx_energy"]
        probe_energy_norm = probe_scores["cx_normalized"]
        probe_cx_calibrated = probe_scores["cx_calibrated"]
        probe_failure = probe_scores.get("failure")
        probe_scored = probe_energy_raw is not None
        if probe_scored:
            norm_label = f"{probe_energy_norm:.2f}" if probe_cx_calibrated else "uncalibrated"
            detail = (f"C(x)={probe_energy_raw:.2f} norm={norm_label} "
                      f"G(x)={float(probe_scores['gx_score']):.2f} "
                      f"({probe_scores['verdict']})")
        else:
            # No score to report: the allocator below reads the verdict
            # "unscored" and keeps its floor, and says so.
            detail = (f"unscored "
                      f"({scoring.describe_lens_failure(probe_failure or {})})")
        emit("probe_scored", detail,
             scored=probe_scored,
             gx_score=probe_scores["gx_score"],
             gx_available=probe_scores["gx_available"],
             verdict=probe_scores["verdict"],
             failure=probe_failure)
        probe_passed, probe_stdout, probe_stderr, probe_evidence = verified_sandbox(probe_code)
        emit("probe_sandbox", f"passed={probe_passed} stderr={probe_stderr[:80] if probe_stderr else ''}")
        result["total_tokens"] += tokens
    return _Probe(probe_code, probe_scores, probe_energy_raw, probe_energy_norm,
                  probe_cx_calibrated, probe_passed, probe_stdout, probe_stderr,
                  probe_evidence)
def _smoke_language(file_path):
    """The language the smoke check verifies the target file in: from its
    extension, or Python when the task names no file."""
    # Derive the language from the target file's extension. Used
    # by smoke_compile_check below to pick the right syntax checker, and
    # to keep the Python-only checks off other files. Python only when no
    # file_path is supplied (a bench task names no file).
    _ext = Path(file_path).suffix.lower() if file_path else ""
    # Only languages scoring.smoke_compile_check can actually verify —
    # an entry here that the checker rejects would fail every candidate
    # with "verification unavailable" instead of checking anything.
    _ext_to_lang = {
        ".py": "python", ".pyw": "python", ".pyi": "python",
        ".html": "html", ".htm": "html",
        ".json": "json",
        ".yaml": "yaml", ".yml": "yaml",
        ".js": "javascript", ".mjs": "javascript", ".cjs": "javascript",
        ".ts": "typescript", ".tsx": "typescript",
        ".xml": "xml",
        ".sh": "bash", ".bash": "bash",
        ".go": "go",
        ".java": "java",
        ".kt": "kotlin",
        ".rs": "rust",
        ".rb": "ruby",
        ".php": "php",
    }
    # A named file of a class the checker cannot verify keeps its own
    # name, so it fails as "verification unavailable". Defaulting it to
    # Python would parse a stylesheet, a C file or a Makefile as Python:
    # every candidate would fail with a SyntaxError that says nothing about
    # the file, and the Python-only checks would run on it.
    if file_path:
        smoke_language = _ext_to_lang.get(_ext, _ext.lstrip(".") or "unknown")
    else:
        smoke_language = "python"
    return smoke_language


def _with_file_context(problem, files):
    """The problem, with the project's existing files in front of it when
    the caller sent any."""
    # If existing file context is provided, prepend it to the problem
    # so all V3 modules (PlanSearch, PR-CoT, etc.) can see the code
    if files:
        file_context_parts = []
        for fname, content in files.items():
            file_context_parts.append(f"### Existing file: {fname}\n```\n{content}\n```")
        problem = (
            "The following files already exist in the project:\n\n"
            + "\n\n".join(file_context_parts)
            + "\n\n---\n\nTask:\n" + problem
        )
    return problem


def _client_check(progress_callback, cancel_scope, task_id):
    """The run's check_client(), which the phases call at their boundaries."""
    def check_client():
        """Abort at phase boundaries once the SSE client disconnects.
        The handler sets `disconnected` on the callback when a write
        hits BrokenPipeError; a dead client must not keep burning GPU."""
        if getattr(progress_callback, "disconnected", False):
            raise adapters.ClientDisconnected(f"client disconnected during task {task_id}")
        if cancel_scope is not None and cancel_scope.cancelled:
            raise adapters.Cancelled(f"request cancelled during task {task_id}")
    return check_client


def _new_llm(emit, start, budget_ms, cancel_scope,
             request_identity=None):
    """The run's generation adapter: it streams to emit, carries the
    request's cancel scope and identity, and stops at the run's deadline."""
    # The adapter refuses to start a generation that cannot finish
    # before the cap, so every phase and every loop inside one is
    # covered by a single check rather than a boundary guard each.
    _budget_ms = _remaining_budget_ms(start, budget_ms)
    llm = adapters.LLMAdapter(progress_callback=emit)
    # The request's cancellation handle. Every generation this adapter
    # opens registers with it, so the handler can close them all the
    # moment the parent goes away -- without waiting to discover a broken
    # SSE write, which never happens while a call is in flight.
    llm.cancel_scope = cancel_scope
    # The identity every generation this adapter opens is sent under.
    # Set beside cancel_scope because it has the same lifetime and the
    # same reason for living on the adapter: PlanSearch dispatches from
    # worker threads, and a thread inherits neither a ContextVar nor a
    # local — it only sees the request-scoped object it was handed.
    llm.request_identity = request_identity
    # Assigned rather than passed to the constructor: tests substitute
    # their own LLM doubles here, and a new required kwarg would break
    # every one of them for a value only the real adapter reads.
    if _budget_ms is not None:
        llm.deadline = start + _budget_ms / 1000.0
    return llm


def _new_result(task_id):
    """The result a run fills in: not passed, no code, no phase solved."""
    result = {
        "task_id": task_id,
        "passed": False,
        "code": "",
        "phase_solved": "none",
        "candidates_generated": 0,
        "total_tokens": 0,
        "total_time_ms": 0.0,
        "events": [],
        "verification_evidence": [],
    }
    return result


def _check_incumbent(ctx, verified_sandbox, _task, probe_code):
    """The caller's own bytes, checked as a candidate.

    Returns (incumbent, result). The incumbent is None when the caller sent
    no file; result is the finished run when the incumbent passed and its
    own record closes, else None.
    """
    baseline_code, file_path, emit, capture = ctx.baseline_code, ctx.file_path, ctx.emit, ctx.capture
    result, start, events = ctx.result, ctx.start, ctx.events
    # ===== THE INCUMBENT =====
    # The caller's own bytes are a real candidate. They face the
    # checks every generated candidate faces -- the lens, the sandbox
    # verification, the contract record, and in phase one the vetoes --
    # and the lens ranks them with the rest, so a candidate replaces them
    # only by beating them. They are the EXACT bytes the request carried:
    # nothing re-types them, and a copy that differs only in whitespace
    # is never delivered in their place (_without_retypings).
    incumbent = None
    if baseline_code:
        incumbent = {"index": INCUMBENT_INDEX, "incumbent": True,
                     "code": baseline_code, **_lens_view(baseline_code)}
        inc_passed, inc_out, inc_err, inc_evidence = verified_sandbox(baseline_code)
        incumbent.update(
            passed=inc_passed, stdout=inc_out, stderr=inc_err,
            verification_evidence=inc_evidence,
            contract_record=_evaluate_candidate(
                file_path, baseline_code, inc_passed, emit, task=_task))
        emit("incumbent",
             f"The submitted file: passed={inc_passed}"
             + (f" stderr={inc_err[:80]}" if inc_err else ""),
             passed=inc_passed, energy=incumbent.get("energy"))
        capture.note_candidate(
            role="incumbent", index=INCUMBENT_INDEX, code=baseline_code,
            accepted=inc_passed, record=incumbent["contract_record"],
            phase="incumbent", lens=_candidate_lens_payload(incumbent))
        # The same closure rule as the probe's below, asked first: a
        # submitted file whose own record closes is already the verified
        # winner, and nothing generated could outrank it.
        if inc_passed and _record_closes(incumbent["contract_record"], baseline_code):
            emit("incumbent_holds",
                 "The submitted file passed and closes — keeping it")
            result["passed"] = True
            result["code"] = baseline_code
            result["phase_solved"] = "incumbent"
            result["candidates_generated"] = 1 if probe_code else 0
            result["total_time_ms"] = (time.time() - start) * 1000
            result["verification_evidence"] = inc_evidence
            result["winning_score"] = incumbent.get("energy_norm") or 0.0
            result["evidence_record"] = incumbent["contract_record"]
            result["events"] = events
            return incumbent, result
    return incumbent, None


def _evaluate_probe(ctx, probe, _task):
    """Candidate zero's contract record, kept in the result and the capture.

    Returns (probe_result, result): result is the finished run when the
    probe passed and its own record closes, else None.
    """
    file_path, emit, capture, result, start, events = (
        ctx.file_path, ctx.emit, ctx.capture, ctx.result, ctx.start, ctx.events)
    probe_code, probe_passed, probe_evidence = probe.code, probe.passed, probe.evidence
    probe_scores, probe_energy_raw, probe_energy_norm = probe.scores, probe.energy_raw, probe.energy_norm
    probe_cx_calibrated = probe.cx_calibrated
    probe_result = _evaluate_candidate(
        file_path, probe_code, probe_passed, emit, task=_task)
    probe_adapter = probe_result["adapter_id"]
    result["evidence_record"] = probe_result
    capture.note_candidate(
        role="candidate_zero", index=0, code=probe_code,
        accepted=probe_passed, record=probe_result, phase="probe",
        lens={"energy": probe_energy_raw, "energy_norm": probe_energy_norm,
              "energy_calibrated": probe_cx_calibrated,
              "token_assertion": probe_scores.get("token_assertion"),
              "gx_score": probe_scores.get("gx_score"),
              "verdict": probe_scores.get("verdict"),
              "failure": probe_scores.get("failure")})
    emit("probe_evidence",
         f"adapter={probe_adapter} strength={probe_result['evidence_strength']} "
         f"supported={probe_result['supported']}",
         adapter=probe_adapter, strength=probe_result["evidence_strength"])

    # Early return is contract.select over the candidate's own record: it
    # closes only when that exact record is the VERIFIED WINNER
    # under its own rubric. A best record that is not closure-eligible,
    # anything unsupported, failed or incomparable, and any record whose
    # hash does not match these bytes, all leave the pipeline open. The
    # strength floor comes from the contract, so an artifact class whose
    # contract closes on syntax legitimately may, and one that demands
    # behaviour still cannot close on a compile.
    if probe_passed and _record_closes(probe_result, probe_code):
        emit("probe_pass", "Probe passed — returning early")
        result["passed"] = True
        result["code"] = probe_code
        result["phase_solved"] = "probe"
        result["candidates_generated"] = 1
        result["total_time_ms"] = (time.time() - start) * 1000
        result["verification_evidence"] = probe_evidence
        result["winning_score"] = probe_energy_norm
        result["events"] = events
        return probe_result, result
    return probe_result, None


def _allocate(ctx, probe, diagnostic_total_candidates):
    """Phase 2: k and the thinking tier from the CxGx gate, on the probe's
    Lens scores and the clock; a diagnostic floor may raise k. Returns
    (k, budget_tier, bf_tier)."""
    check_client, emit, llm, start, budget_ms = ctx.check_client, ctx.emit, ctx.llm, ctx.start, ctx.budget_ms
    probe_energy_norm, probe_cx_calibrated, probe_scores = probe.energy_norm, probe.cx_calibrated, probe.scores
    # ===== PHASE 2: CxGx K ALLOCATION =====
    # The probe failed verification, so this task is not trivial: C(x)
    # picks a base tier, G(x) escalates it, and k never drops below the
    # gate's k=3 floor.
    #
    # On the live path the proxy's V3 bridge abandons this call at its
    # cap (ATLAS_V3_TIMEOUT, default 300s, and at most half the session's
    # remaining time). An unbounded escalation to k=8 here would spend the
    # whole budget on generation and hand the user a timeout fallback
    # instead of the k=3 answer the clock could have produced. So the
    # remaining wall-clock and the per-call latency observed on THIS task
    # go into the allocation, and the gate lowers the tier to what the
    # budget can actually generate. The floor is not budget-dependent.
    check_client()
    emit("phase2", "Allocating compute budget...")
    alloc = cxgx_gate.allocate(
        cx_normalized=probe_energy_norm,
        cx_calibrated=probe_cx_calibrated,
        gx_score=probe_scores["gx_score"],
        gx_available=probe_scores["gx_available"],
        gx_verdict=probe_scores["verdict"],
        remaining_ms=_remaining_budget_ms(start, budget_ms),
        observed_llm_call_ms=getattr(llm, "avg_call_ms", 0.0),
    )
    k, budget_tier = alloc.k, alloc.tier
    bf_tier = budget_tier
    # Diagnostic floor: raises the pool, never lowers it, and leaves the
    # tier and every budget guard exactly as allocated.
    if diagnostic_total_candidates and diagnostic_total_candidates > k:
        emit("diagnostic_allocation",
             f"k raised {k} -> {diagnostic_total_candidates} for measurement",
             allocated_k=k, diagnostic_k=diagnostic_total_candidates)
        k = diagnostic_total_candidates
    emit("phase2_allocated", f"k={k} tier={budget_tier}",
         k=k, tier=budget_tier, base_tier=alloc.base_tier,
         gx_escalation=alloc.gx_escalation,
         capped_from=alloc.capped_from, reason=alloc.reason)
    return k, budget_tier, bf_tier


@dataclasses.dataclass
class _RunPool:
    """The selectable pool of one run from phase one on: the incumbent, when
    the caller sent one, and the generated candidates, the probe first when
    it produced code. `probe_code` names candidate zero in the capture."""
    incumbent: Optional[Dict[str, Any]]
    candidates: List[Dict[str, Any]]
    probe_code: str

    def members(self):
        """The selectable pool: the incumbent, when the caller sent one,
        and every generated candidate. `candidates` stays the generated
        ones alone, so `k` counts generations and nothing else."""
        incumbent, candidates = self.incumbent, self.candidates
        return ([incumbent] if incumbent else []) + candidates


def _out_of_budget(ctx, reserve_ms: Optional[float] = None) -> bool:
    """True when too little of the budget is left to start more work and
    still hand back a result.

    The reserve defaults to the cost of one more LLM call as observed on
    this run (x1.2) plus 10s to serialize the result, and never less than
    20s. A flat reserve does not work: the unit of work is a generation,
    so that is what has to fit.
    """
    start, budget_ms, llm = ctx.start, ctx.budget_ms, ctx.llm
    left = _remaining_budget_ms(start, budget_ms)
    if left is None:
        return False
    if reserve_ms is None:
        observed = getattr(llm, "avg_call_ms", 0.0) or 0.0
        reserve_ms = max(20000.0, observed * 1.2 + 10000.0)
    return left < reserve_ms


def _finish_with_best(ctx, run_pool, reason: str) -> Dict[str, Any]:
    """Hand back the best candidate found so far.

    V3 is an anytime algorithm: the refinement loop is budget-gated, so it
    expands to fill the budget rather than converging early, and the clock
    is its terminal condition.

    An anytime algorithm whose clock expires has to return its best
    answer; returning nothing lets the caller's deadline cancel the
    request, discard every verified candidate, and fall back to the
    model's own output.

    Vetoed candidates stay excluded for the same reason the end-of-run
    fallback excludes them: "executes but is wrong" is worse than an
    honest failure.
    """
    emit, capture, result, start, budget_ms, events = (
        ctx.emit, ctx.capture, ctx.result, ctx.start, ctx.budget_ms, ctx.events)
    candidates, members, probe_code = run_pool.candidates, run_pool.members, run_pool.probe_code
    emit("budget_exhausted", reason,
         candidates=len(candidates),
         remaining_ms=round(_remaining_budget_ms(start, budget_ms) or 0))
    pool = [c for c in members() if not c.get("vetoed_by")]
    passing = _without_retypings([c for c in pool if c.get("passed")], emit)
    chosen = None
    if passing:
        passing.sort(key=energy_rank_key)
        chosen = passing[0]
        result["passed"] = True
        if chosen.get("incumbent"):
            result["phase_solved"] = "incumbent"
        else:
            result["phase_solved"] = "budget"
    else:
        # No code rather than an unverified candidate. The caller's
        # baseline is the model's own write, which is syntax- and
        # structure-gated; a candidate that failed the sandbox is
        # not better than that, and ranking the failures by energy
        # picks among them without evidence.
        #
        # Measured across a 28-session run: 7 of 8 returns took the
        # unverified path and task success fell 20/28 to 17/28 —
        # aoc_course and aoc_slope went 2/2 to 0/2 — while harness
        # integrity reached 28/28. The same reasoning the vetoed
        # path already uses: "executes but is wrong" is worse than
        # an honest failure.
        emit("budget_no_verified_candidate",
             f"{len(pool)} candidate(s), none verified — "
             f"leaving the caller's gated baseline in place")
    # A budget boundary can land after Lens scoring but before the
    # sandbox loop.  Preserve those scored candidates as explicitly
    # unverified observations so diagnostic capture accounts for
    # every pre-score tokenization/embedding without changing which
    # candidate the live pipeline returns.
    for candidate in pool:
        _capture_pool_member(capture, candidate, probe_code)
    capture.note_pool(
        phase="budget", pool=passing,
        lens_index=(chosen or {}).get("index"),
        status="budget_exhausted", reason=reason,
        ineligible=len(pool) - len(passing))
    if chosen is not None:
        result["code"] = chosen["code"]
        result["verification_evidence"] = chosen.get("verification_evidence", [])
        result["winning_score"] = chosen.get("energy_norm", 0.0)
    result["total_time_ms"] = (time.time() - start) * 1000
    result["events"] = events
    return result


def _seed_with_probe(candidates, probe, probe_result):
    """Phase one's first candidate: the probe, as candidate 0 with its
    phase-zero evidence, when it produced code."""
    probe_code, probe_scores, probe_passed = probe.code, probe.scores, probe.passed
    probe_energy_raw, probe_energy_norm, probe_cx_calibrated = probe.energy_raw, probe.energy_norm, probe.cx_calibrated
    probe_stdout, probe_stderr, probe_evidence = probe.stdout, probe.stderr, probe.evidence
    # Start with probe if it produced code
    if probe_code:
        candidates.append({
            # Phase-zero evidence is cached here so candidate zero is
            # never re-probed, and never enters selection with defaults.
            # One canonical record, no parallel copies of its fields.
            "contract_record": probe_result,
            "index": 0, "code": probe_code,
            "energy": probe_energy_raw, "energy_norm": probe_energy_norm,
            "energy_calibrated": probe_cx_calibrated,
            "lens_failure": probe_scores.get("failure"),
            "passed": probe_passed, "stdout": probe_stdout, "stderr": probe_stderr,
            "verification_evidence": probe_evidence,
        })


def _test_pool(ctx, run_pool, verified_sandbox, _task):
    """Sandbox-test every pool member, easiest first, and give each its
    contract record. The incumbent and a candidate already verified keep
    their verdict. Returns (pool_members, passing)."""
    emit, check_client, capture, file_path = ctx.emit, ctx.check_client, ctx.capture, ctx.file_path
    candidates, probe_code = run_pool.candidates, run_pool.probe_code
    # ===== SANDBOX TESTING =====
    # The incumbent is in the pool from here on: tested once already,
    # ranked with the rest.
    pool_members = run_pool.members()
    emit("sandbox_test", f"Testing {len(pool_members)} candidates...",
         candidates=len(pool_members))
    # Sort by energy (easy first) for early-exit potential; an
    # unscored candidate has no energy and goes last.
    candidates.sort(key=energy_rank_key)
    pool_members.sort(key=energy_rank_key)

    passing = []
    for c in pool_members:
        check_client()
        if c.get("incumbent"):
            # Checked once, above, on its exact bytes; a failure is
            # final and it enters repair like any failing candidate.
            _capture_pool_member(capture, c, probe_code)
            if c.get("passed"):
                passing.append(c)
            continue
        if c.get("passed"):
            # Cached execution result -- do NOT skip evidence. A compile
            # smoke sets passed=True for an interactive artifact, so
            # continuing here without a record would let candidate zero
            # enter the pool with no behavioural evidence, and rank_key
            # would see defaults for it.
            if "contract_record" not in c:
                c["contract_record"] = _evaluate_candidate(
                    file_path, c["code"], True, emit, task=_task)
            _capture_pool_member(capture, c, probe_code)
            passing.append(c)
            continue
        sb_start = time.time()
        passed, stdout, stderr, verification_evidence = verified_sandbox(c["code"])
        sb_ms = int((time.time() - sb_start) * 1000)
        c["passed"] = passed
        c["stdout"] = stdout
        c["stderr"] = stderr
        c["verification_evidence"] = verification_evidence
        # ONE structured evaluation path: candidate zero and every
        # generated candidate get the same adapter, the same probe and
        # the same evidence record. A second set of verification
        # semantics would leave generated candidates with only a boolean,
        # and ATLAS could not rank the alternatives it generates.
        c["contract_record"] = _evaluate_candidate(
            file_path, c["code"], passed, emit, task=_task)
        _capture_pool_member(capture, c, probe_code)
        if passed:
            passing.append(c)
            emit("sandbox_pass", f"Candidate {c['index']} passed",
                 index=c["index"], elapsed_ms=sb_ms,
                 energy=c.get("energy_norm", 0.0))
        else:
            emit("sandbox_fail", f"Candidate {c['index']} failed",
                 index=c["index"], elapsed_ms=sb_ms,
                 stderr=(stderr or "")[:120])

    emit("sandbox_done", f"{len(passing)}/{len(pool_members)} passed",
         passed=len(passing), total=len(pool_members))
    return pool_members, passing


def _agree_by_consensus(ctx, passing, pool_members, self_tests):
    """When nothing passed, the candidates that agree on the generated
    inputs (CodeT) become selectable, marked consensus. Returns passing."""
    sandbox, emit, task_input_file = ctx.sandbox, ctx.emit, ctx.task_input_file
    # Nothing passed: every candidate failed to execute (or, outside
    # Python, its syntax check) or failed the project's build command.
    # The generated cases decided none of that -- they carry no
    # rejection authority -- so this runs whenever nothing passed and
    # a generated suite exists.
    #
    # Fall back to the agreement signal CodeT actually uses: run the
    # candidates on the generated INPUTS and take the largest cluster
    # that produced the same answers. The generated expected outputs
    # are not used.
    #
    # Agreement is not verification. Winners are marked `consensus`,
    # never `passed`, and a candidate that failed a project check is
    # left out: a candidate that failed the project's own build
    # command is still one that failed it.
    if not passing and self_tests and self_tests.test_cases:
        agreed = _consensus_winners(
            [c for c in pool_members if not _failed_a_project_check(c)],
            self_tests.test_cases, sandbox, emit, task_input_file)
        for c in agreed:
            c["consensus"] = True
            passing.append(c)
        if agreed:
            emit("sandbox_done",
                 f"0/{len(pool_members)} passed; {len(agreed)} agree, selectable by consensus",
                 passed=0, agreed=len(agreed), total=len(pool_members))
    return passing


def _select(ctx, passing):
    """Candidate selection. The Lens picks the lowest-energy candidate that
    passed (or agrees, by consensus); contract selection over the records is
    what the evidence envelope reports. Returns the finished run, or None
    when no candidate is selectable."""
    emit, capture, result = ctx.emit, ctx.capture, ctx.result
    # ===== CANDIDATE SELECTION =====
    # Lens selection: minimum C(x) energy among the passing candidates.
    # (S* tiebreaking used to run first for 2+ passers; across 118 H200
    # tiebreaks every pair scored 0-0 and 110/110 winners equaled the
    # lens min-energy pick, so it carried zero discriminating signal.)
    #
    # The incumbent is ranked like any other candidate. A rule
    # that kept it on every tie was measured against: in 3 of 23
    # recorded AoC deliveries the model's own file was wrong yet ran
    # cleanly, and a lens-ranked fresh candidate was right.
    passing = _without_retypings(passing, emit)
    if passing:
        ci_list = [
            CandidateInfo(c["index"], c["code"], c["energy"],
                          bool(c["passed"] or c.get("consensus")))
            for c in passing
        ]
        selected = select_candidate(ci_list, strategy="lens")

        _select_by_contract(passing, selected, result, capture)

        if selected:
            winner = _announce_selection(emit, passing, selected)
            return _deliver_selected(ctx, selected, winner)
    return None


def _deliver_selected(ctx, selected, winner):
    """The finished run of a selected candidate: its code, the phase it
    solved in, its evidence and its score."""
    result, start, events = ctx.result, ctx.start, ctx.events
    result["passed"] = True
    result["code"] = selected.code
    if (winner or {}).get("consensus"):
        # A phase the proxy's verifiedPhase does not list: the
        # pick rests on agreement, not on a check it passed.
        result["phase_solved"] = "consensus"
    elif (winner or {}).get("incumbent"):
        # The caller's own bytes held. The proxy reads that as
        # no proposal, whatever the phase says.
        result["phase_solved"] = "incumbent"
    else:
        result["phase_solved"] = "phase1"
    result["total_time_ms"] = (time.time() - start) * 1000
    result["verification_evidence"] = (winner or {}).get("verification_evidence", [])
    result["winning_score"] = (winner or {}).get("energy_norm", 0.0)
    result["evidence_record"] = ((winner or {}).get("contract_record")
                                 or result.get("evidence_record"))
    result["events"] = events
    return result


def _test_and_select(ctx, run_pool, verified_sandbox, self_tests, _task):
    """Phase one after generation: sandbox-test the pool, fall back to
    consensus, apply the three vetoes and select. Returns the finished run,
    or None when nothing is selectable."""
    emit, files, file_path, smoke_language = ctx.emit, ctx.files, ctx.file_path, ctx.smoke_language
    pool_members, passing = _test_pool(ctx, run_pool, verified_sandbox, _task)
    passing = _agree_by_consensus(ctx, passing, pool_members, self_tests)
    passing = _lens_veto(passing, emit)
    passing = _structural_veto(passing, files, smoke_language, emit)
    passing = _call_graph_veto(passing, files, file_path, smoke_language, emit)
    return _select(ctx, passing)


def _enter_repair(ctx, run_pool, task_type):
    """Phase 3's entry. With the budget spent, or for an interactive task
    whose submitted baseline compiles, the best so far is the result: it is
    returned. Else None, and repair runs."""
    check_client, emit, sandbox = ctx.check_client, ctx.emit, ctx.sandbox
    baseline_code, smoke_language, file_path = ctx.baseline_code, ctx.smoke_language, ctx.file_path
    # ===== PHASE 3: VERIFIED ITERATIVE REFINEMENT =====
    check_client()
    if _out_of_budget(ctx):
        return _finish_with_best(ctx, run_pool, "budget spent before the repair phase")
    # Repair-budget rule: if every candidate failed and the submitted
    # interactive baseline compiles, return to the outer loop with that
    # baseline rather than spending more
    # generations. An import comparison rejects observed regressions;
    # it does not prove requested features or justify relaxing this
    # rule. A compiling baseline is NOT necessarily a runnable app.
    if task_type == "interactive" and baseline_code:
        base_ok, _, _ = scoring.smoke_compile_check(
            baseline_code, sandbox, language=smoke_language, filename=file_path)
        if base_ok:
            emit("repair_skip_baseline_ok",
                 "interactive task and the baseline compiles — no verified "
                 "replacement; retaining the baseline under the existing "
                 "repair-budget rule",
                 strategy="repair", task_type=task_type)
            return _finish_with_best(
                ctx, run_pool, "interactive task: baseline meets the compile bar, repair skipped")
    emit("phase3", "All candidates failed — entering repair phase...",
         failing=len([c for c in run_pool.members() if not c.get("passed")]))
    return None


def _failing_candidates(run_pool):
    """Every pool member that did not pass, easiest first, as the
    FailingCandidate list repair works on."""
    failing = [
        FailingCandidate(
            index=c["index"], code=c["code"],
            error_output=c.get("stderr", ""),
        )
        # The incumbent too: the caller's own file is a candidate, and
        # when it fails, it is the likeliest one to repair.
        for c in sorted(run_pool.members(), key=energy_rank_key) if not c.get("passed")
    ]
    return failing


def _call_chain_context(ctx, failing):
    """The call-graph context of the first failing candidate's failing
    function, built once for both repair strategies; "" when there is none."""
    files, emit = ctx.files, ctx.emit
    # Build call-graph context for the failing
    # function once, reuse across PR-CoT + refinement. Skips
    # cleanly when stderr isn't a Python traceback or the failing
    # function isn't defined in the project — both arms get plain
    # error_output in that case. The block is a multi-hop
    # reachability slice (entry-point path, transitive impact,
    # callees). Fail-soft on any graph failure.
    chain_context_block = ""
    if failing:
        failing_func = symbols._failing_function_from_stderr(failing[0].error_output)
        if failing_func and files:
            try:
                from graph import repair_context as _cg_repair
                chain_context_block = _cg_repair(files, failing_func, transitive=True)
            except Exception as cge:
                print(f"  [phase3] graph repair-context skipped: {cge}", flush=True)
            if chain_context_block:
                emit("call_chain_context",
                     f"Built call-chain for failing `{failing_func}`",
                     function=failing_func)
                print(
                    f"  [phase3] call-chain context built for `{failing_func}`",
                    flush=True,
                )
    return chain_context_block


def _enriched_error(stderr: str, chain_context_block: str) -> str:
    """Append call-chain context to a candidate's stderr if available."""
    if not chain_context_block:
        return stderr
    return (stderr or "") + "\n\n" + chain_context_block


def _check_repair(ctx, failing, pr_result, repair_code, verified_sandbox, _task):
    """One PR-CoT repair: its cost and lineage, its sandbox verification, and
    its capture record. Returns (passed, its verification evidence)."""
    file_path, emit, capture = ctx.file_path, ctx.emit, ctx.capture
    # Lineage: a repair is a child of the candidate it was
    # asked to fix. Without the link a repaired artifact
    # looks like a fresh generation and its parent's
    # evidence cannot be found again.
    capture.note_cost(
        code=repair_code or None, phase="repair_pr_cot",
        tokens=getattr(pr_result, "total_tokens", 0),
        latency_ms=getattr(pr_result, "total_time_ms", 0.0),
        parent_code=(failing[0].code if failing else None))
    passed, _, _, repair_evidence = verified_sandbox(repair_code)
    # Repairs are late-generated candidates.  In evidence
    # mode they need the same observational Lens record as
    # phase-one candidates; otherwise a successful repair
    # is captured with an empty Lens object and the frozen
    # diagnostic cannot prove its pre-score token bound.
    # Keep this capture-only so ordinary product behavior
    # and live repair selection remain unchanged.
    repair_lens = (_lens_view(repair_code)
                   if capture.enabled else None)
    # A repair is a candidate, and a candidate with no record
    # is one nothing can say anything about. Same canonical
    # adapter->contract path as every other candidate; it adds
    # no generation and no sandbox run of its own.
    capture.note_candidate(
        role="repair", index=None, code=repair_code,
        accepted=passed,
        record=_evaluate_candidate(
            file_path, repair_code, passed, emit,
            task=_task),
        phase="repair_pr_cot", lens=repair_lens)
    return passed, repair_evidence


def _refinement_affordable(ctx, failing):
    """Strategy 2's gate: the refinement loop runs only when there is a
    failing candidate and the remaining clock affords one iteration."""
    emit, llm, start, budget_ms = ctx.emit, ctx.llm, ctx.start, ctx.budget_ms
    # Strategy 2: Refinement loop, entered only when the remaining
    # wall-clock can afford one iteration (~3 sequential LLM calls,
    # estimated at the per-call latency observed on THIS run). The
    # budget is the cap the proxy's V3 bridge enforces (budget_ms, else
    # ATLAS_V3_TIMEOUT): starting work the bridge will abandon only
    # delays the fallback the user ends up with.
    run_refinement = bool(failing)
    if run_refinement:
        est_ms = estimate_iteration_ms(getattr(llm, "avg_call_ms", 0.0))
        remaining_ms = _remaining_budget_ms(start, budget_ms)
        if (remaining_ms is not None
                and not can_afford_iteration(remaining_ms, est_ms)):
            run_refinement = False
            emit("refinement_skip",
                 f"remaining budget {remaining_ms / 1000:.0f}s cannot "
                 f"afford one iteration (~{est_ms / 1000:.0f}s) — "
                 f"skipping to fallback",
                 strategy="refinement",
                 remaining_ms=round(remaining_ms),
                 estimated_iteration_ms=round(est_ms))
    if run_refinement and _out_of_budget(ctx):
        run_refinement = False
    return run_refinement


def _leave_baseline(ctx, run_pool):
    """The end of a run with nothing verified: no code, so the caller keeps
    its own gated baseline. Returns the result."""
    emit, result, start, events = ctx.emit, ctx.result, ctx.start, ctx.events
    # ===== FALLBACK: nothing passed, so no code is returned =====
    # The caller keeps its baseline, the model's own write, which is
    # syntax- and structure-gated; a candidate that failed the sandbox is
    # not better than that, and ranking failures by energy picks among
    # them without evidence.
    #
    # Vetoed candidates are excluded outright: a veto means "executes but
    # is wrong" (stub, NameError-in-waiting), which is worse than an
    # honest sandbox failure. Same reasoning as the budget boundary.
    unverified = [c for c in run_pool.members() if not c.get("vetoed_by")]
    if unverified:
        emit("fallback_unverified",
             f"{len(unverified)} candidate(s), none passed verification — "
             f"leaving the caller's gated baseline in place")
    elif run_pool.members():
        emit("fallback_all_vetoed",
             "Every candidate was vetoed — returning no code")
    result["total_time_ms"] = (time.time() - start) * 1000
    result["events"] = events
    return result


class _CandidateVerifier:
    """verified_sandbox(code) of one run: sandbox + verification of one
    candidate, as steps.

    It holds what every check reads: the run context, the task type, the
    import comparison against the submitted file (None when it does not
    apply), whether the generated cases are a trusted oracle, and
    `self_tests`, the run's generated suite. Phase 3 sets `self_tests` again
    when it generates a suite, so a repair runs the new cases.
    """

    def __init__(self, ctx, task_type, self_tests, import_comparison, has_trusted_oracle):
        self.ctx = ctx
        self.task_type = task_type
        self.self_tests = self_tests
        self.import_comparison = import_comparison
        self.has_trusted_oracle = has_trusted_oracle

    def __call__(self, code):
        """Sandbox + verification. Algorithmic tasks: execution, with the
        I/O self-tests recorded as diagnostics; interactive: compile smoke.

        A replacement for a file the caller already wrote must first keep
        that file's role: every top-level name it binds. It runs
        here, ahead of the sandbox, so every path that can hand back code
        -- phase one, PR-CoT, refinement, the budget boundary -- is held
        to it, and the reason reaches repair as the error to fix."""
        sandbox, smoke_language, task_type = self.ctx.sandbox, self.ctx.smoke_language, self.task_type
        verification_evidence: List[Dict[str, Any]] = []

        failed_role = self._check_role(code, verification_evidence)
        if failed_role is not None:
            return failed_role

        # Non-Python candidates always use the language-aware syntax path.
        # Python self-tests cannot establish correctness for another language.
        if smoke_language not in ("python", "py"):
            return self._smoke_other_language(code, verification_evidence)

        # Interactive tasks: skip the run-and-test; just verify the code
        # parses and compiles. Running curses/pygame/flask in the sandbox
        # would fail for environmental reasons (no TTY, no display) even
        # when the code is correct.
        if task_type == "interactive":
            return self._smoke_interactive(code, verification_evidence)

        ok, out, err = sandbox(code)
        if not ok:
            return False, out, err, verification_evidence
        self._record_self_tests(code)
        # The verdict is the part that does not depend on the self-test
        # cases: the candidate executed, and the project's own build
        # command still gets to speak.
        return self._verify_build_if_requested(code, verification_evidence, out, err)

    def _check_role(self, code, verification_evidence):
        """The submitted file's role: a replacement must keep every
        top-level name the file binds. Returns the failed verdict, or None."""
        baseline_code, file_path, files, emit = self.ctx.baseline_code, self.ctx.file_path, self.ctx.files, self.ctx.emit
        if baseline_code and code != baseline_code:
            dropped = symbols.dropped_top_level_names(
                baseline_code, code, file_path, files)
            if dropped:
                shown = ", ".join(dropped[:6]) + (" and more" if len(dropped) > 6 else "")
                reason = (f"role check: this candidate drops {shown}, which "
                          f"{Path(file_path).name if file_path else 'the file'} "
                          f"has now. A replacement must keep every top-level "
                          f"name the file binds.")
                emit("role_check", reason, dropped=dropped)
                verification_evidence.append({
                    "verifier": "incumbent_role", "status": "failed",
                    "dropped": dropped, "stderr": reason})
                return False, "", reason, verification_evidence
        return None

    def _verify_build_if_requested(self, code, verification_evidence, out="", err=""):
        """The checks after the syntax check or the run: the import comparison
        against the submitted file, then the project's build command."""
        import_comparison, emit, sandbox = self.import_comparison, self.ctx.emit, self.ctx.sandbox
        build_command, file_path, files, working_dir = (
            self.ctx.build_command, self.ctx.file_path, self.ctx.files, self.ctx.working_dir)
        if import_comparison is not None:
            admitted, diagnostic, runtime_evidence = import_comparison.check(code)
            verification_evidence.append(runtime_evidence)
            emit("runtime_compare", runtime_evidence["status"],
                 status=runtime_evidence["status"], admitted=admitted,
                 reason=runtime_evidence.get("reason", ""),
                 candidate_hash=runtime_evidence["candidate_hash"])
            if not admitted:
                return False, out, diagnostic, verification_evidence
        ok, build_out, build_err, evidence = scoring.verify_build_command(
            code=code,
            sandbox=sandbox,
            build_command=build_command,
            file_path=file_path,
            project_files=files,
            working_dir=working_dir or DEFAULT_WORKING_DIR,
            emit=emit,
        )
        if evidence:
            verification_evidence.append(evidence)
        if not ok:
            return False, build_out, build_err, verification_evidence
        return True, out, err, verification_evidence

    def _smoke_other_language(self, code, verification_evidence):
        """A candidate in another language than Python: its language-aware
        syntax check, then the checks after it."""
        sandbox, smoke_language, file_path, emit = self.ctx.sandbox, self.ctx.smoke_language, self.ctx.file_path, self.ctx.emit
        ok, out, err = scoring.smoke_compile_check(code, sandbox, language=smoke_language, filename=file_path)
        emit("smoke_check", f"compile={'OK' if ok else 'FAIL'} ({smoke_language})")
        if not ok:
            return ok, out, err, verification_evidence
        return self._verify_build_if_requested(code, verification_evidence, out, err)

    def _smoke_interactive(self, code, verification_evidence):
        """An interactive Python task: the compile smoke and the interactive
        lint instead of a run, then the checks after them."""
        sandbox, smoke_language, file_path, emit = self.ctx.sandbox, self.ctx.smoke_language, self.ctx.file_path, self.ctx.emit
        # Pass the detected language so HTML/JSON/etc. files
        # don't get parsed as Python (which produces spurious
        # SYNTAX_ERROR cascades into PR-CoT repair + LLM timeouts).
        ok, out, err = scoring.smoke_compile_check(code, sandbox, language=smoke_language, filename=file_path)
        emit("smoke_check", f"compile={'OK' if ok else 'FAIL'} ({smoke_language})")
        if not ok:
            return ok, out, err, verification_evidence
        # Interactive lint is Python-AST based — only meaningful for
        # Python files. Skip for HTML/CSS/JSON/etc.
        if smoke_language not in ("python", "py"):
            return True, out, err, verification_evidence
        # Interactive lint: catch raw stdin reads / blocking input loops
        # that compile fine but don't actually work for keystroke
        # handling.
        lint_ok, lint_reason = scoring.interactive_lint(code)
        if lint_ok:
            emit("interactive_lint",
                 f"SKIPPED ({lint_reason})" if lint_reason else "OK")
            return self._verify_build_if_requested(code, verification_evidence, out, err)
        emit("interactive_lint", f"FAIL: {lint_reason}")
        return False, out, f"interactive_lint: {lint_reason}", verification_evidence

    def _record_self_tests(self, code):
        """Run the generated cases on a candidate that executed and record its
        score. Diagnostics only: the cases carry no rejection authority."""
        self_tests, sandbox, task_input_file = self.self_tests, self.ctx.sandbox, self.ctx.task_input_file
        capture, emit, _has_trusted_oracle = self.ctx.capture, self.ctx.emit, self.has_trusted_oracle
        if self_tests and self_tests.test_cases:
            p = 0
            observed = []
            for i, tc in enumerate(self_tests.test_cases):
                try:
                    tc_code, tc_files = _make_self_test(
                        code, tc, task_input_file)
                    tp, to, te = sandbox(tc_code, files=tc_files)
                    if tp and "SELF_TEST_PASS" in to:
                        p += 1
                    observed.append(_capture_case(i, tc, tp, to, te))
                except Exception as ex:
                    observed.append(_capture_case(i, tc, False, "", "",
                                                  harness_error=str(ex)[:200]))
            total = len(self_tests.test_cases)
            # p and total are computed here and discarded at the return
            # below, which hands back a boolean. Capture keeps them,
            # and the per-case pairs behind them, without touching the
            # verdict.
            capture.note_oracle(code, observed, p, total)
            emit("self_test_verify", f"{p}/{total} passed")
            if not _has_trusted_oracle:
                # Observed, recorded, and given no authority. These cases
                # were written by the same model as the candidate, from
                # the problem statement alone, so letting them reject would
                # discard candidates that match the task's own reference.
                # The score and every per-case pair stay in telemetry and
                # in the pool capture for offline analysis; nothing
                # downstream reads them as a verdict.
                emit("self_test_untrusted",
                     f"{p}/{total} against model-generated cases — "
                     f"diagnostic only, no rejection authority",
                     cases=total, passed_cases=p,
                     provenance=PROVENANCE_GENERATED)


def _new_verifier(ctx, task_type, self_tests):
    """The run's verified_sandbox, with the trusted-oracle answer and the
    import comparison it needs, both settled before its first call."""
    sandbox, baseline_code, file_path, working_dir = ctx.sandbox, ctx.baseline_code, ctx.file_path, ctx.working_dir
    smoke_language, start, budget_ms, check_client = ctx.smoke_language, ctx.start, ctx.budget_ms, ctx.check_client
    # Computed once, before verified_sandbox's first caller (the phase-0
    # probe). The phase-3 regeneration below also writes generated cases,
    # so it cannot change the answer.
    _has_trusted_oracle = _trusted_oracle(self_tests)

    import_comparison = None
    if task_type == "interactive" and smoke_language == "python" and baseline_code and file_path:
        import_comparison = PythonImportComparison(
            sandbox, baseline_code, file_path, working_dir,
            remaining_ms=lambda: _remaining_budget_ms(start, budget_ms), check_cancel=check_client)
    return _CandidateVerifier(ctx, task_type, self_tests, import_comparison, _has_trusted_oracle)


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
