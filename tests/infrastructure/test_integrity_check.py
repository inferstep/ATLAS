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

    def tracked(self):
        return sorted(self.now)


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
        ["new `pytestmark`: every test of this file stops running, with its reason: tree-sitter is not installed here"]


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


def test_a_renamed_test_that_loses_a_decorator_is_removed(ic):
    path = "tests/cli/test_doctor.py"
    cases = "@pytest.mark.parametrize('fault', FAULTS)"
    lost = diff(path, removed=[cases, "def test_doctor_reports(fault):"], added=["def test_doctor_names_a_fault():"])
    assert whats(ic, lost) == ["test removed: test_doctor_reports"]
    kept = diff(path, removed=[cases, "def test_doctor_reports(fault):"],
                added=[cases, "def test_doctor_names_each_fault(fault):"])
    assert whats(ic, kept) == ["test renamed: test_doctor_reports -> test_doctor_names_each_fault"]
    other = diff(path, removed=[cases, "def test_doctor_reports(fault):"],
                 added=["@pytest.mark.parametrize('fault', FAULTS[:1])", "def test_doctor_names_each_fault(fault):"])
    assert whats(ic, other) == ["test removed: test_doctor_reports"]


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
    ("extensions/vscode/src/sse.ts", "  /* v8 ignore next */"),
    ("extensions/vscode/src/sse.ts", "  /* istanbul ignore else */"),
    ("scripts/install.sh", f"{HASH} shellcheck disable=SC2086"),
    ("scripts/install.sh", f"curl \"$url\"  {HASH} NOSONAR"),
    (".github/workflows/test.yml", f"          persist-credentials: false  {HASH} zizmor: ignore[artipacked]"),
    (".github/workflows/test.yml", f"      {HASH} yamllint disable-line rule:line-length"),
    (".github/workflows/test.yml", f"        run: curl \"$URL\"  {HASH} NOSONAR"),
    ("sandbox/Dockerfile", f"{HASH} hadolint ignore=DL3008"),
    ("inference/Dockerfile.vulkan", f"{HASH} hadolint global ignore=DL3003"),
    ("sandbox/Dockerfile", f"RUN curl -fsSL \"$URL\" | sh  {HASH} NOSONAR"),
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
    ("scripts/change_base.py", "change to a file that configures the checks"),
    ("scripts/queue_run.py", "change to a file that configures the checks"),
    ("scripts/go_test_build.py", "change to a file that configures the checks"),
    (".github/actions/upload-coverage/action.yml", "change to a file that configures the checks"),
    ("scripts/fix_tests.py", "change to a file that configures the checks"),
    ("scripts/dockerfile_lint.py", "change to a file that configures the checks"),
    ("scripts/setup/rulesets.sh", "change to a file that configures the checks"),
    (".hadolint.yaml", "change to a file that configures the checks"),
    (".github/codeql/config.yml", "change to a file that configures the checks"),
    (".github/codeql-config.yml", "change to a file that configures the checks"),
    (".github/dependency-review-config.yml", "change to a file that configures the checks"),
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
    "Size check": (), "Go lint": ("golangci-lint",), "Extension lint": ("ESLint",), "Coverage": ("vitest coverage",),
    "Codecov": ("Codecov",), "SonarQube Cloud": ("SonarQube",), "CodeScene": ("CodeScene",),
    "Dockerfile lint": ("hadolint",), "Image scan": ("Trivy",), "Workflow lint": ("zizmor", "actionlint"),
    "Integrity check": (), "Hashed installs": (),
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


# --- the forms by which a test stops running, by runner ---

ONE = "new skip or todo marker in a test"
FILE = "every test of this file stops running"
CLASS = "every test of this class stops running"
GROUP = "every test of this group stops running"
NEEDS_YAML = ", with its reason: needs the module yaml"
IN_A_TEST = "1 new call(s) to `pytest.importorskip` in a test"
PY, GO, TS = "tests/cli/test_doctor.py", "proxy/gates_test.go", "extensions/vscode/test/sse.test.ts"
STOPPED = [
    (PY, "@pytest.mark.skip", ONE),
    (PY, "@pytest.mark.skipif(sys.platform == 'darwin')", ONE),
    (PY, "@pytest.mark.xfail", ONE),
    (PY, "    pytest.skip()", ONE),
    (PY, "    pytest.xfail()", ONE),
    (PY, "    pytest.param(2, marks=pytest.mark.skip),", ONE),
    (PY, "    pytest.param(2, marks=[pytest.mark.xfail]),", ONE),
    (PY, "    pytest.param(\"it's\", 'a \"b\"', marks=pytest.mark.skip),", ONE),
    (PY, "    yaml = pytest.importorskip('yaml')", IN_A_TEST + NEEDS_YAML),
    (PY, "pytestmark = pytest.mark.skipif(sys.platform == 'darwin')", f"new `pytestmark`: {FILE}"),
    (PY, "pytestmark = [pytest.mark.slow, pytest.mark.skip]", f"new `pytestmark`: {FILE}"),
    (PY, "yaml = pytest.importorskip('yaml')", f"new `pytest.importorskip`: {FILE}" + NEEDS_YAML),
    (PY, "pytest.importorskip('yaml')", f"new `pytest.importorskip`: {FILE}" + NEEDS_YAML),
    (PY, "needs_gpu = pytest.mark.skipif(not GPU)", "new skip marker with a name of its own: needs_gpu"),
    (PY, "skip_it = pytest.mark.skip", "new skip marker with a name of its own: skip_it"),
    (PY, "pytest.skip(allow_module_level=True)", f"new `pytest.skip`: {FILE}"),
    (PY, "__test__ = False", f"new `__test__`: {FILE}"),
    (PY, "    __test__ = False", f"new `__test__`: {CLASS}"),
    ("tests/conftest.py", "collect_ignore = ['test_slow.py']", "new `collect_ignore`: whole test files are left out"),
    ("tests/conftest.py", "collect_ignore_glob = ['*_slow.py']", "new `collect_ignore_glob`: whole test files are left out"),
    ("tests/conftest.py", "def pytest_ignore_collect(collection_path, config):",
     "new `pytest_ignore_collect`: whole test files are left out"),
    ("tests/conftest.py", "        item.add_marker(pytest.mark.skip)",
     "new `item.add_marker`: the tests the hook picks stop running"),
    ("geometric-lens/conftest.py", "collect_ignore = ['tests/test_gpu.py']",
     "new `collect_ignore`: whole test files are left out"),
    (PY, "@unittest.skip", ONE),
    (PY, "@unittest.skipIf(sys.platform == 'darwin', '')", ONE),
    (PY, "@unittest.skipUnless(HAS_GPU, '')", ONE),
    (PY, "@unittest.expectedFailure", ONE),
    (PY, "        self.skipTest()", ONE),
    (PY, "        raise unittest.SkipTest", ONE),
    (PY, "        raise SkipTest()", ONE),
    (PY, "raise unittest.SkipTest", f"new `raise unittest.SkipTest`: {FILE}"),
    (GO, "\tt.Skip()", ONE),
    (GO, "\tt.SkipNow()", ONE),
    (GO, "\ttb.Skip()", ONE),
    (GO, "//go:build ignore", f"new `//go:build ignore`: {FILE}"),
    (GO, "//go:build linux", f"new `//go:build linux`: {FILE}"),
    (GO, "// +build integration", f"new `// +build integration`: {FILE}"),
    (TS, "  it.skip('parses a frame', () => {", ONE),
    (TS, "  test.todo('parses a frame')", ONE),
    (TS, "  it.fails('parses a frame', () => {", ONE),
    (TS, "  it.concurrent.skip('parses a frame', () => {", ONE),
    (TS, "  it.skip.each([1, 2])('parses frame %i', () => {", ONE),
    (TS, "  it.skipIf(isCI)('parses a frame', () => {", ONE),
    (TS, "  it.runIf(isLinux)('parses a frame', () => {", ONE),
    (TS, "  xit('parses a frame', () => {", ONE),
    (TS, "describe.skip('frames', () => {", f"new `describe.skip`: {GROUP}"),
    (TS, "describe.todo('frames')", f"new `describe.todo`: {GROUP}"),
    (TS, "describe.skipIf(isCI)('frames', () => {", f"new `describe.skipIf`: {GROUP}"),
    (TS, "describe.runIf(isLinux)('frames', () => {", f"new `describe.runIf`: {GROUP}"),
    (TS, "xdescribe('frames', () => {", f"new `xdescribe`: {GROUP}"),
    (TS, "  it.only('parses a frame', () => {", "new `it.only`: every other test of this file stops running"),
    (TS, "describe.only('frames', () => {", "new `describe.only`: every other test of this file stops running"),
    (TS, "  test.concurrent.only('parses a frame', () => {",
     "new `test.concurrent.only`: every other test of this file stops running"),
]


