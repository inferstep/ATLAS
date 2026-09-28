"""Outbound service adapters for the V3 service: llama-server chat/embedding
clients, the sandbox client, and internal-auth plumbing."""

import json
import os
import re
import socket
import threading
import time
import contextlib
import dataclasses
import http.client
import urllib.request
import urllib.error
import urllib.parse
from typing import Any, Dict, List, Optional, Tuple

from stages.llm_client import chatml_to_messages

# --- Configuration -----------------------------------------------------------

INFERENCE_URL = os.environ.get("ATLAS_INFERENCE_URL", "http://localhost:8080")
LENS_URL = os.environ.get("ATLAS_LENS_URL", "http://localhost:8099")
SANDBOX_URL = os.environ.get("ATLAS_SANDBOX_URL", "http://localhost:30820")
# Where the sandbox mounts the workspace. A caller's absolute path is made
# relative to this before it is sent; see _sandbox_safe_filename.
SANDBOX_WORKSPACE_ROOT = os.environ.get("ATLAS_SANDBOX_WORKSPACE", "/workspace")


def _max_inflight() -> int:
    """How many generations may be in flight against llama.cpp at once.

    Defaults to the server's slot count, since a request beyond that queues
    behind a slot rather than gaining anything — llama.cpp oversubscribed
    past its slots degrades latency sharply. ATLAS_V3_MAX_INFLIGHT overrides;
    1 restores the fully serialized behaviour.
    """
    for var in ("ATLAS_V3_MAX_INFLIGHT", "ATLAS_PARALLEL_SLOTS", "PARALLEL_SLOTS"):
        raw = os.environ.get(var)
        if raw and raw.strip().isdigit() and int(raw) > 0:
            return int(raw)
    return 4


def _load_service_token() -> str:
    """Internal-auth token (Authorization: Bearer). Empty = auth
    disabled — an install without `atlas init` keeps the open-localhost
    behavior and `atlas doctor` warns. The value is never logged."""
    path = os.environ.get("ATLAS_SERVICE_TOKEN_FILE",
                          "/run/atlas-secrets/service-token")
    try:
        with open(path) as fh:
            return fh.read().strip()
    except OSError:
        return ""


SERVICE_TOKEN = _load_service_token()

if SERVICE_TOKEN:
    # Outbound injection: one opener covers every urllib call site
    # (llama, lens, sandbox). urllib merges addheaders under explicit
    # per-request headers, so requests that already set Authorization
    # keep their own value.
    _opener = urllib.request.build_opener()
    _opener.addheaders = [("Authorization", f"Bearer {SERVICE_TOKEN}")]
    urllib.request.install_opener(_opener)


REQUEST_ID_HEADER = "X-ATLAS-Request-ID"
INVOCATION_ID_HEADER = "X-ATLAS-V3-Invocation-ID"


@dataclasses.dataclass(frozen=True)
class RequestIdentity:
    """The identity of the request an adapter is serving.

    Frozen and owned by the request thread that builds it. Worker threads
    read it off the request-scoped adapter instance, which is how it reaches
    them — never off a ContextVar, which a new thread does not inherit.
    """
    request_id: str = ""
    invocation_id: str = ""

    def headers(self) -> Dict[str, str]:
        out: Dict[str, str] = {}
        if self.request_id:
            out[REQUEST_ID_HEADER] = self.request_id
        if self.invocation_id:
            out[INVOCATION_ID_HEADER] = self.invocation_id
        return out


class RequestIdentityMissing(Exception):
    """An adapter serving a request has no identity to send.

    Raised instead of sending an unattributed inference call. A permissive
    upstream would accept that call and answer it, so the omission would
    otherwise surface only as a missing correlation ID in someone else's
    logs — or, against an upstream that enforces attribution, as candidate
    scarcity with no stated cause.
    """


def _service_headers(rid: str = "", invocation_id: str = "") -> dict:
    """Headers for outbound service-to-service calls: forwards the
    current request's correlation ID and V3 invocation ID so lens/sandbox/
    llama records join the same trace, and so a service that calls the
    model on this request's behalf (the Lens embedding a candidate) can
    forward the same pair. Both fall back to the request's ContextVars
    (structured_log), which the handler binds; pass them explicitly from
    background threads, which inherit neither. Attribution only: nothing
    downstream decides on these headers."""
    headers = {"Content-Type": "application/json"}
    if not rid or not invocation_id:
        try:
            from structured_log import get_invocation_id, get_request_id
            rid = rid or get_request_id()
            invocation_id = invocation_id or get_invocation_id()
        except ImportError:
            pass
    if rid:
        headers[REQUEST_ID_HEADER] = rid
    if invocation_id:
        headers[INVOCATION_ID_HEADER] = invocation_id
    return headers


# --- PC-061 step B: typed event emission ------------------------------------
# --- LLM Adapter (calls llama-server /v1/chat/completions) ----------------------------

