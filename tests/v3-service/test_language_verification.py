"""Contract tests for language-aware V3 candidate verification."""

import sys
from pathlib import Path

import pytest


PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import main as v3main


class RecordingSandbox:
    def __init__(self, valid=True, error=""):
        self.valid = valid
        self.error = error
        self.calls = []

    def syntax_check(self, code, language, filename=""):
        self.calls.append((code, language, filename))
        return self.valid, "", self.error


class BuildRecordingSandbox:
    def __init__(self, success=True, stdout="", stderr=""):
        self.success = success
        self.stdout = stdout
        self.stderr = stderr
        self.calls = []

    def run_command(self, command, files=None, cwd="/workspace", timeout=60):
        self.calls.append({
            "command": command,
            "files": files or {},
            "cwd": cwd,
            "timeout": timeout,
        })
        return self.success, self.stdout, self.stderr, {
            "exit_code": 0 if self.success else 1,
            "elapsed_ms": 25,
        }


def test_javascript_uses_language_aware_syntax_check():
    sandbox = RecordingSandbox(valid=False, error="Unexpected token")
    ok, _, error = v3main.smoke_compile_check("function {", sandbox, "javascript")
    assert ok is False
    assert error == "Unexpected token"
    assert sandbox.calls == [("function {", "javascript", "")]


def test_candidate_source_is_passed_as_data_without_rewriting():
    source = "const value = `'''\\n${example}`;"
    sandbox = RecordingSandbox(valid=True)
    ok, _, _ = v3main.smoke_compile_check(source, sandbox, "javascript")
    assert ok is True
    assert sandbox.calls[0][0] == source


def test_unknown_language_is_not_automatic_success():
    ok, _, error = v3main.smoke_compile_check("body {}", lambda _: (True, "", ""), "css")
    assert ok is False
    assert "unavailable" in error


def test_language_alias_is_normalized_for_sandbox():
    sandbox = RecordingSandbox(valid=True)
    ok, _, _ = v3main.smoke_compile_check("print('ok')", sandbox, "py")
    assert ok is True
    assert sandbox.calls[0][1] == "python"


def test_xml_uses_explicit_syntax_check():
    sandbox = RecordingSandbox(valid=True)
    ok, _, _ = v3main.smoke_compile_check("<root />", sandbox, "xml")
    assert ok is True
    assert sandbox.calls == [("<root />", "xml", "")]


def test_build_verification_overlays_only_candidate_at_project_path():
    sandbox = BuildRecordingSandbox(success=True, stdout="ok")

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


def test_build_verification_rejects_disallowed_command():
    sandbox = BuildRecordingSandbox(success=True)

    ok, _, err, evidence = v3main.verify_build_command(
        code="print('candidate')\n",
        sandbox=sandbox,
        build_command="python app.py; rm -rf .",
        file_path="/workspace/app.py",
        project_files={},
        working_dir="/workspace",
    )

    assert ok is False
    assert "not allowed" in err
    assert evidence["status"] == "unavailable"
    assert sandbox.calls == []


def test_build_verification_rejects_shell_redirection():
    sandbox = BuildRecordingSandbox(success=True)

    ok, _, err, evidence = v3main.verify_build_command(
        code="print('candidate')\n",
        sandbox=sandbox,
        build_command="pytest > /tmp/out",
        file_path="/workspace/app.py",
        project_files={},
        working_dir="/workspace",
    )

    assert ok is False
    assert "not allowed" in err
    assert evidence["status"] == "unavailable"
    assert sandbox.calls == []


def test_build_verification_allows_detected_node_package_managers():
    for command in ("pnpm run build", "yarn build", "bun run build"):
        sandbox = BuildRecordingSandbox(success=True)
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


def test_build_verification_rejects_target_outside_workspace():
    sandbox = BuildRecordingSandbox(success=True)

    ok, _, err, evidence = v3main.verify_build_command(
        code="print('candidate')\n",
        sandbox=sandbox,
        build_command="python -m py_compile app.py",
        file_path="/tmp/app.py",
        project_files={},
        working_dir="/workspace",
    )

    assert ok is False
    assert "under working_dir" in err
    assert evidence["status"] == "unavailable"
    assert sandbox.calls == []


@pytest.mark.parametrize(
    ("file_path", "working_dir", "expected"),
    [
        ("/workspace/src/app.py", "/workspace", "src/app.py"),
        ("src/app.py", "/workspace", "src/app.py"),
        ("/workspace/app.py", "/workspace", "app.py"),
    ],
)
def test_project_relative_path_accepts_only_project_paths(file_path, working_dir, expected):
    assert v3main._project_relative_path(file_path, working_dir) == expected


@pytest.mark.parametrize(
    ("file_path", "working_dir"),
    [
        ("/tmp/app.py", "/workspace"),
        ("../app.py", "/workspace"),
        ("src/../../app.py", "/workspace"),
        ("/workspace/app.py", "workspace"),
        ("", "/workspace"),
    ],
)
def test_project_relative_path_rejects_escape_and_invalid_roots(file_path, working_dir):
    with pytest.raises(ValueError):
        v3main._project_relative_path(file_path, working_dir)


def test_filename_is_forwarded_so_the_sandbox_can_scope_path_checks():
    # The Jinja-template check in the sandbox is path-scoped (templates/*.html).
    # V3's smoke path must carry the target file path, or that check can never
    # apply to a candidate V3 generated -- which let a broken template ship.
    sandbox = RecordingSandbox(valid=True)
    v3main.smoke_compile_check("<html></html>", sandbox, "html",
                               filename="templates/index.html")
    assert sandbox.calls, "the sandbox was not consulted"
    assert sandbox.calls[-1][2] == "templates/index.html", \
        f"filename not forwarded: {sandbox.calls[-1]}"


def test_filename_defaults_to_empty_when_absent():
    # A call with no filename (the historical signature) must still work.
    sandbox = RecordingSandbox(valid=True)
    v3main.smoke_compile_check("print('ok')", sandbox, "python")
    assert sandbox.calls[-1][2] == ""
