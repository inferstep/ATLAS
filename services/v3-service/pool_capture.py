"""The benchmark-only candidate-pool capture. When ATLAS_V3_CAPTURE_POOL
names a file, _PoolCapture writes the candidate pool, the observed cases and
the selection to it. When it is not set, the capture opens no file and
decides nothing. pipeline.py opens the capture for each run, and the run
steps write to it."""

import base64
import fcntl
import hashlib
import json
import os
import re
import stat
import threading
from pathlib import Path
from typing import Any, Dict, Optional

import contract
from candidate_records import _candidate_lens_payload

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

# Sizing: candidates average about 1.9 KB (p95 4.1 KB, max 13 KB), at most 3
# per generation call and roughly 2 calls per task. A candidate_evaluation is
# the base64 of that plus ≤5 cases and the contract record — ~10 KB typical,
# ~25 KB worst — so a 12-task diagnostic is a few megabytes. 64 MiB is ~25x that headroom and
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
