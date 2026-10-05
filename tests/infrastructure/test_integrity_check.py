"""The integrity check reads a diff and reports what could weaken the checks.

Each rule gets a change that must be reported and a near miss that must not.
"""
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "integrity_check.py"
TASKS = {"flask_pause", "offbyone"}
# Comment markers are built here, so that this file has no line that looks
# like the comments it describes.
HASH, SLASHES = "#", "//"


@pytest.fixture(scope="module")
def ic():
    spec = importlib.util.spec_from_file_location("integrity_check", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


def diff(path, added=(), removed=(), status="modified"):
    lines = [f"diff --git a/{path} b/{path}"]
    if status != "modified":
        lines.append(f"{'new' if status == 'added' else 'deleted'} file mode 100644")
    lines += [f"--- a/{path}", f"+++ b/{path}", f"@@ -10,{len(removed)} +10,{len(added)} @@"]
    return "\n".join(lines + ["-" + r for r in removed] + ["+" + a for a in added]) + "\n"


def whats(ic, *diffs):
    return [f.what for f in ic.check("".join(diffs), TASKS)]


def test_an_ordinary_change_reports_nothing(ic):
    change = (diff("proxy/retry.go", added=[f"\t{SLASHES} Wait before the next attempt.", "\twait(delay)"])
              + diff("proxy/retry_test.go", added=["func TestRetryWaits(t *testing.T) {", "\tt.Fatal(err)"]))
    assert whats(ic, change) == []


def test_the_diff_is_read_per_file_with_line_numbers(ic):
    changes = ic.parse_diff(diff("a.py", added=["x = 1", "y = 2"], removed=["x = 0"])
                            + diff("docs/NEW.md", added=["hello"], status="added"))
    assert [(c.path, c.status) for c in changes] == [("a.py", "modified"), ("docs/NEW.md", "added")]
    assert [(line, text) for _, line, text in changes[0].added] == [(10, "x = 1"), (11, "y = 2")]
    assert [text for _, text in changes[0].removed] == ["x = 0"]


def test_a_deleted_test_file_is_reported(ic):
    assert whats(ic, diff("tests/v3/test_plan.py", removed=["def test_plan():"], status="deleted")) == \
        ["test file deleted"]


@pytest.mark.parametrize("path, line, name", [
    ("proxy/gates_test.go", "func TestGateRefuses(t *testing.T) {", "TestGateRefuses"),
    ("tests/cli/test_doctor.py", "def test_doctor_reports():", "test_doctor_reports"),
    ("extensions/vscode/test/sse.test.ts", "  it('parses a frame', () => {", "parses a frame"),
])
def test_a_removed_test_is_reported_unless_it_moved(ic, path, line, name):
    assert whats(ic, diff(path, removed=[line])) == [f"test removed: {name}"]
    moved = diff(path, removed=[line]) + diff(path.replace("test", "test2", 1), added=[line])
    assert whats(ic, moved) == []


@pytest.mark.parametrize("path, line", [
    ("proxy/gates_test.go", '\tt.Skip("flaky")'),
    ("tests/cli/test_doctor.py", "@pytest.mark.skip(reason='later')"),
    ("tests/cli/test_doctor.py", "@pytest.mark.xfail"),
    ("tests/cli/test_doctor.py", "pytestmark = pytest.mark.skipif(sys.platform == 'darwin', reason='x')"),
    ("extensions/vscode/test/sse.test.ts", "  it.skip('parses a frame', () => {"),
])
def test_a_new_skip_is_reported(ic, path, line):
    assert whats(ic, diff(path, added=[line])) == ["new skip or todo marker in a test"]


def test_skip_words_inside_a_string_are_not_a_skip(ic):
    line = """    ("proxy/gates_test.go", 't.Skip("flaky")'),"""
    assert whats(ic, diff("tests/infrastructure/test_x.py", added=[line])) == []


def test_fewer_assertions_are_reported(ic):
    change = diff("tests/cli/test_doctor.py", removed=["    assert a == 1", "    assert b == 2"],
                  added=["    assert a == 1"])
    assert whats(ic, change) == ["2 assertion(s) removed, 1 added"]


def test_a_rewritten_assertion_is_reported_only_with_a_product_change(ic):
    rewritten = diff("proxy/gates_test.go", removed=['\tt.Fatal("want 3")'], added=['\tt.Fatal("want 4")'])
    assert whats(ic, rewritten) == []
    with_product = rewritten + diff("proxy/gates.go", added=["\treturn 4"])
    assert whats(ic, with_product) == ["1 assertion(s) rewritten while product code changed"]


@pytest.mark.parametrize("comment, label", [
    ("fixed on 2026-09-30 after the outage", "a date"),
    ("regressed in 3f75f62", "a commit ID"),
    ("seen in smoke5 and cycle 12", "a run name"),
])
def test_history_in_a_new_comment_is_reported(ic, comment, label):
    found = whats(ic, diff("proxy/retry.go", added=[f"\t{SLASHES} {comment}"]))
    assert len(found) == 1 and found[0].startswith(f"{label} in a new comment")
    assert whats(ic, diff("atlas/cli.py", added=[f"x = 1  {HASH} {comment}"]))[0].startswith(label)


def test_the_same_text_outside_a_comment_or_in_a_document_is_not_history(ic):
    assert whats(ic, diff("proxy/retry.go", added=['\tday := "2026-09-30"'])) == []
    assert whats(ic, diff("CHANGELOG.md", added=["Fixed on 2026-09-30 in 3f75f62."])) == []


def test_an_evaluation_task_named_in_product_code_is_reported(ic):
    line = '\tif task == "flask_pause" {'
    assert whats(ic, diff("proxy/agent.go", added=[line])) == \
        ["evaluation task named in product code: 'flask_pause'"]
    assert whats(ic, diff("proxy/agent_test.go", added=[line])) == []
    assert whats(ic, diff("scripts/e2e-reliability.py", added=['    "flask_pause": Task('])) == []
    assert whats(ic, diff("proxy/agent.go", added=['\tname := "flask_pauses"'])) == []


def test_a_new_document_needs_approval_outside_the_allowed_folders(ic):
    assert whats(ic, diff("docs/NOTES.md", added=["x"], status="added")) == ["new documentation file"]
    assert whats(ic, diff(".github/ISSUE_TEMPLATE/bug.md", added=["x"], status="added")) == []
    assert whats(ic, diff("docs/SETUP.md", added=["x"])) == []


def test_a_change_to_a_gate_file_needs_approval(ic):
    for path in (".github/workflows/test.yml", ".github/code-health-baseline.json",
                 "scripts/integrity_check.py", "codecov.yml"):
        assert whats(ic, diff(path, added=["x: 1"])) == ["change to a file that configures the checks"], path
    assert whats(ic, diff(".github/requirements/ci.txt", added=["pytest==9.0.0"])) == []


@pytest.mark.parametrize("path, line", [
    ("proxy/retry.go", f"\tcall() {SLASHES}nolint:errcheck"),
    ("atlas/cli.py", f"import os  {HASH} noqa: F401"),
    ("extensions/vscode/src/sse.ts", f"  {SLASHES} @ts-ignore"),
])
def test_a_new_suppression_needs_approval(ic, path, line):
    findings = [f for f in ic.check(diff(path, added=[line]), TASKS)]
    assert [(f.level, f.what) for f in findings] == [("approval", "new suppression marker")]


def test_a_suppression_word_in_code_is_not_a_suppression(ic):
    assert whats(ic, diff("atlas/cli.py", added=['MARKERS = ("noqa", "nolint")'])) == []


def test_new_dependencies_are_listed_as_notes(ic):
    change = (diff("proxy/go.mod", added=["\tgithub.com/example/retry v1.2.0"])
              + diff("extensions/vscode/package.json", added=['    "left-pad": "^1.3.0",', '  "version": "0.0.2",'])
              + diff("sandbox/requirements-runtime.txt", added=["fastapi==0.200.0"]))
    findings = ic.check(change, TASKS)
    assert [f.level for f in findings] == ["note", "note", "note"]
    assert [f.what.split(": ")[1] for f in findings] == ["github.com/example/retry", "left-pad", "fastapi"]


def test_a_large_change_is_noted_and_lock_files_do_not_count(ic):
    big = diff("proxy/agent.go", added=[f"\tstep{i}()" for i in range(400)])
    assert whats(ic, big) == ["large change: 400 changed lines"]
    assert whats(ic, diff("extensions/vscode/package-lock.json", added=["x"] * 900)) == []


def test_every_finding_says_why_and_how_to_fix(ic):
    change = (diff("tests/v3/test_plan.py", removed=["def test_plan():"], status="deleted")
              + diff("proxy/agent.go", added=[f'\tif t == "offbyone" {{ {SLASHES} 2026-01-02 nolint'])
              + diff("docs/NOTES.md", added=["x"], status="added")
              + diff(".github/workflows/test.yml", added=["x: 1"])
              + diff("proxy/go.mod", added=["\tgithub.com/example/retry v1.2.0"]))
    findings = ic.check(change, TASKS)
    assert {f.level for f in findings} == {"approval", "warning", "note"}
    for f in findings:
        assert f.why.strip() and f.fix.strip(), f.what


def test_the_task_names_come_from_the_runner(ic):
    names = ic.task_names()
    assert {"flask_pause", "aoc_sonar", "multifile_cli"} <= names


def test_the_report_exits_zero_unless_strict(ic):
    def run(*args):
        return subprocess.run([sys.executable, str(SCRIPT), "--base", "HEAD", *args],
                              capture_output=True, text=True)
    plain, strict = run(), run("--strict")
    assert plain.returncode == 0 and strict.returncode == 0, plain.stderr + strict.stderr
    assert "nothing found" in plain.stdout
    missing = run("--base", "no-such-ref")
    assert missing.returncode == 2 and "fix:" in missing.stderr
