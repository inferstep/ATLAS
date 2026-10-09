"""The proxy's control-plane endpoints, against a running stack.

/cancel and /v1/permission are how a user stops a runaway turn and how the
default permission mode approves a destructive call. Both were untested: every
session measured in this campaign ran in yolo mode and none was ever
cancelled, so the endpoints a user actually reaches for had no coverage.

These are contract checks — status codes and error shapes — which is what
matters for a client that has to distinguish "no such session" from "bad
request" from "wrong method". Live-stack test: carries the integration marker
and is deselected by default.
"""
import json
import os
import urllib.error
import urllib.request

import pytest

PROXY = os.environ.get("ATLAS_PROXY_URL", "http://127.0.0.1:8090")


def _post(path: str, body: dict, method: str = "POST"):
    """Return (status, parsed_body). Errors carry a body worth asserting on."""
    req = urllib.request.Request(
        f"{PROXY}{path}", data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.status, json.loads(resp.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw or "{}")
        except json.JSONDecodeError:
            return e.code, {"raw": raw}
    except urllib.error.URLError as e:
        pytest.skip(f"proxy not reachable at {PROXY}: {e}")
        raise  # unreachable: skip() raises Skipped. Keeps every path explicit.


def test_cancel_unknown_session_is_404_not_an_error():
    """A session that already finished is not a failure — the TUI cancels
    optimistically and must be able to tell that apart from a real error."""
    status, body = _post("/cancel", {"session_id": "no-such-session"})
    assert status == 404
    assert body.get("cancelled") is False


def test_cancel_without_session_id_is_a_client_error():
    status, body = _post("/cancel", {})
    assert status == 400
    assert body.get("error") == "invalid_input"
    assert "session_id" in body.get("detail", "")


def test_cancel_rejects_the_wrong_method():
    status, body = _post("/cancel", {}, method="GET")
    assert status == 405
    assert body.get("error") == "unsupported_operation"


def test_permission_requires_its_identifiers():
    """The approve/deny flow keys on both ids; missing either must not be
    silently treated as an approval."""
    status, body = _post("/v1/permission", {})
    assert status == 400
    assert body.get("error") == "invalid_input"
    detail = body.get("detail", "")
    assert "session_id" in detail and "tool_call_id" in detail


def test_control_plane_errors_carry_the_api_version():
    """Clients branch on api_version; an error response that omits it forces
    them to special-case the error path."""
    for path, payload in (("/cancel", {}), ("/v1/permission", {})):
        _, body = _post(path, payload)
        assert body.get("api_version"), f"{path} error omitted api_version"


def test_canary_must_fail():
    # Planted by scripts/canary.py for the canary pull request. Never merge it.
    assert 1 == 2, "canary: this test fails on purpose"