class LLMAdapter:
    """Calls llama-server's /v1/chat/completions, parsing ChatML prompts into messages.

    PC-206: `thinking` controls template-level reasoning when supported.
    - False (default) — `enable_thinking=False`.
      Required for grammar-constrained JSON output (the agent's tool-call
      shape) and for the tight V3 sampling loop where reasoning would 5-20×
      output token cost. This matches the previously hardcoded behavior.
    - True — `enable_thinking=True`. Use for
      high-reasoning-value calls (planner, verification, claim-check) where
      the output can absorb a preamble and the strip pattern in __call__
      cleans up `<think>...</think>` blocks before downstream JSON parse.

    The default is set per-instance; individual __call__ invocations can
    override via the `thinking` keyword for ad-hoc switches.
    """

    # Bounds how many generations this service has in flight at once.
    #
    # This was a Lock, added when the service moved to ThreadingHTTPServer so
    # a long pipeline call could not starve /health and /internal/* — its job
    # was to keep concurrent REQUESTS from oversubscribing llama.cpp. Being
    # class-level, it also serialized the calls inside a single pipeline run,
    # which is where the cost landed: PlanSearch generates its candidates from
    # independent plans and seeds, and 4 of them ran end to end at ~22s each
    # while three of llama-server's four slots sat idle. Measured 2026-08-03,
    # 8 sequential calls totalling 166s against the proxy's 180s cap.
    #
    # A semaphore keeps the original guarantee — never more in flight than the
    # backend has slots — while letting independent generations share them.
    # llama.cpp batches concurrent slots itself; serializing here did that job
    # a second time, worse.
    _slots = threading.BoundedSemaphore(_max_inflight())

    # Counter updates are read-modify-write and now run under concurrency.
    _counter_lock = threading.Lock()

    def __init__(self, progress_callback=None, thinking: bool = False,
                 deadline: Optional[float] = None):
        self.call_count = 0
        self.total_tokens = 0
        self.total_time_ms = 0.0
        self._progress = progress_callback
        # Request-scoped cancellation. None for callers with no request
        # (bench, CLI): they are never cancelled and must not be affected.
        self.cancel_scope = None
        # Request-scoped identity, set by the request thread that builds this
        # adapter and read by every generation it opens — including ones
        # dispatched from PlanSearch's worker threads, which is the whole
        # reason it lives here rather than in a ContextVar. None carries the
        # same meaning as a None cancel_scope: no request (bench, CLI).
        self.request_identity: Optional[RequestIdentity] = None
        self.thinking = thinking
        # Monotonic wall-clock (time.time()) after which no new generation
        # may start. None leaves the adapter unbounded, which is what the
        # bench and any caller without a cap want.
        self.deadline = deadline

    # Fallback decode rate (tokens/sec) for sizing the first call, before
    # this run has observed one. Measured on a 12B Q4 at 4 slots: ~25 tok/s
    # single-stream. Deliberately conservative — overestimating the rate
    # asks for more tokens than the clock can deliver, which is the failure
    # this exists to prevent.
    _ASSUMED_TOK_PER_SEC = 20.0

    # Below this many tokens a generation cannot produce anything useful,
    # so the budget is spent rather than nearly spent.
    _MIN_USEFUL_TOKENS = 128

    def _observed_tok_per_sec(self) -> float:
        if self.total_time_ms <= 0 or self.total_tokens <= 0:
            return self._ASSUMED_TOK_PER_SEC
        return max(1.0, self.total_tokens / (self.total_time_ms / 1000.0))

    def _budget_max_tokens(self, max_tokens: int) -> int:
        """Shrink max_tokens to what the remaining clock can actually decode.

        A generation runs until it stops or hits max_tokens, so an 8192-token
        ceiling at ~25 tok/s is a 327-second call — longer than the whole
        180s budget. Measured 2026-08-04: every V3 hang-up in a 28-session
        run was the pipeline cut mid-probe, the first generation, which had
        the full budget and still did not finish. Refusing the call is not
        the answer either — that produces nothing at all. Asking for a
        length the clock can deliver is.

        Raises BudgetExhausted when even a minimal generation will not fit.
        """
        if self.deadline is None:
            return max_tokens
        left_s = self.deadline - time.time()
        # Leave room to read the response and hand back a result.
        affordable = int((left_s - 5.0) * self._observed_tok_per_sec() * 0.8)
        if affordable < self._MIN_USEFUL_TOKENS:
            raise BudgetExhausted(
                f"{left_s:.0f}s left decodes ~{max(affordable, 0)} tokens at "
                f"{self._observed_tok_per_sec():.0f} tok/s")
        return min(max_tokens, affordable)

    @property
    def avg_call_ms(self) -> float:
        """Average observed per-call latency (0.0 before the first call).
        Feeds the refinement loop's one-iteration cost estimate."""
        if not self.call_count:
            return 0.0
        return self.total_time_ms / self.call_count

    def _emit(self, stage: str, detail: str = "", **data):
        if self._progress:
            try:
                self._progress(stage, detail, **data)
            except TypeError:
                # Older two-arg callbacks don't accept **data — call back
                # to the legacy signature so we stay compatible.
                self._progress(stage, detail)

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int],
                 thinking: Optional[bool] = None) -> Tuple[str, int, float]:
        # No new inference once the caller is gone.
        #
        # V3 is a synchronous server: its only disconnect signal is a
        # BrokenPipeError on the next SSE write, which sets `disconnected` on
        # the progress callback, and the pipeline consulted that flag only at
        # PHASE boundaries. Nothing consulted it where the GPU is actually
        # spent. Measured on a real acquisition: a run whose agent request had
        # already returned at its 570 s work deadline STARTED a 24th
        # generation at +569.8 s and ran it 39.8 s to completion, leaving the
        # relay holding an in-flight call after the terminal.
        #
        # This is the one chokepoint every V3 generation passes through, so
        # the check belongs here rather than at each of its callers. It stops
        # a call from STARTING; a call already in flight is a separate
        # concern and is not claimed to be handled by this.
        if getattr(self._progress, "disconnected", False):
            raise ClientDisconnected(
                "client disconnected; refusing to start another generation")
        # The scope is the signal that does not depend on discovering a broken
        # output socket: the handler cancels it the moment the parent goes.
        if self.cancel_scope is not None and self.cancel_scope.cancelled:
            raise Cancelled("request cancelled; refusing to start another generation")
        max_tokens = self._budget_max_tokens(max_tokens)
        with LLMAdapter._counter_lock:
            self.call_count += 1
            call_no = self.call_count

        # Resolve per-call override against the instance default (PC-206).
        thinking_resolved = self.thinking if thinking is None else thinking

        body = {
            "model": "default",
            "prompt": prompt,
            "max_tokens": max_tokens,
            "temperature": temperature,
            "stream": True,  # streaming: per-token visibility + no
                              # 300s urllib read-timeout on long gens.
            "stop": ["\n\n\n\n"],
            "top_k": 20,
            "top_p": 0.95,
            "_thinking": thinking_resolved,  # consumed by _send, popped before send
        }
        if seed is not None:
            body["seed"] = seed

        start = time.time()
        # Marker so the TUI can frame this LLM call. Mirrors what
        # atlas-proxy emits around its own llama.cpp calls.
        self._emit("llm_start", f"call #{call_no}",
                   call=call_no, max_tokens=max_tokens,
                   temperature=temperature)
        data = self._send(body, call_no)
        # The streaming send already emitted token events; emit a
        # closing marker with totals so the TUI can replace the live
        # row with a compact summary.
        elapsed_ms = (time.time() - start) * 1000
        completion_tokens = data.get("usage", {}).get("completion_tokens", 0) \
            or data.get("usage", {}).get("total_tokens", 0)
        with LLMAdapter._counter_lock:
            self.total_time_ms += elapsed_ms
        self._emit("llm_end", f"{completion_tokens} tok · {elapsed_ms:.0f}ms",
                   call=call_no, tokens=completion_tokens,
                   elapsed_ms=int(elapsed_ms))

        # Parse response
        content = ""
        tokens = completion_tokens
        if "choices" in data:
            content = data["choices"][0].get("text", "")

        # Strip thinking blocks
        content = re.sub(r'<think>.*?</think>\s*', '', content, flags=re.DOTALL)
        if '</think>' in content and '<think>' not in content:
            content = content[content.index('</think>') + len('</think>'):].strip()

        with LLMAdapter._counter_lock:
            self.total_tokens += tokens
        return content, tokens, elapsed_ms

    def _inference_headers(self) -> Dict[str, str]:
        """Headers for one inference call, resolved from the adapter's own
        request identity.

        Deliberately does NOT fall back to the request-ID ContextVar. A
        generation dispatched from a PlanSearch worker thread does not
        inherit the request thread's ContextVar, so a fallback here reads as
        "no request" and silently strips attribution from exactly the calls
        that are hardest to trace back. The identity is request-scoped state
        and travels on the request-scoped adapter, like cancel_scope.

        Serving a request without an identity is a wiring error, not a
        runtime condition, so it raises rather than sending the call: an
        upstream that does not enforce attribution would answer it, and the
        defect would survive as missing correlation IDs instead of an error.
        """
        if self.request_identity is None:
            if self.cancel_scope is not None:
                raise RequestIdentityMissing(
                    "adapter is serving a request (cancel_scope set) but has "
                    "no request_identity; refusing to send an unattributed "
                    "inference call")
            # No request at all (bench, CLI): nothing to attribute.
            return {"Content-Type": "application/json"}
        headers = {"Content-Type": "application/json"}
        headers.update(self.request_identity.headers())
        return headers

    @contextlib.contextmanager
    def _open_inference(self, payload: bytes):
        """One inference connection, registered with the request's scope.

        Registration is the cancellation handle. A scope already cancelled
        refuses to open at all, so no call can slip through between the
        dispatch check and the socket.
        """
        parsed = urllib.parse.urlsplit(INFERENCE_URL)
        host, port = parsed.hostname or "127.0.0.1", parsed.port
        if parsed.scheme == "https":
            conn = http.client.HTTPSConnection(host, port, timeout=600)
        else:
            conn = http.client.HTTPConnection(host, port, timeout=600)
        scope = self.cancel_scope
        if scope is not None and not scope.register(conn):
            conn.close()
            raise Cancelled("request cancelled before the connection opened")
        try:
            # After registration, so a cancelled scope is reported as
            # cancellation rather than as whatever the next check happens to
            # be. Registration is where cancellation is decided.
            headers = self._inference_headers()
            path = (parsed.path or "") + "/v1/chat/completions"
            conn.request("POST", path, body=payload, headers=headers)
            resp = conn.getresponse()
            if resp.status != 200:
                detail = resp.read()[:200]
                raise urllib.error.HTTPError(
                    INFERENCE_URL, resp.status, detail.decode("utf-8", "replace"),
                    hdrs=None, fp=None)
            yield resp
        finally:
            if scope is not None:
                scope.unregister(conn)
            try:
                conn.close()
            except Exception:  # noqa: BLE001
                pass

    def _send(self, body: dict, call_no: int = 0) -> dict:
        """Send to llama-server via /v1/chat/completions.

        V3 modules generate ChatML prompts. We parse them into messages format
        for the chat endpoint. ChatML format:
            <|im_start|>system\n...\n<|im_end|>\n<|im_start|>user\n...\n<|im_end|>\n<|im_start|>assistant\n
        """
        prompt = body.pop("prompt", "")
        model_name = os.environ.get("ATLAS_MODEL_NAME", "local-model")

        # PC-206: thinking flag drops down from __call__. Default False so
        # callers get bounded generation unless they opt into reasoning.
        thinking = bool(body.pop("_thinking", False))

        # Convert the internal prompt carrier into structured messages before
        # llama-server applies the selected model's own template.
        messages = chatml_to_messages(prompt)
        if "<|im_start|>" not in prompt:
            print(f"  [LLM] ChatML parse failed, using raw prompt ({len(prompt)} chars)", flush=True)
        else:
            print(f"  [LLM] Parsed {len(messages)} messages from ChatML"
                  f" (thinking={'on' if thinking else 'off'})", flush=True)
            if thinking:
                # Strip the legacy directive from old prompt templates when a
                # caller explicitly enables reasoning.
                for msg in messages:
                    if msg["role"] == "user" and msg["content"].startswith("/nothink"):
                        msg["content"] = msg["content"][len("/nothink"):].lstrip("\n")

        chat_body = {
            "model": model_name,
            "messages": messages,
            "max_tokens": body.get("max_tokens", body.pop("n_predict", 4096)),
            "temperature": body.get("temperature", 0.6),
            "stream": bool(body.get("stream", False)),
            # The chat template may honor enable_thinking; templates that do
            # not support it ignore the kwarg. Reasoning blocks are stripped
            # in __call__ before downstream JSON parsing.
            "chat_template_kwargs": {"enable_thinking": thinking},
        }
        if chat_body["stream"]:
            # Need usage in the final chunk so we can report token counts.
            chat_body["stream_options"] = {"include_usage": True}
        if "seed" in body:
            chat_body["seed"] = body["seed"]

        payload = json.dumps(chat_body).encode()
        for attempt in range(5):
            try:
                with LLMAdapter._slots:
                    # http.client, not urlopen: the connection object exists
                    # BEFORE the request is sent, so a cancelling thread has a
                    # handle at every point -- waiting for response headers,
                    # mid-stream, and deep inside a long generation. urlopen
                    # offers no handle until headers arrive, which is exactly
                    # the window a cancellation has to survive.
                    with self._open_inference(payload) as resp:
                        if not chat_body["stream"]:
                            data = json.loads(resp.read())
                            # Convert chat response to completions format
                            if "choices" in data and len(data["choices"]) > 0:
                                choice = data["choices"][0]
                                if "message" in choice:
                                    choice["text"] = choice["message"].get("content", "")
                            return data
                        # Streaming path: parse SSE chunks, accumulate
                        # delta content, and forward each delta to the
                        # progress callback as ("token", text). The 600s
                        # urllib timeout is per-read; with continuous
                        # token flow each read is sub-second, so long
                        # generations no longer hit the old 300s ceiling.
                        full = []
                        reasoning = []
                        usage = {}
                        first_chunk_logged = False
                        for raw in resp:
                            line = raw.decode("utf-8", "replace").rstrip("\r\n")
                            if not line.startswith("data:"):
                                continue
                            payload = line[5:].lstrip()
                            if payload == "[DONE]":
                                break
                            try:
                                chunk = json.loads(payload)
                            except json.JSONDecodeError:
                                continue
                            choices = chunk.get("choices") or []
                            if choices:
                                delta_obj = choices[0].get("delta", {}) or {}
                                if not first_chunk_logged and delta_obj:
                                    # Metadata only. This line used to carry a
                                    # 200-character sample of the delta, which
                                    # is candidate source: harmless while logs
                                    # were unstructured and local, and a
                                    # retained content leak the moment
                                    # ATLAS_LOG_FORMAT=json makes these records
                                    # evidence. 84 such records were captured in
                                    # one 63-cell rehearsal. The diagnostic
                                    # exists to say which keys a build sends and
                                    # whether anything arrived; request and
                                    # invocation identity already join the line
                                    # to its call, so no sample, prefix, excerpt
                                    # or encoding of the content is needed.
                                    _c = delta_obj.get("content")
                                    _r = delta_obj.get("reasoning_content")
                                    print(f"  [LLM] first delta keys={sorted(delta_obj.keys())} "
                                          f"content_chars={len(_c) if isinstance(_c, str) else 0} "
                                          f"reasoning_chars={len(_r) if isinstance(_r, str) else 0}",
                                          flush=True)
                                    first_chunk_logged = True
                                delta = delta_obj.get("content", "") or ""
                                # Some llama.cpp builds split <think>…</think>
                                # into delta.reasoning_content. Capture it as
                                # a fallback so we don't end up with 2048 tok
                                # of reasoning and zero parseable text.
                                rdelta = delta_obj.get("reasoning_content", "") or ""
                                if delta:
                                    full.append(delta)
                                    # Tagged with the call it belongs to:
                                    # concurrent generations interleave in
                                    # this stream, and an untagged token
                                    # cannot be attributed to one of them.
                                    self._emit("token", delta, call=call_no)
                                if rdelta:
                                    reasoning.append(rdelta)
                            u = chunk.get("usage")
                            if u:
                                usage = u
                        text = "".join(full)
                        if not text and reasoning:
                            # Reasoning-only response: surface it so the
                            # parser at least sees the JSON the model
                            # buried inside its think block.
                            print(f"  [LLM] reasoning-only response ({len(reasoning)} chunks, "
                                  f"{sum(len(r) for r in reasoning)} chars) — using as content",
                                  flush=True)
                            text = "".join(reasoning)
                        return {
                            "choices": [{"text": text}],
                            "usage": usage,
                        }
            except (urllib.error.HTTPError, OSError) as e:
                print(f"  [LLM] Attempt {attempt+1} failed: {e}", flush=True)
                if attempt < 4:
                    time.sleep(2 * (attempt + 1))
                else:
                    raise
        # Unreachable: the for loop above always either returns inside
        # the success branch or raises on the 5th failure. Explicit
        # for py/mixed-returns (the implicit fall-through returns None,
        # which violates the -> dict signature).
        raise RuntimeError("unreachable: _send loop must return or raise")