@pytest.mark.parametrize("path, line, what", STOPPED)
def test_each_form_by_which_a_test_stops_running_is_named_with_how_far_it_reaches(ic, path, line, what):
    findings = ic.check(diff(path, added=[line]), TASKS)
    # `importorskip` names the module it needs, so it has a reason by itself; every other row here has none.
    level = "approval" if what.endswith(NEEDS_YAML) else "warning"
    assert [(f.level, f.what) for f in findings] == [(level, what)]


def test_every_row_of_the_table_of_forms_has_a_test(ic):
    def deciding_row(path, line):
        return next(pattern.pattern for pattern, _ in ic.STOPS[ic.file_kind(path)] if pattern.search(line))
    tried = {(ic.file_kind(path), deciding_row(path, line)) for path, line, _ in STOPPED}
    assert tried == {(kind, pattern.pattern) for kind, rows in ic.STOPS.items() for pattern, _ in rows}


@pytest.mark.parametrize("path, line", [
    (PY, "pytestmark = pytest.mark.slow"),
    (PY, "pytestmark = [pytest.mark.usefixtures('proxy')]"),
    (PY, "slow = pytest.mark.slow"),
    (PY, "    pytest.param(2, marks=pytest.mark.slow),"),
    (PY, "    item.add_marker(pytest.mark.slow)"),
    (PY, "    __test__ = True"),
    (PY, "    (PY, \"    pytest.param(2, marks=pytest.mark.skip),\", ONE),"),
    (PY, "    ('tests/conftest.py', '        item.add_marker(pytest.mark.skip)'),"),
    (PY, "    result = pytest.skipped"),
    (PY, "    ('proxy/gates_test.go', '\tt.Skip()'),"),
    (PY, "    line = 'it.only(\"x\")'"),
    (PY, "//go:build ignore"),
    (GO, "\tline := \"@pytest.mark.skip\""),
    (GO, "\t// go:build is written without a space"),
    (GO, "\tfmt.Println(\"//go:build ignore\")"),
    (TS, "  const marker = 'pytest.skip()';"),
    (TS, "  it('skips only the first frame', () => {"),
    (TS, "  it.each([1, 2])('parses frame %i', () => {"),
    (TS, "  it.concurrent('parses a frame', () => {"),
    (TS, "describe('frames that fail', () => {"),
    ("proxy/gates.go", "//go:build linux"),
    ("atlas/cli.py", "collect_ignore = []"),
    ("docs/SETUP.md", "@pytest.mark.skip"),
])
def test_the_same_words_where_they_stop_no_test_are_not_a_finding(ic, path, line):
    assert not [w for w in whats(ic, diff(path, added=[line])) if "skip" in w or "stops running" in w or "left out" in w]


@pytest.mark.parametrize("path, lines, reason", [
    (PY, [f"{HASH} The suite needs a GPU.", "pytestmark = pytest.mark.skipif(not GPU)"], "The suite needs a GPU."),
    (PY, ["yaml = pytest.importorskip('yaml', reason='the contract needs PyYAML')"], "'the contract needs PyYAML'"),
    (PY, ["        self.skipTest('needs a terminal')"], "needs a terminal"),
    (PY, ["        raise unittest.SkipTest('needs a terminal')"], "needs a terminal"),
    (PY, ["pytest.skip('the suite needs a GPU', allow_module_level=True)"], "the suite needs a GPU"),
    (PY, ["pytest.skip(allow_module_level=True, reason='the suite needs a GPU')"], "the suite needs a GPU"),
    ("tests/conftest.py", [f"{HASH} These files need the model server.", "collect_ignore = ['test_live.py']"],
     "These files need the model server."),
    (GO, [f"{SLASHES} The object hold exists on Linux only.", "//go:build linux"], "The object hold exists on Linux only."),
    (TS, [f"  {SLASHES} One test while the parser is rewritten.", "  it.only('parses a frame', () => {"],
     "One test while the parser is rewritten."),
])
def test_a_form_with_its_reason_asks_for_approval_and_quotes_it(ic, path, lines, reason):
    findings = ic.check(diff(path, added=lines), TASKS)
    assert [f.level for f in findings] == ["approval"]
    assert findings[0].what.endswith(f", with its reason: {reason.strip(chr(39))}")


