"""The weekly cleanup offers only pieces that passed their checks, changes nothing but the text of one issue, and
never lets a file or a tool put a mention into that text.

`make` runs here on a small repository made for the test, with the real git and the real ruff. `go` and `gofmt` are
stand-ins that answer from marks in the files: the Go that a test job has is not the Go of the weekly job, and a test
of the script needs to say what a fixer does. `write` talks to a stand-in for GitHub on this machine. No test
reaches the network.
"""
import http.server
import importlib.util
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import threading
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "weekly_cleanup.py"
TOKEN = "a-token-made-for-this-test-0123456789"
GIT = {"GIT_AUTHOR_NAME": "a test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "a test",
       "GIT_COMMITTER_EMAIL": "test@example.invalid", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_SYSTEM": os.devnull}
# A stand-in is two files. The first line of the one on the path is fixed: a line that named the path of Python would
# not start where that path holds a space.
ON_THE_PATH = '#!/bin/sh\nexec {python} -S {code} "$@"\n'
# What a fixer does is written into the Go files of the test as marks. For a fixer X:
#   old_X        becomes new_X                 twice_X      needs a second run to become new_X
#   grow_X       grows with each run           breakfix_X   makes the next run of the fixer fail
#   breakvet_X, breakbuild_X, breaktest_X, unformat_X   make the result fail that check
#   flaky_X      makes the first test run fail and the later ones pass
#   othertest_X  makes each test run fail with another test
GO = r'''"""A stand-in for go: a fixer changes the marks in the Go files, and a check fails for the mark that names it."""
import json, sys
from pathlib import Path
home, args = Path({home!r}), sys.argv[1:]
with open(home / "calls.log", "a") as log:
    log.write(json.dumps({{"args": args, "in": Path.cwd().name}}) + "\n")
files = sorted(Path.cwd().rglob("*.go"))
holds = lambda mark: [str(f.relative_to(Path.cwd())) for f in files if mark in f.read_text()]
if args[:3] == ["tool", "fix", "help"]:
    names = json.loads((home / "fixers.json").read_text())
    print("fix is a tool.\n\nRegistered analyzers:\n\n" + "\n".join("    %-12s what it does" % name for name in names) + "\n\nBy default all analyzers are run.")
elif args[0] == "fix":
    fixer = args[1][1:]
    if holds("NOFIX"):
        print("fix: ./%s:1:1: this does not compile" % holds("NOFIX")[0])
        sys.exit(1)
    for f in files:
        text = f.read_text()
        for old, new in (("old_" + fixer, "new_" + fixer), ("twice_" + fixer, "old_" + fixer), ("grow_" + fixer, "grow_" + fixer + "x"),
                         ("breakfix_" + fixer, "NOFIX"), ("breakvet_" + fixer, "NOVET"), ("breakbuild_" + fixer, "NOBUILD"),
                         ("breaktest_" + fixer, "NOTEST"), ("unformat_" + fixer, "UNFORMATTED"),
                         ("flaky_" + fixer, "FLAKYTEST"), ("othertest_" + fixer, "OTHERTEST")):
            text = text.replace(old, new)
        if text != f.read_text():
            f.write_text(text)
elif args[0] == "test":
    with open(home / "tested.log", "a") as log:
        log.write(json.dumps({{"in": Path.cwd().name, "holds": sorted(set(w for f in files for w in f.read_text().split() if w.startswith("new_")))}}) + "\n")
    count = home / ("failed-%s.count" % Path.cwd().name)
    before = int(count.read_text()) if count.exists() else 0
    failed = ("TestMarks" if holds("NOTEST") else "TestMarks%d" % before if holds("OTHERTEST")
              else "TestMarks" if holds("FLAKYTEST") and before == 0 else "")
    if failed:
        count.write_text(str(before + 1))
        # As go test prints it: the lines that the tests log, the failed test far above the end, then more lines.
        print("\n".join(["2026/10/08 08:52:47 [info] a line that a test logged"] * 3 + ["--- FAIL: %s (0.00s)" % failed,
              "    marks_test.go:3: said by the test, with @someone and #1 in it"] + ["2026/10/08 08:52:48 [info] a line that a later test logged"] * 30
              + ["FAIL", "FAIL\texample/%s\t0.01s" % Path.cwd().name, "FAIL"]))
        sys.exit(1)
else:
    mark = {{"build": "NOBUILD", "vet": "NOVET"}}[args[0]]
    if holds(mark):
        print("%s: ./%s:3:9: said by the tool, with @someone and #1 in it" % (args[0], holds(mark)[0]))
        sys.exit(1)
'''
GOFMT = r'''"""A stand-in for gofmt -l: it names the Go files that hold the mark UNFORMATTED."""
import sys
from pathlib import Path
for f in sorted(Path(sys.argv[-1]).rglob("*.go")):
    if "UNFORMATTED" in f.read_text():
        print(f)
'''
# The fixers that a test runs, unless it is about the whole list: the stand-in is started once for each fixer and module.
A_FEW = ("any", "forvar", "minmax", "rangeint", "stringscut")
ON_DEV = {
    "proxy/main.go": "package main\n// old_any old_rangeint\n",
    "proxy/tools.go": "package main\n// old_any\n",
    "proxy/quiet.go": "package main\n",
    "tui/view.go": "package main\n// old_minmax\n",
    "pkg/clean.py": "import os\n\nprint(os.sep)\n",
    "pkg/spaces.py": "import os   \n\nprint(os.sep)  \n",
    "docs/page.md": "a page\n",
}


spec = importlib.util.spec_from_file_location("atlas_weekly_cleanup", SCRIPT)
cleanup = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = cleanup
spec.loader.exec_module(cleanup)


@pytest.fixture(autouse=True)
def no_name_of_a_runner(monkeypatch):
    """No test reads the environment of the machine it runs on. A job on a runner has names of its own there
    (`GITHUB_REPOSITORY`, `GITHUB_STEP_SUMMARY`, ...), and a script would take them for the test's. A test that needs
    such a name sets it."""
    for name in [name for name in os.environ if name.startswith("GITHUB_") or name == "CI"]:
        monkeypatch.delenv(name)


def git(where, *args):
    done = subprocess.run(["git", "-C", str(where), *args], capture_output=True, text=True, timeout=60, env={**os.environ, **GIT})
    assert done.returncode == 0, done.stdout + done.stderr
    return done.stdout


class Made:
    """A repository made for the test, the stand-ins on the path, and what `make` did with it."""

    def __init__(self, tmp_path, monkeypatch, capsys, files=None, fixers=None, tools=("go", "gofmt"), run=A_FEW):
        self.tmp, self.capsys = tmp_path, capsys
        monkeypatch.setattr(cleanup, "GO_FIXERS", run or cleanup.GO_FIXERS)
        self.repo, self.out, self.page = tmp_path / "repo", tmp_path / "out", tmp_path / "page.md"
        self.repo.mkdir()
        (tmp_path / "bin").mkdir()
        for name, text in {**ON_DEV, **(files or {})}.items():
            (self.repo / name).parent.mkdir(parents=True, exist_ok=True)
            (self.repo / name).write_text(text)
        subprocess.run(["git", "init", "--quiet", "--initial-branch", "dev", str(self.repo)], check=True, timeout=60, env={**os.environ, **GIT})
        git(self.repo, "add", "--all")
        git(self.repo, "commit", "--quiet", "-m", "the files of dev")
        self.commit = git(self.repo, "rev-parse", "HEAD").strip()
        for name, body in (("go", GO.format(home=str(tmp_path))), ("gofmt", GOFMT)):
            if name in tools:
                (tmp_path / f"{name}.py").write_text(body)
                (tmp_path / "bin" / name).write_text(ON_THE_PATH.format(python=shlex.quote(sys.executable), code=shlex.quote(str(tmp_path / f"{name}.py"))))
                (tmp_path / "bin" / name).chmod(0o755)
        (tmp_path / "fixers.json").write_text(json.dumps(sorted([*cleanup.GO_FIXERS, *cleanup.GO_NOT_RUN]) if fixers is None else fixers))
        # Only the stand-ins and git are on the path: the real go of this machine cannot be reached.
        (tmp_path / "bin" / "git").symlink_to(shutil.which("git"))
        monkeypatch.setenv("PATH", str(tmp_path / "bin"))
        for name, value in GIT.items():
            monkeypatch.setenv(name, value)
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(self.page))

    def make(self, *more):
        self.status = cleanup.main(["make", "--root", str(self.repo), "--out", str(self.out), *more])
        printed = self.capsys.readouterr()
        self.printed = printed.out + printed.err
        return self

    @property
    def text(self):
        return (self.out / "issue.md").read_text()

    @property
    def result(self):
        return json.loads((self.out / "result.json").read_text())

    def piece(self, name):
        (found,) = [piece for piece in self.result["pieces"] if piece["name"] == name]
        return found

    def patch(self, name):
        return (self.out / "pieces" / (name.replace("/", "-") + ".patch")).read_text()

    def calls(self, *first):
        log = self.tmp / "calls.log"
        calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
        return [call for call in calls if call["args"][:len(first)] == list(first)]

    def tested(self):
        log = self.tmp / "tested.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []


