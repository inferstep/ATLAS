"""Shared harness for the deterministic E2E acceptance tests.

Boots the real sandbox executor and provides the SSE driver + port
helpers both acceptance modules use. See test_acceptance.py for the
direct-agent scenario and test_v3_lens_acceptance.py for the V3/Lens
pipeline scenario.

Internal service auth is ENABLED for the whole session: a synthetic
token file is generated once, handed to the proxy and the sandbox
executor via ATLAS_SERVICE_TOKEN_FILE, and every driver request sends
the Bearer header — so the acceptance suite exercises the production
enforcement path, not the auth-disabled fallback. Negative cases
(missing/wrong token) live in test_service_auth.py.
"""

import http.client
import json
import os
import secrets as _secrets
import socket
import subprocess
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
PROXY_BINARY = os.environ.get("ATLAS_PROXY_BINARY", "/tmp/test-atlas-proxy")

# Synthetic per-run token (never a real credential).
SERVICE_TOKEN = "atlas-st-e2e-" + _secrets.token_urlsafe(16)
_TOKEN_FILE = None  # populated by the session fixture


def sandbox_deps_available() -> bool:
    try:
        import fastapi, uvicorn, defusedxml  # noqa: F401
        return True
    except ImportError:
        return False


def proxy_binary_available() -> bool:
    """Whether the compiled proxy these tests boot is present and runnable.

    CI builds it (`go build -o /tmp/test-atlas-proxy .`) before invoking
    pytest; a plain checkout has not. Without the guard the missing file
    surfaces as a FileNotFoundError raised from subprocess deep inside a
    fixture, which reads like a broken test rather than an absent
    prerequisite. Checks the executable bit too, so a half-written or
    non-executable file skips instead of failing at Popen.
    """
    return os.path.isfile(PROXY_BINARY) and os.access(PROXY_BINARY, os.X_OK)


