#!/usr/bin/env python3
"""
ATLAS V3 Pipeline Service — HTTP wrapper around the V3 benchmark pipeline.

Exposes the full V3 pipeline (PlanSearch, DivSampling, BudgetForcing,
PR-CoT, RefinementLoop, etc.) as an HTTP service that the Go proxy can
call for T2/T3 tasks.

Test cases are generated via SelfTestGen since we don't have benchmark
ground truth. The sandbox runs syntax/runtime checks on all candidates.

Streams progress events back as SSE for real-time feedback.

The pipeline itself lives in flat sibling modules: adapters.py (service
clients), scoring.py (candidate verification), symbols.py (tree-sitter
tooling), planning.py (/v3/plan), and pipeline.py (the orchestrator).
"""

import json
import uuid
import threading
import socket
import select

# Watcher poll interval, included in every measured cancellation latency.
WATCH_POLL_SEC = 0.25

# The proxy forwards this; absence is normal for direct callers.
REQUEST_ID_HEADER = "X-ATLAS-Request-ID"
import os
import sys
from pathlib import Path
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler

# Force line-buffered stdout
sys.stdout.reconfigure(line_buffering=True)

import adapters
import contract
import scoring
from pipeline import V3PipelineService, _build_problem_from_request
from planning import generate_plan
from symbols import (structural_edit, structural_score, build_project_symbols,
                     symbol_index, cyclomatic_complexity, embedded_script_check,
                     embedded_region_outline, orphaned_new_symbols,
                     _symbol_index_for_python_source, _STRUCTURAL_EDIT_AVAILABLE,
                     _EMBEDDED_SCRIPT_AVAILABLE)
# The handler does not call these; tests exercise them through `import main`.
# __all__ below states that so linters stop reading them as dead imports —
# deleting any of them breaks the suite, which is the failure this guards.
from pipeline import _candidate_by_index, _make_self_test
from planning import _score_plan, _existing_workspace_files
from scoring import (verify_build_command, smoke_compile_check,
                     score_candidate_per_step, _project_relative_path)
from symbols import _ast_selector_to_query

__all__ = [
    # Re-exported for tests that reach them via `import main`.
    "_candidate_by_index", "_make_self_test", "_score_plan",
    "_existing_workspace_files",
    "verify_build_command", "smoke_compile_check", "score_candidate_per_step",
    "_project_relative_path", "_ast_selector_to_query",
    "_symbol_index_for_python_source", "_STRUCTURAL_EDIT_AVAILABLE",
    "_EMBEDDED_SCRIPT_AVAILABLE",
    # The service surface itself.
    "structural_edit", "structural_score", "build_project_symbols",
    "symbol_index", "cyclomatic_complexity", "embedded_script_check",
    "embedded_region_outline", "orphaned_new_symbols",
    "generate_plan", "V3PipelineService", "_build_problem_from_request",
]

PORT = int(os.environ.get("ATLAS_V3_PORT", "8070"))


# --- HTTP Handler (SSE streaming) --------------------------------------------

pipeline = V3PipelineService()


def _service_log(msg):
    """The one emitter for the watcher lifecycle.

    This service's log path is print() through _PrivateValueStream, which
    applies the private-value filter and stamps the current request and
    invocation ids into each record. logging.getLogger() is NOT that path:
    no handler is installed on the root logger, so an info record would be
    dropped by lastResort and a cancellation would stop being visible at all.
    Naming the emitter is what removes the bare print from the lifecycle; the
    stream stays the one the rest of the service uses, so the record shape is
    unchanged.
    """
    print(msg, flush=True)


