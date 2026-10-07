"""The integrity check reads a diff and reports what could weaken the checks.

Each rule gets a change that must be reported and a near miss that must not.
"""
import importlib.util
import json
import re
import subprocess
import sys
import time
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


def hunks(path, *parts):
    """A diff of one file with one hunk for each (old line, removed lines, new line, added lines)."""
    lines = [f"diff --git a/{path} b/{path}", f"--- a/{path}", f"+++ b/{path}"]
    for old, removed, new, added in parts:
        lines.append(f"@@ -{old},{len(removed)} +{new},{len(added)} @@")
        lines += ["-" + r for r in removed] + ["+" + a for a in added]
    return "\n".join(lines) + "\n"


class Files:
    """The files behind a diff, given as text: as they are now, and as they were at the base."""

    def __init__(self, now=None, before=None):
        self.now, self.before = now or {}, before or {}

    def read(self, path):
        return self.now.get(path)

    def read_base(self, path):
        return self.before.get(path)

    def tests_beside(self, path):
        folder = path.rsplit("/", 1)[0] + "/"
        return sorted(p for p in self.now if p.startswith(folder) and "/" not in p[len(folder):])


def whats(ic, *diffs, tree=None):
    return [f.what for f in ic.check("".join(diffs), TASKS, tree)]


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


@pytest.mark.parametrize("path, line, reason", [
    ("proxy/gates_test.go", '\tt.Skip("flaky on a slow disk")', "flaky on a slow disk"),
    ("proxy/gates_test.go", "\tt.Skip(objectHoldSkipReason)", "objectHoldSkipReason"),
    ("tests/cli/test_doctor.py", "@pytest.mark.skip(reason='needs a GPU')", "needs a GPU"),
    ("tests/cli/test_doctor.py", "pytestmark = pytest.mark.skipif(sys.platform == 'darwin', reason='no cgroups')",
     "no cgroups"),
    ("tests/cli/test_doctor.py", f"    pytest.skip()  {HASH} the fixture needs Docker", "the fixture needs Docker"),
])
def test_a_new_skip_with_a_reason_needs_approval_and_quotes_it(ic, path, line, reason):
    findings = ic.check(diff(path, added=[line]), TASKS)
    assert [(f.level, f.what) for f in findings] == \
        [("approval", f"new skip or todo marker in a test, with its reason: {reason}")]


@pytest.mark.parametrize("path, line", [
    ("proxy/gates_test.go", "\tt.SkipNow()"),
    ("proxy/gates_test.go", "\tt.Skip()"),
    ("proxy/gates_test.go", '\tt.Skip("")'),
    ("tests/cli/test_doctor.py", "@pytest.mark.xfail"),
    ("tests/cli/test_doctor.py", "@pytest.mark.skip(reason='')"),
    ("extensions/vscode/test/sse.test.ts", "  it.skip('parses a frame', () => {"),
])
def test_a_new_skip_with_no_reason_needs_action(ic, path, line):
    findings = ic.check(diff(path, added=[line]), TASKS)
    assert [(f.level, f.what) for f in findings] == [("warning", "new skip or todo marker in a test")]


def test_the_reason_of_a_skip_is_read_from_the_lines_of_its_call(ic):
    lines = ["pytestmark = pytest.mark.skipif(", "    not getattr(main, 'AVAILABLE', False),",
             '    reason="tree-sitter is not installed here",', ")"]
    assert whats(ic, diff("tests/v3-service/test_edit.py", added=lines)) == \
        ["new skip or todo marker in a test, with its reason: tree-sitter is not installed here"]


def test_a_comment_is_a_reason_only_on_the_line_of_the_skip_or_directly_above_it(ic):
    path = "proxy/gates_test.go"
    above = [f"\t{SLASHES} The runner has no loop device.", "\tt.SkipNow()"]
    assert whats(ic, diff(path, added=above)) == \
        ["new skip or todo marker in a test, with its reason: The runner has no loop device."]
    apart = [f"\t{SLASHES} The runner has no loop device.", "", "\tt.SkipNow()"]
    assert whats(ic, diff(path, added=apart)) == ["new skip or todo marker in a test"]