class BudgetExhausted(Exception):
    """Not enough of ATLAS_V3_TIMEOUT is left to start another generation.

    Raised from LLMAdapter.__call__ rather than checked at phase boundaries.
    Boundary checks cannot hold: every phase runs its own internal loop —
    PR-CoT alone issues two calls — so a check that reserves one call is
    already wrong by the second. Measured 2026-08-03: a boundary check with
    ~50s left correctly allowed PR-CoT against a ~34s reserve, and PR-CoT
    spent 44s then started a 21s call, overrunning the cap.

    The pipeline catches this and returns its best candidate so far, which is
    the contract an anytime algorithm owes its caller.
    """


# --- connection teardown ------------------------------------------------------
#
# HONEST NOTE ON THE INTERFACE USED.
#
# Interrupting a thread already blocked in recv() requires shutdown() on the
# underlying socket. close() alone does not do it: measured over real sockets,
# a call blocked waiting for response headers took the upstream's full 5.76s
# to return, and a mid-stream cancellation never reached the upstream at all.
#
# http.client exposes that socket as HTTPConnection.sock. It is an ordinary
# instance attribute -- no leading underscore, stable across every CPython 3.x
# -- but it is NOT part of the documented http.client API. This is the one
# undocumented interface the cancellation path depends on, and it is named
# here rather than buried at the call site.
#
# The dependency is guarded, not assumed: HTTPCONNECTION_EXPOSES_SOCK records
# whether the attribute exists on this runtime, a test fails loudly if a future
# Python removes it, and teardown degrades to close() rather than raising.
# Cancellation would weaken on such a runtime, so the build must break first.