@pytest.fixture
def made(tmp_path, monkeypatch, capsys):
    return lambda **how: Made(tmp_path, monkeypatch, capsys, **how)


def states(result):
    return {piece["name"]: piece["state"] for piece in result["pieces"] if piece["state"] != "nothing to change"}


def applies_alone(run, name):
    """Whether the patch of a piece applies to the commit with no other piece. Gives the files it changes."""
    fresh, patch = run.tmp / ("fresh-" + name.replace("/", "-")), run.out / "pieces" / (name.replace("/", "-") + ".patch")
    subprocess.run(["git", "clone", "--quiet", str(run.repo), str(fresh)], check=True, timeout=60)
    git(fresh, "apply", "--check", str(patch))
    git(fresh, "apply", str(patch))
    return sorted(name for name in git(fresh, "diff", "--name-only", "-z").split("\0") if name)


# --- what a piece is, and that making it changes nothing ----------------------------------------------------------

def test_each_fixer_that_changes_a_file_gives_a_piece_whose_patch_applies_alone_to_the_commit(made):
    run = made().make()
    assert run.status == 0, run.printed
    assert states(run.result) == {"go/proxy/any": "held", "go/proxy/rangeint": "held", "go/tui/minmax": "held", "python/W291": "offered"}
    assert applies_alone(run, "go/proxy/any") == ["proxy/main.go", "proxy/tools.go"]
    assert applies_alone(run, "go/proxy/rangeint") == ["proxy/main.go"]
    assert applies_alone(run, "go/tui/minmax") == ["tui/view.go"]
    assert applies_alone(run, "python/W291") == ["pkg/spaces.py"]
    assert "+// new_any old_rangeint" in run.patch("go/proxy/any")
    assert "+// old_any new_rangeint" in run.patch("go/proxy/rangeint")
    assert run.result["commit"] == run.commit
    assert run.result["anything"] is True
    assert run.result["one_piece"] is False


def test_making_the_text_changes_no_file_of_the_checkout_also_not_one_with_work_that_is_not_committed(made):
    run = made()
    # Work of a person in the checkout: a change that is not committed, in a file that a fixer would change, and a new file.
    (run.repo / "proxy" / "main.go").write_text("package main\n// old_any old_rangeint and a line of mine\n")
    (run.repo / "pkg" / "mine.py").write_text("x = 1   \n")
    before = {path: path.read_bytes() for path in run.repo.rglob("*") if path.is_file() and ".git" not in path.parts}
    run.make()
    assert run.status == 0, run.printed
    assert git(run.repo, "status", "--porcelain") == " M proxy/main.go\n?? pkg/mine.py\n"
    # The text is made for the commit: the work that is not committed is not in it.
    assert "a line of mine" not in run.patch("go/proxy/any")
    assert run.piece("python/W291")["changed"] == ["pkg/spaces.py"]
    assert git(run.repo, "rev-parse", "HEAD").strip() == run.commit
    assert {path: path.read_bytes() for path in run.repo.rglob("*") if path.is_file() and ".git" not in path.parts} == before
    assert git(run.repo, "worktree", "list").count("\n") == 1


def test_for_each_piece_the_files_looked_at_are_the_changed_the_unchanged_and_the_left_out(made):
    run = made(files={"pkg/unused.py": "import os\nimport sys\n\nprint(os.sep)   \n", "proxy/bad.go": "package main\n// breakvet_any\n"}).make()
    looked_at = {"go/proxy": 4, "go/tui": 1, "python": 3}
    for piece in run.result["pieces"]:
        part = piece["name"].rsplit("/", 1)[0]
        assert piece["looked_at"] == looked_at[part], piece["name"]
        patch = run.patch(piece["name"]) if piece["changed"] else ""
        assert len(piece["changed"]) == patch.count("diff --git "), piece["name"]
        assert not set(piece["changed"]) & set(piece["left_out"]), piece["name"]
        unchanged = int(re.search(rf"^\| `{re.escape(piece['name'])}` \| \d+ \| \d+ \| (\d+) \|", run.text, re.MULTILINE).group(1)) \
            if piece["state"] != "nothing to change" else piece["looked_at"]
        assert piece["looked_at"] == len(piece["changed"]) + unchanged + len(piece["left_out"]), piece["name"]
    assert run.piece("go/proxy/any")["left_out"] == {name: "the piece fails a check" for name in ("proxy/bad.go", "proxy/main.go", "proxy/tools.go")}
    assert run.piece("python/F401")["left_out"] == {"pkg/unused.py": "the syntax tree is not the same after the fix"}
    assert "| `go/proxy/rangeint` | 4 | 1 | 3 | 0 | +1 -1 | gofmt: passed, build: passed, vet: passed, tests: passed | held |" in run.text
    assert "| `python/W291` | 3 | 2 | 1 | 0 | +3 -3 | tree and comments: read in each file | offered |" in run.text


# --- which fixers run ----------------------------------------------------------------------------------------------

def test_only_the_fixers_on_the_list_run_and_each_runs_on_each_module(made):
    whole_list = cleanup.GO_FIXERS
    run = made(run=None).make()
    ran = {(call["in"], call["args"][1][1:]) for call in run.calls("fix")}
    assert ran == {(module, fixer) for module in cleanup.MODULES for fixer in whole_list}
    assert not {fixer for _module, fixer in ran} & set(cleanup.GO_NOT_RUN)
    for call in run.calls("fix"):
        assert call["args"] == ["fix", call["args"][1], "./..."]


def test_a_fixer_that_this_go_has_and_that_is_on_no_list_is_named_and_not_run(made):
    run = made(fixers=[*A_FEW, *cleanup.GO_NOT_RUN, "brandnew", "another"]).make()
    assert run.status == 0, run.printed
    assert run.result["unlisted"] == ["another", "brandnew"]
    assert "This Go has fixers that are on no list of the job, so they were not run: `another`, `brandnew`." in run.text
    assert not [call for call in run.calls("fix") if call["args"][1] in ("-brandnew", "-another")]


def test_a_list_of_fixers_that_cannot_be_read_is_not_judged(made):
    run = made(fixers=[]).make()
    assert run.status == 2
    assert "gave no list of fixers that this script can read" in run.printed
    assert not (run.out / "issue.md").exists()


def test_the_lists_of_fixers_do_not_overlap_and_the_three_that_never_run_have_their_reason():
    assert not set(cleanup.GO_FIXERS) & set(cleanup.GO_NOT_RUN)
    assert set(cleanup.GO_NOT_RUN) == {"omitzero", "hostport", "buildtag"}
    assert len(set(cleanup.GO_FIXERS)) == len(cleanup.GO_FIXERS) == 19
    assert cleanup.PYTHON_RULES == ("W291", "W292", "W293", "W391", "F401")
    assert cleanup.MODULES == tuple(sorted(path.parent.name for path in ROOT.glob("*/go.mod")))


# --- the checks of a Go piece --------------------------------------------------------------------------------------