MARK_FILE = "import pytest\n\nneeds_proc = pytest.mark.skipif(not PROC, reason='the sandbox reads /proc')\nbare = pytest.mark.skip\n"
TAKES_BOTH = "from tests.infrastructure.proc_files import bare, needs_proc\n"


def test_the_uses_of_a_named_skip_mark_are_one_finding_with_their_number_and_the_reason(ic):
    path = "tests/infrastructure/test_http_cancellation.py"
    source = "from tests.infrastructure.proc_files import needs_proc\n"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: source})
    change = diff(path, added=["@needs_proc", "def test_a():", "@needs_proc", "def test_b():",
                               "    pytest.param(1, marks=needs_proc),"])
    findings = ic.check(change, TASKS, tree)
    assert [(f.level, f.line, f.what) for f in findings] == [
        ("approval", 10, "3 new use(s) of the skip marker needs_proc, with its reason: the sandbox reads /proc")]


def test_a_named_skip_mark_from_an_imported_module_in_another_folder_is_found(ic):
    path = "tests/cli/test_doctor.py"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE,
                  path: "from tests.infrastructure.proc_files import needs_proc\n"})
    assert whats(ic, diff(path, added=["@needs_proc"]), tree=tree) == [
        "1 new use(s) of the skip marker needs_proc, with its reason: the sandbox reads /proc"]


def test_a_named_skip_mark_with_no_reason_needs_action_and_one_on_the_whole_file_says_so(ic):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: TAKES_BOTH})
    findings = ic.check(diff(path, added=["@bare", "pytestmark = needs_proc"]), TASKS, tree)
    assert [(f.level, f.what) for f in findings] == [
        ("warning", "1 new use(s) of the skip marker bare"),
        ("approval", ("new `pytestmark` with the skip marker needs_proc: every test of this file stops running, "
                      "with its reason: the sandbox reads /proc"))]


def test_a_comment_above_importorskip_is_about_the_test_and_is_not_its_reason(ic):
    lines = ["def test_a_file_of_two_documents_parses(tmp_path):", f"    {HASH} safe_load rejects such a file.",
             "    pytest.importorskip('yaml.nodes')"]
    assert whats(ic, diff(PY, added=lines)) == [IN_A_TEST + ", with its reason: needs the module yaml.nodes"]


def test_the_calls_to_importorskip_in_the_tests_of_a_file_are_one_finding_for_each_module(ic):
    lines = ["def test_a():", "    torch = pytest.importorskip('torch')", "def test_b():", "    pytest.importorskip('torch')",
             "def test_c():", "    pytest.importorskip('yaml')", "def test_d():",
             "    pytest.importorskip('torch', reason='the lens needs torch')"]
    findings = ic.check(diff(PY, added=lines), TASKS)
    assert [(f.level, f.line, f.what) for f in findings] == [
        ("approval", 11, "2 new call(s) to `pytest.importorskip` in a test, with its reason: needs the module torch"),
        ("approval", 15, "1 new call(s) to `pytest.importorskip` in a test, with its reason: needs the module yaml"),
        ("approval", 17, "1 new call(s) to `pytest.importorskip` in a test, with its reason: the lens needs torch")]


HELPER_FILE = """import pytest


def _run(cwd):
    return subprocess.run(
        ['bash'], cwd=cwd,
    )


needs_bash = pytest.mark.skipif(shutil.which('bash') is None, reason='the tests need bash')


@pytest.mark.skip(reason='not yet')
def test_old():
    _run('.')


def _needs_docker():
    if not DOCKER:
        pytest.skip('needs docker')


def _marks():
    slow = pytest.mark.skip
    return [slow]
"""


def test_a_skip_below_a_function_is_not_in_that_function(ic):
    path = "tests/cli/test_x.py"
    tree = Files({path: HELPER_FILE})
    assert ic.skip_helpers(tree, path) == {"_needs_docker": "needs docker"}
    assert whats(ic, diff(path, added=["    _run(tmp_path)", "    _needs_docker()", "    _marks()"]), tree=tree) == [
        "1 new call(s) to the skip helper _needs_docker, with its reason: needs docker"]


TEXT_FILE = '''import pytest

CASE = """
@pytest.mark.skip
def test_inside_a_string():
    pytest.skip()
@needs_proc
pytestmark = pytest.mark.skip
"""


@pytest.mark.skip
def test_real():
    pass
'''


def test_a_form_inside_a_string_of_more_than_one_line_is_text(ic):
    path = "tests/infrastructure/test_x.py"
    change = hunks(path, (0, [], 1, TEXT_FILE.splitlines()))
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: TEXT_FILE})
    assert [(f.line, f.what) for f in ic.check(change, TASKS, tree)] == [(12, ONE)]


def test_when_the_file_cannot_be_read_as_python_every_line_counts_as_code(ic):
    path = "tests/infrastructure/test_x.py"
    broken = TEXT_FILE + "def test_broken(:\n    x = (\n"
    change = hunks(path, (0, [], 1, broken.splitlines()))
    stops = [f.line for f in ic.check(change, TASKS, Files({path: broken})) if "skip" in f.what or "stops" in f.what]
    assert stops == [4, 6, 8, 12]
    assert ic.text_lines(path, None) == set()
    assert ic.text_lines("proxy/gates_test.go", "x := `\nt.Skip()\n`\n") == set()


def test_a_keyword_of_a_skip_call_is_not_its_reason(ic):
    findings = ic.check(diff(PY, added=["pytest.skip(allow_module_level=True)"]), TASKS)
    assert [(f.level, f.what) for f in findings] == [("warning", f"new `pytest.skip`: {FILE}")]