def test_a_comment_on_an_unchanged_line_above_a_new_skip_is_read_from_the_file(ic):
    path = "proxy/gates_test.go"
    source = "\n" * 8 + f"\t{SLASHES} The runner has no loop device.\n\tt.SkipNow()\n"
    assert whats(ic, diff(path, added=["\tt.SkipNow()"]), tree=Files({path: source})) == \
        ["new skip or todo marker in a test, with its reason: The runner has no loop device."]


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
    assert len(found) == 1
    assert found[0].startswith(f"{label} in a new comment")
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
                 "scripts/integrity_check.py", "scripts/checks_ran.py", "codecov.yml"):
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
        assert f.why.strip(), f.what
        assert f.fix.strip(), f.what


def test_the_task_names_come_from_the_runner(ic):
    names = ic.task_names()
    assert {"flask_pause", "aoc_sonar", "multifile_cli"} <= names


def test_the_report_exits_zero_unless_strict(ic):
    def run(*args):
        return subprocess.run([sys.executable, str(SCRIPT), "--base", "HEAD", *args],
                              capture_output=True, text=True)
    plain, strict = run(), run("--strict")
    assert plain.returncode == 0, plain.stderr
    assert strict.returncode == 0, strict.stderr
    assert "nothing found" in plain.stdout
    missing = run("--base", "no-such-ref")
    assert missing.returncode == 2
    assert "fix:" in missing.stderr


def test_a_very_long_line_is_read_in_linear_time(ic):
    # A pattern that backtracks takes seconds on this line; a linear one, milliseconds.
    line = "\t" + "a." * 20_000 + " x"
    started = time.monotonic()
    assert whats(ic, diff("proxy/go.mod", added=[line])) == []
    assert time.monotonic() - started < 1.0


def test_a_base_that_looks_like_an_option_is_not_one(ic, tmp_path):
    target = tmp_path / "written-by-git"
    result = subprocess.run([sys.executable, str(SCRIPT), f"--base=--output={target}"],
                            capture_output=True, text=True)
    assert result.returncode == 2
    assert "cannot read the diff" in result.stderr
    assert not target.exists()
    assert not Path(f"{target}...HEAD").exists()


# --- a test whose name line changed and whose body stayed ---

def test_a_test_renamed_in_place_is_a_note(ic):
    for path, old, new, was, now in [
        ("tests/cli/test_doctor.py", "def test_doctor_reports():", "def test_doctor_names_each_fault():",
         "test_doctor_reports", "test_doctor_names_each_fault"),
        ("proxy/gates_test.go", "func TestGateRefuses(t *testing.T) {", "func TestGateRefusesAWrite(t *testing.T) {",
         "TestGateRefuses", "TestGateRefusesAWrite"),
        ("extensions/vscode/test/sse.test.ts", "  it('parses a frame', () => {", "  it('parses one frame', () => {",
         "parses a frame", "parses one frame"),
    ]:
        findings = ic.check(diff(path, removed=[old], added=[new]), TASKS)
        assert [(f.level, f.what) for f in findings] == [("note", f"test renamed: {was} -> {now}")], path


def test_a_test_renamed_to_a_name_the_runner_does_not_collect_is_removed(ic):
    for new in ("def _test_doctor_reports():", "def doctor_reports():", "def xtest_doctor_reports():"):
        change = diff("tests/cli/test_doctor.py", removed=["def test_doctor_reports():"], added=[new])
        findings = ic.check(change, TASKS, Files({"tests/cli/test_doctor.py": new + "\n    assert True\n"}))
        assert [(f.level, f.what) for f in findings] == [("warning", "test removed: test_doctor_reports")], new


def test_a_test_removed_with_its_body_is_removed_though_another_is_written(ic):
    change = diff("tests/cli/test_doctor.py",
                  removed=["def test_reads_the_workspace(tmp_path):", "    files = scan(tmp_path)"],
                  added=["def test_reads_the_listing():"])
    assert whats(ic, change) == ["test removed: test_reads_the_workspace"]


HELPER_SOURCE = """import pytest


def _run_with(host):
    return host.run()


def test_a_record_ties_its_result(tmp_path):
    code = _run_with(FakeHost())
    assert code == 0


def _unused():
    return _run_with(None)
"""


def helper_change(*more):
    path = "tests/e2e/test_driver.py"
    return path, hunks(path, (4, ["def test_a_run_records_each_session(tmp_path):"], 4, ["def _run_with(host):"]), *more)