def test_a_fixer_runs_until_its_result_stays_the_same(made):
    run = made(files={"tui/view.go": "package main\n// twice_minmax\n"}).make()
    assert "+// new_minmax" in run.patch("go/tui/minmax")
    assert run.piece("go/tui/minmax")["checks"] == {"gofmt": "passed", "build": "passed", "vet": "passed", "tests": "passed"}
    # Three runs: two change the file and the third changes nothing. The test run of the module makes the piece again.
    assert len([call for call in run.calls("fix", "-minmax") if call["in"] == "tui"]) == 2 * 3


@pytest.mark.parametrize("mark, why, said, runs", [
    pytest.param("breakfix_minmax", "the fixer fails on its own result",
                 ["`go fix -minmax` ended with status 1 on its own result:", "fix: ./view.go:1:1: this does not compile"], 2,
                 id="fails on its own result"),
    # The first run and three more: then the script stops asking.
    pytest.param("grow_minmax", "the fixer did not come to an end",
                 [f"`go fix -minmax` still changes files after {cleanup.PASSES} runs"], cleanup.PASSES + 1,
                 id="does not come to an end"),
])
def test_a_fixer_that_fails_on_its_own_result_or_does_not_come_to_an_end_is_left_out_and_named(made, mark, why, said, runs):
    run = made(files={"tui/view.go": f"package main\n// {mark}\n"}).make()
    piece = run.piece("go/tui/minmax")
    assert piece["state"] == "left out"
    assert piece["checks"] == {"fix": "failed"}
    assert piece["changed"] == []
    assert piece["left_out"] == {"tui/view.go": why}
    assert piece["said"] == said
    assert not (run.out / "pieces" / "go-tui-minmax.patch").exists()
    assert len([call for call in run.calls("fix", "-minmax") if call["in"] == "tui"]) == runs


@pytest.mark.parametrize("mark, check, checks", [
    pytest.param("unformat", "gofmt", {"gofmt": "failed", "build": "not run", "vet": "not run"}, id="gofmt fails"),
    pytest.param("breakbuild", "build", {"gofmt": "passed", "build": "failed", "vet": "not run"}, id="build fails"),
    pytest.param("breakvet", "vet", {"gofmt": "passed", "build": "passed", "vet": "failed"}, id="vet fails"),
])
def test_a_piece_that_fails_a_check_is_left_out_with_what_the_tool_said_and_the_others_are_not(made, mark, check, checks):
    run = made(files={"proxy/tools.go": f"package main\n// old_any old_stringscut {mark}_stringscut\n"}).make()
    assert run.status == 0, run.printed
    piece = run.piece("go/proxy/stringscut")
    assert piece["state"] == "left out"
    assert piece["checks"] == checks
    assert piece["left_out"] == {"proxy/tools.go": "the piece fails a check"}
    assert check in piece["said"][0]
    assert "tools.go" in "\n".join(piece["said"])
    assert not (run.out / "pieces" / "go-proxy-stringscut.patch").exists()
    assert states(run.result)["go/proxy/any"] == "held"
    assert run.piece("go/proxy/any")["checks"]["tests"] == "passed"
    assert "go/proxy/stringscut:\n  proxy/tools.go: the piece fails a check\n  | " in run.text
    # The tests of each module ran once, with the pieces that passed: new_stringscut is not in them.
    assert run.tested() == [{"in": "proxy", "holds": ["new_any", "new_rangeint"]}, {"in": "tui", "holds": ["new_minmax"]}]


def test_a_file_that_gofmt_named_before_is_no_fault_of_a_piece(made):
    run = made(files={"proxy/quiet.go": "package main\n// UNFORMATTED on dev as it is\n"}).make()
    assert run.piece("go/proxy/any")["checks"]["gofmt"] == "passed"
    assert states(run.result)["go/proxy/any"] == "held"


def test_the_text_is_made_from_a_checkout_of_one_commit_with_no_branch_as_a_job_has_it(made):
    run = made()
    job = run.tmp / "the checkout of a job"
    subprocess.run(["git", "init", "--quiet", str(job)], check=True, timeout=60)
    git(job, "remote", "add", "origin", f"file://{run.repo}")
    git(job, "fetch", "--quiet", "--no-tags", "--depth=1", "origin", "+refs/heads/dev:refs/remotes/pull/9/merge")
    git(job, "checkout", "--quiet", "--force", "refs/remotes/pull/9/merge")
    assert git(job, "rev-parse", "--is-shallow-repository").strip() == "true"
    assert git(job, "branch", "--list", "--format=%(refname)").strip() in ("", "(HEAD detached at pull/9/merge)")
    run.repo = job
    run.make()
    assert run.status == 0, run.printed
    assert states(run.result) == {"go/proxy/any": "held", "go/proxy/rangeint": "held", "go/tui/minmax": "held", "python/W291": "offered"}
    assert run.result["commit"] == run.commit


def test_when_the_tests_fail_with_the_pieces_together_none_of_the_module_is_offered_and_the_other_module_is(made):
    run = made(files={"proxy/tools.go": "package main\n// old_any breaktest_forvar\n"}).make()
    assert run.status == 0, run.printed
    for name in ("go/proxy/any", "go/proxy/rangeint", "go/proxy/forvar"):
        piece = run.piece(name)
        assert piece["state"] == "left out", name
        assert piece["checks"]["tests"] == "failed twice"
        assert set(piece["left_out"].values()) == {"the tests of the module fail with the pieces of the module together"}
        assert not (run.out / "pieces" / (name.replace("/", "-") + ".patch")).exists()
    assert states(run.result)["go/tui/minmax"] == "held"
    # After the first failed run the tests ran on the files as they are, to tell a fault of a piece from a fault of the
    # branch, and then once more with the pieces, to tell it from a test that fails only sometimes.
    assert [entry["holds"] for entry in run.tested() if entry["in"] == "proxy"] == [["new_any", "new_rangeint"], [], ["new_any", "new_rangeint"]]


def test_the_tests_of_a_module_run_with_the_flags_of_the_go_test_jobs(made):
    run = made().make()
    assert [["go", *call["args"]] for call in run.calls("test")] == [cleanup.GO_TEST, cleanup.GO_TEST]
    gates = (ROOT / "scripts" / "production-readiness.py").read_text(encoding="utf-8")
    assert "(" + ", ".join(f'"{word}"' for word in cleanup.GO_TEST) + ")," in gates, "the Go test gate runs other flags"
    assert "-race" in cleanup.GO_TEST
    assert "-count=1" in cleanup.GO_TEST


def test_of_a_test_run_that_failed_twice_the_text_names_the_failed_test_and_files_of_the_run_hold_the_whole_output(made):
    run = made(files={"proxy/tools.go": "package main\n// old_any breaktest_forvar\n"}).make()
    assert run.status == 0, run.printed
    later = "2026/10/08 08:52:48 [info] a line that a later test logged"
    assert run.piece("go/proxy/any")["said"] == [
        "`go test` failed twice with the pieces of the module together, with the same tests both times, and passed on the files as they are:",
        "--- FAIL: TestMarks (0.00s)",
        "    marks_test.go:3: said by the test, with @someone and #1 in it",
        "FAIL", "FAIL\texample/proxy\t0.01s", "FAIL",
        "The last lines of the output:", *[later] * 5, "FAIL", "FAIL\texample/proxy\t0.01s", "FAIL",
        "The whole output of both runs is in the files `tests-proxy.txt` and `tests-proxy-second-run.txt` of the run.",
        "Which piece is the cause is not looked for here. `--piece` runs the tests with one piece alone."]
    # The lines of the tool stand once in the text, under the first piece of the module.
    for name in ("go/proxy/forvar", "go/proxy/rangeint"):
        assert run.piece(name)["said"] == ["What the tests said stands under go/proxy/any."]
    assert run.text.count("--- FAIL: TestMarks (0.00s)") == 1
    for name in ("tests-proxy.txt", "tests-proxy-second-run.txt"):
        whole = (run.out / name).read_text()
        assert whole.count(later) == 30, name
        assert "--- FAIL: TestMarks (0.00s)" in whole, name
    assert not (run.out / "tests-tui.txt").exists()
    assert "@" not in outside_code(run.text)


def test_with_one_piece_the_text_does_not_send_the_reader_to_the_one_piece_command(made):
    run = made(files={"proxy/tools.go": "package main\n// old_any breaktest_any\n"}).make("--piece", "go/proxy/any")
    assert run.status == 0, run.printed
    said = run.piece("go/proxy/any")["said"]
    assert said[0] == "`go test` failed twice, with the same tests both times, and passed on the files as they are:"
    assert said[-1] == "The whole output of both runs is in the files `tests-proxy.txt` and `tests-proxy-second-run.txt` of the run."


