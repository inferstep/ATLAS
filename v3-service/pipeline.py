"""The V3 pipeline orchestrator. V3PipelineService runs one task through its
phases (probe, allocation, generation, sandbox verification, selection,
repair) as the steps defined here. Also here: the benchmark-only pool
capture, the self-test and output-probe case wrappers with the CodeT
consensus over them, the candidate verifier, and the /v3/generate problem
builder. Stage telemetry, the candidate records and the vetoes are modules
of their own beside this one."""

import base64
import dataclasses
import fcntl
import hashlib
import json
import os
import re
import stat
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
                                  PROVENANCE_GENERATED, PROVENANCE_TRUSTED)
from stages.candidate_selection import (CandidateInfo, energy_rank_key,
                                        select_candidate)

import adapters
import contract
import scoring
import symbols
from runtime_verification import PythonImportComparison
from stage_telemetry import _progress_emitter, _resolve_telemetry_dir, _summarize_phases
from candidate_records import (INCUMBENT_INDEX, _announce_selection, _candidate_lens_payload,
                               _ensure_delivered_evidence, _lens_view, _note_lens, _record_closes,
                               _select_by_contract, _task_identity, _without_retypings)
from vetoes import _call_graph_veto, _lens_veto, _structural_veto

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


# --- Benchmark-only candidate-pool capture -----------------------------------
#
# Verification hands the rest of the pipeline one bit: accepted or not. A
# candidate that passed 9 of 10 generated cases and one that passed none
# arrive at the contract record identically, and the rejected candidates'
# bytes are gone the moment the run returns. That is what made the measured
# "every suite scored 0/N" pattern unattributable — nothing retained could
# separate a wrong candidate from a wrong answer key.
#
# This writes the pool, the per-case expected/actual pairs and the selection
# identities to one append-only file so the question can be settled offline.
# It is a measurement instrument, not a feature: unset means no file is
# opened, capture decides nothing, and every failure mode disables capture
# rather than touching the run. Candidate source appears here and nowhere
# else — never in a response, an SSE frame, telemetry or a log line.

CAPTURE_ENV = "ATLAS_V3_CAPTURE_POOL"
CAPTURE_SCHEMA = "atlas.v3_candidate_capture/1"

# Sizing, from the Suite A run this instrument exists to explain: candidates
# averaged 1.9 KB (p95 4.1 KB, max 13 KB), at most 3 per generation call and
# roughly 2 calls per task. A candidate_evaluation is the base64 of that plus
# ≤5 cases and the contract record — ~10 KB typical, ~25 KB worst — so a
# 12-task diagnostic is a few megabytes. 64 MiB is ~25x that headroom and
# still bounds a runaway loop to something the telemetry volume absorbs.
CAPTURE_DEFAULT_MAX_BYTES = 64 * 1024 * 1024
# One pathological candidate must not consume the whole budget. Its identity
# is still recorded; only the bytes are dropped, and the record says so.
CAPTURE_MAX_RECORD_BYTES = 2 * 1024 * 1024
# Execution output is diagnostic context, not evidence: tails are enough.
CAPTURE_MAX_FIELD_BYTES = 4096

_CAPTURE_GOT = re.compile(r"got\s+(.*)", re.DOTALL)
_CAPTURE_TIMEOUT = re.compile(r"tim(?:ed?\s*)?out", re.IGNORECASE)


def _capture_clip(text: str) -> str:
    text = text or ""
    if len(text) <= CAPTURE_MAX_FIELD_BYTES:
        return text
    return text[:CAPTURE_MAX_FIELD_BYTES] + "…[clipped]"


def _capture_case(index, tc, ran_ok, out, err, harness_error=""):
    """One generated case as observed, with the failure kinds kept apart.

    A wrong answer, a crash, a timeout and a case the harness could not even
    build are four different findings about a run that all reduce to "not
    passed" in production. Classification is read off what the sandbox
    already returned; nothing extra is executed.
    """
    inp = (getattr(tc, "input_str", "") or "").strip()
    exp = (getattr(tc, "expected_output", "") or "").strip()
    out, err = out or "", err or ""
    passed = bool(ran_ok) and "SELF_TEST_PASS" in out
    # The generated assertion reports the value it saw as `got <actual>`, so
    # the candidate's real output survives in the failure text.
    match = None if passed else _CAPTURE_GOT.search(err)
    actual = match.group(1).strip() if match else None
    if harness_error:
        outcome = "harness_error"
    elif passed:
        outcome = "pass"
    elif not inp or not exp:
        outcome = "generated_test_malformed"
    elif _CAPTURE_TIMEOUT.search(err):
        outcome = "timeout"
    elif actual is not None or "AssertionError" in err:
        outcome = "wrong_answer"
    else:
        outcome = "execution_error"
    return {"index": index, "input": inp, "expected": exp, "actual": actual,
            "passed": passed, "outcome": outcome,
            "stdout": _capture_clip(out), "stderr": _capture_clip(err),
            "harness_error": harness_error}