def _watch_parent_for(handler, label):
    """Request-scoped cancellation plus an EOF watcher on the parent socket.

    Shared by every production handler that runs inference. A handler that
    forgets this leaves its generations uncancellable -- exactly the defect
    this exists to prevent -- so a structural test asserts each one calls it.

    MSG_PEEK is deliberate: the watcher must never consume a byte the handler
    still needs, and it starts only after the request body has been read.
    """
    scope = adapters.CancelScope(invocation_id=str(uuid.uuid4()))
    # Bind it for logging on the handler thread: every record this invocation
    # emits now names the invocation, so a log line joins to a relay call
    # without time-window inference.
    from structured_log import bind_identity, current_identity
    from structured_log import set_invocation_id as _set_inv
    _set_inv(scope.invocation_id)
    # Captured HERE, on the owning thread, and handed to the watcher.
    #
    # A threading.Thread inherits no ContextVar, so the watcher used to run
    # with both ids empty: of 159,533 records the sealed Stage-A acquisition
    # produced, the only two that could not be attributed were this watcher
    # reporting the cancellations it had just performed.
    #
    # Explicitly, not through copy_context(): the watcher can outlive the
    # context it was started from, and a copied context would keep answering
    # with an identity whose request is already gone.
    identity = current_identity()
    stop_watch = threading.Event()

    def _watch():
        bind_identity(*identity)
        sock = handler.connection
        while not stop_watch.is_set():
            try:
                r, _, _ = select.select([sock], [], [], WATCH_POLL_SEC)
            except (OSError, ValueError):
                break
            if not r:
                continue
            try:
                if sock.recv(1, socket.MSG_PEEK | socket.MSG_DONTWAIT) == b"":
                    closed = scope.cancel()
                    _service_log(f"[{label}] parent disconnected; cancelled "
                                 f"{closed} in-flight generation(s)")
                    return
            except BlockingIOError:
                continue
            except (OSError, ValueError):
                scope.cancel()
                return

    t = threading.Thread(target=_watch, daemon=True)
    t.start()
    return scope, stop_watch, t


def _release_scope(scope, stop_watch, watcher, label):
    """Idempotent teardown: stop the watcher, close anything still open."""
    from structured_log import bind_identity
    stop_watch.set()
    leaked = scope.cancel()
    if leaked:
        _service_log(f"[{label}] closed {leaked} connection(s) still open "
                     f"at handler exit")
    watcher.join(timeout=2)
    # Last statement of the request's lifecycle: drop the identity so it
    # cannot be answered by whatever runs on this thread next. Absent has to
    # stay absent -- a borrowed id is worse than none, because it reads as
    # evidence.
    bind_identity("", "")


# Progress stages whose `detail` IS model output rather than a description of
# it. Logging those verbatim puts candidate source into operational records --
# and because the stdout wrapper emits one record per line, a multi-line token
# becomes several records, each carrying source. Measured: 84 such records in
# one 63-cell rehearsal, after the first-delta sample had already been fixed.
#
# The SSE payload the proxy receives is unchanged. Only the local debug line is
# reduced to metadata.
_CONTENT_BEARING_STAGES = frozenset({"token", "v3_token", "reasoning"})


def _safe_progress_detail(stage, detail):
    """What may be logged for a progress event: never model output.

    Fail closed twice over -- a named content stage is always reduced, and so
    is any detail that spans lines, since a description of progress does not.
    """
    text = detail if isinstance(detail, str) else str(detail)
    if stage in _CONTENT_BEARING_STAGES or "\n" in text:
        return f"<{len(text)} chars>"
    return text[:80]