@pytest.mark.parametrize("mark, runs", [
    pytest.param("flaky_forvar", "The first run failed: TestMarks\nThe second run passed\n", id="failed once and then passed"),
    pytest.param("othertest_forvar", "The first run failed: TestMarks0\nThe second run failed: TestMarks1\n", id="failed twice with other tests"),
])
def test_tests_that_do_not_fail_the_same_way_twice_with_the_pieces_are_not_judged_and_no_piece_is_called_at_fault(made, mark, runs):
    run = made(files={"proxy/tools.go": f"package main\n// old_any {mark}\n"}).make()
    assert run.status == 2
    assert ("weekly cleanup: not judged: the tests of `proxy` did not give the same result twice with the pieces of the module "
            "together. They pass on the files as they are.") in run.printed
    assert runs in run.printed
    assert "Fix: a test that fails only sometimes is a fault of the tests of `proxy`, also for its test job" in run.printed
    assert "left out" not in run.printed
    assert not (run.out / "issue.md").exists()
    assert [entry["holds"] for entry in run.tested() if entry["in"] == "proxy"] == [["new_any", "new_rangeint"], [], ["new_any", "new_rangeint"]]
    assert run.page.read_text().splitlines()[2].startswith("**Not judged.**")
    assert "@" not in outside_code(run.page.read_text())


@pytest.mark.parametrize("printed, names", [
    pytest.param("log\n--- FAIL: TestB (0.01s)\n    b_test.go:3: no\n--- FAIL: TestA (1.20s)\nFAIL\nFAIL\tm\t1.2s\n", ["TestA", "TestB"],
                 id="two tests"),
    pytest.param("--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/part (0.00s)\n--- FAIL: TestA (0.02s)\n", ["TestA", "TestA/part"],
                 id="a subtest and a name twice"),
    # No test is named: the build failed, or the run did not end. Then the module's own line stands for a name, without its time.
    pytest.param("# m [m.test]\n./a_test.go:3:1: undefined: x\nFAIL\tm [build failed]\nFAIL\n", ["FAIL", "FAIL m"], id="the build failed"),
    pytest.param("panic: test timed out after 20m0s\nFAIL\tm\t1200.01s\n", ["FAIL m"], id="the tests timed out"),
    pytest.param("ok  \tm\t1.2s\n", [], id="all passed"),
    pytest.param("no end after 2400 s", [], id="stopped at the time limit"),
])
def test_the_names_of_the_tests_that_failed_are_read_from_what_go_test_printed(printed, names):
    assert cleanup.failed_tests(printed) == names


@pytest.mark.parametrize("printed, kept", [
    pytest.param("log\n--- FAIL: TestA (0.01s)\n    a_test.go:3: got 1, want 2\n    a_test.go:4: and more\nlog\nFAIL\nFAIL\tm\t1.2s\n",
                 "--- FAIL: TestA (0.01s)\n    a_test.go:3: got 1, want 2\n    a_test.go:4: and more\nFAIL\nFAIL\tm\t1.2s", id="a test and its lines"),
    pytest.param("--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/part (0.00s)\n        a_test.go:9: no\nok\n",
                 "--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/part (0.00s)\n        a_test.go:9: no", id="a subtest"),
    pytest.param("log\nWARNING: DATA RACE\nlog\npanic: boom\nlog\nFAIL\tm\t0.1s\n", "WARNING: DATA RACE\npanic: boom\nFAIL\tm\t0.1s",
                 id="a data race and a panic"),
    pytest.param("--- FAIL: TestA (0.01s)\n--- PASS: TestB (0.00s)\n    b_test.go:1: a note\n", "--- FAIL: TestA (0.01s)",
                 id="not the lines of a test that passed"),
])
def test_of_what_go_test_printed_the_lines_about_failed_tests_come_first_and_then_its_last_lines(printed, kept):
    tail = "\n".join(printed.strip().splitlines()[-8:])
    assert cleanup.of_failed_tests(printed) == f"{kept}\nThe last lines of the output:\n{tail}"


def test_when_nothing_names_a_failed_test_the_end_of_the_output_is_what_is_kept():
    assert cleanup.of_failed_tests("one\ntwo\nexit status 2\n") == "one\ntwo\nexit status 2"


def test_very_many_lines_about_failed_tests_are_cut_with_a_word():
    printed = "\n".join(f"--- FAIL: Test{n} (0.00s)" for n in range(75))
    kept = cleanup.of_failed_tests(printed).splitlines()
    assert len(kept) == 60 + 1 + 1 + 8
    assert kept[60] == "and 15 more line(s) of this kind"
    assert kept[61] == "The last lines of the output:"


def test_a_module_with_nothing_to_change_gets_no_test_run(made):
    run = made(files={"tui/view.go": "package main\n"}).make()
    assert [entry["in"] for entry in run.tested()] == ["proxy"]


@pytest.mark.parametrize("files, says", [
    pytest.param({"proxy/quiet.go": "package main\n// NOTEST\n"}, "the tests of proxy on the files as they are: status 1.", id="tests fail"),
    pytest.param({"proxy/quiet.go": "package main\n// NOVET\n"}, "go vet on proxy as it is: status 1.", id="vet fails"),
    pytest.param({"proxy/quiet.go": "package main\n// NOFIX\n"}, "`go fix -any` on `proxy` as it is: status 1.", id="a fixer fails"),
])
def test_a_tool_that_fails_on_the_files_as_they_are_is_not_judged_and_no_text_is_made(made, files, says):
    run = made(files=files).make()
    assert run.status == 2
    assert f"weekly cleanup: not judged: {says}" in run.printed
    assert "Fix: " in run.printed
    assert not (run.out / "issue.md").exists()
    page = run.page.read_text()
    assert page.splitlines()[2].startswith("**Not judged.** Nothing is known about what there is to clean. This is not a pass.")


def test_without_go_nothing_is_judged_and_a_python_piece_can_still_be_made(made):
    run = made(tools=()).make()
    assert run.status == 2
    assert "`go` is not on the PATH." in run.printed
    again = run.make("--piece", "python/W291")
    assert again.status == 0, again.printed
    assert states(again.result) == {"python/W291": "offered"}


# --- a Python piece ------------------------------------------------------------------------------------------------

def test_a_fix_that_leaves_the_syntax_tree_and_the_comments_the_same_is_in_the_piece(made):
    run = made(files={"pkg/note.py": "x = 1  # a note   \n# the end"}).make()
    assert run.piece("python/W291")["changed"] == ["pkg/note.py", "pkg/spaces.py"]
    assert run.piece("python/W292")["changed"] == ["pkg/note.py"]
    assert run.piece("python/W291")["left_out"] == {}


def test_a_fix_that_changes_the_syntax_tree_is_left_out_and_the_file_is_named(made):
    run = made(files={"pkg/unused.py": "import os\nimport sys\n\nprint(os.sep)\n"}).make()
    piece = run.piece("python/F401")
    assert piece["state"] == "left out"
    assert piece["changed"] == []
    assert piece["left_out"] == {"pkg/unused.py": "the syntax tree is not the same after the fix"}
    assert not (run.out / "pieces" / "python-F401.patch").exists()
    assert "python/F401:\n  pkg/unused.py: the syntax tree is not the same after the fix" in run.text
    assert "| `python/F401` | 3 | 0 | 2 | 1 | +0 -0 | tree and comments: read in each file | left out |" in run.text


def test_a_fix_that_takes_a_comment_away_is_left_out_though_the_syntax_tree_is_the_same(made, monkeypatch):
    # The fix of this rule removes a `noqa` mark that the rule set of the run does not need. Another check may need it.
    monkeypatch.setattr(cleanup, "PYTHON_RULES", ("RUF100",))
    run = made(files={"pkg/marked.py": "import os  # noqa: E402\n\nprint(os.sep)\n"}).make("--piece", "python/RUF100")
    assert run.status == 0, run.printed
    assert run.piece("python/RUF100")["left_out"] == {"pkg/marked.py": "a comment is not the same after the fix"}
    assert run.piece("python/RUF100")["changed"] == []