class _PoolCapture:
    """An append-only JSONL sink for one run's candidate pool.

    Disabled is the default and the only state that costs anything: with no
    path configured every method is a no-op. Once enabled, no error it can
    hit — encoding, a full disk, a revoked directory — is allowed to reach
    the pipeline; capture turns itself off and the diagnostic record is
    invalid instead of the run being different.
    """

    def __init__(self, path: Optional[Path] = None, fd: Optional[int] = None,
                 error: str = ""):
        self.path = path
        self._fd = fd
        self.enabled = fd is not None
        self.write_error = error
        self.records_written = 0
        self.bytes_written = 0
        self.limit_reached = False
        self._lock = threading.Lock()
        self._seen = set()
        self._oracle: Dict[str, Dict[str, Any]] = {}
        self._selection: Optional[Dict[str, Any]] = None
        self._next_index = 0
        self._session_id = ""
        # Per-candidate cost, keyed by the candidate's own bytes. Keying on the
        # digest is what makes parallel generation safe: every call site owns
        # the code it just produced, so no thread can attribute its tokens to a
        # sibling. Calls that produce no candidate -- the probe, self-test
        # generation -- are shared overhead and are named as such rather than
        # divided across candidates.
        self._cost: Dict[str, Dict[str, Any]] = {}
        self._shared: Dict[str, Dict[str, Any]] = {}
        self._parent: Dict[str, str] = {}
        # Trace identity joins a pool to the request that caused it; the
        # invocation id is what makes THIS pool unique, because one request can
        # call V3 several times and a filename or pool name cannot tell them
        # apart. An absent trace id is recorded as unattributed and is never
        # inferred from order.
        self._trace_request_id = ""
        self._v3_invocation_id = ""
        self._instances: Dict[str, str] = {}
        # Lens scoring happens before sandbox evaluation.  Keep scored
        # candidates pending until their full evaluation record is written;
        # if cancellation closes the run first, close() flushes an explicit
        # unverified observation instead of losing the scoring evidence.
        self._pending_lens: Dict[Any, Dict[str, Any]] = {}

    # -- lifecycle ----------------------------------------------------------

    @classmethod
    def disabled(cls) -> "_PoolCapture":
        return cls()

    @classmethod
    def from_env(cls, env: Optional[Dict[str, str]] = None) -> "_PoolCapture":
        """Open the configured sink, or stay inert.

        Every rejection here is a refusal to write, never an exception: a
        relative path, a missing parent, a symlink at the final component, or
        anything that is not a regular file. O_NOFOLLOW is what stops the
        configured path being aimed at something else through a link that was
        planted first.
        """
        configured = (env or os.environ).get(CAPTURE_ENV, "").strip()
        if not configured:
            return cls()
        path = Path(configured)
        if not path.is_absolute():
            return cls(error="capture path must be absolute")
        if not path.parent.is_dir():
            return cls(error="capture parent directory does not exist")
        try:
            fd = os.open(str(path),
                         os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW,
                         0o600)
        except OSError as exc:
            return cls(error=f"open: {exc}")
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode):
                raise OSError("capture path is not a regular file")
            # A file that already existed may carry wider permissions than
            # the create mode would have given it.
            if stat.S_IMODE(info.st_mode) != 0o600:
                os.fchmod(fd, 0o600)
        except OSError as exc:
            os.close(fd)
            return cls(error=f"validate: {exc}")
        return cls(path=path, fd=fd)

    def close(self, result: Optional[Dict[str, Any]] = None) -> None:
        """Write the delivered artifact, the selection summary and the status
        line, then release the descriptor."""
        if not self.enabled:
            return
        try:
            for pending in list(self._pending_lens.values()):
                self.note_candidate(**pending)
            self._pending_lens.clear()
            if result:
                code = result.get("code") or ""
                if code:
                    self.note_candidate(
                        role="delivered", index=None, code=code,
                        accepted=bool(result.get("passed")),
                        record=result.get("evidence_record"), phase="delivered")
            self._write_selection(result)
            self._write_reconciliation(result)
        except Exception as exc:
            self.write_error = self.write_error or f"close: {exc}"
        self._write_status()
        fd, self._fd = self._fd, None
        self.enabled = False
        if fd is not None:
            try:
                os.close(fd)
            except OSError:
                pass

    # -- collection ---------------------------------------------------------

    def bind(self, session_id: str) -> None:
        self._session_id = session_id or ""

    def note_oracle(self, code: str, cases, passed: int, total: int) -> None:
        """Keep a verification run's per-case detail, keyed by the bytes it
        judged. Production keeps only the boolean this discards."""
        if not self.enabled:
            return
        self._oracle[contract.content_hash(code)] = {
            "suite_available": True, "cases": list(cases),
            "cases_passed": passed, "cases_total": total}

    def note_candidate(self, *, role: str, index, code: str, accepted: bool,
                       record, phase: str, lens=None) -> None:
        """One candidate_evaluation record.

        Deduplicated on (role, bytes): candidate zero is evaluated twice by
        design — once as the probe, once as pool member 0 — and one record
        per role is the honest count of distinct candidates, not of visits.
        """
        if not self.enabled or not code:
            return
        raw = code.encode("utf-8")
        digest = hashlib.sha256(raw).hexdigest()
        key = (role, digest)
        if key in self._seen:
            return
        self._pending_lens.pop(key, None)
        self._seen.add(key)
        if index is None:
            index = self._next_index
        self._next_index = max(self._next_index, int(index) + 1)
        oracle = self._oracle.get(digest) or {
            "suite_available": False, "cases": [],
            "cases_passed": 0, "cases_total": 0}
        instance = self.instance_id(role, index)
        parent_instance = self._instances.get(
            self._parent.get(self.candidate_id(code), ""), "")
        self._instances[self.candidate_id(code)] = instance
        payload = {
            "type": "candidate_evaluation",
            **self._identity(),
            "candidate_instance_id": instance,
            "parent_candidate_instance_id": parent_instance,
            "session_id": self._session_id, "phase": phase,
            "candidate_index": index, "role": role,
            "code_b64": base64.b64encode(raw).decode("ascii"),
            "code_sha256": digest, "code_bytes": len(raw),
            "adapter_id": (record or {}).get("adapter_id", ""),
            "contract_record": record,
            "contract_record_source":
                "production" if record else "not_built_in_production",
            "oracle": oracle,
            "accepted": bool(accepted),
            "lens": lens or {},
            "candidate_id": self.candidate_id(code),
            "parent_id": self._parent.get(self.candidate_id(code), ""),
            "cost": self._cost.get(self.candidate_id(code),
                                   {"tokens": 0, "latency_ms": 0.0, "model_calls": 0}),
        }
        self.write(payload)

    def note_lens_candidate(self, *, role: str, index, code: str,
                            phase: str, lens: Dict[str, Any]) -> None:
        """Retain a scored candidate until its sandbox record is available.

        This is capture-only bookkeeping.  A normal run replaces the pending
        entry with its full candidate evaluation.  Cancellation or another
        exceptional exit flushes it as explicitly unverified evidence.
        """
        if not self.enabled or not code:
            return
        digest = hashlib.sha256(code.encode("utf-8")).hexdigest()
        key = (role, digest)
        if key in self._seen:
            return
        self._pending_lens[key] = {
            "role": role, "index": index, "code": code,
            "accepted": False, "record": None,
            "phase": phase, "lens": dict(lens or {}),
        }

    def identify(self, trace_request_id: str, v3_invocation_id: str) -> None:
        """Bind the request and invocation this pool belongs to."""
        self._trace_request_id = trace_request_id or ""
        self._v3_invocation_id = v3_invocation_id or ""

    def _identity(self) -> Dict[str, Any]:
        return {
            "request_id": self._trace_request_id,
            "request_id_state": "attributed" if self._trace_request_id else "unattributed",
            "v3_invocation_id": self._v3_invocation_id,
        }

    def instance_id(self, role: str, index) -> str:
        """Instance identity, distinct from content identity.

        Two independently generated candidates can hold identical bytes, and a
        repair can return exactly its parent's bytes; a content hash cannot
        tell those apart, so lineage keyed on bytes would silently merge them.
        The index is preassigned at generation time, before parallel
        completion order can affect it, so this is stable across replay.
        """
        return f"{self._v3_invocation_id or 'noinv'}:{role}:{index}"

    # -- cost attribution ---------------------------------------------------

    @staticmethod
    def candidate_id(code: str) -> str:
        """Stable identity for a candidate: the first 12 hex of its bytes.

        Derived from content, not from a counter, so two threads finishing in
        either order name the same candidate the same way and neither can take
        the other's id.
        """
        if not code:
            return ""
        return hashlib.sha256(code.encode("utf-8")).hexdigest()[:12]

    def note_cost(self, *, code: Optional[str], phase: str, tokens: int,
                  latency_ms: float, parent_code: Optional[str] = None) -> None:
        """One model call's tokens and latency.

        `code` is the candidate the call produced. None means the call served
        the run rather than a candidate, and it lands in shared overhead under
        its phase. A call is never split across candidates and never guessed
        at: unattributed cost shows up in the reconciliation as a named
        remainder instead of being smeared over the pool.
        """
        if not self.enabled:
            return
        try:
            tokens = int(tokens or 0)
            latency_ms = float(latency_ms or 0.0)
        except (TypeError, ValueError):
            return
        with self._lock:
            if code:
                cid = self.candidate_id(code)
                slot = self._cost.setdefault(
                    cid, {"tokens": 0, "latency_ms": 0.0, "model_calls": 0})
                slot["tokens"] += tokens
                slot["latency_ms"] += latency_ms
                slot["model_calls"] += 1
                if parent_code:
                    self._parent.setdefault(cid, self.candidate_id(parent_code))
            else:
                slot = self._shared.setdefault(
                    phase or "unattributed",
                    {"tokens": 0, "latency_ms": 0.0, "model_calls": 0})
                slot["tokens"] += tokens
                slot["latency_ms"] += latency_ms
                slot["model_calls"] += 1

    def _write_reconciliation(self, result: Optional[Dict[str, Any]]) -> None:
        """Prove the per-candidate ledger adds up to the run total.

        Reports the remainder rather than hiding it. A run whose candidate
        costs and named overhead do not reach the reported total has cost that
        was never attributed, and the diagnostic says so.
        """
        cand_tokens = sum(v["tokens"] for v in self._cost.values())
        cand_calls = sum(v["model_calls"] for v in self._cost.values())
        shared_tokens = sum(v["tokens"] for v in self._shared.values())
        shared_calls = sum(v["model_calls"] for v in self._shared.values())
        total = int((result or {}).get("total_tokens") or 0)
        self.write({
            "type": "cost_reconciliation",
            **self._identity(),
            "session_id": self._session_id,
            "candidates": {cid: dict(v) for cid, v in sorted(self._cost.items())},
            "shared_overhead": {k: dict(v) for k, v in sorted(self._shared.items())},
            "candidate_tokens": cand_tokens,
            "candidate_model_calls": cand_calls,
            "shared_tokens": shared_tokens,
            "shared_model_calls": shared_calls,
            "attributed_tokens": cand_tokens + shared_tokens,
            "run_total_tokens": total,
            "unattributed_tokens": total - (cand_tokens + shared_tokens),
            "reconciles": total == cand_tokens + shared_tokens,
            "schema": CAPTURE_SCHEMA,
        })

    def note_pool(self, *, phase: str, pool, lens_index=None,
                  evidence_index=None, verified_index=None,
                  status: str = "", reason: str = "", tied: int = 0,
                  incomparable: int = 0, ineligible: int = 0) -> None:
        """What the selectable pool was and what each selector picked."""
        if not self.enabled:
            return
        self._selection = {
            "phase": phase,
            "pool": [contract.content_hash(c.get("code") or "") for c in pool],
            "pool_indices": [c.get("index") for c in pool],
            "lens_index": lens_index, "evidence_index": evidence_index,
            "verified_index": verified_index,
            "selection_status": status, "selection_reason": reason,
            "tied_count": tied, "incomparable_count": incomparable,
            "ineligible_count": ineligible,
        }


    def write(self, record: Dict[str, Any]) -> bool:
        """Append one complete record, or nothing at all."""
        if not self.enabled or self._fd is None:
            return False
        record = dict(record)
        record.setdefault("schema", CAPTURE_SCHEMA)
        try:
            blob = (json.dumps(record, separators=(",", ":"), default=str)
                    + "\n").encode("utf-8")
        except (TypeError, ValueError) as exc:
            self.write_error = self.write_error or f"encode: {exc}"
            return False
        if len(blob) > CAPTURE_MAX_RECORD_BYTES:
            # Identity survives; the bytes do not, and the record says which.
            trimmed = dict(record)
            trimmed["code_b64"] = ""
            trimmed["omitted"] = "record_exceeds_per_record_limit"
            try:
                blob = (json.dumps(trimmed, separators=(",", ":"), default=str)
                        + "\n").encode("utf-8")
            except (TypeError, ValueError) as exc:
                self.write_error = self.write_error or f"encode: {exc}"
                return False
        return self._append(blob)

    def _append(self, blob: bytes) -> bool:
        """The cap is checked and the bytes are written under one exclusive
        lock, so a second worker process cannot slip a record in between and
        neither can interleave a partial line."""
        with self._lock:
            fd = self._fd
            if fd is None:
                return False
            try:
                fcntl.flock(fd, fcntl.LOCK_EX)
                try:
                    size = os.lseek(fd, 0, os.SEEK_END)
                    if size + len(blob) > CAPTURE_DEFAULT_MAX_BYTES:
                        self.limit_reached = True
                        return False
                    os.write(fd, blob)
                finally:
                    fcntl.flock(fd, fcntl.LOCK_UN)
            except OSError as exc:
                self.write_error = self.write_error or f"append: {exc}"
                self.enabled = False
                return False
        self.records_written += 1
        self.bytes_written += len(blob)
        return True

    def _write_selection(self, result: Optional[Dict[str, Any]]) -> None:
        summary = dict(self._selection or {
            "phase": (result or {}).get("phase_solved", "none"),
            "pool": [], "pool_indices": [], "lens_index": None,
            "evidence_index": None, "verified_index": None,
            "selection_status": "not_run", "selection_reason": "",
            "tied_count": 0, "incomparable_count": 0, "ineligible_count": 0})
        summary["type"] = "selection_summary"
        summary.update(self._identity())
        summary["session_id"] = self._session_id
        # Name the instance that was selected, not only its bytes: two
        # instances can share content, and a summary that says only "this
        # hash" cannot say which candidate the lens actually picked.
        lens_index = summary.get("lens_index")
        summary["selected_candidate_instance_id"] = (
            self.instance_id("generated", lens_index) if lens_index is not None else "")
        # The service does not know what Go finally wrote. This names the
        # bytes the service RETURNED; the delivered artifact is joined
        # offline from the runner's own authorization telemetry.
        code = (result or {}).get("code") or ""
        summary["service_returned_candidate_hash"] = \
            contract.content_hash(code) if code else ""
        self.write(summary)

    def _write_status(self) -> None:
        status = {"type": "capture_status", **self._identity(),
                  "session_id": self._session_id,
                  "max_bytes": CAPTURE_DEFAULT_MAX_BYTES,
                  "bytes_written": self.bytes_written,
                  "records_written": self.records_written,
                  "write_error": self.write_error,
                  "limit_reached": self.limit_reached}
        if not self.write(status):
            # The cap refused the full line; a bare marker still tells the
            # reader the file is truncated rather than complete.
            self.limit_reached = True
            self._append(b'{"type":"capture_status","limit_reached":true}\n')