def test_a_test_turned_into_a_helper_that_a_test_of_the_change_calls_is_a_note(ic):
    path, change = helper_change((6, [], 8, ["def test_a_record_ties_its_result(tmp_path):",
                                             "    code = _run_with(FakeHost())", "    assert code == 0"]))
    findings = ic.check(change, TASKS, Files({path: HELPER_SOURCE}))
    assert [(f.level, f.what) for f in findings] == [
        ("note", "test turned into a helper: test_a_run_records_each_session is now _run_with, which 1 test(s) call")]


def test_a_helper_that_no_test_of_the_change_calls_leaves_the_test_removed(ic):
    path, change = helper_change((9, [], 13, ["def _unused():", "    return _run_with(None)"]))
    assert whats(ic, change, tree=Files({path: HELPER_SOURCE})) == ["test removed: test_a_run_records_each_session"]


def test_a_helper_in_a_file_that_cannot_be_read_leaves_the_test_removed(ic):
    _, change = helper_change((6, [], 8, ["def test_a_record_ties_its_result(tmp_path):",
                                          "    code = _run_with(FakeHost())"]))
    assert whats(ic, change) == ["test removed: test_a_run_records_each_session"]


# --- a function that skips the test that calls it ---

SKIP_HELPER = """package main

func needsObjectHold(t *testing.T) {
\tt.Helper()
\tif !objectHoldSupported {
\t\tt.Skip(objectHoldSkipReason)
\t}
}

func needsNothing(t *testing.T) {
\tt.Helper()
}

func skipsWithoutSaying(t *testing.T) {
\tt.SkipNow()
}
"""


def test_calls_to_a_skip_helper_are_one_finding_with_their_number_and_the_reason(ic):
    tree = Files({"proxy/object_hold_test.go": SKIP_HELPER, "proxy/permissions_test.go": ""})
    change = diff("proxy/permissions_test.go",
                  added=["\tneedsObjectHold(t)", "\tneedsObjectHold(t)", "\tneedsNothing(t)", "\tneedsObjectHold(t)"])
    findings = ic.check(change, TASKS, tree)
    assert [(f.level, f.line, f.what) for f in findings] == [
        ("approval", 10, "3 new call(s) to the skip helper needsObjectHold, with its reason: objectHoldSkipReason")]


def test_a_skip_helper_that_gives_no_reason_needs_action(ic):
    tree = Files({"proxy/object_hold_test.go": SKIP_HELPER})
    findings = ic.check(diff("proxy/permissions_test.go", added=["\tskipsWithoutSaying(t)"]), TASKS, tree)
    assert [(f.level, f.what) for f in findings] == [("warning", "1 new call(s) to the skip helper skipsWithoutSaying")]


def test_a_skip_helper_is_looked_for_beside_the_test_and_nowhere_else(ic):
    tree = Files({"tui/object_hold_test.go": SKIP_HELPER})
    assert whats(ic, diff("proxy/permissions_test.go", added=["\tneedsObjectHold(t)"]), tree=tree) == []


# --- suppression markers, by kind of file ---

MARKED = [
    ("proxy/retry.go", f"\tcall() {SLASHES}nolint:errcheck"),
    ("proxy/retry.go", f"\t{SLASHES}lint:ignore SA1019 the old call is the one under test"),
    ("proxy/retry.go", f"{SLASHES}lint:file-ignore U1000 generated"),
    ("proxy/retry.go", f"\tcall() {SLASHES} NOSONAR"),
    ("proxy/retry.go", f"\tcall() {SLASHES} {HASH}nosec G204"),
    ("atlas/cli.py", f"import os  {HASH} noqa: F401"),
    ("atlas/cli.py", f"x = load()  {HASH} type: ignore[arg-type]"),
    ("atlas/cli.py", f"run(cmd)  {HASH} nosec B603"),
    ("atlas/cli.py", f"if debug:  {HASH} pragma: no cover"),
    ("atlas/cli.py", f"x = 1  {HASH} NOSONAR"),
    ("extensions/vscode/src/sse.ts", f"  {SLASHES} eslint-disable-next-line max-lines-per-function"),
    ("extensions/vscode/src/sse.ts", f"  {SLASHES} @ts-ignore"),
    ("extensions/vscode/src/sse.ts", f"  {SLASHES} @ts-expect-error"),
    ("extensions/vscode/eslint.config.mjs", f"  {SLASHES} NOSONAR"),
    ("scripts/install.sh", f"{HASH} shellcheck disable=SC2086"),
    (".github/workflows/test.yml", f"          persist-credentials: false  {HASH} zizmor: ignore[artipacked]"),
    (".github/workflows/test.yml", f"      {HASH} yamllint disable-line rule:line-length"),
    ("sandbox/Dockerfile", f"{HASH} hadolint ignore=DL3008"),
    ("inference/Dockerfile.vulkan", f"{HASH} hadolint global ignore=DL3003"),
]