HTTPCONNECTION_EXPOSES_SOCK = "sock" in http.client.HTTPConnection("localhost").__dict__


def _abort_connection(conn) -> bool:
    """Tear a connection down hard. Returns True if the socket was shut down.

    Safe when no socket exists yet, when the connection is already closed, and
    when called twice: every failure path falls through to close(). A raise
    here would leave later connections in the same cancel() unclosed, so
    nothing is allowed to propagate.
    """
    did_shutdown = False
    try:
        sock = getattr(conn, "sock", None)
        if sock is not None:
            sock.shutdown(socket.SHUT_RDWR)
            did_shutdown = True
    except (OSError, AttributeError):
        # Already closed, never connected, or an unexpected object. close()
        # below still runs.
        pass
    try:
        conn.close()
    except Exception:  # noqa: BLE001 - a close that fails is still cancelled
        pass
    return did_shutdown


class CancelScope:
    """Request-scoped cancellation for one V3 invocation.

    V3 is a synchronous server, so there is no task tree to cancel. What it
    does have is a set of live outbound connections, and closing those is what
    actually stops work: a blocked read returns, and the upstream sees its
    client go away.

    Measured before this existed: a generation dispatched at a parent's work
    deadline ran 39.8s to completion after the agent request had returned,
    and the inference stub recorded its client as still connected the whole
    time. Waiting for a broken SSE write to notice cannot fix that -- while a
    call is in flight nothing is being written.

    Cancellation is idempotent, scoped to one invocation, and never reaches
    another request's connections.
    """

    def __init__(self, invocation_id: str = ""):
        self.invocation_id = invocation_id
        self._lock = threading.Lock()
        self._cancelled = False
        self._live = set()
        self.closed_on_cancel = 0

    @property
    def cancelled(self) -> bool:
        with self._lock:
            return self._cancelled

    def register(self, conn) -> bool:
        """Track a live connection. Returns False if already cancelled, in
        which case the caller must not proceed."""
        with self._lock:
            if self._cancelled:
                return False
            self._live.add(conn)
            return True

    def unregister(self, conn) -> None:
        with self._lock:
            self._live.discard(conn)

    def cancel(self) -> int:
        """Close every live connection. Safe to call repeatedly."""
        with self._lock:
            self._cancelled = True
            live, self._live = list(self._live), set()
        for conn in live:
            _abort_connection(conn)
        with self._lock:
            self.closed_on_cancel += len(live)
        return len(live)