@pytest.mark.parametrize("old, new, why", [
    pytest.param("x = 1   \n", "x = 1\n", "", id="spaces at the end of a line"),
    pytest.param("x = 1", "x = 1\n", "", id="a newline at the end"),
    pytest.param("x = 1  # a note   \n", "x = 1  # a note\n", "", id="spaces at the end of a comment"),
    pytest.param("x = (1,\n     2)\n", "x = (1, 2)\n", "", id="two lines made one"),
    pytest.param("import os\nx = 1\n", "x = 1\n", "the syntax tree is not the same after the fix", id="an import taken away"),
    pytest.param("x = f'a'\n", "x = 'a'\n", "the syntax tree is not the same after the fix", id="an f-string made plain"),
    pytest.param("x = 1  # noqa: E402\n", "x = 1\n", "a comment is not the same after the fix", id="a noqa mark taken away"),
    pytest.param("x = 1  # type: ignore\n", "x = 1  # type: ignore[assignment]\n", "a comment is not the same after the fix",
                 id="a type comment changed"),
    pytest.param("# one\n# two\nx = 1\n", "# two\n# one\nx = 1\n", "a comment is not the same after the fix", id="two comments swapped"),
    pytest.param("x = 1\n", "x = = 1\n", "this Python cannot read the file before or after the fix", id="no Python after the fix"),
    pytest.param("print 'x'   \n", "print 'x'\n", "this Python cannot read the file before or after the fix", id="no Python before the fix"),
])
def test_a_fix_is_taken_only_when_the_syntax_tree_and_the_comments_are_the_same(old, new, why):
    assert cleanup.differs(old.encode(), new.encode()) == why


def test_a_file_whose_name_holds_spaces_and_marks_is_fixed_and_named_like_any_other(made):
    run = made(files={"pkg/a file @someone #1.py": "x = 1   \n", "pkg/unused @other #2.py": "import os\n"}).make()
    assert run.status == 0, run.printed
    assert "pkg/a file @someone #1.py" in run.piece("python/W291")["changed"]
    assert run.piece("python/F401")["left_out"] == {"pkg/unused @other #2.py": "the syntax tree is not the same after the fix"}
    assert applies_alone(run, "python/W291") == ["pkg/a file @someone #1.py", "pkg/spaces.py"]


# --- held ----------------------------------------------------------------------------------------------------------

def test_the_go_pieces_are_held_with_these_words_and_listed_with_their_numbers_and_checks_and_not_among_the_patches(made):
    held = ("Held by a maintainer: open work is written against these Go files as they are, and a rewrite "
            "of the same files now would make it not apply.")
    assert cleanup.HELD == {"go": held}
    run = made().make()
    assert "| `go/proxy/any` | 3 | 2 | 1 | 0 | +2 -2 | gofmt: passed, build: passed, vet: passed, tests: passed | held |" in run.text
    assert held in run.text.splitlines()
    patches = run.text.split("## The patches", 1)[1]
    assert "### `python/W291`" in patches
    assert "go/" not in patches
    assert "new_any" not in run.text
    # The patch of a held piece is made all the same, for the day the hold is lifted.
    assert "new_any" in run.patch("go/proxy/any")


def test_with_the_hold_taken_off_the_list_the_go_pieces_are_offered(made, monkeypatch):
    monkeypatch.setattr(cleanup, "HELD", {})
    run = made().make()
    assert states(run.result) == {"go/proxy/any": "offered", "go/proxy/rangeint": "offered", "go/tui/minmax": "offered", "python/W291": "offered"}
    assert "Held by a maintainer" not in run.text
    assert "### `go/proxy/any`" in run.text
    assert "+// new_any old_rangeint" in run.text


def test_with_no_held_piece_that_changes_a_file_the_text_does_not_speak_of_a_hold(made):
    run = made(files={"proxy/main.go": "package main\n", "proxy/tools.go": "package main\n", "tui/view.go": "package main\n"}).make()
    assert states(run.result) == {"python/W291": "offered"}
    assert "Held by a maintainer" not in run.text


# --- one piece -----------------------------------------------------------------------------------------------------

def test_one_piece_is_made_alone_and_its_tests_run_with_that_piece_alone(made):
    run = made().make("--piece", "go/proxy/rangeint")
    assert run.status == 0, run.printed
    assert [piece["name"] for piece in run.result["pieces"]] == ["go/proxy/rangeint"]
    assert run.result["one_piece"] is True
    assert {call["args"][1] for call in run.calls("fix")} == {"-rangeint"}
    assert {call["in"] for call in run.calls("fix")} == {"proxy"}
    assert run.tested() == [{"in": "proxy", "holds": ["new_rangeint"]}]
    assert applies_alone(run, "go/proxy/rangeint") == ["proxy/main.go"]
    assert sorted(path.name for path in (run.out / "pieces").iterdir()) == ["go-proxy-rangeint.patch"]


def test_a_folder_that_holds_files_of_another_run_is_not_written_into(made):
    run = made()
    (run.out / "pieces").mkdir(parents=True)
    (run.out / "pieces" / "go-proxy-any.patch").write_text("the patch of another day\n")
    run.make()
    assert run.status == 2
    assert "is not empty, and the patches of another run in it would read as this run's" in run.printed
    assert (run.out / "pieces" / "go-proxy-any.patch").read_text() == "the patch of another day\n"
    assert sorted(path.name for path in run.out.iterdir()) == ["pieces"]
    assert run.calls() == []


def test_an_error_of_the_script_itself_is_not_judged_and_never_a_finding(made, monkeypatch):
    def broken(*_args):
        raise TypeError("a fault of the script")

    monkeypatch.setattr(cleanup, "issue_text", broken)
    run = made().make("--piece", "python/W291")
    assert run.status == 2
    assert "weekly cleanup: not judged: an error of the script itself: TypeError('a fault of the script')" in run.printed
    assert run.page.read_text().splitlines()[2].startswith("**Not judged.**")


@pytest.mark.parametrize("name", [
    pytest.param("go/proxy/omitzero", id="a fixer that never runs"),
    pytest.param("go/sandbox/any", id="a part with no Go module"),
    pytest.param("python/E501", id="a rule that is on no list"),
    pytest.param("rangeint", id="a fixer without its part"),
    pytest.param("none", id="a word"),
])
def test_a_name_that_is_no_piece_is_refused_with_the_names_that_are(made, name):
    run = made().make("--piece", name)
    assert run.status == 2
    assert "is no piece. Fix: give one of: go/proxy/any, " in run.printed
    assert run.calls() == []


# --- the text ------------------------------------------------------------------------------------------------------

def test_the_text_starts_with_the_mark_and_says_who_applies_a_piece_and_how_to_stop_it(made):
    run = made().make()
    lines = run.text.splitlines()
    assert lines[0] == cleanup.MARK
    assert re.fullmatch(r"Made on \d{4}-\d\d-\d\d for commit `" + run.commit[:12] + r"`\.", lines[1])
    for said in ("- A maintainer applies a piece, one piece at a time. Applying a piece is a normal pull request through every check. "
                 "Please open no pull request from this list.",
                 "- This issue is not for a claim.",
                 "- Closing this issue stops the weekly text. Open it again to start it.",
                 "- Older than 8 days: the job did not end, or this issue was closed; see its runs."):
        assert said in lines, said
    assert "The checks of a Go piece are a test result. They are no proof that the code does what it did before." in lines
    assert "python3 scripts/weekly_cleanup.py make --piece <piece> --out <folder>" in lines
    assert ("Nothing to change: `go/proxy/forvar`, `go/proxy/minmax`, `go/proxy/stringscut`, `go/tui/any`, `go/tui/forvar`, "
            "`go/tui/rangeint`, `go/tui/stringscut`, `python/W292`, `python/W293`, `python/W391`, `python/F401`.") in lines
    # Each fixer that this Go has is on a list of the job.
    assert "on no list" not in run.text