CLASS_FILE = """from tests.infrastructure.proc_files import needs_proc

@pytest.mark.skipif(not GPU,
                    reason='the class needs a GPU')
@pytest.mark.usefixtures('proxy')
class TestOnTheGpu:
    @pytest.mark.skip
    def test_a(self):
        pass

@unittest.skip
class TestOld(unittest.TestCase):
    pass

@needs_proc
class TestProc:
    @needs_proc
    def test_b(self):
        pass
"""


def test_a_skip_on_a_class_says_that_it_reaches_every_test_of_the_class(ic):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: CLASS_FILE})
    findings = ic.check(hunks(path, (0, [], 1, CLASS_FILE.splitlines())), TASKS, tree)
    assert [(f.line, f.level, f.what) for f in findings] == [
        (3, "approval", f"new `@pytest.mark.skipif`: {CLASS}, with its reason: the class needs a GPU"),
        (7, "warning", ONE),
        (11, "warning", f"new `@unittest.skip`: {CLASS}"),
        (15, "approval", "2 new use(s) of the skip marker needs_proc, 1 of them on a whole class, "
                         "with its reason: the sandbox reads /proc")]


def test_a_decorator_that_is_no_skip_mark_is_not_a_use(ic):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: TAKES_BOTH})
    assert whats(ic, diff(path, added=["@needs_procedure", "@pytest.fixture", "@other", "@needs_proc.with_args"]),
                 tree=tree) == []


SAMPLE_FILE = '''import pytest

SAMPLE = """
slow = pytest.mark.skip
"""


def helper():
    quick = pytest.mark.skip
    return quick
'''


def test_a_name_in_a_text_or_in_a_function_of_another_file_is_not_a_skip_marker(ic):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/test_samples.py": SAMPLE_FILE, path: "import pytest\n"})
    lines = ["pytestmark = pytest.mark.slow", "pytestmark = [pytest.mark.slow, pytest.mark.quick]", "@pytest.mark.slow",
             "@slow", "@quick"]
    assert whats(ic, diff(path, added=lines), tree=tree) == []
    assert ic.skip_marks_of(SAMPLE_FILE, "tests/infrastructure/test_samples.py") == {}


def test_the_same_name_in_a_file_that_is_not_imported_is_another_name(ic):
    path = "tests/infrastructure/test_x.py"
    beside = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: "import pytest\n"})
    assert whats(ic, diff(path, added=["@bare", "pytestmark = needs_proc"]), tree=beside) == []
    assert ic.named_marks(beside, path) == {}


def test_a_pytest_mark_of_the_same_name_is_not_a_use_of_a_name_of_the_file(ic):
    path = "tests/infrastructure/test_x.py"
    own = "import pytest\n\nslow = pytest.mark.skip(reason='takes an hour')\n"
    tree = Files({path: own})
    assert whats(ic, diff(path, added=["@pytest.mark.slow", "pytestmark = pytest.mark.slow",
                                       "pytestmark = [pytest.mark.slow]", "    pytest.param(1, marks=pytest.mark.slow),"]),
                 tree=tree) == []
    assert whats(ic, diff(path, added=["@slow", "    pytest.param(1, marks=slow),"]), tree=tree) == [
        "2 new use(s) of the skip marker slow, with its reason: takes an hour"]
    for line in ("pytestmark = slow", "pytestmark = [pytest.mark.usefixtures('proxy'), slow]"):
        assert whats(ic, diff(path, added=[line]), tree=tree) == [
            "new `pytestmark` with the skip marker slow: every test of this file stops running, with its reason: takes an hour"]


@pytest.mark.parametrize("taken, use", [
    ("from tests.infrastructure.proc_files import needs_proc as np", "@np"),
    ("from .proc_files import needs_proc", "@needs_proc"),
    ("from . import proc_files", "@proc_files.needs_proc"),
    ("import tests.infrastructure.proc_files as pf", "@pf.needs_proc"),
    ("import tests.infrastructure.proc_files", "@tests.infrastructure.proc_files.needs_proc"),
])
def test_a_named_skip_mark_is_followed_through_each_form_of_import(ic, taken, use):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/proc_files.py": MARK_FILE, path: taken + "\n"})
    name = use.lstrip("@")
    assert whats(ic, diff(path, added=[use]), tree=tree) == [
        f"1 new use(s) of the skip marker {name}, with its reason: the sandbox reads /proc"]


def test_a_named_skip_mark_in_a_file_that_cannot_be_read_as_python_still_counts(ic):
    path = "tests/infrastructure/test_x.py"
    broken = MARK_FILE + "    inside = pytest.mark.skip\ndef broken(:\n"
    tree = Files({"tests/infrastructure/proc_files.py": broken, path: TAKES_BOTH})
    assert whats(ic, diff(path, added=["@bare"]), tree=tree) == ["1 new use(s) of the skip marker bare"]
    assert sorted(ic.skip_marks_of(broken, "tests/infrastructure/proc_files.py")) == ["bare", "needs_proc"]


REASON_FILE = ('import pytest\n\nNEEDS_PROC_REASON = ("this system has no /proc, and the sandbox reads its limits "\n'
               '                     "from there")\n'
               "needs_proc = pytest.mark.skipif(not PROC, reason=NEEDS_PROC_REASON)\n"
               "elsewhere = pytest.mark.skipif(not PROC, reason=REASON_FROM_ANOTHER_FILE)\n")


def test_a_reason_given_by_a_name_shows_the_text_of_that_name(ic):
    path = "tests/infrastructure/test_x.py"
    tree = Files({"tests/infrastructure/proc_files.py": REASON_FILE,
                  path: "from tests.infrastructure.proc_files import elsewhere, needs_proc\n"})
    with_the_text = ("1 new use(s) of the skip marker needs_proc, with its reason: this system has no /proc, and the "
                     "sandbox reads its limits from there")
    assert whats(ic, diff(path, added=["@needs_proc", "@elsewhere"]), tree=tree) == [
        "1 new use(s) of the skip marker elsewhere, with its reason: REASON_FROM_ANOTHER_FILE", with_the_text]