@pytest.mark.parametrize("path, line", MARKED)
def test_a_marker_in_a_file_of_its_kind_needs_approval(ic, path, line):
    findings = [f for f in ic.check(diff(path, added=[line]), TASKS) if f.what == "new suppression marker"]
    assert [(f.level, f.path) for f in findings] == [("approval", path)]


@pytest.mark.parametrize("path, line", [
    ("atlas/cli.py", f"x = 1  {HASH} shellcheck disable=SC2086"),
    ("atlas/cli.py", f"x = 1  {HASH} hadolint ignore=DL3008"),
    ("atlas/cli.py", f"x = 1  {HASH} nolint"),
    ("proxy/retry.go", f"\tcall() {SLASHES} noqa"),
    ("proxy/retry.go", f"\tcall() {SLASHES} eslint-disable"),
    ("extensions/vscode/src/sse.ts", f"  {SLASHES} nolint"),
    ("scripts/install.sh", f"{HASH} noqa"),
    ("scripts/install.sh", f"{HASH} hadolint ignore=DL3008"),
    (".github/workflows/test.yml", f"      {HASH} noqa"),
    (".github/workflows/test.yml", f"      {HASH} shellcheck disable=SC2086"),
    ("sandbox/Dockerfile", f"{HASH} zizmor: ignore[artipacked]"),
    ("sandbox/Dockerfile", f"{HASH} noqa"),
    ("docker-compose.yml", f"  {HASH} zizmor: ignore[artipacked]"),
    ("docs/SETUP.md", f"{HASH} hadolint ignore=DL3008"),
])
def test_the_marker_of_another_kind_of_file_is_text(ic, path, line):
    assert "new suppression marker" not in whats(ic, diff(path, added=[line]))


def test_every_kind_of_file_with_markers_has_a_test_for_each_marker(ic):
    tried = {(ic.file_kind(path), pattern) for path, line in MARKED
             for pattern in ic.MARKERS[ic.file_kind(path)] if ic.re.search(pattern, line)}
    assert tried == {(kind, pattern) for kind, patterns in ic.MARKERS.items() for pattern in patterns}


def test_a_marked_line_that_only_moved_is_not_new(ic):
    line = f"x = load()  {HASH} type: ignore[arg-type]"
    moved = hunks("atlas/cli.py", (10, [line], 9, []), (30, [], 40, [line]))
    assert whats(ic, moved) == []
    twice = hunks("atlas/cli.py", (10, [line], 9, []), (30, [], 40, [line, line]))
    assert whats(ic, twice) == ["new suppression marker"]


IMPORT = f"import main  {HASH} noqa: E402"
AFTER_PATH = "import sys\n\nsys.path.insert(0, 'v3-service')\n\n" + IMPORT + "\n"


def test_an_import_after_the_line_that_sets_the_import_path_is_not_a_suppression(ic):
    path = "tests/v3-service/test_edit.py"
    change = hunks(path, (4, [], 5, [IMPORT]))
    assert whats(ic, change, tree=Files({path: AFTER_PATH})) == []


@pytest.mark.parametrize("line, source", [
    (IMPORT, "import sys\n\n\n\n" + IMPORT + "\n"),
    (IMPORT, None),
    (f"import main  {HASH} noqa: E402,F401", AFTER_PATH),
    (f"import main  {HASH} noqa", AFTER_PATH),
    (f"main = load()  {HASH} noqa: E402", AFTER_PATH),
])
def test_any_other_marked_import_is_a_suppression(ic, line, source):
    path = "tests/v3-service/test_edit.py"
    tree = Files({path: source}) if source else None
    assert whats(ic, hunks(path, (4, [], 5, [line])), tree=tree) == ["new suppression marker"]


# --- workflow settings that turn a guard off ---