def test_in_a_job_the_text_names_the_runs_of_the_workflow_and_only_at_githubs_usual_address(made, monkeypatch):
    monkeypatch.setenv("GITHUB_SERVER_URL", "https://github.com")
    monkeypatch.setenv("GITHUB_REPOSITORY", "inferstep/ATLAS")
    run = made().make()
    assert ("- Older than 8 days: the job did not end, or this issue was closed; see its runs: "
            "https://github.com/inferstep/ATLAS/actions/workflows/weekly-cleanup.yml") in run.text.splitlines()
    for server, repo in (("https://example.invalid", "inferstep/ATLAS"), ("https://github.com", "a/b @someone #1"), ("", "")):
        monkeypatch.setenv("GITHUB_SERVER_URL", server)
        monkeypatch.setenv("GITHUB_REPOSITORY", repo)
        assert cleanup.runs_address() == ""


def test_a_week_with_nothing_to_clean_says_so_with_the_date(made):
    quiet = {"proxy/main.go": "package main\n", "proxy/tools.go": "package main\n", "tui/view.go": "package main\n", "pkg/spaces.py": "x = 1\n"}
    run = made(files=quiet).make()
    assert run.status == 0, run.printed
    assert run.result["anything"] is False
    assert f"**Nothing to clean this week.** {len(run.result['pieces'])} piece(s) were tried, and no fixer changes a file." in run.text.splitlines()
    assert re.search(r"^Made on \d{4}-\d\d-\d\d for commit ", run.text, re.MULTILINE)
    assert "## The pieces" not in run.text
    assert list((run.out / "pieces").iterdir()) == []


def outside_code(text):
    plain = cleanup.plain_part(text)
    assert "a fenced block is not closed" not in plain
    return plain


def test_nothing_from_a_file_or_a_tool_can_be_read_as_a_mention_or_a_link_to_an_issue(made, monkeypatch):
    monkeypatch.setattr(cleanup, "HELD", {})
    files = {"pkg/a file @someone #1.py": "x = 1   \n", "pkg/unused @other #2.py": "import os\n",
             "pkg/text.py": "x = '''\n## A heading @here   \nCloses #5\n``````\n'''   \n",
             "proxy/tools.go": "package main\n// old_any breakvet_stringscut @inthecode #7\n"}
    run = made(files=files).make()
    assert run.status == 0, run.printed
    # They are all in the text: the file names, the line of the tool, the lines of the patches.
    for part in ("pkg/a file @someone #1.py", "pkg/unused @other #2.py", "said by the tool, with @someone and #1 in it", "@inthecode #7",
                 "## A heading @here", "Closes #5", "``````"):
        assert part in run.text, part
    plain = outside_code(run.text)
    assert "@" not in plain
    assert not re.search(r"#\d", plain)
    assert "Closes" not in plain
    offered = [name for name, state in states(run.result).items() if state == "offered"]
    assert offered == ["go/proxy/any", "go/proxy/rangeint", "go/tui/minmax", "python/W291"]
    assert [line for line in plain.splitlines() if line.startswith("#")] == ["## The pieces", "## To make one piece", "## Left out, and why",
                                                                              "## The patches"] + ["### "] * len(offered)
    page = outside_code(run.page.read_text())
    assert "@" not in page
    assert not re.search(r"#\d", page)


@pytest.mark.parametrize("text, fence", [
    pytest.param("no marks", "```", id="no marks"),
    pytest.param("three ``` in it", "````", id="a run of three"),
    pytest.param("a line\n``````\nof six", "```````", id="a line of six"),
])
def test_the_fence_of_a_block_is_longer_than_any_run_of_marks_in_it(text, fence):
    block = cleanup.fenced(text)
    assert block.splitlines()[0] == fence + "text"
    assert block.splitlines()[-1] == fence
    assert cleanup.plain_part("before\n" + block + "after") == "before\nafter"


@pytest.mark.parametrize("line, plain", [
    pytest.param("words `in marks` and after", "words  and after", id="code in a line"),
    pytest.param("`one` and `two`", " and ", id="two pieces of code"),
    pytest.param("``two ` marks inside`` after", " after", id="one mark inside two"),
    # A run of another length does not end the code: only the next run of the same length does.
    pytest.param("`@a`` @b`", "", id="two marks inside one"),
    pytest.param("``@a` @b", "``@a` @b", id="two marks that are not closed"),
    pytest.param("an open `@someone", "an open `@someone", id="a mark that is not closed"),
    pytest.param("`closed` then open `@someone", " then open `@someone", id="closed code, then a mark that is not closed"),
    pytest.param("no marks, #12", "no marks, #12", id="no marks"),
    pytest.param("", "", id="an empty line"),
])
def test_what_stands_in_code_marks_is_taken_out_of_a_line_and_an_open_mark_hides_nothing(line, plain):
    assert cleanup.outside_marks(line) == plain


def test_a_block_that_is_not_closed_does_not_read_as_plain_text_with_nothing_in_it():
    assert "@" in cleanup.plain_part("words\n```text\n@someone\n")


def pieces_with(sizes):
    return [cleanup.Piece(f"python/{rule}", looked_at=9, changed=["a.py"], added=1, removed=1, checks={"tree and comments": "read in each file"},
                          patch=f"diff --git a/a.py b/a.py\n+{rule} " + "x" * size + "\n")
            for rule, size in zip(cleanup.PYTHON_RULES, sizes)]


def test_whole_patches_go_into_the_text_as_many_as_fit_and_each_other_piece_is_named():
    pieces = pieces_with([2000, 30000, 2000, 30000, 2000])
    text = cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces, limit=40000)
    assert len(text) <= 40000
    for piece in pieces:
        whole = piece.patch.strip("\n") in text
        assert whole is (piece.name != "python/W391"), piece.name
    assert "Only in the files of the run, because the text of an issue has a size limit: `python/W391`." in text.splitlines()
    # Every row of the table is there, also for the piece whose patch is not.
    assert text.count("| offered |") == 5


def test_no_patch_is_cut_and_the_text_is_never_over_the_limit():
    # GitHub takes a text of 65,536 characters at most.
    assert cleanup.LIMIT < 65536
    for limit in (3000, 5000, 9000, 20000, 70000):
        pieces = pieces_with([1500, 1500, 1500, 1500, 1500])
        text = cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces, limit=limit)
        assert len(text) <= limit
        inside = sum(1 for piece in pieces if piece.patch.strip("\n") in text)
        named = re.search(r"^Only in the files of the run, because the text of an issue has a size limit: (.+)\.$", text, re.MULTILINE)
        assert inside + (named.group(1).count("`") // 2 if named else 0) == 5
        assert not re.search(r"x{10,}(?<!x{1500})\n", text), "a patch was cut"


def test_a_text_that_cannot_be_made_short_enough_is_not_judged():
    with pytest.raises(cleanup.NotJudged, match="the text of the issue has \\d+ characters, and the limit is 500"):
        cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces_with([10, 10, 10, 10, 10]), limit=500)


def test_a_long_list_of_what_is_left_out_is_cut_with_a_word_and_the_file_of_the_run_holds_all_of_it(made):
    files = {f"pkg/unused_{n:03}.py": "import os\n" for n in range(cleanup.LEFT_OUT_LINES + 30)}
    run = made(files=files).make()
    assert run.status == 0, run.printed
    assert len(run.piece("python/F401")["left_out"]) == cleanup.LEFT_OUT_LINES + 30
    assert "And 31 more line(s)." in run.text.splitlines()
    assert "The whole list is in the file `left-out.txt` of the run." in run.text.splitlines()
    whole = (run.out / "left-out.txt").read_text()
    assert whole.count("the syntax tree is not the same after the fix") == cleanup.LEFT_OUT_LINES + 30
    assert f"pkg/unused_{cleanup.LEFT_OUT_LINES + 29:03}.py" in whole
    assert f"pkg/unused_{cleanup.LEFT_OUT_LINES + 29:03}.py" not in run.text


def test_a_long_line_of_a_tool_is_cut_with_a_word():
    piece = cleanup.Piece("go/proxy/any", looked_at=1, left_out={"a.go": "the piece fails a check"}, said=["y" * 1000])
    text = cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, [piece])
    (line,) = [line for line in text.splitlines() if "yyy" in line]
    assert line.endswith(f" [and {1000 + len('  | ') - cleanup.LEFT_OUT_WIDTH} more characters]")
    assert len(line) < cleanup.LEFT_OUT_WIDTH + 40