class Cancelled(Exception):
    """Raised where a cancelled scope stops work, so callers can tell an
    intentional stop from a transport failure."""


class ClientDisconnected(Exception):
    """SSE client went away mid-pipeline. Raised at phase boundaries in
    V3PipelineService.run so a dead client doesn't keep burning GPU minutes;
    the HTTP handlers catch it and stop without writing a response."""


# --- Sandbox Adapter (calls sandbox /execute) ---------------------------------

def _sandbox_safe_filename(filename: str) -> Optional[str]:
    """The relative name the sandbox will accept, or None when none exists.

    The sandbox refuses an absolute path, a backslash-rooted one, or one
    containing ".." -- _safe_overlay_path raises HTTP 400 -- and this boundary
    used to forward the caller's spelling unchanged. The proxy relativises at
    its own sandbox boundary for exactly this reason (proxy/gates.go,
    workspaceRelativeName); this one did not. The proxy sends V3 a RESOLVED
    absolute file_path, so the filename reaching here was always absolute, and
    every candidate came back "syntax verification unavailable: HTTP Error 400:
    Bad Request" -- a verification that never ran, reported as a candidate that
    failed. Measured live on an exposed task 2026-09-18: 3 of 3 candidates
    failed that way, the pipeline proposed nothing, and ~4 minutes of the
    session's budget bought a result that could not have been otherwise.

    Relativised here, at the one boundary that talks to the sandbox, so every
    caller is fixed at once. When no safe relative form exists the filename is
    omitted and the check still runs: that only forgoes the path-scoped checks
    (the Jinja-template scoping, which keys off a "templates/" segment), which
    is exactly what happens today for a caller that supplies no filename.

    This never decides anything about the code. It decides whether the sandbox
    is asked at all, and the sandbox still returns the verdict.
    """
    name = (filename or "").strip().replace("\\", "/")
    if not name:
        return None
    root = (SANDBOX_WORKSPACE_ROOT or "").rstrip("/")
    if name.startswith("/"):
        # Only a path under the workspace has a relative form that means the
        # same thing there. Anything else absolute is refused rather than
        # rebased, which is what the proxy's workspaceRelativeName does when
        # filepath.Rel escapes the working directory.
        if not root or not name.startswith(root + "/"):
            return None
        name = name[len(root) + 1:]
    parts = [p for p in name.split("/") if p and p != "."]
    if not parts or any(p == ".." for p in parts):
        return None
    return "/".join(parts)


# The sandbox's run cap for one execution, sent with every request.
_EXECUTE_TIMEOUT_S = 15