def _positive_budget_ms(value):
    """The caller's cap in ms, or None when absent or unusable.

    A bool is not a number here, and a non-positive or non-finite value is
    not a cap: the run then keeps its ATLAS_V3_TIMEOUT behaviour.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    if value != value or value in (float("inf"), float("-inf")) or value <= 0:
        return None
    return float(value)

class V3Handler(BaseHTTPRequestHandler):
    def _authorized(self) -> bool:
        """Token check for every route except /health. Constant-time
        compare; 401 bodies never echo token material."""
        if not adapters.SERVICE_TOKEN or self.path == "/health":
            return True
        import hmac
        got = self.headers.get("Authorization", "")
        want = f"Bearer {adapters.SERVICE_TOKEN}"
        if hmac.compare_digest(got, want):
            return True
        self._json_response(401, {
            "error": "unauthorized",
            "detail": "internal service auth is enabled; send "
                      "Authorization: Bearer <service-token> "
                      "(secrets/service-token)"})
        return False

    def do_POST(self):
        from structured_log import set_request_id as _set_rid
        _set_rid(self.headers.get("X-ATLAS-Request-ID", ""))
        if not self._authorized():
            return
        if self.path == "/v3/generate":
            self._handle_generate()
        elif self.path == "/v3/plan":
            self._handle_plan()
        elif self.path == "/internal/structural_edit":
            self._handle_structural_edit()
        elif self.path == "/internal/cyclomatic_complexity":
            self._handle_cyclomatic_complexity()
        elif self.path == "/internal/symbol_index":
            self._handle_symbol_index()
        elif self.path == "/internal/outline":
            self._handle_outline()
        elif self.path == "/internal/structural_check":
            self._handle_structural_check()
        elif self.path == "/internal/embedded_script_check":
            self._handle_embedded_script_check()
        elif self.path == "/internal/orphaned_symbols":
            self._handle_orphaned_symbols()
        elif self.path == "/health":
            self._json_response(200, {"status": "ok"})
        else:
            self._json_response(404, {"error": "not found"})

    def do_GET(self):
        from structured_log import set_request_id as _set_rid
        _set_rid(self.headers.get("X-ATLAS-Request-ID", ""))
        if not self._authorized():
            return
        if self.path == "/health":
            self._json_response(200, {"status": "ok", "service": "v3-pipeline"})
        else:
            self._json_response(404, {"error": "not found"})

    def _handle_generate(self):
        """Handle /v3/generate — accepts arbitrary file generation requests from Go proxy.

        Request format (V3GenerateRequest):
            file_path: str          — target file path
            baseline_code: str      — incumbent_baseline: the caller's own
                                      content. It becomes prose in the problem
                                      statement and is NOT a V3 candidate; pool
                                      index 0 is the phase-zero probe candidate
            project_context: dict   — other files in project {path: content}
            framework: str          — detected framework
            build_command: str      — build verification command
            constraints: list[str]  — extracted requirements
            tier: int               — 2 or 3
            working_dir: str        — project root
            budget_ms: int          — optional; the wall-clock cap the
                                      caller applies to this call. The run
                                      plans against it instead of
                                      ATLAS_V3_TIMEOUT when it is positive

        Response format (V3GenerateResponse):
            code: str               — winning candidate
            passed: bool            — whether a candidate is returned: it passed
                                      V3's own check (which may be a compile or
                                      an input-less run), or, when none did, it
                                      was picked by agreement; phase_solved says
                                      which. Not proof the task is done; the
                                      evidence envelope says what was shown.
            phase_solved: str       — how it was chosen: probe, phase1, pr_cot,
                                      refinement or budget after a check it
                                      passed, consensus by agreement alone
            candidates_tested: int
            winning_score: float
            total_tokens: int
            total_time_ms: float
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"error": f"invalid JSON body: {e}"})
            return

        file_path = body.get("file_path", "")
        baseline_code = body.get("baseline_code", "")
        project_context = body.get("project_context", {})
        framework = body.get("framework", "")
        build_command = body.get("build_command", "")
        constraints = body.get("constraints", [])
        user_message = body.get("user_message", "")
        tier = body.get("tier", 2)
        working_dir = body.get("working_dir", "")
        budget_ms = _positive_budget_ms(body.get("budget_ms"))

        if not file_path and not baseline_code:
            self._json_response(400, {"error": "file_path or baseline_code required"})
            return

        # Build problem description from the adapter request
        problem = _build_problem_from_request(
            file_path, baseline_code, project_context,
            framework, build_command, constraints, user_message,
        )

        # Build file context for the pipeline
        files = dict(project_context) if project_context else {}

        print(f"[generate] file={file_path} framework={framework} tier=T{tier}", flush=True)
        if build_command:
            print(f"[generate] requested build command: {build_command}", flush=True)
        print(f"[generate] constraints: {constraints}", flush=True)

        # Stream V3 pipeline progress as SSE events, then final result as JSON
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def emit_progress(stage, detail="", **data):
            """Stream progress events to the Go proxy."""
            payload = {"stage": stage, "detail": detail}
            if data:
                payload["data"] = data
            event = json.dumps(payload)
            try:
                self.wfile.write(f"data: {event}\n\n".encode())
                self.wfile.flush()
                # Also log for debugging
                print(f"  [SSE] {stage}: {_safe_progress_detail(stage, detail)}",
                      flush=True)
            except (BrokenPipeError, ConnectionResetError):
                # Client gone — flag it so pipeline.run aborts at the
                # next phase boundary instead of grinding on.
                emit_progress.disconnected = True
            except Exception as e:
                print(f"  [SSE ERROR] {e}", flush=True)

        scope, stop_watch, watcher = _watch_parent_for(self, "generate")
        # Trace identity is a JOIN KEY, never authority: it may be missing, it
        # may repeat, and the invocation id is what makes a pool unique.
        trace_id = self.headers.get(REQUEST_ID_HEADER, "") or ""

        # Run V3 pipeline with streaming progress
        try:
            result = pipeline.run(
                cancel_scope=scope,
                trace_request_id=trace_id,
                v3_invocation_id=scope.invocation_id,
                problem=problem,
                task_id=f"gen-{Path(file_path).stem}",
                progress_callback=emit_progress,
                files=files,
                file_path=file_path,  # PC-048: language-aware smoke check
                build_command=build_command,
                working_dir=working_dir or "/workspace",
                # The incumbent's exact request bytes, before
                # _build_problem_from_request wrapped them in prose. Read
                # only by the diagnostic capture.
                baseline_code=baseline_code,
                budget_ms=budget_ms,
            )
        except (adapters.ClientDisconnected, adapters.Cancelled) as e:
            print(f"[generate] pipeline aborted: {e}", flush=True)
            return
        except scoring.LensUnavailable as e:
            # The lens is required: the run stops, and the caller is told why
            # in the result it is waiting for. No code is sent, not even the
            # baseline, so nothing can read this as a finished generation.
            print(f"[generate] stopped: the lens cannot score ({e.reason})", flush=True)
            self._write_result({
                "code": "", "passed": False, "phase_solved": "lens_unavailable",
                "candidates_tested": 0, "winning_score": 0.0,
                "lens_unavailable": e.reason,
            })
            return
        finally:
            _release_scope(scope, stop_watch, watcher, "generate")

        # If baseline code was provided and pipeline didn't produce anything better,
        # use the baseline
        if not result.get("code") and baseline_code:
            result["code"] = baseline_code
            result["phase_solved"] = "baseline"

        # Structured evidence, serialised by evidence_wire. No policy is
        # decided here: this handler neither ranks, nor closes, nor infers a
        # strength -- it asks the serialiser for an envelope and writes it.
        # A record that cannot be serialised is sent as no evidence at all
        # rather than as a malformed envelope; the reason travels beside it so
        # the gap is visible instead of silent.
        evidence_envelope, evidence_unavailable = None, ""
        try:
            evidence_envelope = adapters.evidence_envelope(
                result, delivered_code=result.get("code", ""))
        except contract.ContractError as e:
            evidence_unavailable = str(e)
            print(f"[generate] evidence not serialisable: {e}", flush=True)

        # Send final result
        response = {
            "code": result.get("code", ""),
            "passed": result.get("passed", False),
            "phase_solved": result.get("phase_solved", "none"),
            "candidates_tested": result.get("candidates_generated", 0),
            "winning_score": result.get("winning_score", 0.0),
            "total_tokens": result.get("total_tokens", 0),
            "total_time_ms": result.get("total_time_ms", 0.0),
            "verification_evidence": result.get("verification_evidence", []),
            # Nested, versioned, and absent rather than empty when nothing was
            # measured. A consumer that does not know the field ignores it.
            "evidence": evidence_envelope,
            "evidence_unavailable_reason": evidence_unavailable,
        }

        self._write_result(response)

    def _write_result(self, response: dict) -> None:
        """The terminal `event: result` frame and the stream's end."""
        final = json.dumps(response)
        try:
            self.wfile.write(f"event: result\ndata: {final}\n\n".encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # Client closed mid-stream (timed out, cancelled, etc).
            # See ISSUES.md PC-026.
            pass

    def _handle_plan(self):
        """Handle /v3/plan — generate a structured plan for an agent task.

        Same SSE shape as /v3/generate so the Go proxy's SSE parser can
        reuse its frame-reading logic; only the stage names and the
        final result envelope differ.

        Request:
            user_message: str       — the prompt the user typed
            working_dir: str        — proxy's container working dir
            project_context: dict   — files the agent has read so far
            existing_files: list    — every path already in the workspace.
                                      This service has no /workspace mount, so
                                      it cannot look; without the list the
                                      planner proposes creating files that are
                                      already there.
            tier: int               — 2 or 3
            n_candidates: int       — optional; default 3

        Response (event: result):
            steps: list[dict]
            verify_step: str | null
            candidates_tested: int
            winning_score: float
            winning_index: int
            rationale: str
            reasons: list[str]
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"error": f"invalid JSON body: {e}"})
            return

        user_message = body.get("user_message", "")
        working_dir = body.get("working_dir", "")
        project_context = body.get("project_context", {}) or {}
        n_candidates = int(body.get("n_candidates", 3))

        if not user_message:
            self._json_response(400, {"error": "user_message required"})
            return

        print(f"[plan] msg={user_message[:80]!r} cwd={working_dir} files={len(project_context)} n={n_candidates}",
              flush=True)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def emit_progress(stage, detail="", **data):
            payload = {"stage": stage, "detail": detail}
            if data:
                payload["data"] = data
            event = json.dumps(payload)
            try:
                self.wfile.write(f"data: {event}\n\n".encode())
                self.wfile.flush()
                print(f"  [SSE plan] {stage}: {_safe_progress_detail(stage, detail)}",
                      flush=True)
            except BrokenPipeError:
                # best-effort: swallow on failure (caller continues)
                pass
            except Exception as e:
                print(f"  [SSE plan ERROR] {e}", flush=True)

        scope, stop_watch, watcher = _watch_parent_for(self, "plan")
        trace_id = self.headers.get(REQUEST_ID_HEADER, "") or ""
        try:
            plan = generate_plan(
                user_message=user_message,
                working_dir=working_dir,
                project_context=project_context,
                existing_files=body.get("existing_files") or [],
                n_candidates=n_candidates,
                progress_callback=emit_progress,
                cancel_scope=scope,
                request_identity=adapters.RequestIdentity(
                    request_id=trace_id, invocation_id=scope.invocation_id),
            )
        except Exception as e:
            print(f"  [plan ERROR] {e}", flush=True)
            plan = {
                "steps": [], "verify_step": None,
                "candidates_tested": 0, "winning_score": 0.0,
                "winning_index": -1,
                "rationale": f"planner failed: {e}",
                "reasons": [str(e)],
            }
        finally:
            _release_scope(scope, stop_watch, watcher, "plan")

        final = json.dumps(plan)

        try:
            self.wfile.write(f"event: result\ndata: {final}\n\n".encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # best-effort: swallow on failure (caller continues)
            pass

    def _handle_structural_edit(self):
        """POST /internal/structural_edit — friendly-selector structural replacement.

        Request:
            {"path": "...",  "source": "<full file content>",
             "selector": "function:foo" | "<body>" | ..., "content": "..."}
        Response (success):
            {"success": true, "new_content": "...", "byte_range": [start, end],
             "old_size": int, "new_size": int, "language": "python" | "html"}
        Response (failure):
            {"success": false, "error": "..."}

        Stateless transform — caller (the proxy) reads the file, sends content
        in, gets new content out. v3-service does no file IO; proxy writes
        after lens-scoring, matching write_file's flow so PC-207 lens-veto
        can still reject stub-shaped replacements.
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"success": False, "error": f"invalid JSON body: {e}"})
            return

        path = body.get("path", "")
        source_text = body.get("source", "")
        selector = body.get("selector", "")
        content = body.get("content", "")
        if not path or not selector or not source_text:
            self._json_response(400, {"success": False, "error": "missing required field(s): path, source, selector"})
            return

        result = structural_edit(path, source_text, selector, content)
        # Log per-call signal — matches the verbose-logging pattern we added
        # to score_candidate_per_step. Lets `docker logs atlas-v3-service-1`
        # answer "what did the model ask structural_edit to do" without SSE capture.
        if result.get("success"):
            print(
                f"  [structural_edit] {result['language']} {path} selector={selector!r} "
                f"matched bytes [{result['byte_range'][0]}-{result['byte_range'][1]}] "
                f"old={result['old_size']}B new={result['new_size']}B",
                flush=True,
            )
        else:
            print(f"  [structural_edit] FAIL path={path} selector={selector!r}: {result['error']}", flush=True)
        self._json_response(200, result)

    def _handle_symbol_index(self):
        """POST /internal/symbol_index — resolve candidate symbols to project snippets.

        Request:
            {"file_map": {"app.py": "...", "utils.py": "..."},
             "symbols": ["dashboard", "UserModel", ...],
             "max_snippets": 3, "max_lines_per_snippet": 200}
        Response:
            {"matched": [{name, kind, file, snippet, n_lines, truncated}],
             "skipped": [{name, reason}]}

        Caps default to 3 snippets / 200 lines each. Caller is the proxy,
        which extracts symbols from the user message via regex and walks
        the working directory for .py files (with its own size cap).
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"matched": [], "skipped": [], "error": f"invalid JSON body: {e}"})
            return

        file_map = body.get("file_map") or {}
        symbols = body.get("symbols") or []
        max_snippets = int(body.get("max_snippets", 3))
        max_lines = int(body.get("max_lines_per_snippet", 200))

        result = symbol_index(file_map, symbols, max_snippets=max_snippets, max_lines_per_snippet=max_lines)
        n_matched = len(result.get("matched", []))
        n_skipped = len(result.get("skipped", []))
        n_files = len(file_map)

        # Phase 3 (#39 point 4): attach each matched symbol's graph
        # neighborhood (callers / callees / impact) so the proxy can inject
        # structurally related code instead of name-matched snippets alone.
        # Additive: the "matched"/"skipped" shape is unchanged.
        try:
            from graph import symbol_neighborhood, build_graph
            if result.get("matched"):
                # Build the project graph ONCE, then neighborhood each matched
                # symbol against it (not once per symbol).
                g = build_graph(file_map)
                related = []
                for m in result["matched"]:
                    nb = symbol_neighborhood(file_map, m["name"], graph=g)
                    if nb["callers"] or nb["callees"] or nb["impact"]:
                        related.append(nb)
                if related:
                    result["graph"] = related
        except Exception as cge:
            print(f"  [symbol_index] graph neighborhood skipped: {cge}", flush=True)

        print(
            f"  [symbol_index] {n_files} files, {len(symbols)} candidates → "
            f"matched={n_matched} skipped={n_skipped}",
            flush=True,
        )
        self._json_response(200, result)

    def _handle_structural_check(self):
        """POST /internal/structural_check — does every direct-identifier call
        in this Python source resolve (local def, import, builtin, or a
        supplied project symbol)?

        Request:  {"path": "app.py", "source": "<file text>",
                   "project_context": {"other.py": "..."}}   # optional
        Response: {"ok": bool, "unresolved": ["render_template", ...],
                   "wildcard_imports": bool}

        Used by the proxy's structural gate on edit_file, structural_edit, and the
        write_file branches to refuse landing a .py file whose change
        introduces a NameError — a direct call to a name the file neither
        imports, defines, nor gets from builtins (#147: render_template
        called with only render_template_string imported landed as verified
        because the in-pipeline veto was skipped when no project_context was
        sent). Same resolver as the V3 structural veto; no execution.
        Returns the FULL unresolved list (no cap) — the proxy diffs
        original-vs-edited lists and a truncated list makes that comparison
        unsound. `ok:false` means the check couldn't run (tree-sitter
        missing / non-UTF-8) — the caller treats that as pass (fail-open).
        Malformed Python does NOT produce ok:false; tree-sitter parses it
        tolerantly and returns a best-effort extraction.
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"ok": False, "error": f"invalid JSON body: {e}"})
            return
        source = body.get("source", "") or ""
        project_symbols = build_project_symbols(body.get("project_context") or {})
        struct = structural_score(project_symbols, source, max_names=0)
        if not struct.get("ok"):
            # couldn't parse / tree-sitter unavailable → fail-open
            self._json_response(200, {"ok": False, "error": struct.get("error", "unavailable")})
            return
        self._json_response(200, {
            "ok": True,
            "unresolved": struct.get("unresolved_calls", []),
            "n_unresolved": struct.get("n_unresolved", 0),
            "wildcard_imports": struct.get("wildcard_imports", False),
        })

    def _handle_embedded_script_check(self):
        """POST /internal/embedded_script_check — does the JavaScript/CSS
        EMBEDDED in this file parse?

        Request:  {"path": "app.py", "source": "<file text>",
                   "previous": "<pre-edit file text, optional>"}
        Response: {"ok": bool, "findings": [{line, column, kind, where,
                                            message, hint, text}]}

        Covers what the sandbox's /syntax-check is
        blind to: a `<script>` block inside an .html/.jinja file, and — the
        2026-08-01 dogfooding case — inside a Python string literal handed to
        render_template_string. A stray `)` in that JavaScript leaves the
        Python compiling, the server starting and `curl /` returning 200 while
        the page is dead in the browser.

        With `previous` it also reports a render loop the edit stopped driving
        — a function a repeating timer used to call that now fires once and
        never re-arms. Same blind spot, one level up: that code parses.

        `ok: false` means the check couldn't run (tree-sitter-javascript
        missing, non-UTF-8 source) and the caller fails open. An unsupported
        extension is `ok: true` with no findings — nothing was wrong with it.
        Conservative by design: ambiguous blocks (template statement tags,
        `<script src>`, non-JS `type`, escaped Python strings) report nothing.
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"ok": False, "error": f"invalid JSON body: {e}"})
            return
        path = body.get("path", "") or ""
        source = body.get("source", "") or ""
        previous = body.get("previous", "") or ""
        result = embedded_script_check(path, source, previous)
        if result.get("findings"):
            first = result["findings"][0]
            print(f"  [embedded_script] {path} line {first['line']}: "
                  f"{first['kind']} {first['message']}", flush=True)
        self._json_response(200, result)

    def _handle_orphaned_symbols(self):
        """POST /internal/orphaned_symbols — top-level functions this edit
        ADDED that nothing in the file references.

        Request:  {"path": "todo.py", "previous": "<pre-run>", "source": "<now>"}
        Response: {"orphans": [{"name": "done_task", "line": 38}]}

        The mirror of /internal/structural_check: that reports a call with no
        definition, this reports a definition with no callers. Adding a
        function and never wiring it up is how "add a feature" fails —
        observed on "add a done command": the function was written correctly,
        the argv dispatcher was never touched, and `todo.py done 1` exited 0
        while doing nothing.

        `.py` only. Private (`_`-prefixed) and `test_`-prefixed names are
        skipped, and any mention of the name outside its own `def` line counts
        as a reference, so the check errs toward silence.
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"orphans": [], "error": f"invalid JSON body: {e}"})
            return
        path = body.get("path", "") or ""
        if not path.endswith(".py"):
            self._json_response(200, {"orphans": []})
            return
        orphans = orphaned_new_symbols(body.get("previous", "") or "",
                                       body.get("source", "") or "")
        if orphans:
            print(f"  [orphaned_symbols] {path}: "
                  f"{', '.join(o['name'] for o in orphans)}", flush=True)
        self._json_response(200, {"orphans": orphans})

    def _handle_outline(self):
        """POST /internal/outline — list a file's top-level functions/classes.

        Request:  {"path": "app.py", "source": "<file text>"}
        Response: {"symbols": [{name, kind, start_line, end_line}], "supported": bool}

        Reuses the same decorator-aware tree-sitter walk structural_edit uses, so a
        symbol the outline names is selectable by structural_edit with the same name.
        Bodies are NOT returned — this is the cheap "what's in here" probe so
        the model can then read just the one function's line range instead of
        the whole file. .py only here; the proxy regex-falls-back for the rest.

        Each symbol also carries its intra-file call-graph neighborhood
        (`calls` / `called_by`). The outline is the
        artifact the model inspects right before it decides WHICH symbol to
        edit, so this is where structural context earns its keep: it lets the
        model follow `total_value -> item_subtotal` to a callee-rooted bug
        instead of editing the function where the symptom merely surfaces
        (issue #39). Scoped to this one file — no project-wide scan — so it's
        cheap and never misses the file in a large repo. Additive: the
        symbols/supported shape is unchanged.
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"symbols": [], "supported": False, "error": f"invalid JSON body: {e}"})
            return
        path = body.get("path", "") or ""
        source = body.get("source", "") or ""
        symbols = []
        supported = False
        if path.endswith(".py") and _STRUCTURAL_EDIT_AVAILABLE:
            supported = True
            src = source.encode("utf-8")
            for name, kind, sb_, eb in _symbol_index_for_python_source(src):
                symbols.append({
                    "name": name,
                    "kind": kind,
                    "start_line": src[:sb_].count(b"\n") + 1,
                    "end_line": src[:eb].count(b"\n") + 1,
                })

        # Regions holding another language. The host grammar cannot see into a
        # string literal, so without this the outline of a Flask app whose UI
        # is one template reports `function:index` and nothing else — and the
        # model goes looking for `function:draw`.
        embedded = embedded_region_outline(path, source)

        # Call-graph neighborhood (issue #39). Build the single-file graph
        # once and attach callers/callees to each symbol the model can see.
        if symbols:
            try:
                from graph import symbol_neighborhood, build_graph
                file_map = {path: source}
                g = build_graph(file_map)
                for s in symbols:
                    nb = symbol_neighborhood(file_map, s["name"], graph=g)
                    if nb["callees"]:
                        s["calls"] = nb["callees"]
                    if nb["callers"]:
                        s["called_by"] = nb["callers"]
            except Exception as cge:  # pragma: no cover - import/extract guard
                print(f"  [outline] call-graph neighborhood skipped: {cge}", flush=True)

        out = {"symbols": symbols, "supported": supported}
        if embedded:
            out["embedded_regions"] = embedded
        self._json_response(200, out)

    def _handle_cyclomatic_complexity(self):
        """POST /internal/cyclomatic_complexity — McCabe CC for tier classification.

        Request:  {"path": "...", "source": "<full file content>"}
        Response: {"ok": true, "language": "python", "cyclomatic_complexity": 12}
                  or {"ok": false, "error": "..."}
        """
        content_len = int(self.headers.get("Content-Length", 0))
        try:
            body = json.loads(self.rfile.read(content_len) or b"{}")
        except json.JSONDecodeError as e:
            self._json_response(400, {"ok": False, "error": f"invalid JSON body: {e}"})
            return

        path = body.get("path", "")
        source_text = body.get("source", "")
        if not path or source_text == "":
            self._json_response(400, {"ok": False, "error": "missing required field(s): path, source"})
            return

        result = cyclomatic_complexity(path, source_text)
        # Per-call signal — same pattern as structural_edit. Lets us correlate
        # tier upgrades to the file that triggered them in docker logs.
        if result.get("ok"):
            print(
                f"  [cc] {result['language']} {path} cc={result['cyclomatic_complexity']}",
                flush=True,
            )
        # Don't log the not-supported case — it'd flood the log on every HTML/JSON write.
        self._json_response(200, result)

    def _json_response(self, code, data):
        try:
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(data).encode())
        except (BrokenPipeError, ConnectionResetError):
            # Client closed before we finished writing — typically a
            # docker healthcheck that hit its timeout. Not actionable.
            # See ISSUES.md PC-026.
            pass

    def log_message(self, format, *args):
        # Suppress default HTTP logging
        pass


# --- Main --------------------------------------------------------------------

class _PrivateValueStream:
    """Line-filtering wrapper for stdout/stderr: the v3 service logs
    via print(), so the stream is the serialization choke point (the
    equivalent of the root-handler filter in the FastAPI services)."""

    def __init__(self, stream):
        self._stream = stream

    def write(self, text):
        from private_values import filter_private_values
        filtered = filter_private_values(text)
        if os.environ.get("ATLAS_LOG_FORMAT", "").lower() == "json" \
                and filtered.strip():
            # Wrap each non-empty print line as a structured record so v3
            # matches the other services' JSON logs (it logs via print()).
            import json as _json
            from structured_log import current_identity as _identity
            for line in filtered.splitlines():
                if not line.strip():
                    continue
                rec = {"service": "v3-service", "level": "info", "msg": line}
                rid, inv = _identity()
                if rid:
                    rec["request_id"] = rid
                if inv:
                    rec["invocation_id"] = inv
                self._stream.write(_json.dumps(rec) + "\n")
            return
        self._stream.write(filtered)

    def flush(self):
        self._stream.flush()

    def __getattr__(self, name):
        return getattr(self._stream, name)


if __name__ == "__main__":
    sys.stdout = _PrivateValueStream(sys.stdout)
    sys.stderr = _PrivateValueStream(sys.stderr)
    from structured_log import install as _install_logging
    _install_logging("v3-service")
    print(f"ATLAS V3 Pipeline Service starting on :{PORT}")
    print(f"  Inference:     {adapters.INFERENCE_URL}")
    print(f"  Geometric Lens: {adapters.LENS_URL}")
    print(f"  Sandbox: {adapters.SANDBOX_URL}")

    # ThreadingHTTPServer: a long pipeline call must not starve /health and
    # the /internal/* endpoints. Shared state is thread-safe by construction:
    # LLMAdapter._lock serializes the llama.cpp backend, pipeline components
    # are read-only after __init__ except telemetry appends (each stage event
    # is one small O_APPEND write; the per-task pipeline summary takes
    # pipeline._SUMMARY_LOCK), per-run state (events, candidates, adapters)
    # is local to each request, and the graph package's FileGraphCache
    # carries its own lock.
    server = ThreadingHTTPServer(("0.0.0.0", PORT), V3Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nShutting down...")
        server.shutdown()
