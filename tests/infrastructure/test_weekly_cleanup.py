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
                         ("breaktest_" + fixer, "NOTEST"), ("unformat_" + fixer, "UNFORMATTED")):
            text = text.replace(old, new)
        if text != f.read_text():
            f.write_text(text)
else:
    mark = {{"build": "NOBUILD", "vet": "NOVET", "test": "NOTEST"}}[args[0]]
    if args[0] == "test":
        with open(home / "tested.log", "a") as log:
            log.write(json.dumps({{"in": Path.cwd().name, "holds": sorted(set(w for f in files for w in f.read_text().split() if w.startswith("new_")))}}) + "\n")
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


def load():
    spec = importlib.util.spec_from_file_location("atlas_weekly_cleanup", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


THE_SCRIPT = load()


@pytest.fixture
def cleanup():
    return THE_SCRIPT


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

    def __init__(self, cleanup, tmp_path, monkeypatch, capsys, files=None, fixers=None, tools=("go", "gofmt"), run=A_FEW):
        self.cleanup, self.tmp, self.capsys = cleanup, tmp_path, capsys
        monkeypatch.setattr(cleanup, "GO_FIXERS", run or cleanup.GO_FIXERS)
        self.repo, self.out, self.page = tmp_path / "repo", tmp_path / "out", tmp_path / "page.md"
        self.repo.mkdir()
        (tmp_path / "bin").mkdir()
        for name, text in {**ON_DEV, **(files or {})}.items():
            if text is not None:
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
        self.every_fixer = sorted([*cleanup.GO_FIXERS, *cleanup.GO_NOT_RUN])
        # Only the stand-ins and git are on the path: the real go of this machine cannot be reached.
        (tmp_path / "bin" / "git").symlink_to(shutil.which("git"))
        monkeypatch.setenv("PATH", str(tmp_path / "bin"))
        for name, value in GIT.items():
            monkeypatch.setenv(name, value)
        monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(self.page))

    def make(self, *more):
        self.status = self.cleanup.main(["make", "--root", str(self.repo), "--out", str(self.out), *more])
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
def made(cleanup, tmp_path, monkeypatch, capsys):
    return lambda **how: Made(cleanup, tmp_path, monkeypatch, capsys, **how)


def states(result):
    return {piece["name"]: piece["state"] for piece in result["pieces"] if piece["state"] != "nothing to change"}


def applies_alone(run, name):
    """Whether the patch of a piece applies to the commit with no other piece. Gives the files it changes."""
    fresh = run.tmp / ("fresh-" + name.replace("/", "-"))
    subprocess.run(["git", "clone", "--quiet", str(run.repo), str(fresh)], check=True, timeout=60)
    git(fresh, "apply", "--check", str(run.out / "pieces" / (name.replace("/", "-") + ".patch")))
    git(fresh, "apply", str(run.out / "pieces" / (name.replace("/", "-") + ".patch")))
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

def test_only_the_fixers_on_the_list_run_and_each_runs_on_each_module(made, cleanup):
    whole_list = cleanup.GO_FIXERS
    run = made(run=None).make()
    assert cleanup.GO_FIXERS == whole_list
    ran = {(call["in"], call["args"][1][1:]) for call in run.calls("fix")}
    assert ran == {(module, fixer) for module in cleanup.MODULES for fixer in whole_list}
    assert not {fixer for _module, fixer in ran} & set(cleanup.GO_NOT_RUN)
    for call in run.calls("fix"):
        assert call["args"] == ["fix", call["args"][1], "./..."]


def test_a_fixer_that_this_go_has_and_that_is_on_no_list_is_named_and_not_run(made, cleanup):
    run = made(fixers=[*A_FEW, *cleanup.GO_NOT_RUN, "brandnew", "another"]).make()
    assert run.status == 0, run.printed
    assert run.result["unlisted"] == ["another", "brandnew"]
    assert "This Go has fixers that are on no list of the job, so they were not run: `another`, `brandnew`." in run.text
    assert not [call for call in run.calls("fix") if call["args"][1] in ("-brandnew", "-another")]


def test_with_every_fixer_on_a_list_the_text_names_none(made):
    assert "on no list" not in made().make().text


