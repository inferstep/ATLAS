"""The steps of one V3 pipeline run and the candidate verifier.
V3PipelineService._run_impl in pipeline.py calls these steps in order:
the probe, the allocation, the pool tests, the selection and the repair.
_RunContext holds the values of one run that no step reassigns, and _RunPool
holds the selectable pool. _CandidateVerifier checks each candidate in the
sandbox."""

import dataclasses
import os
import time
from pathlib import Path
from typing import Any, Dict, List, NamedTuple, Optional

from stages import cxgx_gate
from stages.failure_analysis import FailingCandidate
from stages.refinement_loop import can_afford_iteration, estimate_iteration_ms
from stages.self_test_gen import PROVENANCE_GENERATED
from stages.candidate_selection import (CandidateInfo, energy_rank_key,
                                        select_candidate)

import adapters
import scoring
import symbols
from runtime_verification import PythonImportComparison
from candidate_records import (INCUMBENT_INDEX, _announce_selection, _candidate_lens_payload,
                               _lens_view, _record_closes, _select_by_contract,
                               _without_retypings)
from vetoes import _call_graph_veto, _lens_veto, _structural_veto
from pool_capture import _capture_case, _capture_pool_member
from candidate_cases import (_consensus_winners, _evaluate_candidate, _failed_a_project_check,
                             _make_self_test, _trusted_oracle)

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