class SandboxAdapter:
    """Calls the sandbox service for code execution.

    PC-046: optional `project_files` dict ships supporting files (other
    modules from the user's project) into the sandbox workspace so
    multi-file imports resolve. Without this, a candidate that does
    `from utils import helper` fails ImportError in the sandbox even
    though it would work on the user's machine.

    `test_input` is piped to the run as standard input (the /execute
    `stdin` field) — the same stdin contract the bench sandbox adapters
    implement, so per-candidate test inputs reach the candidate under
    test.
    """

    def __init__(self, project_files: Optional[Dict[str, str]] = None):
        self.project_files = project_files or {}

    def __call__(self, code: str, test_input: str = "",
                 files: Optional[Dict[str, str]] = None) -> Tuple[bool, str, str]:
        """Execute Python `code` in the sandbox."""
        body = {
            "code": code,
            "language": "python",
            "timeout": _EXECUTE_TIMEOUT_S,
        }
        if test_input:
            # Empty string keeps the executor default (inherit server
            # stdin) — every no-input call site passes "" positionally.
            body["stdin"] = test_input
        # Per-call staging on top of project context: a self-test case's own
        # input file is this request's, and where the two name the same file
        # the case wins. Omitting `files` leaves every existing caller's body
        # byte for byte what it was.
        staged = dict(self.project_files or {})
        if files:
            staged.update(files)
        if staged:
            body["files"] = staged
        try:
            req = urllib.request.Request(
                f"{SANDBOX_URL}/execute",
                data=json.dumps(body).encode(),
                headers=_service_headers(),
            )
            # 45s client timeout: the sandbox's server-side budgets (syntax
            # check + optional pip install + lint + the 15s run cap) can sum
            # past 30s, and the old 20s read timeout gave up on executions
            # the sandbox would still have completed.
            # Client read timeout is derived from the execution budget plus
            # bounded overhead, never a fixed value below it.
            _client_timeout = max(45, _EXECUTE_TIMEOUT_S + 30)
            with urllib.request.urlopen(req, timeout=_client_timeout) as resp:
                data = json.loads(resp.read())
                return data.get("success", False), data.get("stdout", ""), data.get("stderr", "")
        except Exception as e:
            return False, "", str(e)

    def syntax_check(self, code: str, language: str, filename: str = "") -> Tuple[bool, str, str]:
        """Ask the sandbox to parse or compile source without executing it."""
        body = {
            "code": code,
            "language": language,
            "filename": _sandbox_safe_filename(filename),
        }
        try:
            req = urllib.request.Request(
                f"{SANDBOX_URL}/syntax-check",
                data=json.dumps(body).encode(),
                headers=_service_headers(),
            )
            with urllib.request.urlopen(req, timeout=20) as resp:
                data = json.loads(resp.read())
            if data.get("status") == "not_run":
                # The checker was stopped before a verdict: not a pass, and
                # not a syntax error in the candidate either.
                return False, "", ("syntax verification unavailable: the checker ended "
                                   f"{data.get('outcome', 'early')}")
            errors = data.get("errors", [])
            error_text = "\n".join(str(error) for error in errors)
            return bool(data.get("valid", False)), "", error_text
        except Exception as e:
            return False, "", f"syntax verification unavailable: {e}"

    def run_command(
        self,
        command: str,
        files: Optional[Dict[str, str]] = None,
        cwd: str = "/workspace",
        timeout: int = 60,
    ) -> Tuple[bool, str, str, Dict[str, Any]]:
        """Run a project command through the sandbox /shell endpoint.

        `files` is an ephemeral overlay: the sandbox snapshots /workspace,
        applies these relative paths in the temp copy, runs the command there,
        then deletes the temp copy. It lets V3 verify a candidate without
        writing it to the user's real workspace.
        """
        body = {
            "command": command,
            "cwd": cwd or "/workspace",
            "timeout": timeout,
        }
        if files:
            body["files"] = files
        try:
            req = urllib.request.Request(
                f"{SANDBOX_URL}/shell",
                data=json.dumps(body).encode(),
                headers=_service_headers(),
            )
            with urllib.request.urlopen(req, timeout=timeout + 10) as resp:
                data = json.loads(resp.read())
            return (
                bool(data.get("success", False)),
                data.get("stdout", ""),
                data.get("stderr", ""),
                data,
            )
        except urllib.error.HTTPError as e:
            detail = e.read().decode("utf-8", "replace")
            return False, "", detail, {"exit_code": None, "elapsed_ms": 0}
        except Exception as e:
            return False, "", f"build verification unavailable: {e}", {"exit_code": None, "elapsed_ms": 0}


# --- Embedding Adapter --------------------------------------------------------

class EmbedAdapter:
    """Calls llama-server /v1/embeddings for code embeddings."""

    def __call__(self, text: str) -> List[float]:
        body = {"model": "default", "input": text}
        try:
            req = urllib.request.Request(
                f"{INFERENCE_URL}/v1/embeddings",
                data=json.dumps(body).encode(),
                headers=_service_headers(),
            )
            with urllib.request.urlopen(req, timeout=30) as resp:
                data = json.loads(resp.read())
                return data.get("data", [{}])[0].get("embedding", [])
        except Exception:
            return []


# ---------------------------------------------------------------------------
# Adapter records
# ---------------------------------------------------------------------------
#
# ADAPTER KNOWLEDGE, which is why it lives here: which criteria each adapter
# can observe, and what its verifier can therefore demonstrate. contract.py
# must not learn either -- it stays generic and derives completeness, coverage
# and closure from what an adapter reports.
#
# Records are built by calling contract.build directly. Nothing here consults
# the retiring evidence.py: strength is read from the OBSERVATIONS, not from
# that module's graded string, so this file is a producer of contract records
# rather than a translator of someone else's grade.

import contract

# Adapter identities. These are the ids the pipeline records carry, declared
# here because they name THIS layer's verifiers. test_adapters.py pins them
# against the retiring module's copies for as long as that module exists.
ADAPTER_JAVASCRIPT_COMPILE = "javascript_compile"
ADAPTER_CSS_SYNTAX = "css_syntax"
ADAPTER_PYTHON_COMPILE = "python_compile"
ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED = "interactive_python_unsupported"
ADAPTER_UNSUPPORTED = "unsupported"

# Identity of this producer's grading. It must change whenever the grading
# changes, or two incomparable measurements would compare equal.
LIVE_ADAPTER_VERSION = "0.1.0-prototype"


# Criterion ids this layer declares. Opaque above it: nothing interprets them,
# and no task vocabulary appears here.
CRITERION_PARSES = "parses"