def _capture_pool_member(capture: "_PoolCapture", candidate, probe_code: str) -> None:
    """Record a pool member under the role that explains where it came from.

    Index 0 is candidate zero only when a probe actually produced code; with
    no probe the pool is generated candidates all the way down, and calling
    the first of them "candidate zero" would misname the comparison the
    diagnostic exists to make.
    """
    capture.note_candidate(
        role=("incumbent" if candidate.get("incumbent")
              else "candidate_zero" if probe_code and candidate.get("index") == 0
              else "generated"),
        index=candidate.get("index"), code=candidate.get("code") or "",
        accepted=bool(candidate.get("passed")),
        record=candidate.get("contract_record"), phase="sandbox",
        lens=_candidate_lens_payload(candidate))


# --- V3 Pipeline Orchestrator ------------------------------------------------


def _entry_function(code: str) -> Optional[str]:
    """Name the function a self-test should call.

    The generator used to take the first `def` in the file, which is the
    entry point only when the solution happens to define no helpers above
    it. `def parse(...)` followed by `def solve(...)` meant every test
    called parse with the case input and compared it to the final answer —
    a guaranteed failure that says nothing about the code.

    Measured across a 28-session run: 0 of 44 candidates passed, and the
    self-test results were 0/5, 0/4, 0/3 — never partial. Imperfect code
    fails some cases; only a harness fault fails all of them uniformly.

    The entry point is the top-level function nothing else in the file
    calls. Where several qualify a conventional name wins, then the last
    one, since helpers are conventionally defined above their caller.
    Unparseable code falls back to the first def, which is what the old
    behaviour was.
    """
    import ast as _ast
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        m = re.search(r'^def (\w+)\(', code, re.MULTILINE)
        return m.group(1) if m else None
    top = [n for n in tree.body if isinstance(n, _ast.FunctionDef)]
    if not top:
        return None
    names = {n.name for n in top}
    called = set()
    for node in _ast.walk(tree):
        if isinstance(node, _ast.Call) and isinstance(node.func, _ast.Name):
            if node.func.id in names:
                called.add(node.func.id)
    roots = [n.name for n in top if n.name not in called]
    for preferred in ("solve", "main", "run"):
        if preferred in roots:
            return preferred
    if roots:
        return roots[-1]
    return top[-1].name