def test_the_page_of_the_run_says_what_was_made_and_holds_the_text(made):
    run = made().make()
    page = run.page.read_text()
    assert page.splitlines()[0] == "### The weekly cleanup"
    assert page.splitlines()[2] == ("**Made.** 15 piece(s): 1 offered, 3 held, 0 left out, 11 with nothing to change. The text of the issue, "
                                    "as the job would write it, follows.")
    assert run.text in page


# --- the one issue -------------------------------------------------------------------------------------------------

def issue(number, state="open", login="github-actions[bot]", body=None, **more):
    return {"number": number, "state": state, "user": {"login": login, "type": "Bot"}, "body": cleanup.MARK + "\nan older text\n" if body is None else body, **more}


# What the script asks first, and the second page of that list.
LIST = "/repos/o/r/issues?state=all&per_page=100&creator=github-actions%5Bbot%5D"
SECOND_PAGE = LIST + "&page=2"


class GitHub(http.server.BaseHTTPRequestHandler):
    """A stand-in for GitHub: it lists the issues of the plan, takes a new text or a new issue, and keeps every call.

    No header of an answer is made from what a request holds: the address of the next page and the address that a
    302 points to are fixed texts of this file, with the port of the stand-in.
    """

    seen: list = []
    issues: list = []
    fails: dict = {}
    pages = 1
    next_page = ""

    def answer(self, status, body, link="", location=""):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        if location:
            self.send_header("Location", location)
        if link:
            self.send_header("Link", link)
        self.end_headers()
        self.wfile.write(data)

    def handle_call(self, method):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length)) if length else None
        GitHub.seen.append((method, self.path, self.headers.get("Authorization"), body))
        if method in GitHub.fails:
            elsewhere = f"http://127.0.0.1:{self.server.server_port}/elsewhere" if GitHub.fails[method] == 302 else ""
            return self.answer(GitHub.fails[method], {"message": "a planted failure"}, location=elsewhere)
        if method == "GET":
            page = 2 if self.path == SECOND_PAGE else 1
            share = GitHub.issues[page - 1::GitHub.pages]
            more = f'<http://127.0.0.1:{self.server.server_port}{SECOND_PAGE}>; rel="next"' if page < GitHub.pages else ""
            return self.answer(200, share, GitHub.next_page or more)
        return self.answer(201 if method == "POST" else 200, {"number": 77})

    def do_GET(self):
        self.handle_call("GET")

    def do_POST(self):
        self.handle_call("POST")

    def do_PATCH(self):
        self.handle_call("PATCH")

    def do_PUT(self):
        self.handle_call("PUT")

    def do_DELETE(self):
        self.handle_call("DELETE")

    def log_message(self, *_args):
        pass


@pytest.fixture
def github():
    GitHub.seen, GitHub.issues, GitHub.fails, GitHub.pages, GitHub.next_page = [], [], {}, 1, ""
    server = http.server.HTTPServer(("127.0.0.1", 0), GitHub)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    thread.join(timeout=10)
    # What a test gave the stand-in does not reach the next test.
    GitHub.seen, GitHub.issues, GitHub.fails, GitHub.pages, GitHub.next_page = [], [], {}, 1, ""


class Written:
    """A folder as `make` leaves it, and what `write` did with it."""

    def __init__(self, tmp_path, monkeypatch, capsys, api, anything=True, text=None, one_piece=False, token=TOKEN, repo="o/r"):
        self.folder, self.page = tmp_path / "made", tmp_path / "page.md"
        self.folder.mkdir()
        self.text = cleanup.MARK + "\nMade on 2026-10-12 for commit `cccccccccccc`.\n\n```diff\n+@someone #1\n```\n" if text is None else text
        (self.folder / "issue.md").write_text(self.text)
        (self.folder / "result.json").write_text(json.dumps({"anything": anything, "one_piece": one_piece, "title": cleanup.TITLE}))
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(self.page))
        for name, value in (("GITHUB_TOKEN", token), ("GITHUB_REPOSITORY", repo)):
            monkeypatch.setenv(name, value) if value else monkeypatch.delenv(name, raising=False)
        # The stand-in takes the place of GitHub's address inside the test. The script has no option for that.
        monkeypatch.setattr(cleanup, "GITHUB_API", api)
        self.status = cleanup.main(["write", "--from", str(self.folder)])
        printed = capsys.readouterr()
        self.printed = printed.out + printed.err
        self.said = self.page.read_text() if self.page.exists() else ""

    @property
    def writes(self):
        return [(method, path, body) for method, path, _token, body in GitHub.seen if method != "GET"]


@pytest.fixture
def written(tmp_path, monkeypatch, capsys, github):
    return lambda **how: Written(tmp_path, monkeypatch, capsys, how.pop("api", github), **how)


@pytest.mark.parametrize("issues, anything", [
    pytest.param([issue(12)], True, id="one open issue"),
    pytest.param([issue(12)], False, id="a week with nothing to clean"),
    pytest.param([issue(30, state="closed"), issue(12)], True, id="a closed and an open issue"),
])
def test_the_text_of_the_one_open_issue_is_replaced_and_nothing_else_of_it_is_changed(written, issues, anything, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", issues)
    done = written(anything=anything)
    assert done.status == 0, done.printed
    assert [(method, path, token) for method, path, token, _body in GitHub.seen] == [("GET", LIST, f"Bearer {TOKEN}"), ("PATCH", "/repos/o/r/issues/12", f"Bearer {TOKEN}")]
    assert done.writes == [("PATCH", "/repos/o/r/issues/12", {"body": done.text})]
    assert "weekly cleanup: The text of issue 12 was replaced." in done.printed
    assert done.said.splitlines()[2] == "**Made.** The text of issue 12 was replaced."


def test_with_no_issue_and_something_to_clean_one_issue_is_made_with_the_title_and_the_text_only(written):
    done = written()
    assert done.status == 0, done.printed
    assert done.writes == [("POST", "/repos/o/r/issues", {"title": cleanup.TITLE, "body": done.text})]
    assert "There was no issue with the mark, so issue 77 was made." in done.printed


def test_with_no_issue_and_nothing_to_clean_none_is_made(written):
    done = written(anything=False)
    assert done.status == 0, done.printed
    assert done.writes == []
    assert "There is nothing to clean and no issue with the mark, so none was made." in done.said


@pytest.mark.parametrize("anything", [pytest.param(True, id="something to clean"), pytest.param(False, id="nothing to clean")])
def test_a_closed_issue_stays_closed_and_nothing_is_written_and_the_run_says_so(written, anything, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12, state="closed")])
    done = written(anything=anything)
    assert done.status == 0, done.printed
    assert done.writes == []
    assert done.said.splitlines()[2] == ("**Made.** Issue 12 is closed, so nothing was written. Closing the issue stops the weekly text; "
                                         "open it again to start it.")


def test_two_open_issues_with_the_mark_are_a_finding_and_nothing_is_written(written, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12), issue(30)])
    done = written()
    assert done.status == 1
    assert done.writes == []
    assert "weekly cleanup: a finding: 2 open issues carry the mark of the weekly cleanup (numbers 12, 30)" in done.printed
    assert "Fix: close all but one." in done.printed
    assert done.said.splitlines()[2].startswith("**A finding.**")
    assert "numbers 12, 30" not in cleanup.plain_part(done.said)


@pytest.mark.parametrize("other", [
    pytest.param(issue(5, login="a-stranger"), id="made by a person"),
    pytest.param(issue(5, login="inferstep-atlas-bot[bot]"), id="made by another app"),
    pytest.param(issue(5, pull_request={"url": "x"}), id="a pull request"),
    pytest.param(issue(5, body="some words\n<!-- weekly-cleanup: a job writes this text each week; a change by hand is lost -->\n"),
                 id="the mark after the first line"),
    pytest.param(issue(5, body=""), id="an empty text"),
    pytest.param({**issue(5), "body": None}, id="no text"),
    pytest.param({**issue(5), "user": None}, id="no author"),
])
def test_only_an_issue_that_the_jobs_own_account_made_and_that_starts_with_the_mark_counts(written, other, monkeypatch):
    # GitHub is asked for the issues of that account only. The script does not rely on that: it reads who made each.
    monkeypatch.setattr(GitHub, "issues", [other])
    done = written()
    assert done.status == 0, done.printed
    assert done.writes == [("POST", "/repos/o/r/issues", {"title": cleanup.TITLE, "body": done.text})]