def test_a_list_of_fixers_that_cannot_be_read_is_not_judged(made):
    run = made(fixers=[]).make()
    assert run.status == 2
    assert "gave no list of fixers that this script can read" in run.printed
    assert not (run.out / "issue.md").exists()


def test_the_lists_of_fixers_do_not_overlap_and_the_three_that_never_run_have_their_reason():
    cleanup = load()
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
    assert len([call for call in run.calls("fix", "-minmax") if call["in"] == "tui"]) >= 3


def test_a_fixer_that_does_not_come_to_an_end_is_left_out_and_named(made, cleanup):
    run = made(files={"tui/view.go": "package main\n// grow_minmax\n"}).make()
    piece = run.piece("go/tui/minmax")
    assert piece["state"] == "left out"
    assert piece["changed"] == []
    assert piece["left_out"] == {"tui/view.go": "the fixer did not come to an end"}
    assert piece["said"] == [f"`go fix -minmax` still changes files after {cleanup.PASSES} runs"]
    assert not (run.out / "pieces" / "go-tui-minmax.patch").exists()
    # The first run and three more: then the script stops asking.
    assert len([call for call in run.calls("fix", "-minmax") if call["in"] == "tui"]) == cleanup.PASSES + 1


@pytest.mark.parametrize("mark, check, checks", [
    ("unformat", "gofmt", {"gofmt": "failed", "build": "not run", "vet": "not run"}),
    ("breakbuild", "build", {"gofmt": "passed", "build": "failed", "vet": "not run"}),
    ("breakvet", "vet", {"gofmt": "passed", "build": "passed", "vet": "failed"}),
])
def test_a_piece_that_fails_a_check_is_left_out_with_what_the_tool_said_and_the_others_are_not(made, mark, check, checks):
    run = made(files={"proxy/tools.go": f"package main\n// old_any {mark}_stringscut\n"}).make()
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


def test_a_file_that_gofmt_named_before_is_no_fault_of_a_piece(made):
    run = made(files={"proxy/quiet.go": "package main\n// UNFORMATTED on dev as it is\n"}).make()
    assert run.piece("go/proxy/any")["checks"]["gofmt"] == "passed"
    assert states(run.result)["go/proxy/any"] == "held"


def test_a_fixer_that_fails_on_its_own_result_is_left_out(made):
    run = made(files={"tui/view.go": "package main\n// breakfix_minmax\n"}).make()
    piece = run.piece("go/tui/minmax")
    assert piece["state"] == "left out"
    assert piece["checks"] == {"fix": "failed"}
    assert piece["left_out"] == {"tui/view.go": "the fixer fails on its own result"}
    assert piece["said"][0] == "`go fix -minmax` ended with status 1 on its own result:"
    assert "this does not compile" in piece["said"][1]


def test_the_tests_of_a_module_run_once_with_its_pieces_that_passed_and_not_with_one_that_failed(made):
    run = made(files={"proxy/tools.go": "package main\n// old_any old_stringscut breakvet_stringscut\n"}).make()
    assert run.piece("go/proxy/stringscut")["state"] == "left out"
    assert run.tested() == [{"in": "proxy", "holds": ["new_any", "new_rangeint"]}, {"in": "tui", "holds": ["new_minmax"]}]


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
        assert piece["checks"]["tests"] == "failed"
        assert set(piece["left_out"].values()) == {"the tests of the module fail with the pieces of the module together"}
        assert not (run.out / "pieces" / (name.replace("/", "-") + ".patch")).exists()
    assert states(run.result)["go/tui/minmax"] == "held"
    # The tests ran again on the files as they are, to tell a fault of a piece from a fault of the branch.
    assert [entry["holds"] for entry in run.tested() if entry["in"] == "proxy"] == [["new_any", "new_rangeint"], []]


def test_the_tests_of_a_module_run_with_the_flags_of_the_go_test_jobs(made, cleanup):
    run = made().make()
    assert [["go", *call["args"]] for call in run.calls("test")] == [cleanup.GO_TEST, cleanup.GO_TEST]
    gates = (ROOT / "scripts" / "production-readiness.py").read_text(encoding="utf-8")
    assert "(" + ", ".join(f'"{word}"' for word in cleanup.GO_TEST) + ")," in gates, "the Go test gate runs other flags"
    assert "-race" in cleanup.GO_TEST
    assert "-count=1" in cleanup.GO_TEST