def _entry_takes_case_input(code: str, name: str) -> bool:
    """Whether the case input can be passed to `name` and its answer read
    back as a return value.

    The two self-test shapes are not interchangeable. A function-shaped
    solution takes the case input as arguments and returns the answer. A
    script-shaped one reads stdin and prints — calling it with the case
    input raises TypeError, and there is no return value to compare.

    Measured on aoc_sonar: candidates are `def main():` reading sys.stdin
    and printing, so the function path called main(case) and every case
    failed. The stdin/stdout path below handles exactly this and was never
    reached, because the choice keyed on a function merely existing.

    Requires all three: the function takes at least one parameter, it
    returns a value somewhere, and the file does not read stdin itself.
    """
    import ast as _ast
    if "sys.stdin" in code or "input()" in code:
        return False
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        return True  # old behaviour for code that does not parse
    for node in _ast.walk(tree):
        if isinstance(node, _ast.FunctionDef) and node.name == name:
            args = node.args
            takes = bool(args.args or args.posonlyargs or args.kwonlyargs
                         or args.vararg)
            returns = any(isinstance(n, _ast.Return) and n.value is not None
                          for n in _ast.walk(node))
            return takes and returns
    return False


def _reads_input_file(code: str):
    """The literal filename a candidate opens for reading, if it opens one.

    Returns None when the program takes no named file, which is the case the
    stdin path below already covers.

    Only string literals count. A computed path cannot be materialised without
    running the program, and guessing one wrong is worse than falling through
    to the existing behaviour.
    """
    import ast as _ast
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        return None
    for node in _ast.walk(tree):
        if not isinstance(node, _ast.Call):
            continue
        func = node.func
        name = getattr(func, "id", None) or getattr(func, "attr", None)
        if name not in ("open", "read_text", "read_bytes", "Path"):
            continue
        if not node.args:
            continue
        target = node.args[0]
        if not (isinstance(target, _ast.Constant) and isinstance(target.value, str)):
            continue
        # A write/append target is an output file, not the program's input.
        mode = node.args[1] if len(node.args) > 1 else None
        if isinstance(mode, _ast.Constant) and isinstance(mode.value, str):
            if any(ch in mode.value for ch in ("w", "a", "x")):
                continue
        for kw in node.keywords:
            if kw.arg == "mode" and isinstance(kw.value, _ast.Constant):
                if any(ch in str(kw.value.value) for ch in ("w", "a", "x")):
                    break
        else:
            if "/" not in target.value and "\\" not in target.value:
                return target.value
    return None