def test_a_reason_by_name_on_the_skip_itself_shows_the_text_and_a_quoted_reason_stays_as_written(ic):
    path = "tests/infrastructure/test_x.py"
    source = "import pytest\n\nWHY = 'needs a GPU'\n\n\n@pytest.mark.skipif(not GPU, reason=WHY)\ndef test_a():\n    pass\n"
    assert whats(ic, hunks(path, (0, [], 6, ["@pytest.mark.skipif(not GPU, reason=WHY)"])), tree=Files({path: source})) == [
        ONE + ", with its reason: needs a GPU"]
    quoted = source.replace("reason=WHY", "reason='WHY'")
    assert whats(ic, hunks(path, (0, [], 6, ["@pytest.mark.skipif(not GPU, reason='WHY')"])), tree=Files({path: quoted})) == [
        ONE + ", with its reason: WHY"]
    assert whats(ic, diff(path, added=["@pytest.mark.skipif(not GPU, reason=WHY)"])) == [
        ONE + ", with its reason: WHY"]



# --- tests that leave the plain test jobs through a mark the runner's settings leave out ---

SETTINGS = '[tool.pytest.ini_options]\ntestpaths = ["tests"]\naddopts = "-m \'not integration\'"\n'
LEFT = "tests/cli/test_doctor.py"
TWO_TESTS = "import pytest\n\n\ndef test_a():\n    pass\n\n\ndef test_b():\n    pass\n"


def left(ic, path, now, before=None, status="modified", more_now=None, more_before=None, more_diff=""):
    """The findings about left-out tests for one changed file, given as it is now and as it was at the base."""
    files_now = {"pyproject.toml": SETTINGS, path: now, **(more_now or {})}
    files_before = {"pyproject.toml": SETTINGS, **({path: before} if before is not None else {}), **(more_before or {})}
    change = diff(path, added=["x"], status=status) + more_diff
    return [(f.level, f.line, f.what) for f in ic.check(change, TASKS, Files(files_now, files_before))
            if "plain test jobs" in f.what]


@pytest.mark.parametrize("line, marks", [
    ("addopts = \"-m 'not integration'\"", ("integration",)),
    ("addopts = '-m \"not slow and not integration\"'", ("slow", "integration")),
    ("addopts = [\"-q\", \"-m\", \"not integration\"]", ("integration",)),
    ("addopts = \"-q --strict-markers\"", ()),
    ("addopts = \"-m 'integration'\"", ()),
    ("testpaths = [\"tests\"]", ()),
])
def test_the_marks_a_plain_run_leaves_out_are_read_from_the_runners_settings(ic, line, marks):
    assert ic.left_out_marks(f"[tool.pytest.ini_options]\n{line}\n") == marks
    assert ic.left_out_marks(None) == ()


def test_a_test_that_was_there_and_gets_the_mark_leaves_the_plain_jobs(ic):
    now = TWO_TESTS.replace("def test_a", "@pytest.mark.integration\ndef test_a")
    assert left(ic, LEFT, now, TWO_TESTS) == [
        ("warning", 4, "1 test(s) leave the plain test jobs through the mark `integration`: test_a")]


def test_a_comment_beside_the_mark_is_its_reason_and_asks_for_approval(ic):
    now = TWO_TESTS.replace("def test_a", f"{HASH} Needs a running sandbox; the e2e job runs it.\n"
                                          "@pytest.mark.integration\ndef test_a")
    assert left(ic, LEFT, now, TWO_TESTS) == [
        ("approval", 5, "1 test(s) leave the plain test jobs through the mark `integration`: test_a, with its reason: "
                        "Needs a running sandbox; the e2e job runs it.")]


def test_the_tests_that_leave_are_one_finding_for_each_reason_with_their_names(ic):
    now = (TWO_TESTS.replace("def test_a", "@pytest.mark.integration\ndef test_a")
           .replace("def test_b", "@pytest.mark.integration\ndef test_b"))
    assert left(ic, LEFT, now, TWO_TESTS) == [
        ("warning", 4, "2 test(s) leave the plain test jobs through the mark `integration`: test_a, test_b")]


def test_a_mark_on_a_class_reaches_every_test_of_the_class(ic):
    before = "import pytest\n\n\nclass TestLive:\n    def test_a(self):\n        pass\n\n    def test_b(self):\n        pass\n"
    now = before.replace("class TestLive", "@pytest.mark.integration\nclass TestLive")
    assert left(ic, LEFT, now, before) == [
        ("warning", 4, "2 test(s) leave the plain test jobs through the mark `integration`: TestLive::test_a, "
                       "TestLive::test_b")]


def test_a_test_that_is_new_with_the_mark_is_listed_for_information(ic):
    now = TWO_TESTS + "\n\n@pytest.mark.integration\ndef test_c():\n    pass\n"
    assert left(ic, LEFT, now, TWO_TESTS) == [
        ("note", 12, "1 new test(s) with the mark `integration`: they do not run in the plain test jobs")]


def test_a_pytestmark_with_the_mark_takes_the_whole_file_out(ic):
    now = TWO_TESTS.replace("import pytest\n", "import pytest\n\npytestmark = [pytest.mark.integration]\n")
    assert left(ic, LEFT, now, TWO_TESTS) == [
        ("warning", 3, "new `pytestmark` with the mark `integration`: every test of this file leaves the plain test jobs")]
    assert left(ic, LEFT, now, status="added") == [
        ("note", 3, "new test file with the mark `integration` on every test: they do not run in the plain test jobs")]


def test_a_file_that_was_out_already_is_not_named_again(ic):
    before = TWO_TESTS.replace("import pytest\n", "import pytest\n\npytestmark = pytest.mark.integration\n")
    now = before.replace("def test_a", "@pytest.mark.integration\ndef test_a") + "\n\ndef test_c():\n    pass\n"
    assert left(ic, LEFT, now, before) == []
    kept = TWO_TESTS.replace("def test_a", "@pytest.mark.integration\ndef test_a")
    assert left(ic, LEFT, kept + "\n\ndef test_c():\n    pass\n", kept) == []


def test_a_mark_that_the_settings_do_not_leave_out_is_not_named(ic):
    now = TWO_TESTS.replace("def test_a", "@pytest.mark.slow\ndef test_a")
    assert left(ic, LEFT, now, TWO_TESTS) == []
    marked = TWO_TESTS.replace("def test_a", "@pytest.mark.integration\ndef test_a")
    no_settings = Files({LEFT: marked}, {LEFT: TWO_TESTS})
    assert [f.what for f in ic.check(diff(LEFT, added=["x"]), TASKS, no_settings) if "plain test jobs" in f.what] == []


