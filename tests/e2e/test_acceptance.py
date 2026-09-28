"""Deterministic end-to-end acceptance test — direct agent path.

Boots the REAL control plane:

    fake llama-server (scripted SSE)  <-- atlas-proxy binary -->  real
                                                sandbox executor (uvicorn)

then drives one complete agent turn through the real protocol:

    open session -> read_file -> edit_file -> run_command (sandbox
    verification, gated by an interactive permission approve) -> done

and asserts every stage happened, in order, with the file actually
fixed on disk. The model is a four-step script served by the fake
llama-server; everything else (agent loop, guardrails, permission
gate, workspace containment, sandbox execution, SSE protocol) is the
production code path. The V3 service here is a dead port, so generation
fails and the model's own edit lands; the V3/Lens pipeline path is covered
by test_v3_lens_acceptance.py.

Requirements (provided by the e2e CI job; skipped cleanly when absent
locally): the proxy binary at $ATLAS_PROXY_BINARY (default
/tmp/test-atlas-proxy) and the sandbox runtime deps.
"""

import http.server
import json
import shutil
import threading
import uuid

import pytest

from tests.e2e.test_v3_lens_acceptance import fake_lens  # noqa: F401  (fixture)
from tests.e2e.conftest import (
    drive_agent_turn, free_port, ordered_subsequence,
    sandbox_deps_available, start_proxy, proxy_binary_available,
    SKIP_NO_PROXY_BINARY,
)

BUGGY_APP = '''def greeting(name):
    return "Hello, " + nmae


if __name__ == "__main__":
    print(greeting("world"))
'''

OLD_STR = 'return "Hello, " + nmae'
NEW_STR = 'return "Hello, " + name'


pytestmark = [
    pytest.mark.skipif(
        not proxy_binary_available(), reason=SKIP_NO_PROXY_BINARY),
    pytest.mark.skipif(
        not sandbox_deps_available(),
        reason="sandbox runtime deps missing — "
               "pip install -r sandbox/requirements-runtime.txt"),
]


def _model_script(tool_results_seen: int) -> str:
    """The model's next envelope, keyed off how many tool results it has
    been shown — robust to any extra user-role nudges the proxy injects."""
    if tool_results_seen == 0:
        return json.dumps({"type": "tool_call", "name": "read_file",
                           "args": {"path": "app.py"}})
    if tool_results_seen == 1:
        return json.dumps({"type": "tool_call", "name": "edit_file",
                           "args": {"path": "app.py", "old_str": OLD_STR,
                                    "new_str": NEW_STR}})
    if tool_results_seen == 2:
        return json.dumps({"type": "tool_call", "name": "run_command",
                           "args": {"command": "python3 -m py_compile app.py",
                                    "timeout": 30}})
    return json.dumps({"type": "done",
                       "summary": "Fixed the NameError in app.py and "
                                  "verified it compiles."})


class _FakeLlamaHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):  # keep pytest output clean
        pass

    def do_GET(self):
        if self.path.startswith("/health"):
            body = b'{"status":"ok"}'
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:  # /slots etc. — the prompt-progress poller stops on 404
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        if self.path.startswith("/slots"):
            # The proxy's per-session slot erase, a POST with no body. This
            # fake has no slots, like a llama-server without
            # --slot-save-path, and says so instead of failing to parse.
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        req = json.loads(raw)
        tool_results = sum(
            1 for m in req.get("messages", [])
            if m.get("role") == "user"
            and m.get("content", "").startswith("[tool result]"))
        envelope = _model_script(tool_results)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        delta = json.dumps({"choices": [{"delta": {"content": envelope}}]})
        usage = json.dumps({"choices": [],
                            "usage": {"total_tokens": 20,
                                      "prompt_tokens": 15,
                                      "completion_tokens": 5}})
        for line in (delta, usage, "[DONE]"):
            self.wfile.write(f"data: {line}\n\n".encode())
        self.wfile.flush()


@pytest.fixture(scope="module")
def fake_llama():
    port = free_port()
    server = http.server.ThreadingHTTPServer(("127.0.0.1", port),
                                             _FakeLlamaHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield port
    server.shutdown()


@pytest.fixture()
def workspace(workspace_root):
    ws = workspace_root / f"e2e-{uuid.uuid4().hex[:8]}"
    ws.mkdir(parents=True)
    (ws / "app.py").write_text(BUGGY_APP)
    yield ws
    shutil.rmtree(ws, ignore_errors=True)


@pytest.fixture()
def proxy(fake_llama, fake_lens, sandbox_executor):  # noqa: F811
    port, proc = start_proxy({
        "ATLAS_INFERENCE_URL": f"http://127.0.0.1:{fake_llama}",
        "ATLAS_SANDBOX_URL": f"http://127.0.0.1:{sandbox_executor}",
        # The lens is required: a request is refused while it cannot score.
        "ATLAS_LENS_URL": f"http://127.0.0.1:{fake_lens}",
        "ATLAS_V3_URL": "http://127.0.0.1:9",    # dead: generation fails, the model's edit lands
    })
    yield port
    proc.terminate()
    proc.wait(timeout=10)


def test_full_agent_turn_read_edit_verify_permission_done(proxy, workspace):
    session = f"e2e-{uuid.uuid4().hex[:8]}"
    events = drive_agent_turn(proxy, {
        "message": "Fix the NameError in app.py, then verify it compiles "
                   "with python3 -m py_compile app.py",
        "working_dir": str(workspace),
        "mode": "default",
        "session_id": session,
    })

    def tool_call(name):
        return (f"tool_call:{name}",
                lambda ev: ev["type"] == "tool_call"
                and ev["data"].get("name") == name)

    def tool_ok(name):
        return (f"tool_result:{name}",
                lambda ev: ev["type"] == "tool_result"
                and ev["data"].get("tool") == name
                and ev["data"].get("success") is True)

    # Every stage, in order — a silently skipped stage fails here.
    ordered_subsequence(
        events,
        tool_call("read_file"),
        tool_ok("read_file"),
        tool_call("edit_file"),
        tool_ok("edit_file"),
        tool_call("run_command"),
        ("permission_request",
         lambda ev: ev["type"] == "permission_request"
         and ev["data"].get("tool_name") == "run_command"),
        tool_ok("run_command"),
        ("done", lambda ev: ev["type"] == "done"),
    )

    prompts = [e for e in events if e["type"] == "permission_request"]
    assert len(prompts) == 1, [e["type"] for e in events]
    assert not any(e["type"] == "permission_denied" for e in events)

    # The edit really landed: the sandbox verified the FIXED file.
    final = (workspace / "app.py").read_text()
    assert NEW_STR in final and "nmae" not in final

    # The verification ran in the real sandbox executor (py_compile
    # writes __pycache__ next to the file).
    assert (workspace / "__pycache__").is_dir(), (
        "run_command did not execute in the workspace")

    done_ev = next(e for e in events if e["type"] == "done")
    assert done_ev["data"].get("summary")


def test_session_less_destructive_call_is_denied(proxy, workspace):
    """Fail-closed contract: no session_id + default mode means the
    run_command permission prompt cannot be answered — the proxy must
    deny it, not silently execute."""
    events = drive_agent_turn(proxy, {
        "message": "Fix the NameError in app.py, then verify it compiles "
                   "with python3 -m py_compile app.py",
        "working_dir": str(workspace),
        "mode": "default",
        "session_id": "",
    }, deadline_s=60.0)

    assert any(e["type"] == "permission_denied" for e in events), (
        f"no denial event: {[e['type'] for e in events]}")
    assert not (workspace / "__pycache__").exists()
