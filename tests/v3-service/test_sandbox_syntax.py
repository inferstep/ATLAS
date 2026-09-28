import importlib.util
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from tests.conftest import run_shell_sync
from fastapi import HTTPException

SANDBOX_DIR = Path(__file__).parents[2] / "sandbox"


_SANDBOX_MODULE = None


def _load_sandbox_module():
    """executor_server, imported once per session.

    Every import starts the module's background-job janitor thread, which
    never exits; importing per test left one per test running for the rest of
    the session, and thread-count assertions in tests/v3 then failed when the
    suites ran together. Tests that change module attributes restore them
    (try/finally or monkeypatch), so sharing the module is safe.
    """
    global _SANDBOX_MODULE
    if _SANDBOX_MODULE is None:
        _SANDBOX_MODULE = _import_sandbox_module()
    return _SANDBOX_MODULE


def _import_sandbox_module():
    # executor_server imports its sibling `structured_log`, which only
    # resolves when sandbox/ is importable. In the container that holds
    # because the module runs from its own directory; loading it by path
    # from the test suite does not, so put the directory on sys.path
    # first. Without this every test here fails at import with
    # ModuleNotFoundError before reaching an assertion.
    if str(SANDBOX_DIR) not in sys.path:
        sys.path.insert(0, str(SANDBOX_DIR))
    module_path = SANDBOX_DIR / "executor_server.py"
    spec = importlib.util.spec_from_file_location("atlas_sandbox_executor", module_path)
    module = importlib.util.module_from_spec(spec)
    # Register before exec: executor_server defers annotation evaluation, so
    # pydantic resolves model field types by looking the module up in
    # sys.modules. Loading by path without registering leaves it absent and
    # every model raises "is not fully defined". In the container the module
    # runs as __main__ and is registered, so this only bites by-path loads.
    sys.modules[spec.name] = module
    try:
        spec.loader.exec_module(module)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise
    return module


def test_json_syntax_check_accepts_valid_document(tmp_path):
    sandbox = _load_sandbox_module()

    assert sandbox._syntax_check_impl("json", '{"ready": true}', tmp_path) == []


def test_json_syntax_check_rejects_invalid_document(tmp_path):
    sandbox = _load_sandbox_module()

    errors = sandbox._syntax_check_impl("json", '{"ready": }', tmp_path)

    assert errors


def test_yaml_syntax_check_accepts_a_multi_document_file(tmp_path):
    # Compose and Kubernetes manifests are multi-document. safe_load rejects
    # them ("expected a single document in the stream"), and that false
    # rejection once kept the write gate off every new file.
    pytest.importorskip("yaml")
    sandbox = _load_sandbox_module()

    assert sandbox._syntax_check_impl("yaml", "a: 1\n---\nb: 2\n", tmp_path) == []


def test_yaml_syntax_check_rejects_malformed_yaml(tmp_path):
    pytest.importorskip("yaml")
    sandbox = _load_sandbox_module()

    assert sandbox._syntax_check_impl("yaml", "a: [1, 2\nb: }\n", tmp_path)


def test_xml_syntax_check_rejects_invalid_document(tmp_path):
    sandbox = _load_sandbox_module()

    errors = sandbox._syntax_check_impl("xml", "<root>", tmp_path)

    assert errors


def test_xml_syntax_check_rejects_entity_expansion(tmp_path):
    sandbox = _load_sandbox_module()
    document = """<!DOCTYPE bomb [
      <!ENTITY a "1234567890">
      <!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">
    ]><root>&b;</root>"""

    errors = sandbox._syntax_check_impl("xml", document, tmp_path)

    assert errors


def test_overlay_write_rejects_symlink_leaf(tmp_path):
    sandbox = _load_sandbox_module()
    root = tmp_path / "snapshot"
    root.mkdir()
    outside = tmp_path / "outside.txt"
    outside.write_text("do not overwrite")
    (root / "candidate.py").symlink_to(outside)

    with pytest.raises(HTTPException):
        sandbox._write_overlay_files(root, {"candidate.py": "attacker content"})

    assert outside.read_text() == "do not overwrite"


def test_overlay_write_rejects_symlink_parent(tmp_path):
    sandbox = _load_sandbox_module()
    root = tmp_path / "snapshot"
    root.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    (root / "src").symlink_to(outside, target_is_directory=True)

    with pytest.raises(HTTPException):
        sandbox._write_overlay_files(root, {"src/candidate.py": "attacker content"})

    assert not (outside / "candidate.py").exists()


