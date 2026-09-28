"""D4: SandboxAdapter must actually send `test_input` to the sandbox.

The adapter's ``__call__(code, test_input)`` accepted a test input and
silently dropped it — per-candidate test inputs never reached the
candidate under test, so per-step "verification" only proved the code
ran with no input at all.

These tests mock the HTTP layer and assert the input lands in the
``/execute`` request payload under the field name the sandbox schema
declares (``ExecuteRequest.stdin`` in sandbox/executor_server.py), the
same stdin contract the bench implements via execute_code_stdio.
"""

import importlib.util
import json
import sys
from pathlib import Path

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402

SANDBOX_DIR = PROJECT_ROOT / "sandbox"


class _FakeResponse:
    def __init__(self, payload):
        self._payload = payload

    def read(self):
        return json.dumps(self._payload).encode()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _capture_urlopen(monkeypatch, captured):
    def fake_urlopen(req, timeout=None):
        captured["url"] = req.full_url
        captured["body"] = json.loads(req.data.decode())
        return _FakeResponse({"success": True, "stdout": "7\n", "stderr": ""})

    monkeypatch.setattr(adapters.urllib.request, "urlopen", fake_urlopen)


def test_test_input_is_sent_as_stdin(monkeypatch):
    captured = {}
    _capture_urlopen(monkeypatch, captured)

    sandbox = adapters.SandboxAdapter()
    ok, out, err = sandbox("print(int(input()) + 4)", "3")

    assert ok and out == "7\n"
    assert captured["url"].endswith("/execute")
    assert captured["body"]["stdin"] == "3"
    assert captured["body"]["code"] == "print(int(input()) + 4)"


def test_no_test_input_omits_stdin(monkeypatch):
    # None/absent means "inherit server stdin" in the executor — an empty
    # test_input must not flip that default.
    captured = {}
    _capture_urlopen(monkeypatch, captured)

    sandbox = adapters.SandboxAdapter()
    sandbox("print('x')")

    assert "stdin" not in captured["body"]


def test_stdin_field_name_matches_executor_schema():
    """The adapter's field name is only honest if the server declares it:
    load the real executor module and check ExecuteRequest."""
    if str(SANDBOX_DIR) not in sys.path:
        sys.path.insert(0, str(SANDBOX_DIR))
    module_path = SANDBOX_DIR / "executor_server.py"
    spec = importlib.util.spec_from_file_location(
        "atlas_sandbox_executor_stdin", module_path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    try:
        spec.loader.exec_module(module)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise

    assert "stdin" in module.ExecuteRequest.model_fields


# --- per-call staged files -------------------------------------------------
#
# The self-test wrapper used to create the case's input file by executing
# `open('input.txt','w')` inside the sandbox. The sandbox already stages
# files for a request — that is how project context reaches a candidate —
# and staging is the mechanism that works: the executable write lands in the
# process's working directory, which is read-only in a `read_only: true`
# container, so it raised before the candidate ran. These pin the per-call
# files map on the real serialized body, not on a signature.


def test_per_call_files_reach_the_execute_body(monkeypatch):
    captured = {}
    _capture_urlopen(monkeypatch, captured)

    sandbox = adapters.SandboxAdapter()
    sandbox("print(open('input.txt').read())", files={"input.txt": "199\n200\n"})

    assert captured["body"]["files"] == {"input.txt": "199\n200\n"}


def test_per_call_files_merge_over_project_files(monkeypatch):
    """Project context still reaches the candidate; the case's own input
    wins where the two name the same file."""
    captured = {}
    _capture_urlopen(monkeypatch, captured)

    sandbox = adapters.SandboxAdapter(
        project_files={"helper.py": "X = 1\n", "input.txt": "project copy\n"})
    sandbox("print(1)", files={"input.txt": "case copy\n"})

    assert captured["body"]["files"] == {
        "helper.py": "X = 1\n", "input.txt": "case copy\n"}


def test_omitting_files_leaves_every_existing_caller_unchanged(monkeypatch):
    captured = {}
    _capture_urlopen(monkeypatch, captured)

    adapters.SandboxAdapter()("print('x')")
    assert "files" not in captured["body"]

    adapters.SandboxAdapter(project_files={"helper.py": "X = 1\n"})("print('x')")
    assert captured["body"]["files"] == {"helper.py": "X = 1\n"}


def test_files_field_name_matches_executor_schema():
    """Same honesty check the stdin field gets: the server must declare it."""
    if str(SANDBOX_DIR) not in sys.path:
        sys.path.insert(0, str(SANDBOX_DIR))
    module_path = SANDBOX_DIR / "executor_server.py"
    spec = importlib.util.spec_from_file_location(
        "atlas_sandbox_executor_files", module_path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    try:
        spec.loader.exec_module(module)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise

    assert "files" in module.ExecuteRequest.model_fields
