"""Contract tests for language-aware V3 candidate verification."""

import sys
from pathlib import Path

import pytest


PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import main as v3main


class RecordingSandbox:
    """A sandbox whose syntax check gives a fixed verdict and records each call."""

    def __init__(self, valid=True, error=""):
        self.valid = valid
        self.error = error
        self.calls = []

    def syntax_check(self, code, language, filename=""):
        self.calls.append((code, language, filename))
        return self.valid, "", self.error


class RecordingBuildSandbox:
    """A sandbox where every command succeeds; it records each call."""

    def __init__(self):
        self.calls = []

    def run_command(self, command, files=None, cwd="/workspace", timeout=60):
        self.calls.append({"command": command, "files": files or {}})
        return True, "ok", "", {"exit_code": 0, "elapsed_ms": 25}


def test_javascript_uses_language_aware_syntax_check():
    sandbox = RecordingSandbox(valid=False, error="Unexpected token")

    ok, _, error = v3main.smoke_compile_check("function {", sandbox, "javascript")

    assert ok is False
    assert error == "Unexpected token"
    # No filename was given, so an empty one is sent.
    assert sandbox.calls == [("function {", "javascript", "")]


@pytest.mark.parametrize("code,language,filename,sandbox_language", [
    pytest.param("const value = `'''\\n${example}`;", "javascript", "", "javascript",
                 id="the source is passed as data, not rewritten"),
    pytest.param("print('ok')", "py", "", "python", id="an alias is normalized"),
    pytest.param("<root />", "xml", "", "xml", id="xml"),
    # The sandbox's Jinja check applies to templates/*.html only, so without
    # the filename it never applies to a V3 candidate, and a broken template
    # ships.
    pytest.param("<html></html>", "html", "templates/index.html", "html", id="the filename is forwarded"),
])
def test_smoke_check_sends_the_code_to_the_sandbox_syntax_check(code, language, filename, sandbox_language):
    sandbox = RecordingSandbox(valid=True)

    ok, _, _ = v3main.smoke_compile_check(code, sandbox, language, filename=filename)

    assert ok is True
    assert sandbox.calls == [(code, sandbox_language, filename)]


def test_unknown_language_is_not_automatic_success():
    # The sandbox would pass anything, so only the language decides here.
    sandbox = RecordingSandbox(valid=True)

    ok, _, error = v3main.smoke_compile_check("body {}", sandbox, "css")

    assert ok is False
    assert "unavailable" in error
    assert sandbox.calls == []


def test_build_verification_overlays_only_candidate_at_project_path():
    sandbox = RecordingBuildSandbox()

    ok, _, _, evidence = v3main.verify_build_command(
        code="print('candidate')\n",
        sandbox=sandbox,
        build_command="python -m py_compile app.py",
        file_path="/workspace/app.py",
        project_files={"util.py": "VALUE = 1\n"},
        working_dir="/workspace",
    )

    assert ok is True
    assert evidence["status"] == "passed"
    assert sandbox.calls[0]["files"]["app.py"] == "print('candidate')\n"
    assert "util.py" not in sandbox.calls[0]["files"]


@pytest.mark.parametrize("command", ["pnpm run build", "yarn build", "bun run build"])
def test_build_verification_allows_detected_node_package_managers(command):
    sandbox = RecordingBuildSandbox()

    ok, _, _, evidence = v3main.verify_build_command(
        code="console.log('candidate')\n",
        sandbox=sandbox,
        build_command=command,
        file_path="/workspace/app.js",
        project_files={},
        working_dir="/workspace",
    )

    assert ok is True
    assert evidence["status"] == "passed"
    assert sandbox.calls[0]["command"] == command


@pytest.mark.parametrize("build_command,file_path,message", [
    pytest.param("python app.py; rm -rf .", "/workspace/app.py", "not allowed",
                 id="a command that is not allowed"),
    pytest.param("pytest > /tmp/out", "/workspace/app.py", "not allowed", id="shell redirection"),
    pytest.param("python -m py_compile app.py", "/tmp/app.py", "under working_dir",
                 id="a file outside the workspace"),
])
def test_build_verification_refuses_without_running(build_command, file_path, message):
    sandbox = RecordingBuildSandbox()

    ok, _, err, evidence = v3main.verify_build_command(
        code="print('candidate')\n",
        sandbox=sandbox,
        build_command=build_command,
        file_path=file_path,
        project_files={},
        working_dir="/workspace",
    )

    assert ok is False
    assert message in err
    assert evidence["status"] == "unavailable"
    assert sandbox.calls == []


@pytest.mark.parametrize("file_path,working_dir,expected", [
    pytest.param("/workspace/src/app.py", "/workspace", "src/app.py", id="absolute path in a subfolder"),
    pytest.param("src/app.py", "/workspace", "src/app.py", id="relative path"),
    pytest.param("/workspace/app.py", "/workspace", "app.py", id="absolute path at the root"),
])
def test_project_relative_path_accepts_only_project_paths(file_path, working_dir, expected):
    assert v3main._project_relative_path(file_path, working_dir) == expected


@pytest.mark.parametrize("file_path,working_dir", [
    pytest.param("/tmp/app.py", "/workspace", id="absolute path outside the root"),
    pytest.param("../app.py", "/workspace", id="relative path that climbs out"),
    pytest.param("src/../../app.py", "/workspace", id="climbs out through a subfolder"),
    pytest.param("/workspace/app.py", "workspace", id="relative root"),
    pytest.param("", "/workspace", id="empty path"),
])
def test_project_relative_path_rejects_escape_and_invalid_roots(file_path, working_dir):
    with pytest.raises(ValueError):
        v3main._project_relative_path(file_path, working_dir)
