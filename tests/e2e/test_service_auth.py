"""Internal service auth — negative-path and rotation acceptance.

All tokens are synthetic fixtures generated per run. The positive path
(valid token end-to-end through a full agent turn) is exercised by
every other E2E module, since conftest enables auth session-wide.
"""

import json

import pytest

from .conftest import (SKIP_NO_PROXY_BINARY, SERVICE_TOKEN, proxy_binary_available,
                       request_status, sandbox_deps_available, start_proxy)

pytestmark = [
    pytest.mark.skipif(
        not sandbox_deps_available(),
        reason="sandbox executor deps (fastapi/uvicorn) not installed"),
    pytest.mark.skipif(
        not proxy_binary_available(), reason=SKIP_NO_PROXY_BINARY),
]


@pytest.fixture
def proxy(tmp_path):
    port, proc = start_proxy({
        # No llama needed: auth is rejected before any upstream call.
        "ATLAS_LLAMA_URL": "http://127.0.0.1:9",  # closed port
    })
    yield port
    proc.terminate()
    proc.wait(timeout=10)


def test_proxy_rejects_missing_token(proxy):
    status = request_status(proxy, "POST", "/v1/agent",
                            {"messages": []},
                            {"Content-Type": "application/json"})
    assert status == 401


def test_proxy_rejects_wrong_token(proxy):
    status = request_status(
        proxy, "POST", "/cancel", {},
        {"Content-Type": "application/json",
         "Authorization": "Bearer atlas-st-wrong-fixture"})
    assert status == 401


def test_proxy_accepts_correct_token(proxy):
    # /v1/models proxies to llama (closed port here) — anything but
    # 401 proves the gate passed; upstream failure is a 5xx.
    status = request_status(
        proxy, "GET", "/v1/models", None,
        {"Authorization": f"Bearer {SERVICE_TOKEN}"})
    assert status != 401


def test_proxy_health_stays_open(proxy):
    # Compose healthchecks are headerless curl — /health must never
    # require the token. (Body reports upstream state; status is 200.)
    status = request_status(proxy, "GET", "/health", None, {})
    assert status == 200


def test_sandbox_executor_enforces(sandbox_executor):
    # Missing token
    status = request_status(
        sandbox_executor, "POST", "/shell",
        {"command": "echo hi", "cwd": "/workspace"},
        {"Content-Type": "application/json"})
    assert status == 401
    # Wrong token
    status = request_status(
        sandbox_executor, "POST", "/shell",
        {"command": "echo hi", "cwd": "/workspace"},
        {"Content-Type": "application/json",
         "Authorization": "Bearer atlas-st-wrong-fixture"})
    assert status == 401
    # Health stays open
    assert request_status(sandbox_executor, "GET", "/health", None, {}) == 200


def test_rotation_invalidates_old_token(tmp_path):
    """A proxy started against a rotated token file rejects the old
    token and accepts the new one (the documented rotation flow:
    atlas init --rotate-token && restart)."""
    rotated = "atlas-st-rotated-fixture"
    tok_file = tmp_path / "service-token"
    tok_file.write_text(rotated + "\n")
    tok_file.chmod(0o600)

    port, proc = start_proxy({
        "ATLAS_LLAMA_URL": "http://127.0.0.1:9",
        "ATLAS_SERVICE_TOKEN_FILE": str(tok_file),
    })
    try:
        old = request_status(
            port, "GET", "/v1/models", None,
            {"Authorization": f"Bearer {SERVICE_TOKEN}"})
        assert old == 401, "pre-rotation token still accepted"
        new = request_status(
            port, "GET", "/v1/models", None,
            {"Authorization": f"Bearer {rotated}"})
        assert new != 401, "rotated token rejected"
    finally:
        proc.terminate()
        proc.wait(timeout=10)


def test_401_body_never_echoes_token(proxy):
    import http.client
    conn = http.client.HTTPConnection("127.0.0.1", proxy, timeout=10)
    try:
        conn.request("POST", "/v1/agent", json.dumps({}),
                     {"Content-Type": "application/json",
                      "Authorization": f"Bearer {SERVICE_TOKEN[:-2]}xx"})
        resp = conn.getresponse()
        body = resp.read().decode()
        assert resp.status == 401
        assert SERVICE_TOKEN not in body
        assert SERVICE_TOKEN[:-2] not in body
    finally:
        conn.close()