_SOURCE_SUFFIXES = (".py", ".js", ".ts", ".go", ".java", ".rb", ".php", ".rs",
                    ".c", ".cpp", ".h", ".html", ".css", ".md", ".json",
                    ".yaml", ".yml", ".toml")


def _task_input_file(project_files) -> str:
    """The data file the task supplies for the program to read, if any.

    Verification has to hold a candidate to the environment's contract, not
    to whichever contract the candidate happens to prefer. The caller runs
    `python solve.py` with this file on disk and NO stdin, so a candidate
    that reads stdin must fail here rather than be handed a stdin the real
    run will never provide.

    Measured: with the self-test choosing its shape from the candidate's own
    code, stdin-reading candidates passed verification and were selected in 3
    sessions — every one of which then failed the task, printing 0 because no
    stdin arrived.
    """
    if not project_files:
        return ""
    for name in project_files:
        if not name.lower().endswith(_SOURCE_SUFFIXES):
            return name
    return ""


# The candidate is staged beside its input rather than spliced into the
# wrapper, so the bytes that run are the bytes that were generated.
_CANDIDATE_FILE = "candidate.py"


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


def _trusted_oracle(self_tests) -> bool:
    """Whether every case in this suite declares trusted provenance.

    Model-generated cases are not trusted: the same model wrote the code and
    the answer, from the problem statement alone, and for these tasks
    producing the expected output IS solving the problem. Measured on the
    captured pool: 21 of 36 valid generated keys disagreed with the task's
    own reference, and a correct candidate scored 2/5 against its own suite.

    The answer decides only whether verified_sandbox labels the score
    `self_test_untrusted`; no suite rejects a candidate. Nothing in this
    service produces a trusted case -- self_test_gen marks every case it
    writes as generated -- so every score here carries that label.

    Unknown provenance fails closed. A case that cannot say where it came
    from is not trusted, so a future producer must opt in deliberately
    rather than inherit trust by omission.
    """
    cases = getattr(self_tests, "test_cases", None) or []
    if not cases:
        return False
    return all(getattr(tc, "provenance", None) == PROVENANCE_TRUSTED
               for tc in cases)