def test_with_no_copy_of_the_base_every_marked_test_counts_as_one_that_was_there(ic):
    now = TWO_TESTS.replace("def test_a", "@pytest.mark.integration\ndef test_a")
    assert left(ic, LEFT, now) == [
        ("warning", 4, "1 test(s) leave the plain test jobs through the mark `integration`: test_a")]


HOOK = '''import pytest


def pytest_collection_modifyitems(config, items):
    """Separate the tests that need a running service."""
    for item in items:
        path = str(item.fspath).replace("\\\\", "/")
        live = path.endswith((
            "/tests/infrastructure/test_llm.py",
        ))
        if "/tests/integration/" in path or live:
            item.add_marker(pytest.mark.integration)
'''
CONFTEST = "tests/conftest.py"


def test_the_paths_a_hook_gives_the_mark_to_are_read_from_the_hook(ic):
    assert ic.hook_targets(HOOK, ("integration",)) == {
        "/tests/infrastructure/test_llm.py": ("integration", 9), "/tests/integration/": ("integration", 11)}
    assert ic.hook_targets(HOOK, ("slow",)) == {}
    assert ic.hook_targets(HOOK.replace("item.add_marker(pytest.mark.integration)", "pass"), ("integration",)) == {}
    assert ic.picked("tests/integration/test_x.py", "/tests/integration/")
    assert ic.picked("tests/infrastructure/test_llm.py", "/tests/infrastructure/test_llm.py")
    assert not ic.picked("tests/infrastructure/test_llm_client.py", "/tests/infrastructure/test_llm.py")
    assert not ic.picked("tests/cli/test_x.py", "/tests/integration/")


def test_a_file_added_to_the_hooks_list_leaves_the_plain_jobs(ic):
    now = HOOK.replace('            "/tests/infrastructure/test_llm.py",\n',
                       '            "/tests/infrastructure/test_llm.py",\n            "/tests/cli/test_doctor.py",\n')
    assert left(ic, CONFTEST, now, HOOK) == [
        ("warning", 10, "the hook gives the mark `integration` to `/tests/cli/test_doctor.py`: the tests there leave "
                        "the plain test jobs")]
    reasoned = now.replace('            "/tests/cli/test_doctor.py",', '            "/tests/cli/test_doctor.py",  # needs a GPU')
    assert left(ic, CONFTEST, reasoned, HOOK) == [
        ("approval", 10, "the hook gives the mark `integration` to `/tests/cli/test_doctor.py`: the tests there leave "
                         "the plain test jobs, with its reason: needs a GPU")]


def test_a_new_file_on_the_hooks_list_is_listed_for_information(ic):
    now = HOOK.replace('            "/tests/infrastructure/test_llm.py",\n',
                       '            "/tests/infrastructure/test_llm.py",\n            "/tests/cli/test_live.py",\n')
    born = diff("tests/cli/test_live.py", added=["def test_a():"], status="added")
    assert left(ic, CONFTEST, now, HOOK, more_now={"tests/cli/test_live.py": "def test_a():\n    pass\n"}, more_diff=born) == [
        ("note", 10, "the hook gives the mark `integration` to the new file `/tests/cli/test_live.py`: its tests do not "
                     "run in the plain test jobs")]


def test_a_hook_that_did_not_change_names_nothing(ic):
    assert left(ic, CONFTEST, HOOK + "\n\ndef helper():\n    return '/tmp/x'\n", HOOK) == []


def test_a_test_file_moved_under_a_folder_the_hook_names_leaves_the_plain_jobs(ic):
    gone = diff("tests/cli/test_doctor.py", removed=["def test_a():"], status="deleted")
    files = {CONFTEST: HOOK}
    found = left(ic, "tests/integration/test_doctor.py", TWO_TESTS, status="added", more_now=files, more_before=files,
                 more_diff=gone)
    assert found == [("warning", 0, "test file moved from tests/cli/test_doctor.py to where the hook of "
                                    "tests/conftest.py gives the mark `integration`: its tests leave the plain test jobs")]
    assert left(ic, "tests/integration/test_new.py", TWO_TESTS, status="added", more_now=files, more_before=files) == [
        ("note", 0, "new test file where the hook of tests/conftest.py gives the mark `integration`: its tests do not "
                    "run in the plain test jobs")]
    assert left(ic, "tests/cli/test_new.py", TWO_TESTS, status="added", more_now=files, more_before=files) == []