def test_every_page_of_the_list_is_read(written, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(5, login="a-stranger"), issue(41)])
    monkeypatch.setattr(GitHub, "pages", 2)
    done = written()
    assert [path for method, path, _token, _body in GitHub.seen if method == "GET"] == [LIST, SECOND_PAGE]
    assert done.writes == [("PATCH", "/repos/o/r/issues/41", {"body": done.text})]


def test_a_next_page_at_another_address_is_not_followed_with_the_token(written, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    monkeypatch.setattr(GitHub, "next_page", '<http://127.0.0.1:9/repos/o/r/issues?page=2>; rel="next"')
    done = written()
    assert done.status == 2
    assert "GitHub named a next page at another address" in done.printed
    assert len(GitHub.seen) == 1
    assert done.writes == []


@pytest.mark.parametrize("fails, says", [
    pytest.param({"GET": 403}, "GitHub answered 403 to GET", id="403 to GET"),
    pytest.param({"GET": 500}, "GitHub answered 500 to GET", id="500 to GET"),
    pytest.param({"PATCH": 403}, "GitHub answered 403 to PATCH", id="403 to PATCH"),
    pytest.param({"PATCH": 422}, "GitHub answered 422 to PATCH", id="422 to PATCH"),
])
def test_when_github_refuses_nothing_is_judged_and_that_is_not_a_pass(written, fails, says, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    monkeypatch.setattr(GitHub, "fails", fails)
    done = written()
    assert done.status == 2
    assert f"weekly cleanup: not judged: {says}" in done.printed
    assert done.said.splitlines()[2].startswith("**Not judged.**")
    assert TOKEN not in done.printed + done.said


def test_an_answer_that_points_to_another_address_is_not_followed_with_the_token(written, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    monkeypatch.setattr(GitHub, "fails", {"GET": 302})
    done = written()
    assert done.status == 2
    assert "GitHub answered 302 to GET" in done.printed
    assert [path for _method, path, _token, _body in GitHub.seen] == [LIST]


@pytest.mark.parametrize("number", [
    pytest.param("12/../../pulls/3", id="a path"),
    pytest.param("12?state=closed", id="a query"),
    pytest.param(None, id="none"),
    pytest.param([12], id="a list"),
])
def test_a_number_of_an_issue_that_is_no_number_is_not_put_into_an_address(written, number, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(number)])
    done = written()
    assert done.status == 2
    assert done.writes == []


def test_when_github_gives_no_answer_nothing_is_judged(written):
    done = written(api="http://127.0.0.1:9")
    assert done.status == 2
    assert "GitHub gave no answer that can be read to GET http://127.0.0.1:9/repos/o/r/issues" in done.printed


def test_the_script_names_one_address_for_the_token_and_takes_no_option_for_another(tmp_path, capsys):
    assert cleanup.GITHUB_API == "https://api.github.com"
    source = SCRIPT.read_text()
    # The two addresses of the script: where the token goes, and the usual form of the address of the runs.
    assert sorted(set(re.findall(r"https?://[^/\"' ]+", source))) == ["https://api.github.com", "https://github\\.com"]
    assert "http://" not in source
    with pytest.raises(SystemExit) as stopped:
        cleanup.main(["write", "--from", str(tmp_path), "--api", "https://api.github.example"])
    assert stopped.value.code == 2
    assert "unrecognized arguments: --api" in capsys.readouterr().err


def test_a_test_does_not_see_the_names_that_a_runner_gives_its_jobs():
    # The fixture above took them out. Where it does not, this test is red on a runner and green on a desk.
    assert [name for name in os.environ if name.startswith("GITHUB_") or name == "CI"] == []


@pytest.mark.parametrize("token, repo", [
    pytest.param("", "o/r", id="no token"),
    pytest.param(TOKEN, "", id="no repository"),
    pytest.param(TOKEN, "o/r/../x", id="a repository with a path"),
    pytest.param(TOKEN, "o r", id="a repository with a space"),
])
def test_without_the_token_or_the_name_of_the_repository_nothing_is_asked(written, token, repo):
    done = written(token=token, repo=repo)
    assert done.status == 2
    assert "GITHUB_TOKEN or GITHUB_REPOSITORY is not set" in done.printed
    assert GitHub.seen == []


@pytest.mark.parametrize("how", [
    pytest.param({"one_piece": True}, id="a text for one piece"),
    pytest.param({"text": "Made on 2026-10-12.\n"}, id="no mark"),
    pytest.param({"text": cleanup.MARK + "\n" + "x" * 70000}, id="over the limit"),
])
def test_a_text_for_one_piece_or_with_no_mark_or_over_the_limit_is_never_written_into_the_issue(written, how, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    done = written(**how)
    assert done.status == 2
    assert "the text in the folder is not the weekly text" in done.printed
    assert GitHub.seen == []


@pytest.mark.parametrize("more", [
    pytest.param("\n@someone look here\n", id="a mention"),
    pytest.param("\nSee #12.\n", id="a link to an issue"),
    pytest.param("\n```text\nnot closed @someone\n", id="in a block that is not closed"),
])
def test_a_text_with_a_mention_or_a_link_to_an_issue_outside_code_is_not_written(written, more, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    done = written(text=cleanup.MARK + "\nMade on 2026-10-12.\n" + more)
    assert done.status == 2
    assert "holds an `@` or a `#` with a number outside code marks and fenced blocks" in done.printed
    assert "Nothing was written." in done.printed
    assert GitHub.seen == []


def test_the_same_marks_inside_code_are_written(written, monkeypatch):
    monkeypatch.setattr(GitHub, "issues", [issue(12)])
    done = written(text=cleanup.MARK + "\nMade on 2026-10-12.\n\n`@someone #12`\n\n````diff\n+@someone\n```\n+Closes #5\n````\n")
    assert done.status == 0, done.printed
    assert len(done.writes) == 1


def test_no_call_changes_a_state_a_title_a_label_or_writes_a_comment():
    source = SCRIPT.read_text()
    # The two places that send: the new text of the issue, and a new issue with the title and the text.
    assert re.findall(r'github\("(?:POST|PATCH|PUT|DELETE)",[^\n]*', source) == [
        'github("PATCH", f"{GITHUB_API}/repos/{repo}/issues/{number}", token, {"body": text})',
        'github("POST", f"{GITHUB_API}/repos/{repo}/issues", token, {"title": TITLE, "body": text})']
    for word in ('"labels"', '"assignees"', '"milestone"', "/comments", "/labels", '"DELETE"', '"PUT"', '"push"', "/pulls", "/git/"):
        assert word not in source, word


def test_the_script_is_on_the_list_of_scripts_that_run_with_a_credential_that_can_write():
    spec = importlib.util.spec_from_file_location("atlas_integrity_for_weekly", ROOT / "scripts" / "integrity_check.py")
    integrity = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = integrity
    spec.loader.exec_module(integrity)
    assert "scripts/weekly_cleanup.py" in integrity.WRITE_CREDENTIAL_SCRIPTS


def test_the_gates_page_names_each_fixer_and_what_is_left_out():
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    section = page.split("\n## The weekly cleanup\n", 1)[1]
    row = {line.split("|")[1].strip(): line for line in section.splitlines() if line.startswith("| ")}
    assert sorted(re.findall(r"`([a-z]+)`", row["Go: `proxy`, `tui`"].split("|")[2].replace("`go fix`", ""))) == sorted(cleanup.GO_FIXERS)
    assert re.findall(r"`([A-Z]\d+)`", row["Python"]) == list(cleanup.PYTHON_RULES)
    for name in cleanup.GO_NOT_RUN:
        assert f"`{name}`" in section.split("Not run, with the reason:", 1)[1].split("\n- ", 1)[0], name
    for words in ("`knip`", "`deadcode`", "They are no proof", "The list of fixers is read again when that version changes",
                  "Closing the issue stops the weekly text"):
        assert words in section, words