def _staged_candidate_run(code: str, inp: str, infile: str):
    """How a case runs a candidate, for both builders that need it.

    Returns ``(header, run, files)``: the files the sandbox stages, the
    source that attaches stdin and captures stdout, and the one statement
    that executes the candidate. The self-test and the output probe differ
    only in what they do with the captured output and with an exception, so
    they share this and nothing else -- two copies of "how to run a
    candidate" is how one of them was repaired and the other was not.

    The case's input is staged, never written by executable code: that write
    lands in the candidate's working directory, which is read-only in the
    sandbox, so it raised before the candidate ran. The candidate executes
    once, from a file, under the name a program is really run with; exec'ing
    it inside the imported ``solution`` module left ``__name__`` as
    ``'solution'`` and a ``if __name__ == "__main__":`` body never ran.
    """
    files = {_CANDIDATE_FILE: code}
    if infile:
        # Empty stdin, not absent stdin. The caller runs the program with
        # stdin at EOF, so a candidate that reads sys.stdin must terminate
        # immediately and fail fast. With no stdin attached it BLOCKS until
        # the sandbox timeout instead: measured live, one stdin candidate
        # turned the probe into a 300s hang.
        files[infile] = inp
        stdin_setup = "_s.stdin=_o.StringIO('')\n"
    else:
        stdin_setup = f"_s.stdin=_o.StringIO({repr(inp)})\n"
    header = ("import sys as _s,io as _o,runpy as _rp\n" + stdin_setup
              + "_c=_o.StringIO()\n_old=_s.stdout\n_s.stdout=_c\n")
    run = f"_rp.run_path({repr(_CANDIDATE_FILE)},run_name='__main__')\n"
    return header, run, files


def _make_self_test(code: str, tc, task_input_file: str = ""):
    """Build one case's executable check, and the files the sandbox stages.

    Returns ``(wrapper_source, files)``. The wrapper creates nothing: the
    case's input file is staged through the sandbox's request files map, the
    same mechanism that already carries project context. Writing it from
    executable code instead put the write in the candidate's working
    directory, which is read-only in the sandbox container, so the case died
    with ``OSError: [Errno 30]`` before the candidate ran. Measured on the
    captured candidate pool: 50 of 50 generated cases failed there, which is
    why every suite in a 50-task run scored 0/N and a partial score was
    structurally impossible.

    The candidate then runs through ``runpy.run_path(..., run_name="__main__")``
    -- once, from a file, with the name a program is actually run under. The
    previous form exec'd it inside the imported ``solution`` module, where
    ``__name__`` is ``'solution'``, so a ``if __name__ == "__main__":`` body
    never executed and the case compared against empty output.

    Uses ast.literal_eval (safe — only parses Python literals) to convert
    I/O string representations to actual values for comparison.
    All code runs inside the sandboxed container.
    """
    inp = tc.input_str.strip()
    exp = tc.expected_output.strip()
    name = _entry_function(code)
    if name and _entry_takes_case_input(code, name):
        return (code + "\nimport ast as _a\n"
            + f"_i={repr(inp)}\n_e={repr(exp)}\n"
            + "try:\n _p=_a.literal_eval(_i)\nexcept:\n _p=_i\n"  # bare except inside generated user code, intentional
            + f"_r={name}(*_p) if isinstance(_p,tuple) else {name}(_p) if isinstance(_p,list) else {name}(_p)\n"
            + "try:\n _ev=_a.literal_eval(_e)\nexcept:\n _ev=_e\n"  # bare except inside generated user code, intentional
            + "assert str(_r)==str(_ev) or _r==_ev,f'got {_r}'\nprint('SELF_TEST_PASS')\n"), {}
    # A program that reads a named file has to be given that file. Feeding it
    # stdin instead tests a contract the task never stated, and the verdict
    # comes out backwards: a candidate that correctly reads input.txt finds no
    # such file in the sandbox and FAILS, while one that reads stdin passes.
    # Verification then selects for the shape that cannot work when the caller
    # runs `python solve.py` with no stdin.
    #
    # Measured on the two AoC tasks whose input is a file: 9 of 12 candidates
    # ATLAS wrote read stdin, against a prompt that says "reads input.txt",
    # and both tasks sat at 7/26. The same model asked directly, with no
    # pipeline, wrote input.txt readers and scored 12/12.
    infile = task_input_file or _reads_input_file(code)
    header, run, files = _staged_candidate_run(code, inp, infile)
    # A SystemExit the program raised itself is ordinary termination when its
    # code is 0 or None, and an execution failure otherwise; anything else
    # propagates and the process exits non-zero.
    return (
        header
        + "try:\n"
        + "    " + run
        + "except SystemExit as _e:\n"
        "    if _e.code not in (0,None):\n"
        "        raise\n"
        "finally:\n"
        " _s.stdout=_old\n"
        f"assert _c.getvalue().strip()=={repr(exp)},f'got {{_c.getvalue().strip()}}'\n"
        "print('SELF_TEST_PASS')\n"), files