# --- registry: what an adapter may declare, and what must back it -----------
#
# Three declarations used to be derived instead of made, and each derivation
# hid a different way of claiming more than the adapter could show:
#
#   supported     inferred from set membership, so an adapter became supported
#                 for an artifact class by not appearing in a list;
#   requirements  built FROM capabilities, so an adapter read its own reach and
#                 called the result the TASK's obligation -- the one thing
#                 contract.py's own docstring forbids it;
#   evaluators    implicit in a branch of _observations, so "can measure X" and
#                 "something computes X" were separate facts nothing compared.
#
# The sealed Stage-A acquisition is the bill: 100 of 103 candidate evaluations
# ran under python_compile, which required four browser-behaviour criteria it
# cannot measure, and every record carried two of them in missing_required
# with capabilities []. No Python
# candidate in that run could reach closure -- not for want of an oracle, but
# because that route could not reach closure at all.
#
# The registry below makes all three explicit, and check_registry checks them
# against each other at import. Each adapter requires exactly the criteria it
# observes, each backed by an evaluator: python_compile requires only that the
# artifact parses, which is what it measures. What a TASK owes is not declared
# here. The proxy derives that from the validated request
# (proxy/obligation_kinds.go), and none of it reaches this service.

SUPPORT_ALWAYS = "always"
SUPPORT_NEVER = "never"
SUPPORT_KINDS = (SUPPORT_ALWAYS, SUPPORT_NEVER)


class AdapterRegistryError(RuntimeError):
    """An adapter declares something nothing can back. Raised at import: a
    registry that cannot be checked is a registry that is not enforced."""


def _eval_on_acceptance(cid):
    """The verifier either accepted the artifact or it did not."""
    def ev(accepted):
        return contract.DEMONSTRATED if accepted else contract.UNOBSERVED
    return ev


def _unsupported_entry(support):
    """Answers the question by declining it.

    It observes nothing. Requirements are empty because an adapter does not
    own the task's obligations: what this artifact class owes is the task's to
    say, and what this verifier can show is nothing. Unsupported is
    unverifiable -- never failed, and never vacuously complete.
    """
    return {
        "support": support,
        "capabilities": [],
        "requirements": [],
        "evaluators": {},
        "unmeasurable": [],
    }


def _measurable_entry(support, criteria):
    return {
        "support": support,
        "capabilities": list(criteria),
        "requirements": [(c, True) for c in criteria],
        "evaluators": {c: _eval_on_acceptance(c) for c in criteria},
        "unmeasurable": [],
    }


REGISTRY = {
    ADAPTER_CSS_SYNTAX: _measurable_entry(SUPPORT_ALWAYS, [CRITERION_PARSES]),
    # Compiles the artifact in its own language and reports whether it parsed.
    # It requires nothing of the task and claims nothing above syntax.
    ADAPTER_PYTHON_COMPILE: _measurable_entry(SUPPORT_ALWAYS, [CRITERION_PARSES]),
    ADAPTER_JAVASCRIPT_COMPILE: _measurable_entry(SUPPORT_ALWAYS, [CRITERION_PARSES]),
    ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED: _unsupported_entry(SUPPORT_NEVER),
    ADAPTER_UNSUPPORTED: _unsupported_entry(SUPPORT_NEVER),
}

ALL_ADAPTERS = tuple(REGISTRY)
SUPPORT_DECLARATION = {a: e["support"] for a, e in REGISTRY.items()}
REQUIREMENT_DECLARATION = {a: list(e["requirements"]) for a, e in REGISTRY.items()}


def evaluators_for(adapter):
    return dict(REGISTRY.get(adapter, {}).get("evaluators", {}))


def unmeasurable_requirements(adapter):
    return list(REGISTRY.get(adapter, {}).get("unmeasurable", []))


def check_registry(registry):
    """Every declaration must be backed by something that can produce it.

    Raised, never warned: a registry whose violations are reported after
    startup is one that ships with them.
    """
    for adapter, entry in sorted(registry.items()):
        support = entry.get("support")
        if support not in SUPPORT_KINDS:
            raise AdapterRegistryError(
                f"{adapter}: support {support!r} is not one of {SUPPORT_KINDS}")
        caps = set(entry.get("capabilities") or [])
        evals = set(entry.get("evaluators") or {})
        unmeasurable = set(entry.get("unmeasurable") or [])
        missing = sorted(caps - evals)
        if missing:
            raise AdapterRegistryError(
                f"{adapter}: declares it can measure {missing} with no evaluator "
                "to produce the observation")
        orphan_eval = sorted(evals - caps)
        if orphan_eval:
            raise AdapterRegistryError(
                f"{adapter}: evaluates {orphan_eval} without declaring it measurable")
        both = sorted(caps & unmeasurable)
        if both:
            raise AdapterRegistryError(
                f"{adapter}: {both} declared both measurable and unmeasurable")
        required = set()
        for item in entry.get("requirements") or []:
            if isinstance(item, str):
                required.add(item)
                continue
            try:
                cid, _req = item
            except (TypeError, ValueError) as exc:
                raise AdapterRegistryError(
                    f"{adapter}: malformed requirement {item!r}") from exc
            required.add(cid)
        unregistered = sorted(required - caps - unmeasurable)
        if unregistered:
            raise AdapterRegistryError(
                f"{adapter}: requires {unregistered} with no evaluator and no "
                "registered reason; an obligation an adapter cannot measure "
                "must be declared unmeasurable, not implied")
    return True


check_registry(REGISTRY)

# The strength a TASK on this artifact class must reach before it may close.
# Declared per adapter, never a universal floor: a stylesheet has no runtime
# behaviour to demand, and code whose behaviour matters but cannot be observed
# here stays open rather than closing on a compile.
_CLOSURE_FLOOR = {
    ADAPTER_CSS_SYNTAX: contract.SYNTAX,
}


def closure_floor(adapter):
    return _CLOSURE_FLOOR.get(adapter, contract.BEHAVIORAL)