def test_the_check_reads_this_repositorys_own_settings_and_hook(ic):
    root = SCRIPT.parents[1]
    marks = ic.left_out_marks((root / "pyproject.toml").read_text(encoding="utf-8"))
    assert marks == ("integration",)
    targets = ic.hook_targets((root / "tests" / "conftest.py").read_text(encoding="utf-8"), marks)
    assert "/tests/integration/" in targets
    files = sorted(target for target in targets if target.endswith(".py"))
    assert files and all((root / target.lstrip("/")).is_file() for target in files)
    # The gates page lists each of these files, so that the page and the hook say the same.
    page = (root / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    assert [target for target in files if f"`{target.lstrip('/')}`" not in page] == []


NAMED = "import pytest\n\nlive = pytest.mark.integration\n\n\ndef test_a():\n    pass\n\n\ndef test_b():\n    pass\n"
ONE_LEAVES = "1 test(s) leave the plain test jobs through the mark `integration`: test_a"


def test_a_name_that_stands_for_the_mark_takes_a_test_out_as_the_mark_does(ic):
    assert left(ic, LEFT, NAMED.replace("def test_a", "@live\ndef test_a"), NAMED) == [("warning", 6, ONE_LEAVES)]
    assert left(ic, LEFT, NAMED.replace("\n\n\ndef test_a", "\npytestmark = [live]\n\n\ndef test_a"), NAMED) == [
        ("warning", 4, "new `pytestmark` with the mark `integration`: every test of this file leaves the plain test jobs")]
    in_a_class = "import pytest\n\nlive = pytest.mark.integration\n\n\nclass TestLive:\n    def test_a(self):\n        pass\n"
    assert left(ic, LEFT, in_a_class.replace("class TestLive", "@live\nclass TestLive"), in_a_class) == [
        ("warning", 6, ONE_LEAVES.replace("test_a", "TestLive::test_a"))]


@pytest.mark.parametrize("taken, use", [
    ("from tests.marks import live", "@live"),
    ("from tests.marks import live as needs_a_stack", "@needs_a_stack"),
    ("import tests.marks as marks", "@marks.live"),
])
def test_a_name_for_the_mark_is_followed_through_an_import(ic, taken, use):
    before = taken + "\n\n\ndef test_a():\n    pass\n"
    files = {"tests/marks.py": "import pytest\n\nlive = pytest.mark.integration\n"}
    found = left(ic, LEFT, before.replace("def test_a", use + "\ndef test_a"), before, more_now=files, more_before=files)
    assert found == [("warning", 4, ONE_LEAVES)]


def test_a_name_that_does_not_stand_for_the_mark_takes_no_test_out(ic):
    in_a_function = NAMED.replace("live = pytest.mark.integration\n", "def marks():\n    live = pytest.mark.integration\n")
    assert left(ic, LEFT, in_a_function.replace("def test_a", "@live\ndef test_a"), in_a_function) == []
    assert left(ic, LEFT, NAMED.replace("def test_a", "@pytest.mark.live\ndef test_a"), NAMED) == []
    other = NAMED.replace("live = pytest.mark.integration", "live = pytest.mark.usefixtures('proxy')")
    assert left(ic, LEFT, other.replace("def test_a", "@live\ndef test_a"), other) == []
    not_imported = {"tests/marks.py": "import pytest\n\nlive = pytest.mark.integration\n"}
    assert left(ic, LEFT, TWO_TESTS.replace("def test_a", "@live\ndef test_a"), TWO_TESTS, more_now=not_imported,
                more_before=not_imported) == []


def test_the_words_of_a_mark_in_a_text_of_a_decorator_are_not_the_mark(ic):
    cases = ('@pytest.mark.parametrize("line", ["@pytest.mark.integration", "pytestmark = pytest.mark.integration"])\n'
             "def test_a(line):\n    pass\n")
    before = "import pytest\n\nSAMPLE = 'live = pytest.mark.integration'\n\n\ndef test_a(line):\n    pass\n"
    now = before.replace("def test_a(line):\n    pass\n", cases)
    assert left(ic, LEFT, now, before) == []
    assert ic.tests_and_marks(now, ("integration",)) == ({"test_a"}, {})
    assert ic.mark_names_of(("integration",))(now, LEFT) == {}
    one_case = now.replace('"pytestmark = pytest.mark.integration"]',
                           'pytest.param("x", marks=pytest.mark.integration)]')
    assert ic.tests_and_marks(one_case, ("integration",))[1] == {"test_a": ("integration", 6)}


def test_a_hook_that_gives_the_mark_by_a_name_is_read_too(ic):
    named = HOOK.replace("import pytest\n", "import pytest\n\nLIVE = pytest.mark.integration\n").replace(
        "item.add_marker(pytest.mark.integration)", "item.add_marker(LIVE)")
    assert sorted(ic.hook_targets(named, ("integration",))) == ["/tests/infrastructure/test_llm.py", "/tests/integration/"]
    assert ic.hook_targets(named.replace("item.add_marker(LIVE)", "item.add_marker(OTHER)"), ("integration",)) == {}


# --- the runner's settings leave out one more mark ---

MORE = SETTINGS.replace("not integration", "not integration and not slow")
SLOW = {
    "tests/cli/test_a.py": TWO_TESTS.replace("def test_a", "@pytest.mark.slow\ndef test_a"),
    # test_a carries the mark twice here: on itself and by its file.
    "tests/cli/test_b.py": TWO_TESTS.replace("import pytest\n", "import pytest\n\npytestmark = pytest.mark.slow\n").replace(
        "def test_a", "@pytest.mark.slow\ndef test_a"),
    "tests/cli/test_c.py": TWO_TESTS,
    "tests/cli/test_d.py": "from tests.marks import takes_long\n\n\n@takes_long\nclass TestLong:\n    def test_a(self):\n"
                           "        pass\n\n    def test_b(self):\n        pass\n\n\ndef test_c():\n    pass\n",
    "tests/marks.py": "import pytest\n\ntakes_long = pytest.mark.slow\n",
    "proxy/agent.py": "import pytest\n\n\n@pytest.mark.slow\ndef test_not_in_a_test_file():\n    pass\n",
}
SLOW_LEAVE = ("the runner's settings leave out one more mark, `slow`: 5 test(s) in 3 file(s) leave the plain test jobs "
              "(tests/cli/test_a.py, tests/cli/test_b.py, tests/cli/test_d.py)")


def settings_change(ic, now, before, files, tree=Files):
    change = diff("pyproject.toml", added=["x"])
    found = ic.check(change, TASKS, tree({"pyproject.toml": now, **files}, {} if before is None else {"pyproject.toml": before}))
    return [(f.level, f.line, f.what) for f in found if "plain test jobs" in f.what]


def test_a_mark_that_the_settings_newly_leave_out_is_named_with_the_tests_that_leave(ic):
    assert settings_change(ic, MORE, SETTINGS, SLOW) == [("warning", 3, SLOW_LEAVE)]
    assert [f.fix for f in ic.check(diff("pyproject.toml", added=["x"]), TASKS, Files({"pyproject.toml": MORE, **SLOW},
                                                                                 {"pyproject.toml": SETTINGS}))
            if "one more mark" in f.what] == [ic.SETTINGS_FIX]


def test_a_comment_beside_the_settings_line_is_the_reason_and_asks_for_approval(ic):
    beside = MORE.replace("not slow'\"", "not slow'\"  # the nightly job runs the slow tests")
    assert settings_change(ic, beside, SETTINGS, SLOW) == [
        ("approval", 3, SLOW_LEAVE + ", with its reason: the nightly job runs the slow tests")]
    above = MORE.replace("addopts", f"{HASH} The nightly job runs the slow tests.\naddopts")
    assert settings_change(ic, above, SETTINGS, SLOW) == [
        ("approval", 4, SLOW_LEAVE + ", with its reason: The nightly job runs the slow tests.")]


def test_a_test_that_an_older_mark_had_taken_out_already_is_not_counted_again(ic):
    hook = HOOK.replace('"/tests/infrastructure/test_llm.py",', '"/tests/cli/test_b.py",')
    out_by_its_own_mark = SLOW["tests/cli/test_a.py"].replace("@pytest.mark.slow", "@pytest.mark.slow\n@pytest.mark.integration")
    files = {**SLOW, CONFTEST: hook, "tests/cli/test_a.py": out_by_its_own_mark}
    assert settings_change(ic, MORE, SETTINGS, files) == [
        ("warning", 3, "the runner's settings leave out one more mark, `slow`: 2 test(s) in 1 file(s) leave the plain "
                       "test jobs (tests/cli/test_d.py)")]


def test_a_file_that_a_hook_gives_the_new_mark_to_counts_with_all_its_tests(ic):
    hook = HOOK.replace("pytest.mark.integration", "pytest.mark.slow").replace('"/tests/infrastructure/test_llm.py",',
                                                                             '"/tests/cli/test_c.py",')
    files = {CONFTEST: hook, "tests/cli/test_c.py": TWO_TESTS, "tests/integration/test_e.py": "def test_e():\n    pass\n",
             "tests/cli/test_f.py": TWO_TESTS}
    assert settings_change(ic, MORE, SETTINGS, files) == [
        ("warning", 3, "the runner's settings leave out one more mark, `slow`: 3 test(s) in 2 file(s) leave the plain "
                       "test jobs (tests/cli/test_c.py, tests/integration/test_e.py)")]


def test_a_mark_that_comes_back_and_a_change_that_leaves_the_marks_as_they_were_give_nothing(ic):
    assert settings_change(ic, SETTINGS, MORE, SLOW) == []
    assert settings_change(ic, MORE, MORE, SLOW) == []
    assert settings_change(ic, SETTINGS.replace("tests\"]", "tests\", \"benchmarks\"]"), SETTINGS, SLOW) == []
    reordered = SETTINGS.replace("not integration", "not slow and not integration")
    assert settings_change(ic, reordered, MORE, SLOW) == []


def test_a_new_mark_that_no_test_carries_is_listed_for_information(ic):
    assert settings_change(ic, MORE, SETTINGS, {"tests/cli/test_c.py": TWO_TESTS}) == [
        ("note", 3, "the runner's settings leave out one more mark, `slow`: no test carries it today, so none leaves "
                    "the plain test jobs")]


def test_a_new_mark_whose_tests_were_all_out_already_is_listed_for_information_with_their_number(ic):
    hook = HOOK.replace('"/tests/infrastructure/test_llm.py",', '"/tests/cli/test_a.py",\n            "/tests/cli/test_b.py",')
    files = {CONFTEST: hook, "tests/cli/test_a.py": SLOW["tests/cli/test_a.py"], "tests/cli/test_b.py": SLOW["tests/cli/test_b.py"]}
    assert settings_change(ic, MORE, SETTINGS, files) == [
        ("note", 3, "the runner's settings leave out one more mark, `slow`: the 3 test(s) that carry it are left out "
                    "already through another mark, so none leaves the plain test jobs")]


def test_each_new_mark_has_a_finding_of_its_own(ic):
    two = SETTINGS.replace("not integration", "not integration and not slow and not gpu")
    files = {**SLOW, "tests/cli/test_g.py": TWO_TESTS.replace("def test_b", "@pytest.mark.gpu\ndef test_b")}
    assert settings_change(ic, two, SETTINGS, files) == [
        ("warning", 3, SLOW_LEAVE),
        ("warning", 3, "the runner's settings leave out one more mark, `gpu`: 1 test(s) in 1 file(s) leave the plain "
                       "test jobs (tests/cli/test_g.py)")]


def test_with_no_copy_of_the_base_settings_there_is_nothing_to_compare(ic):
    assert settings_change(ic, MORE, None, SLOW) == []
    born = diff("pyproject.toml", added=["x"], status="added")
    assert [f.what for f in ic.check(born, TASKS, Files({"pyproject.toml": MORE, **SLOW})) if "one more mark" in f.what] == []


def test_where_the_files_cannot_be_listed_the_finding_says_that_the_tests_were_not_counted(ic):
    class NoListing(Files):
        def tracked(self):
            return None
    assert settings_change(ic, MORE, SETTINGS, SLOW, tree=NoListing) == [
        ("warning", 3, "the runner's settings leave out one more mark, `slow`: its tests leave the plain test jobs, and "
                       "they could not be counted (the files could not be listed)")]


def test_the_checkout_lists_the_files_that_git_tracks_and_no_others(ic, tmp_path):
    def git(*args):
        subprocess.run(["git", "-C", str(tmp_path), "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args],
                       check=True, capture_output=True)
    git("init", "-q")
    (tmp_path / "tests").mkdir()
    (tmp_path / "tests" / "test_a.py").write_text("def test_a():\n    pass\n", encoding="utf-8")
    (tmp_path / "pyproject.toml").write_text(SETTINGS, encoding="utf-8")
    git("add", "-A")
    git("commit", "-q", "-m", "base")
    (tmp_path / "tests" / "test_not_tracked.py").write_text("def test_b():\n    pass\n", encoding="utf-8")
    assert ic.Tree(tmp_path, "HEAD").tracked() == ["pyproject.toml", "tests/test_a.py"]
    assert ic.Tree().tracked() is None
    assert ic.Tree(tmp_path / "tests" / "no-such-folder", "HEAD").tracked() is None


@pytest.mark.parametrize("before, after", [
    ("  it('parses a frame', () => {", "  it.only('parses a frame', () => {"),
    ("  it('parses a frame', () => {", "  it.skipIf(isCI)('parses a frame', () => {"),
    ("  test('parses a frame', () => {", "  test.concurrent.skip('parses a frame', () => {"),
    ("  it('parses a frame', () => {", "  it.skipIf(os.platform() === 'win32')('parses a frame', () => {"),
])
def test_a_test_that_gets_a_modifier_in_front_of_its_name_is_not_a_removed_test(ic, before, after):
    found = whats(ic, diff("extensions/vscode/test/sse.test.ts", added=[after], removed=[before]))
    assert [what for what in found if what.startswith("test removed")] == []
    assert len(found) == 1