_CONSENSUS_MARK = "V3_OUT:"


def _evaluate_candidate(file_path, code, smoke_passed, emit, *, task=None):
    """THE canonical contract record for ONE artifact.

    An unsupported candidate stays available as a fallback but is never
    represented as verified.

    One record, bound to the exact bytes it describes. No parallel strength,
    score or coverage field is kept beside it: a second authoritative copy is
    how two answers about one candidate start to disagree.
    """
    adapter = adapters.select_adapter(file_path, code)
    task = task or _task_identity("", "")
    return adapters.contract_record(
        adapter=adapter, accepted=bool(smoke_passed),
        contract_id=task["contract_id"], contract_version=task["contract_version"],
        artifact_scope=task["artifact_scope"],
        evaluation_context_hash=task["evaluation_context_hash"],
        candidate_content_hash=contract.content_hash(code))


def _make_output_probe(code: str, tc, task_input_file: str = ""):
    """Run a candidate on a case's INPUT and report what it printed.

    Returns ``(wrapper_source, files)`` — the same staged shape the self-test
    uses, through the same ``_staged_candidate_run``.

    The self-test compares that output to the model's predicted answer. For a
    problem the model cannot reliably solve, producing the answer IS the
    problem, so the prediction is wrong and correct code fails its own suite.
    Measured across 42 verifications in one run: every one scored 0/N, while a
    candidate taken from those same logs passed immediately when given a
    correct expected value. Uniform zero is a broken answer key, not broken
    code.

    CodeT (Chen et al., 2022), which this pipeline already cites, does not use
    generated tests as an oracle, for exactly this reason. It runs candidates
    on the generated INPUTS and clusters them by agreement, so the signal is
    the answer candidates converge on rather than the answer the model
    guessed. This probe produces the raw material for that.
    """
    inp = tc.input_str.strip()
    name = _entry_function(code)
    if name and _entry_takes_case_input(code, name):
        # The pair every caller unpacks. A bare string here made the unpack
        # raise, the caller's except turned that into "no answer", and a
        # function-shaped candidate could never join a cluster.
        return (code + "\nimport ast as _a\n"
                + f"_i={repr(inp)}\n"
                + "try:\n _p=_a.literal_eval(_i)\nexcept:\n _p=_i\n"
                + f"_r={name}(*_p) if isinstance(_p,tuple) else {name}(_p)\n"
                + f"print({repr(_CONSENSUS_MARK)}+repr(str(_r).strip()))\n"), {}
    infile = task_input_file or _reads_input_file(code)
    header, run, files = _staged_candidate_run(code, inp, infile)
    # The marker is emitted ONLY on clean completion, and only for non-empty
    # output. Swallowing exceptions and printing the marker regardless let two
    # CRASHING candidates agree: repr('') is the two-character string "''",
    # which is truthy, so their empty outputs clustered and crash consensus
    # won (third-party audit reproduction: two ordinary-exception candidates,
    # WINNERS [0, 1]). A crash prints a CRASH line the clustering explicitly
    # refuses, and silence emits nothing.
    #
    # The probe reports rather than fails, so unlike the self-test it catches
    # what the candidate raised instead of letting it end the process. A
    # SystemExit the program raised itself is ordinary termination at code 0
    # or None -- a candidate that prints its answer and calls sys.exit() has
    # answered, and calling that a crash discarded a real output.
    return (header
            + "_crashed=False\n"
            + "try:\n"
            + "    " + run
            + "except SystemExit as _e:\n    _crashed=_e.code not in (0,None)\n"
            + "except BaseException:\n    _crashed=True\n"
            + "finally:\n _s.stdout=_old\n"
            + "_out=_c.getvalue().strip()\n"
            + f"if _crashed:\n    print({repr(_CONSENSUS_MARK)}+'CRASH')\n"
            + f"elif _out:\n    print({repr(_CONSENSUS_MARK)}+repr(_out))\n"), files


def _failed_a_project_check(candidate) -> bool:
    """The candidate failed a check the generated cases did not write: the
    project's own build command, importing where the submitted baseline
    imports, or keeping the top-level names the submitted file binds.
    Candidates agreeing with each other does not outweigh that."""
    return any(ev.get("verifier") in ("build_command", "python_import_comparison",
                                      "incumbent_role")
               and ev.get("status") == "failed"
               for ev in candidate.get("verification_evidence") or [])