def test_a_module_with_nothing_to_change_gets_no_test_run(made):
    run = made(files={"tui/view.go": "package main\n"}).make()
    assert [entry["in"] for entry in run.tested()] == ["proxy"]


@pytest.mark.parametrize("files, says", [
    ({"proxy/quiet.go": "package main\n// NOTEST\n"}, "the tests of proxy on the files as they are: status 1."),
    ({"proxy/quiet.go": "package main\n// NOVET\n"}, "go vet on proxy as it is: status 1."),
    ({"proxy/quiet.go": "package main\n// NOFIX\n"}, "`go fix -any` on `proxy` as it is: status 1."),
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


def test_a_fix_that_takes_a_comment_away_is_left_out_though_the_syntax_tree_is_the_same(made, cleanup, monkeypatch):
    # The fix of this rule removes a `noqa` mark that the rule set of the run does not need. Another check may need it.
    monkeypatch.setattr(cleanup, "PYTHON_RULES", ("RUF100",))
    run = made(files={"pkg/marked.py": "import os  # noqa: E402\n\nprint(os.sep)\n"}).make("--piece", "python/RUF100")
    assert run.status == 0, run.printed
    assert run.piece("python/RUF100")["left_out"] == {"pkg/marked.py": "a comment is not the same after the fix"}
    assert run.piece("python/RUF100")["changed"] == []


@pytest.mark.parametrize("old, new, why", [
    ("x = 1   \n", "x = 1\n", ""),
    ("x = 1", "x = 1\n", ""),
    ("x = 1  # a note   \n", "x = 1  # a note\n", ""),
    ("x = (1,\n     2)\n", "x = (1, 2)\n", ""),
    ("import os\nx = 1\n", "x = 1\n", "the syntax tree is not the same after the fix"),
    ("x = f'a'\n", "x = 'a'\n", "the syntax tree is not the same after the fix"),
    ("x = 1  # noqa: E402\n", "x = 1\n", "a comment is not the same after the fix"),
    ("x = 1  # type: ignore\n", "x = 1  # type: ignore[assignment]\n", "a comment is not the same after the fix"),
    ("# one\n# two\nx = 1\n", "# two\n# one\nx = 1\n", "a comment is not the same after the fix"),
    ("x = 1\n", "x = = 1\n", "this Python cannot read the file before or after the fix"),
    ("print 'x'   \n", "print 'x'\n", "this Python cannot read the file before or after the fix"),
])
def test_a_fix_is_taken_only_when_the_syntax_tree_and_the_comments_are_the_same(cleanup, old, new, why):
    assert cleanup.differs(old.encode(), new.encode()) == why


def test_a_file_whose_name_holds_spaces_and_marks_is_fixed_and_named_like_any_other(made):
    run = made(files={"pkg/a file @someone #1.py": "x = 1   \n", "pkg/unused @other #2.py": "import os\n"}).make()
    assert run.status == 0, run.printed
    assert "pkg/a file @someone #1.py" in run.piece("python/W291")["changed"]
    assert run.piece("python/F401")["left_out"] == {"pkg/unused @other #2.py": "the syntax tree is not the same after the fix"}
    assert applies_alone(run, "python/W291") == ["pkg/a file @someone #1.py", "pkg/spaces.py"]


# --- held ----------------------------------------------------------------------------------------------------------

def test_the_go_pieces_are_held_with_these_words_and_no_others(cleanup):
    assert cleanup.HELD == {"go": "Held by a maintainer: open work is written against these Go files as they are, and a rewrite "
                                  "of the same files now would make it not apply."}


def test_a_held_piece_is_listed_with_its_numbers_and_checks_and_is_not_among_the_patches(made, cleanup):
    run = made().make()
    assert "| `go/proxy/any` | 3 | 2 | 1 | 0 | +2 -2 | gofmt: passed, build: passed, vet: passed, tests: passed | held |" in run.text
    assert cleanup.HELD["go"] in run.text.splitlines()
    patches = run.text.split("## The patches", 1)[1]
    assert "### `python/W291`" in patches
    assert "go/" not in patches
    assert "new_any" not in run.text
    # The patch of a held piece is made all the same, for the day the hold is lifted.
    assert "new_any" in run.patch("go/proxy/any")


def test_with_the_hold_taken_off_the_list_the_go_pieces_are_offered(made, cleanup, monkeypatch):
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


def test_an_error_of_the_script_itself_is_not_judged_and_never_a_finding(made, cleanup, monkeypatch):
    def broken(*_args):
        raise TypeError("a fault of the script")

    monkeypatch.setattr(cleanup, "issue_text", broken)
    run = made().make("--piece", "python/W291")
    assert run.status == 2
    assert "weekly cleanup: not judged: an error of the script itself: TypeError('a fault of the script')" in run.printed
    assert run.page.read_text().splitlines()[2].startswith("**Not judged.**")


@pytest.mark.parametrize("name", ["go/proxy/omitzero", "go/sandbox/any", "python/E501", "rangeint", ""])
def test_a_name_that_is_no_piece_is_refused_with_the_names_that_are(made, name):
    run = made().make(f"--piece={name}") if name else made().make("--piece", "none")
    assert run.status == 2
    assert "is no piece. Fix: give one of: go/proxy/any, " in run.printed
    assert run.calls() == []


# --- the text ------------------------------------------------------------------------------------------------------

def test_the_text_starts_with_the_mark_and_says_who_applies_a_piece_and_how_to_stop_it(made, cleanup):
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


def test_in_a_job_the_text_names_the_runs_of_the_workflow_and_only_at_githubs_usual_address(made, cleanup, monkeypatch):
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


def outside_code(cleanup, text):
    plain = cleanup.plain_part(text)
    assert "a fenced block is not closed" not in plain
    return plain


def test_nothing_from_a_file_or_a_tool_can_be_read_as_a_mention_or_a_link_to_an_issue(made, cleanup, monkeypatch):
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
    plain = outside_code(cleanup, run.text)
    assert "@" not in plain
    assert not re.search(r"#\d", plain)
    assert "Closes" not in plain
    offered = [name for name, state in states(run.result).items() if state == "offered"]
    assert offered == ["go/proxy/any", "go/proxy/rangeint", "go/tui/minmax", "python/W291"]
    assert [line for line in plain.splitlines() if line.startswith("#")] == ["## The pieces", "## To make one piece", "## Left out, and why",
                                                                              "## The patches"] + ["### "] * len(offered)
    page = outside_code(cleanup, run.page.read_text())
    assert "@" not in page
    assert not re.search(r"#\d", page)


@pytest.mark.parametrize("text, fence", [("no marks", "```"), ("three ``` in it", "````"), ("a line\n``````\nof six", "```````")])
def test_the_fence_of_a_block_is_longer_than_any_run_of_marks_in_it(cleanup, text, fence):
    block = cleanup.fenced(text)
    assert block.splitlines()[0] == fence + "text"
    assert block.splitlines()[-1] == fence
    assert cleanup.plain_part("before\n" + block + "after") == "before\nafter"


@pytest.mark.parametrize("line, plain", [
    ("words `in marks` and after", "words  and after"),
    ("`one` and `two`", " and "),
    ("``two ` marks inside`` after", " after"),
    # A run of another length does not end the code: only the next run of the same length does.
    ("`@a`` @b`", ""),
    ("``@a` @b", "``@a` @b"),
    ("an open `@someone", "an open `@someone"),
    ("`closed` then open `@someone", " then open `@someone"),
    ("no marks, #12", "no marks, #12"),
    ("", ""),
])
def test_what_stands_in_code_marks_is_taken_out_of_a_line_and_an_open_mark_hides_nothing(cleanup, line, plain):
    assert cleanup.outside_marks(line) == plain


def test_a_block_that_is_not_closed_does_not_read_as_plain_text_with_nothing_in_it(cleanup):
    assert "@" in cleanup.plain_part("words\n```text\n@someone\n")


def pieces_with(cleanup, sizes):
    return [cleanup.Piece(f"python/{rule}", looked_at=9, changed=["a.py"], added=1, removed=1, checks={"tree and comments": "read in each file"},
                          patch=f"diff --git a/a.py b/a.py\n+{rule} " + "x" * size + "\n")
            for rule, size in zip(cleanup.PYTHON_RULES, sizes)]


def test_whole_patches_go_into_the_text_as_many_as_fit_and_each_other_piece_is_named(cleanup):
    pieces = pieces_with(cleanup, [2000, 30000, 2000, 30000, 2000])
    text = cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces, limit=40000)
    assert len(text) <= 40000
    for piece in pieces:
        whole = piece.patch.strip("\n") in text
        assert whole is (piece.name != "python/W391"), piece.name
    assert "Only in the files of the run, because the text of an issue has a size limit: `python/W391`." in text.splitlines()
    # Every row of the table is there, also for the piece whose patch is not.
    assert text.count("| offered |") == 5


def test_no_patch_is_cut_and_the_text_is_never_over_the_limit(cleanup):
    for limit in (3000, 5000, 9000, 20000, 70000):
        pieces = pieces_with(cleanup, [1500, 1500, 1500, 1500, 1500])
        text = cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces, limit=limit)
        assert len(text) <= limit
        inside = sum(1 for piece in pieces if piece.patch.strip("\n") in text)
        named = re.search(r"^Only in the files of the run, because the text of an issue has a size limit: (.+)\.$", text, re.MULTILINE)
        assert inside + (named.group(1).count("`") // 2 if named else 0) == 5
        assert not re.search(r"x{10,}(?<!x{1500})\n", text), "a patch was cut"


def test_a_text_that_cannot_be_made_short_enough_is_not_judged(cleanup):
    with pytest.raises(cleanup.NotJudged, match="the text of the issue has \\d+ characters, and the limit is 500"):
        cleanup.issue_text({"date": "2026-10-12", "commit": "c" * 40}, pieces_with(cleanup, [10, 10, 10, 10, 10]), limit=500)


def test_the_limit_of_the_script_is_under_the_limit_of_github(cleanup):
    assert cleanup.LIMIT < 65536


def test_a_long_list_of_what_is_left_out_is_cut_with_a_word_and_the_file_of_the_run_holds_all_of_it(made, cleanup):
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


def test_a_long_line_of_a_tool_is_cut_with_a_word(cleanup):
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
    return {"number": number, "state": state, "user": {"login": login, "type": "Bot"}, "body": THE_SCRIPT.MARK + "\nan older text\n" if body is None else body, **more}


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

    next_page = ""

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

    def __init__(self, cleanup, tmp_path, monkeypatch, capsys, api, anything=True, text=None, one_piece=False, token=TOKEN, repo="o/r"):
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
def written(cleanup, tmp_path, monkeypatch, capsys, github):
    return lambda **how: Written(cleanup, tmp_path, monkeypatch, capsys, how.pop("api", github), **how)


def test_the_text_of_the_one_open_issue_is_replaced_and_nothing_else_of_it_is_changed(written):
    GitHub.issues = [issue(12)]
    done = written()
    assert done.status == 0, done.printed
    assert [(method, path, token) for method, path, token, _body in GitHub.seen] == [("GET", LIST, f"Bearer {TOKEN}"), ("PATCH", "/repos/o/r/issues/12", f"Bearer {TOKEN}")]
    assert done.writes == [("PATCH", "/repos/o/r/issues/12", {"body": done.text})]
    assert "weekly cleanup: The text of issue 12 was replaced." in done.printed
    assert done.said.splitlines()[2] == "**Made.** The text of issue 12 was replaced."


def test_with_no_issue_and_something_to_clean_one_issue_is_made_with_the_title_and_the_text_only(written, cleanup):
    done = written()
    assert done.status == 0, done.printed
    assert done.writes == [("POST", "/repos/o/r/issues", {"title": cleanup.TITLE, "body": done.text})]
    assert "There was no issue with the mark, so issue 77 was made." in done.printed


def test_with_no_issue_and_nothing_to_clean_none_is_made(written):
    done = written(anything=False)
    assert done.status == 0, done.printed
    assert done.writes == []
    assert "There is nothing to clean and no issue with the mark, so none was made." in done.said


def test_in_a_week_with_nothing_to_clean_the_text_of_the_open_issue_is_replaced_all_the_same(written):
    GitHub.issues = [issue(12)]
    done = written(anything=False)
    assert done.writes == [("PATCH", "/repos/o/r/issues/12", {"body": done.text})]


@pytest.mark.parametrize("anything", [True, False])
def test_a_closed_issue_stays_closed_and_nothing_is_written_and_the_run_says_so(written, anything):
    GitHub.issues = [issue(12, state="closed")]
    done = written(anything=anything)
    assert done.status == 0, done.printed
    assert done.writes == []
    assert done.said.splitlines()[2] == ("**Made.** Issue 12 is closed, so nothing was written. Closing the issue stops the weekly text; "
                                         "open it again to start it.")


def test_with_an_open_and_a_closed_issue_the_open_one_gets_the_text(written):
    GitHub.issues = [issue(12, state="closed"), issue(30)]
    done = written()
    assert done.writes == [("PATCH", "/repos/o/r/issues/30", {"body": done.text})]


def test_two_open_issues_with_the_mark_are_a_finding_and_nothing_is_written(written, cleanup):
    GitHub.issues = [issue(12), issue(30)]
    done = written()
    assert done.status == 1
    assert done.writes == []
    assert "weekly cleanup: a finding: 2 open issues carry the mark of the weekly cleanup (numbers 12, 30)" in done.printed
    assert "Fix: close all but one." in done.printed
    assert done.said.splitlines()[2].startswith("**A finding.**")
    assert "numbers 12, 30" not in cleanup.plain_part(done.said)


@pytest.mark.parametrize("other", [
    issue(5, login="a-stranger"),
    issue(5, login="inferstep-atlas-bot[bot]"),
    issue(5, pull_request={"url": "x"}),
    issue(5, body="some words\n<!-- weekly-cleanup: a job writes this text each week; a change by hand is lost -->\n"),
    issue(5, body=""),
    {**issue(5), "body": None},
    {**issue(5), "user": None},
])
def test_only_an_issue_that_the_jobs_own_account_made_and_that_starts_with_the_mark_counts(written, cleanup, other):
    # GitHub is asked for the issues of that account only. The script does not rely on that: it reads who made each.
    GitHub.issues = [other]
    done = written()
    assert done.status == 0, done.printed
    assert done.writes == [("POST", "/repos/o/r/issues", {"title": cleanup.TITLE, "body": done.text})]


def test_every_page_of_the_list_is_read(written):
    GitHub.issues, GitHub.pages = [issue(5, login="a-stranger"), issue(41)], 2
    done = written()
    assert [path for method, path, _token, _body in GitHub.seen if method == "GET"] == [LIST, SECOND_PAGE]
    assert done.writes == [("PATCH", "/repos/o/r/issues/41", {"body": done.text})]


def test_a_next_page_at_another_address_is_not_followed_with_the_token(written):
    GitHub.issues, GitHub.next_page = [issue(12)], '<http://127.0.0.1:9/repos/o/r/issues?page=2>; rel="next"'
    done = written()
    assert done.status == 2
    assert "GitHub named a next page at another address" in done.printed
    assert len(GitHub.seen) == 1
    assert done.writes == []


@pytest.mark.parametrize("fails, says", [({"GET": 403}, "GitHub answered 403 to GET"), ({"GET": 500}, "GitHub answered 500 to GET"),
                                         ({"PATCH": 403}, "GitHub answered 403 to PATCH"), ({"PATCH": 422}, "GitHub answered 422 to PATCH")])
def test_when_github_refuses_nothing_is_judged_and_that_is_not_a_pass(written, fails, says):
    GitHub.issues, GitHub.fails = [issue(12)], fails
    done = written()
    assert done.status == 2
    assert f"weekly cleanup: not judged: {says}" in done.printed
    assert done.said.splitlines()[2].startswith("**Not judged.**")
    assert TOKEN not in done.printed + done.said


def test_an_answer_that_points_to_another_address_is_not_followed_with_the_token(written):
    GitHub.issues, GitHub.fails = [issue(12)], {"GET": 302}
    done = written()
    assert done.status == 2
    assert "GitHub answered 302 to GET" in done.printed
    assert [path for _method, path, _token, _body in GitHub.seen] == [LIST]


@pytest.mark.parametrize("number", ["12/../../pulls/3", "12?state=closed", None, [12]])
def test_a_number_of_an_issue_that_is_no_number_is_not_put_into_an_address(written, number):
    GitHub.issues = [issue(number)]
    done = written()
    assert done.status == 2
    assert done.writes == []


def test_when_github_gives_no_answer_nothing_is_judged(written):
    done = written(api="http://127.0.0.1:9")
    assert done.status == 2
    assert "GitHub gave no answer that can be read to GET http://127.0.0.1:9/repos/o/r/issues" in done.printed


def test_the_script_names_one_address_for_the_token_and_takes_no_option_for_another(tmp_path, capsys):
    cleanup = load()
    assert cleanup.GITHUB_API == "https://api.github.com"
    source = SCRIPT.read_text()
    # The two addresses of the script: where the token goes, and the usual form of the address of the runs.
    assert sorted(set(re.findall(r"https?://[^/\"' ]+", source))) == ["https://api.github.com", "https://github\\.com"]
    assert "http://" not in source
    with pytest.raises(SystemExit) as stopped:
        cleanup.main(["write", "--from", str(tmp_path), "--api", "https://api.github.example"])
    assert stopped.value.code == 2
    assert "unrecognized arguments: --api" in capsys.readouterr().err
    assert GitHub.seen == []


def test_a_test_does_not_see_the_names_that_a_runner_gives_its_jobs():
    # The fixture above took them out. Where it does not, this test is red on a runner and green on a desk.
    assert [name for name in os.environ if name.startswith("GITHUB_") or name == "CI"] == []


@pytest.mark.parametrize("token, repo", [("", "o/r"), (TOKEN, ""), (TOKEN, "o/r/../x"), (TOKEN, "o r")])
def test_without_the_token_or_the_name_of_the_repository_nothing_is_asked(written, token, repo):
    done = written(token=token, repo=repo)
    assert done.status == 2
    assert "GITHUB_TOKEN or GITHUB_REPOSITORY is not set" in done.printed
    assert GitHub.seen == []


def test_a_text_for_one_piece_is_never_written_into_the_issue(written):
    GitHub.issues = [issue(12)]
    done = written(one_piece=True)
    assert done.status == 2
    assert "the text in the folder is not the weekly text" in done.printed
    assert GitHub.seen == []


@pytest.mark.parametrize("text", ["Made on 2026-10-12.\n", "x" * 70000])
def test_a_text_with_no_mark_or_over_the_limit_is_not_written(written, cleanup, text):
    done = written(text=text if text.startswith("Made") else cleanup.MARK + "\n" + text)
    assert done.status == 2
    assert GitHub.seen == []


@pytest.mark.parametrize("more", ["\n@someone look here\n", "\nSee #12.\n", "\n```text\nnot closed @someone\n"])
def test_a_text_with_a_mention_or_a_link_to_an_issue_outside_code_is_not_written(written, cleanup, more):
    GitHub.issues = [issue(12)]
    done = written(text=cleanup.MARK + "\nMade on 2026-10-12.\n" + more)
    assert done.status == 2
    assert "holds an `@` or a `#` with a number outside code marks and fenced blocks" in done.printed
    assert "Nothing was written." in done.printed
    assert GitHub.seen == []


def test_the_same_marks_inside_code_are_written(written, cleanup):
    GitHub.issues = [issue(12)]
    done = written(text=cleanup.MARK + "\nMade on 2026-10-12.\n\n`@someone #12`\n\n````diff\n+@someone\n```\n+Closes #5\n````\n")
    assert done.status == 0, done.printed
    assert len(done.writes) == 1


def test_no_call_changes_a_state_a_title_a_label_or_writes_a_comment(written, cleanup):
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
    cleanup = load()
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
