"""The filename this service sends the sandbox, and what happens when it cannot.

The sandbox refuses an absolute path or one containing ".." with HTTP 400
(_safe_overlay_path). The proxy sends V3 a RESOLVED absolute file_path, and
this boundary forwarded it unchanged, so every candidate's syntax check failed
with "syntax verification unavailable: HTTP Error 400: Bad Request" -- a check
that never ran, reported as a candidate that failed.

Measured live on an exposed task 2026-09-18: 3 of 3 candidates failed that way
and the pipeline proposed nothing, after spending ~4 minutes of the session.

These pin the relativisation, that a check still runs when no safe relative
form exists, and -- the part that matters -- that nothing about the verdict
moved: the sandbox still decides, and a candidate that does not parse still
fails.
"""

import json
import sys
from pathlib import Path

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters as A  # noqa: E402


# --- the name itself --------------------------------------------------------

@pytest.mark.parametrize("given,want", [
    # What the proxy actually sends: a resolved path under the workspace.
    ("/workspace/accept31-O/backend/app/main.py", "accept31-O/backend/app/main.py"),
    # The scoping that made the filename travel in the first place survives:
    # the sandbox keys its Jinja check off a "templates/" segment.
    ("/workspace/accept31-O/templates/index.html", "accept31-O/templates/index.html"),
    ("/workspace/templates/index.html", "templates/index.html"),
    # A caller that already sends a relative name is unchanged.
    ("backend/app/main.py", "backend/app/main.py"),
    ("./app.py", "app.py"),
    # Nothing to say.
    ("", None),
    (None, None),
    # The workspace root itself names no file.
    ("/workspace", None),
    ("/workspace/", None),
    # Traversal is refused, not normalised away.
    ("../../etc/passwd", None),
    ("/workspace/../etc/passwd", None),
    # Absolute and outside the workspace: refused rather than rebased, which is
    # what the proxy does when filepath.Rel escapes the working directory.
    ("/etc/passwd", None),
    ("\\windows\\app.py", None),
])
def test_the_name_the_sandbox_will_accept(given, want):
    assert A._sandbox_safe_filename(given) == want


def test_no_accepted_name_is_one_the_sandbox_refuses():
    """Whatever comes back must pass the sandbox's own guard."""
    for given in ["/workspace/a/b.py", "a/b.py", "/etc/passwd", "../x", "",
                  "/workspace", "./x.py", "\\w\\x.py", "/workspace/../x"]:
        got = A._sandbox_safe_filename(given)
        if got is None:
            continue
        assert not got.startswith("/"), got
        assert not got.startswith("\\"), got
        assert ".." not in got.split("/"), got


# --- what it does to the request, and to the verdict ------------------------

class FakeResponse:
    def __init__(self, payload):
        self._payload = json.dumps(payload).encode()

    def read(self):
        return self._payload

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


@pytest.fixture
def capture(monkeypatch):
    """Record the body this boundary sends, and answer with a fixed verdict."""
    sent = {}

    def fake_urlopen(req, timeout=None):
        sent["url"] = req.full_url
        sent["body"] = json.loads(req.data.decode())
        return FakeResponse(sent["reply"])

    monkeypatch.setattr(A.urllib.request, "urlopen", fake_urlopen)
    sent["reply"] = {"valid": True, "errors": []}
    return sent


def test_an_absolute_path_is_relativised_before_it_is_sent(capture):
    sandbox = A.SandboxAdapter()
    ok, _, err = sandbox.syntax_check(
        "x = 1\n", "python", "/workspace/accept31-O/backend/app/main.py")
    assert capture["body"]["filename"] == "accept31-O/backend/app/main.py"
    assert ok and err == ""


def test_an_unrelativisable_path_still_gets_a_check(capture):
    """The filename is dropped; the check is not."""
    sandbox = A.SandboxAdapter()
    ok, _, err = sandbox.syntax_check("x = 1\n", "python", "/etc/passwd")
    assert capture["body"]["filename"] is None
    assert capture["body"]["code"] == "x = 1\n"
    assert capture["body"]["language"] == "python"
    assert ok and err == ""


def test_the_sandbox_still_decides(capture):
    """The relativisation moves no verdict: a failing check still fails, and
    its errors still reach the caller."""
    capture["reply"] = {"valid": False, "errors": ["SyntaxError: invalid syntax (line 9)"]}
    sandbox = A.SandboxAdapter()
    ok, _, err = sandbox.syntax_check(
        "def f(\n", "python", "/workspace/accept31-O/backend/app/main.py")
    assert ok is False
    assert "SyntaxError: invalid syntax (line 9)" in err


def test_an_unreachable_sandbox_is_still_reported_as_unavailable(monkeypatch):
    """The failure mode this fix removed for one cause still exists for others,
    and still says so rather than claiming a verdict."""
    def boom(req, timeout=None):
        raise OSError("connection refused")

    monkeypatch.setattr(A.urllib.request, "urlopen", boom)
    ok, _, err = A.SandboxAdapter().syntax_check("x = 1\n", "python", "app.py")
    assert ok is False
    assert "syntax verification unavailable" in err


def test_a_stopped_checker_is_unavailable_not_a_syntax_error(capture):
    """The sandbox says its checker ended at a ceiling: no verdict. The
    candidate is not passed, and its code is not blamed for a syntax error."""
    capture["reply"] = {"valid": False, "status": "not_run", "outcome": "timed_out", "errors": []}
    ok, _, err = A.SandboxAdapter().syntax_check("x = 1\n", "python", "app.py")
    assert ok is False
    assert err == "syntax verification unavailable: the checker ended timed_out"