def _consensus_winners(candidates, test_cases, sandbox, emit,
                       task_input_file=""):
    """CodeT agreement: candidates whose outputs match the largest cluster.

    Returns [] when there is nothing to agree on — fewer than two candidates,
    no case produced output, or every candidate disagreed with every other.
    Agreement between independently generated programs is evidence; one
    program agreeing with itself is not, so a lone cluster does not win.

    A candidate enters clustering only if it produced a real answer on EVERY
    probe case. Partial validity is not agreement material: with any(), two
    candidates that crashed on most cases but happened to match on one could
    form the winning cluster, promoting code proven broken on the majority of
    the very inputs the consensus ran (third-party audit finding).

    A cluster is sized by its DISTINCT programs. Byte-identical candidates
    are one program agreeing with itself, and counting them twice let two
    copies of a wrong program outvote one correct program.

    Agreement is not verification: the caller marks winners `consensus`, not
    `passed`.
    """
    if len(candidates) < 2 or not test_cases:
        return []
    sigs = {}
    for c in candidates:
        outs = []
        for tc in test_cases:
            try:
                probe_code, probe_files = _make_output_probe(
                    c["code"], tc, task_input_file)
                ok, out, _ = sandbox(probe_code, files=probe_files)
            except Exception:
                ok, out = False, ""
            marker = ""
            if ok and _CONSENSUS_MARK in (out or ""):
                marker = out.split(_CONSENSUS_MARK)[-1].strip()
            if marker == "CRASH" or marker == "''":
                # A crash or empty output is not an answer to agree on.
                marker = ""
            outs.append(marker)
        if all(outs):
            sigs.setdefault(tuple(outs), []).append(c)
    if not sigs:
        return []

    def distinct(cluster):
        return len({contract.content_hash(c["code"]) for c in cluster})

    best = max(sigs.values(), key=distinct)
    if distinct(best) < 2:
        return []
    emit("consensus", f"{distinct(best)}/{len(candidates)} distinct candidates agree",
         cluster=distinct(best), clusters=len(sigs))
    return best


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
    # PC-048: derive language from the target file's extension. Used
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
    # Python parsed a stylesheet, a C file or a Makefile as Python: every
    # candidate failed with a SyntaxError that said nothing about the
    # file, and the Python-only checks ran on it.
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
    """The caller's own bytes, checked as a candidate (#259).

    Returns (incumbent, result). The incumbent is None when the caller sent
    no file; result is the finished run when the incumbent passed and its
    own record closes, else None.
    """
    baseline_code, file_path, emit, capture = ctx.baseline_code, ctx.file_path, ctx.emit, ctx.capture
    result, start, events = ctx.result, ctx.start, ctx.events
    # ===== THE INCUMBENT =====
    # The caller's own bytes are a real candidate (#259). They face the
    # checks every generated candidate faces -- the lens, the sandbox
    # verification, the contract record, and in phase one the vetoes --
    # and the lens ranks them with the rest, so a candidate replaces them
    # only by beating them. They are the EXACT bytes the request carried:
    # nothing re-types them, and a copy that differs only in whitespace
    # is never delivered in their place (_without_retypings).
    #
    # Until #259 they were only prose in the problem statement. Across 112
    # recorded sessions V3 replaced the model's file in all 33 of its
    # deliveries, three of them test files replaced by module code that
    # ran, tested nothing, and left two runs unable to finish.
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
            # Cached execution result -- do NOT skip evidence. This
            # `continue` is exactly how candidate zero escaped the
            # "one evaluation path": compile smoke set passed=True for
            # an interactive artifact, so the real Snake candidate
            # entered the pool with no behavioural evidence at all and
            # rank_key saw defaults for it.
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
        # the same evidence record. Two sets of verification semantics
        # is how the boolean survived into the candidate path, letting
        # ATLAS generate alternatives it could not rank.
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
    # are not used. Measured across 42 verifications in one run: all
    # 42 scored 0/N against them, and a candidate pulled from those
    # logs passed immediately against a correct expected value.
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
    # The incumbent is ranked like any other candidate (#259). A rule
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
    # Preserve the existing repair-budget rule: if every candidate
    # failed and the submitted interactive baseline compiles, return
    # to the outer loop with that baseline rather than spending more
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
    # GH #39 point 3: build call-graph context for the failing
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
    # A repair is a candidate. It used to be captured with
    # record=None, so the sealed Stage-A run holds three
    # pool members with adapter None and an empty record --
    # a candidate nothing can say anything about. Same
    # canonical adapter->contract path as every other
    # candidate; it adds no generation and no sandbox run
    # of its own.
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
        that file's role: every top-level name it binds (#259). It runs
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
        # when the code is correct — see PC-022.
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
        """The submitted file's role (#259): a replacement must keep every
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
        # PC-048: pass the detected language so HTML/JSON/etc. files
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
        # handling (PC-034).
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
                # the problem statement alone; letting them reject is how
                # a candidate that matched the task's own reference on
                # every input scored 2/5 and was discarded. The score and
                # every per-case pair stay in telemetry and in the pool
                # capture for offline analysis -- what changes is that
                # nothing downstream reads them as a verdict.
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
        self.budget_forcing = BudgetForcing(BudgetForcingConfig(enabled=True),
                                            telemetry_dir=t)
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
        # PC-046: ship the user's other project files into the sandbox so
        # multi-file imports resolve. `files` is the same Dict that V3
        # already prepends to the LLM prompt above; passing it to the
        # sandbox closes the gap where the model writes
        # `from utils import helper` and the sandbox imports a workspace
        # that contains only solution.py.
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
        # parses, and returning on it made candidates_generated=1 with
        # PlanSearch, DivSampling, consensus and ranking never running — the
        # whole test-time-compute apparatus skipped for any interactive artifact.
        # Strength comes from the VERIFIER THAT RAN. Keying it off the file
        # extension mapped every .py to behavioural completeness, which is
        # wrong for Pygame/Tkinter/Flask — those get a compile smoke and
        # nothing more, and would have closed the pipeline claiming behaviour
        # nobody demonstrated.
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
                    # The allocator's tier, not the signature default:
                    # generate() was silently running "standard" thinking
                    # depth whatever the CxGx gate decided (audit finding).
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
        # compile-smoke (PC-022).
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
        # GH #39 point 3: enrich each failing candidate's error_output
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
                # Same gap the repair path had: a refinement winner
                # is a candidate, and a candidate with no record is one
                # nothing can say anything about.
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
    pipeline checks, scores and ranks it beside them (#259) -- and pool index
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