def test_shell_overlay_runs_without_mutating_workspace(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    original = workspace / "app.py"
    original.write_text("raise RuntimeError('real workspace should not run')\n")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command="python3 -m py_compile app.py",
                cwd=str(workspace),
                files={"app.py": "print('candidate overlay')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True
    assert original.read_text() == "raise RuntimeError('real workspace should not run')\n"


def test_shell_overlay_translates_absolute_workspace_paths(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    original = workspace / "app.py"
    original.write_text("def broken(:\n")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command=f"python3 -m py_compile {workspace}/app.py",
                cwd=str(workspace),
                files={"app.py": "print('candidate overlay')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True
    assert original.read_text() == "def broken(:\n"


def test_shell_overlay_rejects_path_traversal(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        with pytest.raises(HTTPException):
            run_shell_sync(sandbox, sandbox.ShellRequest(
                    command="true",
                    cwd=str(workspace),
                    files={"../escape.py": "print('nope')\n"},
                )
            )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base


def test_shell_snapshot_skips_external_symlinks(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    outside = tmp_path / "outside.txt"
    outside.write_text("secret")
    (workspace / "link.txt").symlink_to(outside)

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command="test ! -e link.txt",
                cwd=str(workspace),
                files={"candidate.py": "print('ok')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True


def test_shell_snapshot_preserves_safe_internal_symlinks(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "real.txt").write_text("inside")
    (workspace / "link.txt").symlink_to("real.txt")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command="test -L link.txt && test \"$(cat link.txt)\" = inside",
                cwd=str(workspace),
                files={"candidate.py": "print('ok')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True


def test_shell_snapshot_keeps_small_node_modules(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    package_dir = workspace / "node_modules" / "tiny"
    package_dir.mkdir(parents=True)
    (package_dir / "index.js").write_text("module.exports = 1;\n")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command="test -f node_modules/tiny/index.js",
                cwd=str(workspace),
                files={"candidate.js": "console.log('ok')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True


def test_shell_snapshot_skips_large_artifacts(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "model.gguf").write_text("large model placeholder")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    try:
        response = run_shell_sync(sandbox, sandbox.ShellRequest(
                command="test ! -e model.gguf",
                cwd=str(workspace),
                files={"candidate.py": "print('ok')\n"},
            )
        )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base

    assert response.success is True


def test_shell_snapshot_fails_when_byte_limit_is_exceeded(tmp_path):
    sandbox = _load_sandbox_module()
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "small.txt").write_text("too many bytes for this test")

    previous_root = sandbox.WORKSPACE_ROOT
    previous_base = sandbox.WORKSPACE_BASE
    previous_limit = sandbox.SHELL_SNAPSHOT_MAX_BYTES
    sandbox.WORKSPACE_ROOT = workspace
    sandbox.WORKSPACE_BASE = tmp_path
    sandbox.SHELL_SNAPSHOT_MAX_BYTES = 4
    try:
        with pytest.raises(HTTPException) as exc:
            run_shell_sync(sandbox, sandbox.ShellRequest(
                    command="true",
                    cwd=str(workspace),
                    files={"candidate.py": "print('ok')\n"},
                )
            )
    finally:
        sandbox.WORKSPACE_ROOT = previous_root
        sandbox.WORKSPACE_BASE = previous_base
        sandbox.SHELL_SNAPSHOT_MAX_BYTES = previous_limit

    assert exc.value.status_code == 413


# --- Jinja template syntax (scenario C, 2026-09-15) -------------------------
#
# A Flask template `templates/index.html` shipped `{% for p in people %)` —
# `%)` instead of `%}`. html.parser passes it (all text to it), the server
# starts and the file imports, and GET / returns 500 with a jinja
# TemplateSyntaxError. The html branch now runs a Jinja parse, scoped to files
# that are actually templates so non-Jinja frameworks are never judged.

# The delivered line 58, verbatim (minus the leading whitespace).
C_BROKEN_ROW = (
    "<td> {% for p in people %){% if p.id == expense.paid_by_id %}"
    "{{ p.name }}{% endif %}{% endfor %}{% endfor %} </td>"
)
C_FIXED_ROW = C_BROKEN_ROW.replace("%)", "%}", 1)


def test_jinja_typo_in_a_template_html_is_reported(tmp_path):
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "<table>" + C_BROKEN_ROW + "</table>", tmp_path,
        filename="templates/index.html")
    assert errors, "a `%)` tag typo in a template must be reported"
    assert any("TemplateSyntaxError" in e for e in errors), errors


def test_jinja_correct_template_passes(tmp_path):
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "<table>" + C_FIXED_ROW + "</table>", tmp_path,
        filename="templates/index.html")
    assert errors == [], errors


def test_the_same_broken_bytes_outside_templates_are_not_judged(tmp_path):
    # Identical bytes under src/ (a Vue/Angular/component tree, not Jinja) must
    # NOT be handed to a Jinja parser.
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "<table>" + C_BROKEN_ROW + "</table>", tmp_path,
        filename="src/components/list.html")
    assert errors == [], errors


def test_vue_interpolation_under_templates_is_not_judged(tmp_path):
    # `{{ a || b }}` is valid Vue and invalid Jinja, but with no `{%` statement
    # tag it is not attributed to Jinja.
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "<p>{{ user?.name || 'x' }}</p>", tmp_path,
        filename="templates/widget.html")
    assert errors == [], errors


def test_unknown_jinja_extension_tag_is_not_a_false_positive(tmp_path):
    # A third-party extension tag this parser hasn't loaded is not a typo.
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "{% cache 60 %}<p>hi</p>{% endcache %}", tmp_path,
        filename="templates/page.html")
    assert errors == [], errors


def test_jinja_extension_named_file_is_checked(tmp_path):
    sandbox = _load_sandbox_module()
    errors = sandbox._syntax_check_impl(
        "html", "{% for x in y %) {% endfor %}", tmp_path,
        filename="emails/welcome.jinja2")
    assert any("TemplateSyntaxError" in e for e in errors), errors


# --- subdirectory source files (audit finding, 2026-09-15) ------------------
#
# Once the proxy started sending the real file path, a gated source file in a
# subdirectory (src/app.py, static/app.js) hit a language branch that writes
# the check file to disk WITHOUT creating the parent dir -> FileNotFoundError,
# reported as an unparseable file, refusing a legitimate write.

def test_python_syntax_check_handles_a_subdirectory_path(tmp_path):
    sandbox = _load_sandbox_module()
    # Valid Python in a subdirectory must check clean, not FileNotFoundError.
    assert sandbox._syntax_check_impl(
        "python", "x = 1\n", tmp_path, filename="src/pkg/app.py") == []
    # And a real error is still reported (not masked by a write failure).
    errs = sandbox._syntax_check_impl(
        "python", "def f(\n", tmp_path, filename="src/pkg/app.py")
    assert errs and any("line" in e.lower() or "syntax" in e.lower() for e in errs), errs


def test_javascript_syntax_check_handles_a_subdirectory_path(tmp_path):
    sandbox = _load_sandbox_module()
    assert sandbox._syntax_check_impl(
        "javascript", "const x = 1;\n", tmp_path, filename="static/js/app.js") == []


# --- the Jinja checker's tool dependency must be baked (audit, 2026-09-15) ---
#
# _jinja_template_errors does `import jinja2`; jinja2 is NOT a user app lib the
# sandbox installs per project, so it must be a baked CHECKER tool. Without it
# the check fails open and every broken template ships. This ties the code's
# import to the image's requirements so the gate cannot silently go inert.

def test_jinja2_is_a_baked_sandbox_verify_dependency():
    req = (SANDBOX_DIR / "requirements-verify.txt").read_text().lower()
    assert "jinja2" in req, (
        "jinja2 must be in sandbox/requirements-verify.txt — the Jinja template "
        "syntax check imports it, and without it the check fails open")


# --- a checker that did not finish is not a pass ------------------------------
#
# Audit S-sandbox/INTEGRITY#1, S-sandbox/DEAD#2: a checker stopped at its wall
# clock or memory ceiling, or one that never started, exits non-zero with empty
# stderr. Every branch read its errors out of stderr, so the check came back
# with no errors and valid: true.

def _ended(sandbox, outcome, returncode=-15, stdout="", stderr=""):
    def run(cmd, timeout, cwd=None, env=None, stdin=None, cancelled=None):
        return {"success": False, "stdout": stdout, "stderr": stderr,
                "returncode": returncode, "timed_out": outcome == sandbox.OUTCOME_TIMED_OUT,
                "outcome": outcome, "peak_memory_bytes": 0, "peak_processes": 0,
                "survivors": 0}
    return run


@pytest.mark.parametrize("lang,code", [
    ("python", "def broken(:\n"),
    ("javascript", "const x = (1 + 2;\n"),
    ("typescript", "const x = (1 + 2;\n"),
    ("go", "package main\nfunc main( {\n"),
    ("java", "public class Main { void f( }\n"),
    ("kotlin", "fun main( {\n"),
    ("rust", "fn main( {\n"),
    ("c", "int main( {\n"),
    ("ruby", "def f(\n"),
    ("php", "<?php function f( {\n"),
    ("bash", "if then\n"),
])
@pytest.mark.parametrize("outcome", ["timed_out", "memory_exhausted", "spawn_failed"])
def test_a_stopped_checker_is_not_a_pass(tmp_path, monkeypatch, lang, code, outcome):
    sandbox = _load_sandbox_module()
    monkeypatch.setattr(sandbox, "_run_cmd", _ended(sandbox, outcome))
    monkeypatch.setattr(sandbox, "WORKSPACE_BASE", str(tmp_path))

    resp = sandbox.syntax_check(sandbox.SyntaxCheckRequest(code=code, language=lang))

    assert resp.valid is False
    assert resp.status == "not_run"
    assert resp.outcome == outcome
    assert "unavailable" in resp.errors[0]


def test_a_checker_that_finished_still_gives_its_verdict(tmp_path, monkeypatch):
    sandbox = _load_sandbox_module()
    monkeypatch.setattr(sandbox, "_run_cmd", _ended(sandbox, "completed", returncode=0))
    monkeypatch.setattr(sandbox, "WORKSPACE_BASE", str(tmp_path))

    resp = sandbox.syntax_check(sandbox.SyntaxCheckRequest(code="x = 1\n", language="python"))

    assert resp.valid is True and resp.status == "checked" and resp.errors == []


# --- a JavaScript verdict does not depend on how Node guesses the module type --
#
# Audit GB-4#1: from Node 20.19 a typeless .js file with import/export is
# detected as a module and `node --check` compiles nothing, so garbage and
# truncated modules exited 0; before 20.19 every valid module was rejected.

def _runs(*cmd):
    """The tool is installed and answers. macOS ships a /usr/bin/javac that
    exists without a JDK, so being on PATH is not enough."""
    if shutil.which(cmd[0]) is None:
        return False
    try:
        return subprocess.run(cmd, capture_output=True, timeout=60).returncode == 0
    except Exception:
        return False


_requires_node = pytest.mark.skipif(not _runs("node", "--version"), reason="node not available")


@_requires_node
@pytest.mark.parametrize("code,valid", [
    ('import fs from "fs";\nthis is not javascript at all ###\n', False),
    ('import fs from "fs";\nexport function add(a, b) {\n  return a +\n', False),
    ('export const a = 1; }}}}}\n', False),
    ('import fs from "fs";\nexport const a = await Promise.resolve(1);\n'
     'console.log(import.meta.url, fs);\n', True),
    ('const fs = require("fs");\nmodule.exports = { a: 1 };\n', True),
    ('const x = (1 + 2;\n', False),
])
def test_javascript_module_type_does_not_decide_the_verdict(tmp_path, code, valid):
    sandbox = _load_sandbox_module()

    errors = sandbox._syntax_check_impl("javascript", code, tmp_path, "src/index.js")

    assert (errors == []) is valid, errors


@_requires_node
def test_a_javascript_error_names_the_file_the_caller_sent(tmp_path):
    sandbox = _load_sandbox_module()

    errors = sandbox._syntax_check_impl("javascript", "const x = (1 + 2;\n", tmp_path, "src/app.js")

    assert errors and "app.js" in errors[0] and ".cjs" not in errors[0] and ".mjs" not in errors[0]


# --- a syntax check judges syntax ---------------------------------------------
#
# Audit GB-4#2: the file is checked alone, so references to a sibling class,
# module or package cannot resolve. javac, kotlinc and tsc reported those as
# errors, and valid multi-file code was refused as a syntax error.

def test_typescript_counts_only_syntax_diagnostics(tmp_path, monkeypatch):
    sandbox = _load_sandbox_module()
    unresolvable = ("a.ts(1,21): error TS2307: Cannot find module './math' or its corresponding type declarations.\n"
                    "a.ts(3,7): error TS2322: Type 'string' is not assignable to type 'number'.\n"
                    "a.ts(5,12): error TS7006: Parameter 'a' implicitly has an 'any' type.\n")
    monkeypatch.setattr(sandbox, "_run_cmd", _ended(sandbox, "completed", returncode=2, stdout=unresolvable))
    assert sandbox._syntax_check_impl("typescript", "import { add } from './math';\n", tmp_path) == []

    syntax = unresolvable + "a.ts(4,17): error TS1005: ')' expected.\n"
    monkeypatch.setattr(sandbox, "_run_cmd", _ended(sandbox, "completed", returncode=2, stdout=syntax))
    assert sandbox._syntax_check_impl("typescript", "const x = (1 + 2;\n", tmp_path) == [
        "a.ts(4,17): error TS1005: ')' expected."]

    crashed = "Error: Cannot find module 'typescript'"
    monkeypatch.setattr(sandbox, "_run_cmd", _ended(sandbox, "completed", returncode=1, stderr=crashed))
    assert sandbox._syntax_check_impl("typescript", "const x = 1;\n", tmp_path) == [crashed]


_requires_javac = pytest.mark.skipif(not _runs("javac", "-version"), reason="javac not available")
_requires_kotlinc = pytest.mark.skipif(not _runs("kotlinc", "-version"), reason="kotlinc not available")


@_requires_javac
def test_java_references_to_siblings_are_not_syntax_errors(tmp_path):
    sandbox = _load_sandbox_module()
    app = ("import com.example.model.User;\n"
           "public class App {\n"
           "    public static void main(String[] args) {\n"
           "        Calculator c = new Calculator();\n"
           "        System.out.println(c.add(2, 3));\n"
           "    }\n"
           "}\n")
    assert sandbox._syntax_check_impl("java", app, tmp_path / "a") == []

    broken = "public class Broken {\n    void f() {\n        int x = 5\n    }\n}\n"
    errors = sandbox._syntax_check_impl("java", broken, tmp_path / "b")
    assert errors and "';' expected" in errors[0]


@_requires_kotlinc
def test_kotlin_unresolved_references_are_not_syntax_errors(tmp_path):
    sandbox = _load_sandbox_module()
    uses_sibling = "fun main() {\n    val c = Calculator()\n    println(c.add(2, 3))\n}\n"
    assert sandbox._syntax_check_impl("kotlin", uses_sibling, tmp_path / "a", "Main.kt") == []

    broken = 'fun main() {\n    println("hi"\n}\n'
    errors = sandbox._syntax_check_impl("kotlin", broken, tmp_path / "b", "Main.kt")
    assert errors and "syntax error" in errors[0].lower()


# --- the HTML check can fail ----------------------------------------------------
#
# Audit S-sandbox/INTEGRITY#2, P-gates/INTEGRITY#2: html.parser accepts any
# text, so a page with no HTML in it, or one cut off inside its <script>, was
# a syntax pass, and that pass alone completed an HTML deliverable.

@pytest.mark.parametrize("code,valid", [
    ("<!DOCTYPE html><html><body><canvas></canvas>"
     "<script>let a = 1;</script></body></html>\n", True),
    ("<div>hi", True),                                     # HTML closes it implicitly
    ("<template><p>{{ msg }}</p></template>\n", True),     # a Vue template
    ("{% extends 'base.html' %}{% block content %}Hello{% endblock %}\n", True),
    ("<p>a < b and c</p>\n", True),
    ("const x = 1; // no html at all\n", False),
    ("def f():\n    return 1\n", False),
    ("", False),
    ("<!DOCTYPE html>\n<html><body><script>\nfunction loop( {", False),
    ("<html><body><div class='x", False),
    ("<html><!-- unterminated", False),
    ("<html><head><style>body { color: red;", False),
])
def test_the_html_check_can_fail(tmp_path, code, valid):
    sandbox = _load_sandbox_module()

    errors = sandbox._syntax_check_impl("html", code, tmp_path, "index.html")

    assert (errors == []) is valid, errors