def _capabilities(adapter):
    """What this adapter can observe. Everything else contract.build reports
    not_applicable, so "we could not look" stays distinct from "absent".

    Declared in REGISTRY, and every entry is backed by an evaluator: the
    registry check refuses a capability nothing computes.
    """
    return list(REGISTRY.get(adapter, {}).get("capabilities", []))


def _requirements(adapter):
    """The obligations this record is measured against.

    Declared, never derived from _capabilities. Deriving them is how an
    adapter read its own reach and called the answer the task's requirement;
    an obligation an adapter cannot measure must appear in its `unmeasurable`
    quarantine, which the registry check enforces.

    Adapter-owned: the task's own obligations are derived in the proxy and
    never reach this service.
    """
    return [contract.requirement(cid, required=req)
            for cid, req in REGISTRY.get(adapter, {}).get("requirements", [])]


def _observations(adapter, accepted):
    """One observation per criterion this adapter can measure.

    A criterion it can measure and did not see is UNOBSERVED, never REFUTED.
    """
    observations = {}
    for cid, evaluate in REGISTRY.get(adapter, {}).get("evaluators", {}).items():
        observations[cid] = contract.observation(evaluate(accepted))
    return observations


def _supported(adapter):
    """Whether this adapter could measure this artifact at all. Unsupported is
    unverified, never failed.

    Read from the adapter's own declaration rather than inferred from set
    membership: an adapter used to become supported for an artifact class by
    not appearing in a list, which is support by omission.
    """
    support = SUPPORT_DECLARATION.get(adapter)
    if support == SUPPORT_NEVER:
        return False
    if support == SUPPORT_ALWAYS:
        return True
    raise AdapterRegistryError(
        f"{adapter!r} has no support declaration; refusing to assume one")


def _strength_and_execution(accepted, supported):
    """What this verifier demonstrated, and whether its run completed.

    Derived from the observations themselves. Every verifier here checks
    syntax: one that accepted the artifact demonstrated syntax and nothing
    above it, and an artifact the adapter cannot support is unverified, never
    failed.
    """
    if not accepted:
        # The verifier demonstrated nothing at all.
        return contract.SYNTAX, contract.EXEC_ERROR, False
    if not supported:
        return contract.SYNTAX, contract.EXEC_SKIPPED, False
    return contract.SYNTAX, contract.EXEC_OK, True


def contract_record(*, adapter, accepted, contract_id,
                    contract_version, artifact_scope, evaluation_context_hash,
                    candidate_content_hash):
    """One finalized contract record, built here and derived by contract.py.

    The caller hands over raw observation inputs -- which verifier ran and
    whether it accepted the artifact. Everything
    else is this layer's declaration or the contract's derivation; no grading
    from elsewhere is translated.

    The record is measured against the adapter's own measurable criteria and
    closes at the adapter's own floor. It describes what a verifier saw and
    carries no task authority: the task's obligations are derived in the
    proxy and never reach this service.
    """
    supported = _supported(adapter)
    strength, execution_status, supported = _strength_and_execution(
        accepted, supported)

    task = contract.task_contract(contract_id, contract_version,
                                  _requirements(adapter),
                                  minimum_closure_strength=closure_floor(adapter))
    return contract.build(
        task, adapter, LIVE_ADAPTER_VERSION,
        _observations(adapter, accepted), _capabilities(adapter),
        strength, execution_status=execution_status, supported=supported,
        artifact_scope=artifact_scope,
        evaluation_context_hash=evaluation_context_hash,
        candidate_content_hash=candidate_content_hash)


def evidence_envelope(result, *, delivered_code, selection=None):
    """The one entry point main.py calls. None means: no evidence to send.

    None is a positive statement -- nothing was measured -- and is distinct
    from a malformed envelope, which this never produces: a record that cannot
    be serialised raises instead.
    """
    record = result.get("evidence_record")
    if not record:
        return None
    return contract.envelope(record, selection or result.get("contract_selection"),
                             contract.content_hash(delivered_code))


# ---------------------------------------------------------------------------
# Adapter routing
# ---------------------------------------------------------------------------
#
# Which verifier an artifact gets. contract.py stays generic, and the pipeline
# asks this layer rather than knowing about artifact classes.

# --------------------------------------------------------------- adapters ---
#
# Evidence strength must come from the VERIFIER THAT RAN, never from the file
# extension. The first cut keyed off extension and mapped every .py to
# behavioral_complete, which is wrong for Pygame, Tkinter, curses and Flask:
# those receive a compile smoke and nothing more, and would have closed the
# pipeline claiming behaviour nobody demonstrated. It also sent .css through a
# JavaScript probe and treated every .js as a canvas game.
#
# Adapters carry the domain knowledge. Everything above them -- the strength
# ordering, coverage, early-return policy, ranking, and the unsupported vs
# failed distinction -- stays prompt-agnostic.


_INTERACTIVE_PY_RE = re.compile(
    r"\b(import\s+pygame|from\s+pygame|import\s+tkinter|from\s+tkinter|"
    r"import\s+curses|from\s+curses|Flask\s*\(|FastAPI\s*\(|"
    r"QApplication|import\s+PySide|import\s+PyQt)", re.I)




def select_adapter(file_path: str, code: str) -> str:
    """Which verifier can speak for this artifact. Capability, not keywords."""
    ext = (file_path or "").lower().rsplit(".", 1)
    ext = ("." + ext[-1]) if len(ext) == 2 else ""
    code = code or ""

    if ext in (".py",):
        if _INTERACTIVE_PY_RE.search(code):
            return ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED
        return ADAPTER_PYTHON_COMPILE
    if ext in (".js", ".mjs"):
        return ADAPTER_JAVASCRIPT_COMPILE
    if ext in (".jsx", ".tsx", ".ts"):
        return ADAPTER_UNSUPPORTED          # needs transpilation first
    if ext in (".html", ".htm"):
        return ADAPTER_UNSUPPORTED          # nothing here runs a page
    if ext == ".css":
        return ADAPTER_CSS_SYNTAX
    return ADAPTER_UNSUPPORTED