@pytest.mark.parametrize("line, setting", [
    ("        continue-on-error: true", "continue-on-error: true"),
    ("    continue-on-error: true  " + HASH + " the upload must not fail the job", "continue-on-error: true"),
    ("          persist-credentials: true", "persist-credentials: true"),
])
def test_a_workflow_setting_that_turns_a_guard_off_needs_approval(ic, line, setting):
    findings = ic.check(diff(".github/workflows/test.yml", added=[line]), TASKS)
    assert ("approval", f"new workflow setting: {setting}") in [(f.level, f.what) for f in findings]


@pytest.mark.parametrize("path, line", [
    (".github/workflows/test.yml", "          persist-credentials: false"),
    (".github/workflows/test.yml", "        continue-on-error: false"),
    (".github/workflows/test.yml", "          run: make lint || true"),
    ("docker-compose.yml", "    continue-on-error: true"),
])
def test_other_lines_are_not_such_a_setting(ic, path, line):
    assert not [w for w in whats(ic, diff(path, added=[line])) if w.startswith("new workflow setting")]


# --- the size baseline ---

def baseline(functions, files=None, limits=None):
    return json.dumps({"limits": limits or {"function_lines": 100, "file_lines": 1500},
                       "functions": functions, "files": files or {"proxy/agent.go": 9929}})


def baseline_findings(ic, before, after):
    path = ".github/code-health-baseline.json"
    tree = Files({path: after}, {path: before}) if before else None
    return [(f.level, f.what) for f in ic.check(diff(path, added=['"x": 1']), TASKS, tree)]


def test_a_size_baseline_that_only_goes_down_is_a_note(ic):
    before = baseline({"a.py:f": 300, "b.py:g": 120, "c.py:h": 101})
    assert baseline_findings(ic, before, baseline({"a.py:f": 250, "b.py:g": 120})) == \
        [("note", "size baseline lowered: 1 number(s) lower, 1 entry removed")]
    lower_limit = baseline({"a.py:f": 300}, limits={"function_lines": 90, "file_lines": 1500})
    assert baseline_findings(ic, baseline({"a.py:f": 300}), lower_limit) == \
        [("note", "size baseline lowered: 1 number(s) lower, 0 entries removed")]


@pytest.mark.parametrize("after", [
    {"functions": {"a.py:f": 301}},
    {"functions": {"a.py:f": 301, "b.py:g": 110}},
    {"functions": {"a.py:f": 300, "new.py:n": 150}},
    {"functions": {"a.py:f": 250, "new.py:n": 150}},
    {"functions": {"a.py:f": 300}, "files": {"proxy/agent.go": 9930}},
    {"functions": {"a.py:f": 300}, "limits": {"function_lines": 120, "file_lines": 1500}},
    {"functions": {"a.py:f": 300}, "limits": {"file_lines": 1500}},
    {"functions": {"b.py:g": 120, "a.py:f": 300}},
])
def test_any_other_change_to_the_size_baseline_needs_approval(ic, after):
    changed = baseline(after["functions"], after.get("files"), after.get("limits"))
    assert baseline_findings(ic, baseline({"a.py:f": 300, "b.py:g": 120}), changed) == \
        [("approval", "change to a file that configures the checks")]


def test_a_size_baseline_that_cannot_be_read_needs_approval(ic):
    assert baseline_findings(ic, None, None) == [("approval", "change to a file that configures the checks")]
    path = ".github/code-health-baseline.json"
    broken = Files({path: "{not json"}, {path: baseline({"a.py:f": 300})})
    assert whats(ic, diff(path, added=["x"]), tree=broken) == ["change to a file that configures the checks"]


# --- the lists of files that need approval ---

@pytest.mark.parametrize("path, what", [
    ("tests/replay/recordings/normal_edit.json", "change to a file that configures the checks"),
    (".github/canary.json", "change to a file that configures the checks"),
    ("scripts/verify.py", "change to a file that configures the checks"),
    ("scripts/dockerfile_lint.py", "change to a file that configures the checks"),
    ("scripts/setup/rulesets.sh", "change to a file that configures the checks"),
    (".hadolint.yaml", "change to a file that configures the checks"),
    ("scripts/bot/atlas_bot.py", "change to a script that runs with a credential that can write"),
    ("scripts/star-history-chart.py", "change to a script that runs with a credential that can write"),
])
def test_a_listed_file_needs_approval_for_its_own_reason(ic, path, what):
    findings = ic.check(diff(path, added=["x = 1"]), TASKS)
    assert [(f.level, f.what) for f in findings] == [("approval", what)]


