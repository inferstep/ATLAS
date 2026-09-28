"""Operational logs carry metadata, never content.

The eight-key JSON schema bounds the record SHAPE, not what a caller puts in
`msg`. A 63-cell rehearsal retained 84 records containing candidate source,
because one diagnostic printed a 200-character sample of the first streaming
delta. The schema guard passed the whole time.

These tests are dynamic: a unique sentinel goes through the real code path and
must not come back out of any operational log. A static reading of the source
would not have caught the original leak either.
"""
import io
import json
import os
import re
import sys
import threading
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
if V3 not in sys.path:
    sys.path.insert(0, V3)

import adapters  # noqa: E402
import structured_log as SL  # noqa: E402

CANDIDATE_SENTINEL = "ZZQCANDIDATEZZ_7f3a91"
PROMPT_SENTINEL = "ZZQPROMPTZZ_4b8e02"
TOKEN_SENTINEL = "ZZQTOKENZZ_11c5de"

CANDIDATE_CODE = (
    "```python\n"
    f"# {CANDIDATE_SENTINEL}\n"
    "import sys\n\n"
    "def solve():\n"
    f"    return '{CANDIDATE_SENTINEL}'\n"
    "```\n"
)


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    seen_bodies = []

    def log_message(self, *a):
        pass

    def handle_error(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        Upstream.seen_bodies.append(raw.decode("utf-8", "replace"))
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for piece in (CANDIDATE_CODE[:40], CANDIDATE_CODE[40:]):
            chunk = ("data: " + json.dumps(
                {"choices": [{"delta": {"content": piece}}]}) + "\n\n").encode()
            self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        tail = b"data: [DONE]\n\n"
        self.wfile.write(b"%x\r\n" % len(tail) + tail + b"\r\n")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


@pytest.fixture()
def upstream():
    Upstream.seen_bodies = []
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    old = adapters.INFERENCE_URL
    adapters.INFERENCE_URL = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        yield srv
    finally:
        adapters.INFERENCE_URL = old
        srv.shutdown()
        srv.server_close()


def _adapter():
    llm = adapters.LLMAdapter()
    llm.cancel_scope = adapters.CancelScope(invocation_id="inv-LEAK")
    llm.request_identity = adapters.RequestIdentity("req-LEAK", "inv-LEAK")
    return llm


def _prompt():
    return (f"<|im_start|>system\nyou solve things<|im_end|>\n"
            f"<|im_start|>user\n{PROMPT_SENTINEL} solve it<|im_end|>\n"
            f"<|im_start|>assistant\n")


def _run_capturing(llm, prompt):
    buf = io.StringIO()
    with redirect_stdout(buf):
        out = llm(prompt, 0.2, 512, 7)
    return out, buf.getvalue()


# --- 1, 2: candidate and prompt sentinels -----------------------------------

def test_candidate_source_never_reaches_operational_logs(upstream):
    llm = _adapter()
    (content, _tok, _ms), logs = _run_capturing(llm, _prompt())
    assert CANDIDATE_SENTINEL in content, "the stream did not deliver the candidate"
    assert CANDIDATE_SENTINEL not in logs, (
        "candidate source appeared in an operational log:\n"
        + "\n".join(x for x in logs.splitlines() if CANDIDATE_SENTINEL in x)[:600])


def test_user_prompt_never_reaches_operational_logs(upstream):
    llm = _adapter()
    _out, logs = _run_capturing(llm, _prompt())
    assert PROMPT_SENTINEL not in logs, (
        "user prompt text appeared in an operational log:\n"
        + "\n".join(x for x in logs.splitlines() if PROMPT_SENTINEL in x)[:600])


# --- 3: authorization material ----------------------------------------------

def test_service_token_never_reaches_operational_logs(upstream, monkeypatch):
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", TOKEN_SENTINEL)
    llm = _adapter()
    _out, logs = _run_capturing(llm, _prompt())
    assert TOKEN_SENTINEL not in logs
    assert "Bearer" not in logs


# --- 4, 5: the diagnostic still says something useful ------------------------

def test_delta_keys_and_length_remain_observable(upstream):
    llm = _adapter()
    _out, logs = _run_capturing(llm, _prompt())
    assert "first delta keys=" in logs, "the first-delta diagnostic disappeared"
    assert "content_chars=" in logs, "content length is no longer observable"
    m = re.search(r"content_chars=(\d+)", logs)
    assert m and int(m.group(1)) > 0


def test_identity_remains_available_for_joins(upstream):
    SL.bind_identity("req-LEAK", "inv-LEAK")
    try:
        assert SL.current_identity() == ("req-LEAK", "inv-LEAK")
        import logging
        rec = json.loads(SL.JsonFormatter("v3-service").format(
            logging.LogRecord("t", logging.INFO, __file__, 1,
                              "first delta keys=['content'] content_chars=42",
                              None, None)))
        assert rec["request_id"] == "req-LEAK"
        assert rec["invocation_id"] == "inv-LEAK"
    finally:
        SL.bind_identity("", "")


# --- 6: concurrent attribution ----------------------------------------------

def test_two_concurrent_requests_stay_attributed(upstream):
    seen = {}

    def one(tag):
        SL.bind_identity(f"req-{tag}", f"inv-{tag}")
        llm = adapters.LLMAdapter()
        llm.cancel_scope = adapters.CancelScope(invocation_id=f"inv-{tag}")
        llm.request_identity = adapters.RequestIdentity(f"req-{tag}", f"inv-{tag}")
        buf = io.StringIO()
        with redirect_stdout(buf):
            llm(_prompt(), 0.2, 512, 7)
        seen[tag] = (SL.current_identity(), buf.getvalue())

    ta = threading.Thread(target=one, args=("A",))
    tb = threading.Thread(target=one, args=("B",))
    ta.start()
    tb.start()
    ta.join()
    tb.join()
    assert seen["A"][0] == ("req-A", "inv-A")
    assert seen["B"][0] == ("req-B", "inv-B")
    for tag in ("A", "B"):
        assert CANDIDATE_SENTINEL not in seen[tag][1]


# --- 7: logging cannot change behaviour --------------------------------------

def test_logging_does_not_alter_the_outbound_payload_or_the_result(upstream):
    llm = _adapter()
    (content_a, tok_a, _), _ = _run_capturing(llm, _prompt())
    body_a = Upstream.seen_bodies[-1]

    llm2 = _adapter()
    buf = io.StringIO()
    with redirect_stdout(buf):
        content_b, tok_b, _ = llm2(_prompt(), 0.2, 512, 7)
    body_b = Upstream.seen_bodies[-1]

    assert json.loads(body_a) == json.loads(body_b), "the outbound payload moved"
    assert content_a == content_b and tok_a == tok_b


def test_json_mode_off_still_works_and_stays_clean(upstream, monkeypatch):
    monkeypatch.delenv("ATLAS_LOG_FORMAT", raising=False)
    llm = _adapter()
    (content, _t, _m), logs = _run_capturing(llm, _prompt())
    assert CANDIDATE_SENTINEL in content
    assert CANDIDATE_SENTINEL not in logs
    assert PROMPT_SENTINEL not in logs


def test_json_mode_on_stays_clean(upstream, monkeypatch):
    monkeypatch.setenv("ATLAS_LOG_FORMAT", "json")
    llm = _adapter()
    (content, _t, _m), logs = _run_capturing(llm, _prompt())
    assert CANDIDATE_SENTINEL in content
    assert CANDIDATE_SENTINEL not in logs


# --- 11: the schema cannot gain a free-form content field --------------------

def test_operational_record_schema_is_closed():
    import ast
    import logging
    rec = json.loads(SL.JsonFormatter("v3-service").format(
        logging.LogRecord("t", logging.INFO, __file__, 1, "m", None, None)))
    assert set(rec) <= {"ts", "level", "service", "logger", "msg",
                        "request_id", "invocation_id", "exc"}
    src = ast.parse(open(os.path.join(V3, "structured_log.py"), encoding="utf-8").read())
    fmt = next(n for n in ast.walk(src)
               if isinstance(n, ast.FunctionDef) and n.name == "format")
    keys = set()
    for node in ast.walk(fmt):
        if isinstance(node, ast.Subscript) and isinstance(node.slice, ast.Constant):
            if isinstance(node.slice.value, str):
                keys.add(node.slice.value)
        if isinstance(node, ast.Dict):
            for k in node.keys:
                if isinstance(k, ast.Constant) and isinstance(k.value, str):
                    keys.add(k.value)
    allowed = {"ts", "level", "service", "logger", "msg", "request_id",
               "invocation_id", "exc"}
    assert keys <= allowed, f"the operational record gained a field: {keys - allowed}"


# --- 12: governed private evidence keeps what it must ------------------------

def test_private_evidence_still_retains_candidate_bytes():
    """Sanitising operational logs must not destroy the governed evidence
    records, which are required to carry the candidate and its hash."""
    import pipeline
    src = open(os.path.join(V3, "pipeline.py"), encoding="utf-8").read()
    assert "code_b64" in src, "the pool capture stopped retaining candidate bytes"
    assert "code_sha256" in src, "the pool capture stopped retaining the content hash"
    assert hasattr(pipeline, "_PoolCapture")


# --- the SSE progress emitter ------------------------------------------------
# The first-delta sample was not the only content-bearing log. emit_progress
# logged `detail[:80]`, and for a token stage the detail IS model output --
# and because the stdout wrapper writes one record per line, a multi-line
# token became several records, each carrying candidate source. 84 of them
# survived in a rehearsal taken AFTER the first-delta fix.

def _safe_detail():
    import main
    return main._safe_progress_detail


def test_token_stage_detail_is_never_logged_verbatim():
    safe = _safe_detail()
    out = safe("token", CANDIDATE_CODE)
    assert CANDIDATE_SENTINEL not in out
    assert out.startswith("<") and "chars>" in out


def test_multiline_detail_is_reduced_whatever_the_stage():
    """A description of progress does not span lines; content does."""
    safe = _safe_detail()
    out = safe("some_future_stage", f"line one\n{CANDIDATE_SENTINEL}\nline three")
    assert CANDIDATE_SENTINEL not in out
    assert "chars>" in out


def test_ordinary_metadata_detail_still_reads_normally():
    safe = _safe_detail()
    assert safe("plansearch_done", "2 candidates from PlanSearch") == \
        "2 candidates from PlanSearch"
    assert safe("llm_end", "125 tok · 2ms") == "125 tok · 2ms"


def test_every_sse_debug_print_goes_through_the_reducer():
    """Structural: a new SSE debug line that formats `detail` directly would
    reintroduce exactly this leak."""
    import ast
    src = open(os.path.join(V3, "main.py"), encoding="utf-8").read()
    bad = []
    for i, line in enumerate(src.splitlines(), start=1):
        if "[SSE" not in line or "print(" not in line:
            continue
        if "detail" in line and "_safe_progress_detail" not in line:
            bad.append((i, line.strip()))
    assert not bad, f"SSE debug line logs detail directly: {bad}"
    ast.parse(src)


def test_sse_emitter_payload_to_the_proxy_is_unchanged():
    """Only the local debug line is reduced. The SSE event the proxy receives
    must still carry the full detail, or model-visible behaviour changes."""
    src = open(os.path.join(V3, "main.py"), encoding="utf-8").read()
    assert 'payload = {"stage": stage, "detail": detail}' in src, (
        "the SSE payload stopped carrying the full detail; that is a "
        "behaviour change, not an observability change")