SKIP_NO_PROXY_BINARY = (
    f"atlas-proxy binary not available at {PROXY_BINARY} "
    f"— run `cd proxy && go build -o {PROXY_BINARY} .` first"
)


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for_port(port: int, timeout: float = 20.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return
        except OSError:
            time.sleep(0.1)
    raise TimeoutError(f"port {port} never came up")


@pytest.fixture(scope="session", autouse=True)
def service_token_file(tmp_path_factory):
    """Write the synthetic token once; 0600 like the real installer."""
    global _TOKEN_FILE
    path = tmp_path_factory.mktemp("auth") / "service-token"
    path.write_text(SERVICE_TOKEN + "\n")
    path.chmod(0o600)
    _TOKEN_FILE = str(path)
    return _TOKEN_FILE


@pytest.fixture(scope="session")
def workspace_root(tmp_path_factory):
    return tmp_path_factory.mktemp("workspace-root")


@pytest.fixture(scope="session")
def sandbox_executor(tmp_path_factory, workspace_root, service_token_file):
    port = free_port()
    scratch = tmp_path_factory.mktemp("sandbox-scratch")
    env = {**os.environ,
           "WORKSPACE_BASE": str(scratch),
           "ATLAS_SANDBOX_WORKSPACE_ROOT": str(workspace_root),
           "ATLAS_SERVICE_TOKEN_FILE": service_token_file,
           "MAX_EXECUTION_TIME": "60"}
    proc = subprocess.Popen(
        ["python", "-m", "uvicorn", "executor_server:app",
         "--host", "127.0.0.1", "--port", str(port), "--log-level", "error"],
        cwd=str(REPO / "sandbox"), env=env,
        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        wait_for_port(port)
    except TimeoutError:
        proc.terminate()
        _, err = proc.communicate(timeout=5)
        pytest.fail(f"sandbox executor never started: {err.decode()[-2000:]}")
    yield port
    proc.terminate()
    proc.wait(timeout=10)


def start_proxy(env_overrides: dict) -> tuple:
    """Boot the proxy binary with a pinned minimal env (auth enabled).
    Returns (port, Popen). Caller terminates."""
    port = free_port()
    env = {
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "HOME": os.environ.get("HOME", "/tmp"),
        "ATLAS_PROXY_PORT": str(port),
        "ATLAS_KEEP_LLAMA_WARM": "0",
        "ATLAS_PERMISSION_TIMEOUT_SEC": "30",
        "ATLAS_SERVICE_TOKEN_FILE": _TOKEN_FILE or "/nonexistent",
        **env_overrides,
    }
    proc = subprocess.Popen([PROXY_BINARY], env=env,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.PIPE)
    try:
        wait_for_port(port)
    except TimeoutError:
        proc.terminate()
        _, err = proc.communicate(timeout=5)
        pytest.fail(f"proxy never bound: {err.decode()[-2000:]}")
    return port, proc


def _auth_header() -> dict:
    return {"Authorization": f"Bearer {SERVICE_TOKEN}"}


def post_json(port: int, path: str, body: dict, token: str = None) -> dict:
    headers = {"Content-Type": "application/json", **_auth_header()}
    if token is not None:  # explicit override for negative tests
        headers["Authorization"] = f"Bearer {token}"
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    try:
        conn.request("POST", path, json.dumps(body), headers)
        resp = conn.getresponse()
        return json.loads(resp.read() or b"{}")
    finally:
        conn.close()


def request_status(port: int, method: str, path: str, body: dict = None,
                   headers: dict = None) -> int:
    """Raw status probe for auth tests (no default auth header)."""
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    try:
        payload = json.dumps(body) if body is not None else None
        conn.request(method, path, payload, headers or {})
        return conn.getresponse().status
    finally:
        conn.close()


def agent_request_body(body: dict) -> dict:
    """Add this suite's task contract to an agent request body.

    Every owned sender declares a task mode. A request with no contract is
    indistinguishable from an external caller's, and that absence is reserved
    for the one compatibility fixture. Outputs and verification stay out: this
    helper knows nothing structured about either, and guessing them from the
    prompt text is exactly what the contract replaces.

    A caller that genuinely represents a question passes its own contract, and
    that choice is left alone.
    """
    out = dict(body)
    out.setdefault("task_contract", {"task_mode": "work"})
    return out


def drive_agent_turn(port: int, body: dict, deadline_s: float = 120.0):
    """POST /v1/agent, stream events, answer permission prompts inline.
    Returns the ordered event list."""
    events = []
    body = agent_request_body(body)
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=deadline_s)
    conn.request("POST", "/v1/agent", json.dumps(body),
                 {"Content-Type": "application/json",
                  "Accept": "text/event-stream",
                  **_auth_header()})
    resp = conn.getresponse()
    assert resp.status == 200, resp.read()[:500]

    deadline = time.monotonic() + deadline_s
    buf = b""
    done = False
    while not done:
        assert time.monotonic() < deadline, (
            f"turn did not complete in {deadline_s}s; events so far: "
            f"{[e['type'] for e in events]}")
        chunk = resp.read1(65536)
        if not chunk:
            break
        buf += chunk
        while b"\n\n" in buf:
            frame, buf = buf.split(b"\n\n", 1)
            for line in frame.decode("utf-8", "replace").splitlines():
                if not line.startswith("data: "):
                    continue
                payload = line[len("data: "):]
                if payload == "[DONE]":
                    done = True
                    continue
                ev = json.loads(payload)
                events.append(ev)
                if ev["type"] == "permission_request":
                    answer = post_json(port, "/v1/permission", {
                        "session_id": body["session_id"],
                        "tool_call_id": ev["data"]["tool_call_id"],
                        "decision": "allow",
                        "scope": "once",
                    })
                    assert answer.get("delivered") is True, answer
    conn.close()
    return events


def ordered_subsequence(events, *predicates):
    """Assert the predicates match, in order. Returns matched events."""
    matched = []
    it = iter(events)
    for name, pred in predicates:
        for ev in it:
            if pred(ev):
                matched.append(ev)
                break
        else:
            raise AssertionError(
                f"stage {name!r} missing (or out of order); event sequence: "
                f"{[e['type'] for e in events]}")
    return matched