@pytest.mark.parametrize("path", ["scripts/atlas-bootstrap.sh", "tests/replay/stage.py", "tests/replay/record.py"])
def test_a_script_under_test_and_the_replay_harness_are_ordinary_files(ic, path):
    assert whats(ic, diff(path, added=["x=1"])) == []


SCRIPT_NAME = re.compile(r"scripts/[A-Za-z0-9_./-]+\.(?:py|sh)")


def test_every_script_a_workflow_names_is_on_one_of_the_lists(ic):
    """A new script in a workflow fails here until someone decides what it is.

    What this sees: a script named in a workflow file, and a script named in
    a gate script (one level). A script that only another script calls from
    deeper down is not seen.
    """
    root = SCRIPT.parents[1]
    named = {name for workflow in sorted((root / ".github" / "workflows").glob("*.yml"))
             for name in SCRIPT_NAME.findall(workflow.read_text(encoding="utf-8"))}
    gate_scripts = [name for name in sorted(named) if name.startswith(ic.GATE_FILES) and (root / name).is_file()]
    named |= {name for script in gate_scripts for name in SCRIPT_NAME.findall((root / script).read_text(encoding="utf-8"))
              if (root / name).is_file()}
    undecided = sorted(name for name in named if not name.startswith(ic.GATE_FILES + ic.WRITE_CREDENTIAL_SCRIPTS)
                       and name not in ic.RUN_NOT_GATE)
    assert undecided == [], (
        f"{undecided} are named in a workflow or in a gate script and are on no list in scripts/integrity_check.py. "
        "Fix: add each to GATE_FILES when its result decides a check, to WRITE_CREDENTIAL_SCRIPTS when a workflow "
        "runs it with a credential that can write, or to RUN_NOT_GATE with its reason when it is only the thing "
        "under test in a job that cannot write.")
    assert len(named) >= 10


def test_every_script_on_the_third_list_says_why(ic):
    for name, reason in ic.RUN_NOT_GATE.items():
        assert (SCRIPT.parents[1] / name).is_file(), name
        assert len(reason.split()) >= 5, name


# The rows of the gates page's "Tool settings" table, and the linters each row is about.
SETTINGS_ROWS = {
    "Size check": (), "Go lint": ("golangci-lint",), "Extension lint": ("ESLint",), "Coverage": (),
    "Codecov": ("Codecov",), "SonarQube Cloud": ("SonarQube",), "CodeScene": ("CodeScene",),
    "Dockerfile lint": ("hadolint",), "Image scan": ("Trivy",), "Workflow lint": ("zizmor", "actionlint"),
    "Integrity check": (),
}


def test_every_tool_of_the_gates_page_has_its_markers_decided(ic):
    """A new row in the page's settings table fails here until its linters have a row in LINTER_MARKERS.

    What this sees: the first cell of each row of the "Tool settings" table. A
    linter that the page names only in running text is not seen.
    """
    page = (SCRIPT.parents[1] / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    table = page.split("## Tool settings", 1)[1].split("\n## ", 1)[0]
    rows = [line.split("|")[1].strip().split(",")[0] for line in table.splitlines()
            if line.startswith("| ") and not line.startswith(("| Tool", "|---"))]
    assert len(rows) >= 6
    for row in rows:
        assert row in SETTINGS_ROWS, (
            f"the gates page has a settings row {row!r} that this test does not know. Fix: add it to SETTINGS_ROWS "
            "with its linters, and give each linter a row in LINTER_MARKERS of scripts/integrity_check.py: the kinds "
            "of file its markers are read in, or \"none\".")
        for linter in SETTINGS_ROWS[row]:
            assert linter in ic.LINTER_MARKERS, f"{linter} (row {row!r}) has no row in LINTER_MARKERS"


def test_the_marker_table_and_the_linter_table_agree(ic):
    for linter, kinds in ic.LINTER_MARKERS.items():
        assert kinds == "none" or (kinds and set(kinds) <= set(ic.MARKERS)), linter
    assert {kind for kinds in ic.LINTER_MARKERS.values() if kinds != "none" for kind in kinds} == set(ic.MARKERS)

