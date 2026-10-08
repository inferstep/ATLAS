#!/usr/bin/env python3
"""What the fixers would change on `dev`, as the text of one issue. No fixer changes the repository.

  scripts/weekly_cleanup.py make --out <folder>                  run each fixer on a copy, check each piece, write the text
  scripts/weekly_cleanup.py make --out <folder> --piece <name>   the same for one piece, with a test run of its own
  scripts/weekly_cleanup.py write --from <folder>                put the text into the one issue (the weekly job only)

A piece is one fixer on one part of the repository: `go/proxy/rangeint`, `python/W291`. `make` works on a copy of
the commit that is checked out, and changes no file of the checkout. For each piece it writes a patch that applies
to that commit alone, into <folder>/pieces/. It checks a piece before it offers it:

  - a Go piece: `gofmt` names no file that it did not name before, `go build` and `go vet` pass, and the tests of
    the module pass with all its pieces together (with --piece: with that piece alone). That is a test result. It
    is no proof that the code does what it did before.
  - a Python piece: in each file the syntax tree is the same as before, and so are the comments. A file where one
    of the two differs is left out of the piece and named.

A fixer runs only when it is on a list below. A fixer that a newer Go brings is named in the text and not run.

`write` talks to GitHub's own address and has no option that names another one. It finds the issue among those
that the job's own account made, by the mark in the first line of its text, and replaces the text. It changes nothing else of the issue: not its state, its title or its labels. With no
issue it makes one, when there is something to clean. A closed issue stays closed and nothing is written.

In the text, everything that comes from a file or from a tool stands in code marks or in a fenced block, so that
no line of it is read as a mention, as a link to an issue or as a heading.

Exit status: 0 when the text was made or written, also when there is nothing to clean and when the issue is
closed; 1 for a finding (more than one open issue carries the mark); 2 when nothing was judged: a tool is missing
or fails on the files as they are, or GitHub could not be read. A status of 2 is not a pass.
"""
from __future__ import annotations

import argparse
import ast
import datetime as dt
import io
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import tokenize
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import asdict, dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
MODULES = ("proxy", "tui")
# The fixers of `go fix` that the job runs, by name. Each rewrites code to a newer form.
GO_FIXERS = ("any", "fmtappendf", "forvar", "inline", "mapsloop", "minmax", "newexpr", "plusbuild", "rangeint",
             "reflecttypefor", "slicescontains", "slicessort", "stditerators", "stringsbuilder", "stringscut",
             "stringscutprefix", "stringsseq", "testingcontext", "waitgroup")
# The fixers of `go fix` that the job never runs, each with the reason.
GO_NOT_RUN = {"omitzero": "it changes what is encoded: a struct that is zero is left out where it was written before",
              "hostport": "it changes the address that is dialed, which is a repair and not a rewrite",
              "buildtag": "it checks build lines and rewrites nothing"}
# The rules of ruff whose fixes the job tries. A fix that changes the syntax tree is left out and named.
PYTHON_RULES = ("W291", "W292", "W293", "W391", "F401")
# The pieces that are listed with their numbers and checks and not offered as a patch, by language. Take a line out
# to offer those pieces.
HELD = {
    "go": "Held by a maintainer: open work is written against these Go files as they are, and a rewrite of the same "
          "files now would make it not apply.",
}
# How often a fixer may run before its result has to stay the same.
PASSES = 3
# The test run of a module, with the flags of the Go test jobs: the race detector, and no result from Go's cache.
GO_TEST = ["go", "test", "-race", "-count=1", "-timeout", "20m", "./..."]
MARK = "<!-- weekly-cleanup: a job writes this text each week; a change by hand is lost -->"
TITLE = "Weekly cleanup: what the fixers would change on dev"
AUTHOR = "github-actions[bot]"
GITHUB_API = "https://api.github.com"
# The text of an issue may have 65,536 characters.
LIMIT = 60000
SAID_LINES = 15
# How much of the list of what is left out the text holds: so many lines, each so long. The whole list is in a file.
LEFT_OUT_LINES, LEFT_OUT_WIDTH = 120, 240
FOR_PEOPLE = (
    "- A maintainer applies a piece, one piece at a time. Applying a piece is a normal pull request through every "
    "check. Please open no pull request from this list.",
    "- This issue is not for a claim.",
    "- Closing this issue stops the weekly text. Open it again to start it.",
    "- Each piece applies alone to the commit named above. After one piece is merged, the text of the next week "
    "holds the others again, made for the new `dev`.",
)
FIRST_LINE = {0: "**Made.**", 1: "**A finding.**", 2: "**Not judged.** Nothing is known about what there is to "
                                                      "clean. This is not a pass."}


class NotJudged(Exception):
    """A tool is missing or failed on the files as they are, or GitHub could not be read. The words say what and how to fix it."""


class Finding(Exception):
    """Something is not as it has to be, and a person has to act. The words say what and how."""


@dataclass
class Piece:
    """One fixer on one part of the repository, and what became of it."""
    name: str
    looked_at: int = 0
    changed: list = field(default_factory=list)
    left_out: dict = field(default_factory=dict)
    checks: dict = field(default_factory=dict)
    said: list = field(default_factory=list)
    added: int = 0
    removed: int = 0
    patch: str = ""

    @property
    def state(self) -> str:
        if not self.changed:
            return "left out" if self.left_out else "nothing to change"
        return "held" if self.name.split("/")[0] in HELD else "offered"

    @property
    def file(self) -> str:
        return self.name.replace("/", "-") + ".patch"


# --- commands --------------------------------------------------------------------------------------------------

def run(argv: list, cwd: Path, limit: float) -> subprocess.CompletedProcess:
    """Run a command with a time limit. Its whole process group ends with it. A run over its limit has status 124."""
    try:
        process = subprocess.Popen(argv, cwd=cwd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                                   errors="replace", start_new_session=True)
    except OSError as error:
        raise NotJudged(f"`{argv[0]}` could not be started ({error}). Fix: install it, or put it on the PATH.") from None
    try:
        out, _ = process.communicate(timeout=limit)
    except subprocess.TimeoutExpired:
        os.killpg(process.pid, signal.SIGKILL)
        process.wait()
        return subprocess.CompletedProcess(argv, 124, f"no end after {limit:g} s", "")
    return subprocess.CompletedProcess(argv, process.returncode, out, "")


def must(what: str, argv: list, cwd: Path, limit: float, fix: str) -> str:
    """The output of a command that has to pass on the files as they are."""
    done = run(argv, cwd, limit)
    if done.returncode != 0:
        raise NotJudged(f"{what}: status {done.returncode}.\n{last_lines(done.stdout)}\nFix: {fix}")
    return done.stdout


def last_lines(printed: str, count: int = SAID_LINES) -> str:
    return "\n".join(printed.strip().splitlines()[-count:])


def of_failed_tests(printed: str, limit: int = 60) -> str:
    """Of what `go test` printed: the lines that say which test failed, and why. The tests of a module write many
    log lines while they run, and the name of a failed test stands far above the end of the output."""
    lines, kept = printed.splitlines(), []
    for n, line in enumerate(lines):
        if line.lstrip().startswith("--- FAIL"):
            kept.append(line)
            for more in lines[n + 1:n + 9]:
                if not more.startswith((" ", "\t")) or more.lstrip().startswith(("---", "===")):
                    break
                kept.append(more)
        elif line.startswith(("panic:", "WARNING: DATA RACE", "FAIL")):
            kept.append(line)
    if not kept:
        return last_lines(printed)
    more = [f"and {len(kept) - limit} more line(s) of this kind"] if len(kept) > limit else []
    return "\n".join(kept[:limit] + more)


def git(copy: Path, *args: str) -> str:
    return must(f"git {args[0]}", ["git", "-C", str(copy), *args], copy, 300,
                "run this in a checkout of the repository, with git on the PATH.")


def names(copy: Path, command: str, *args: str) -> list:
    """The file names that a git command prints, also names with spaces or other marks in them."""
    return [name for name in git(copy, command, "-z", *args).split("\0") if name]


def take_patch(copy: Path, piece: Piece, where: str = ".") -> None:
    """Put what the copy holds now into the piece as its patch, and count its lines."""
    piece.patch = git(copy, "diff", "--", where)
    for line in git(copy, "diff", "--numstat", "--", where).splitlines():
        added, removed = line.split("\t")[:2]
        piece.added += int(added) if added.isdigit() else 0
        piece.removed += int(removed) if removed.isdigit() else 0


def restore(copy: Path) -> None:
    git(copy, "checkout", "--quiet", "--", ".")


def make_copy(root: Path, into: Path) -> tuple[Path, str]:
    """A copy of the checked-out commit, to work in. Gives the copy and the commit."""
    commit = git(root, "rev-parse", "HEAD").strip()
    must("the copy of the checkout", ["git", "clone", "--quiet", str(root), str(into / "copy")], root, 600,
         "run this in a checkout of the repository.")
    git(into / "copy", "checkout", "--quiet", "--detach", commit)
    return into / "copy", commit


# --- Go --------------------------------------------------------------------------------------------------------

def go_fixers_on_no_list() -> list:
    """The fixers that this Go has and that are on neither list. They are named in the text and not run."""
    printed = must("the list of the fixers of this Go", ["go", "tool", "fix", "help"], ROOT, 300,
                   "use the Go version that the workflow pins.")
    block = re.search(r"Registered analyzers:\n\n(.*?)\n\n", printed, re.DOTALL)
    found = re.findall(r"^\s+([a-z][a-z0-9]*)\s", block.group(1), re.MULTILINE) if block else []
    if not found:
        raise NotJudged("`go tool fix help` gave no list of fixers that this script can read. Fix: read what that "
                        "command prints with the pinned Go, and change go_fixers_on_no_list to it.")
    return sorted(set(found) - set(GO_FIXERS) - set(GO_NOT_RUN))


def not_in_gofmt_form(copy: Path, module: str) -> set:
    return set(must(f"gofmt on {module}", ["gofmt", "-l", module], copy, 300, "use the Go version that the workflow pins.").split())


def apply_fixer(copy: Path, module: str, fixer: str, first: bool = True) -> tuple:
    """Run one fixer on a module until its result stays the same. Gives why it did not come to that, in a few words
    and as the tool said it, or two empty texts."""
    before = git(copy, "diff", "--", module)
    for turn in range(PASSES + 1):
        done = run(["go", "fix", f"-{fixer}", "./..."], copy / module, 900)
        after = git(copy, "diff", "--", module)
        if done.returncode != 0:
            if first and turn == 0 and after == before:
                raise NotJudged(f"`go fix -{fixer}` on `{module}` as it is: status {done.returncode}.\n{last_lines(done.stdout)}\n"
                                "Fix: make the module build with the Go version that the workflow pins. A fixer that "
                                "this Go does not have has to come off GO_FIXERS.")
            return ("the fixer fails on its own result",
                    f"`go fix -{fixer}` ended with status {done.returncode} on its own result:\n{last_lines(done.stdout)}")
        if after == before:
            return "", ""
        before = after
    return "the fixer did not come to an end", f"`go fix -{fixer}` still changes files after {PASSES} runs"


def go_checks(copy: Path, module: str, before: set, bin_dir: Path) -> tuple[dict, str]:
    """The checks of a Go piece that need no test run. Gives each result, and what the first failed check said."""
    checks = {"gofmt": "not run", "build": "not run", "vet": "not run"}
    worse = sorted(not_in_gofmt_form(copy, module) - before)
    if worse:
        return {**checks, "gofmt": "failed"}, "gofmt would change these files, and it would not before:\n" + "\n".join(worse)
    checks["gofmt"] = "passed"
    for name, argv in (("build", ["go", "build", "-o", str(bin_dir) + os.sep, "./..."]), ("vet", ["go", "vet", "./..."])):
        done = run(argv, copy / module, 1200)
        if done.returncode != 0:
            return {**checks, name: "failed"}, f"`go {name}` ended with status {done.returncode}:\n{last_lines(done.stdout)}"
        checks[name] = "passed"
    return checks, ""


def refuse(piece: Piece, why: str, said: str) -> None:
    """Leave the whole piece out: none of its files is offered."""
    piece.left_out = {name: why for name in piece.changed}
    piece.changed, piece.patch, piece.added, piece.removed = [], "", 0, 0
    piece.said = said.splitlines()


def go_piece(copy: Path, module: str, fixer: str, looked_at: int, before: set, bin_dir: Path) -> Piece:
    """One fixer on one module: what it changes, and whether the result is in gofmt's form, builds and vets."""
    piece = Piece(f"go/{module}/{fixer}", looked_at=looked_at)
    why, said = apply_fixer(copy, module, fixer)
    piece.changed = names(copy, "diff", "--name-only", "--", module)
    if piece.changed:
        take_patch(copy, piece, module)
        if why:
            piece.checks = {"fix": "failed"}
            refuse(piece, why, said)
        else:
            piece.checks, said = go_checks(copy, module, before, bin_dir)
            if said:
                refuse(piece, "the piece fails a check", said)
    restore(copy)
    return piece


def go_tests(copy: Path, module: str, pieces: list, kept: dict) -> None:
    """Run the tests of a module with these pieces together, and give each of them the result.

    Of a run that failed, the first piece gets the lines that name the failed tests, and `kept` gets the whole
    output, for a file of the run."""
    for piece in pieces:
        apply_fixer(copy, module, piece.name.split("/")[2], first=False)
    done = run(GO_TEST, copy / module, 2400)
    restore(copy)
    if done.returncode != 0:
        must(f"the tests of {module} on the files as they are", GO_TEST, copy / module, 2400,
             f"repair the tests of `{module}` on the branch; they fail with no fixer applied.")
    together = "" if len(pieces) == 1 else " with the pieces of the module together"
    if done.returncode != 0:
        kept[f"tests-{module}.txt"] = done.stdout
    for piece in pieces:
        piece.checks["tests"] = "passed" if done.returncode == 0 else "failed"
        if done.returncode != 0:
            said = (f"`go test` ended with status {done.returncode}{together}:\n{of_failed_tests(done.stdout)}\n"
                    f"The whole output is in the file `tests-{module}.txt` of the run.")
            refuse(piece, f"the tests of the module fail{together}",
                   said if piece is pieces[0] else f"What the tests said stands under {pieces[0].name}.")


def go_pieces(copy: Path, wanted: list, bin_dir: Path, kept: dict) -> list:
    pieces = []
    for module in MODULES:
        fixers = [fixer for fixer in GO_FIXERS if f"go/{module}/{fixer}" in wanted]
        if not fixers:
            continue
        looked_at = len([name for name in names(copy, "ls-files", "--", module) if name.endswith(".go")])
        before = not_in_gofmt_form(copy, module)
        must(f"go vet on {module} as it is", ["go", "vet", "./..."], copy / module, 1200,
             f"repair `{module}` on the branch; `go vet` fails with no fixer applied.")
        made = [go_piece(copy, module, fixer, looked_at, before, bin_dir) for fixer in fixers]
        passed = [piece for piece in made if piece.changed]
        if passed:
            go_tests(copy, module, passed, kept)
        pieces += made
    return pieces


# --- Python ----------------------------------------------------------------------------------------------------

def comments(source: bytes) -> list:
    return [token.string.rstrip() for token in tokenize.tokenize(io.BytesIO(source).readline) if token.type == tokenize.COMMENT]


def differs(old: bytes, new: bytes) -> str:
    """Why a fix to a Python file is not taken, or nothing: the syntax tree and the comments have to be the same."""
    try:
        trees = [ast.dump(ast.parse(source)) for source in (old, new)]
        said = [comments(source) for source in (old, new)]
    except (SyntaxError, ValueError, tokenize.TokenError):
        return "this Python cannot read the file before or after the fix"
    if trees[0] != trees[1]:
        return "the syntax tree is not the same after the fix"
    return "" if said[0] == said[1] else "a comment is not the same after the fix"


def python_piece(copy: Path, rule: str, files: list) -> Piece:
    """The fixes of one rule: each file whose tree and comments stay the same is in the piece, each other one is named."""
    piece = Piece(f"python/{rule}", looked_at=len(files), checks={"tree and comments": "read in each file"})
    must(f"ruff for {rule}", [sys.executable, "-m", "ruff", "check", "--fix", "--exit-zero", "--quiet", "--no-cache", "--select", rule,
                              "--", *files], copy, 900,
         "install the packages of .github/requirements/ci.txt for the Python that runs this script.")
    for name in names(copy, "diff", "--name-only"):
        old = subprocess.run(["git", "-C", str(copy), "show", f"HEAD:{name}"], capture_output=True, check=True, timeout=60).stdout
        why = differs(old, (copy / name).read_bytes())
        if why:
            piece.left_out[name] = why
            git(copy, "checkout", "--quiet", "--", name)
        else:
            piece.changed.append(name)
    take_patch(copy, piece)
    restore(copy)
    return piece


def python_pieces(copy: Path, wanted: list) -> list:
    rules = [rule for rule in PYTHON_RULES if f"python/{rule}" in wanted]
    files = [name for name in names(copy, "ls-files") if name.endswith(".py")] if rules else []
    return [python_piece(copy, rule, files) for rule in rules]


# --- the text --------------------------------------------------------------------------------------------------

def fenced(text: str, kind: str = "text") -> str:
    """The text as a fenced block. The fence is longer than any run of backticks in the text, so no line ends the
    block early, and nothing in it is read as a mention, a link to an issue or a heading."""
    fence = "`" * max(3, max((len(marks) for marks in re.findall("`+", text)), default=0) + 1)
    return f"{fence}{kind}\n{text.strip(chr(10))}\n{fence}\n"


def all_names() -> list:
    return [f"go/{module}/{fixer}" for module in MODULES for fixer in GO_FIXERS] + [f"python/{rule}" for rule in PYTHON_RULES]


def row(piece: Piece) -> str:
    unchanged = piece.looked_at - len(piece.changed) - len(piece.left_out)
    checks = ", ".join(f"{name}: {result}" for name, result in piece.checks.items())
    return (f"| `{piece.name}` | {piece.looked_at} | {len(piece.changed)} | {unchanged} | {len(piece.left_out)} | "
            f"+{piece.added} -{piece.removed} | {checks} | {piece.state} |")


def left_out_text(pieces: list) -> str:
    """Each file that is left out, with the reason, and what a failed check said. All of it comes from files and tools."""
    lines = []
    for piece in pieces:
        if piece.left_out:
            lines.append(f"{piece.name}:")
            lines += [f"  {name}: {why}" for name, why in sorted(piece.left_out.items())]
            lines += [f"  | {line}" for line in piece.said]
    return "\n".join(lines)


def head_of_text(result: dict, pieces: list) -> list:
    """The part of the text before the patches."""
    lines = [MARK, f"Made on {result['date']} for commit `{result['commit'][:12]}`.", ""]
    lines += [*FOR_PEOPLE, "- Older than 8 days: the job did not end, or this issue was closed; see its runs"
                           + (f": {result['runs']}" if result.get("runs") else "."), ""]
    shown = [piece for piece in pieces if piece.state != "nothing to change"]
    if not shown:
        return lines + [f"**Nothing to clean this week.** {len(pieces)} piece(s) were tried, and no fixer changes a file."]
    lines += ["## The pieces", "", "A piece is one fixer on one part. Files looked at = changed + unchanged + left out.", "",
              "| Piece | Files looked at | Changed | Unchanged | Left out | Lines | Checks | State |", "|---|---|---|---|---|---|---|---|"]
    lines += [row(piece) for piece in shown]
    quiet = [piece.name for piece in pieces if piece.state == "nothing to change"]
    if quiet:
        lines += ["", "Nothing to change: " + ", ".join(f"`{name}`" for name in quiet) + "."]
    for language, why in HELD.items():
        if any(piece.state == "held" and piece.name.startswith(language + "/") for piece in pieces):
            lines += ["", why]
    if any(piece.name.startswith("go/") for piece in shown):
        lines += ["", "The checks of a Go piece are a test result. They are no proof that the code does what it did before."]
    if result.get("unlisted"):
        lines += ["", "This Go has fixers that are on no list of the job, so they were not run: "
                  + ", ".join(f"`{name}`" for name in result["unlisted"]) + "."]
    lines += ["", "## To make one piece", "",
              fenced("python3 scripts/weekly_cleanup.py make --piece <piece> --out <folder>\n"
                     "git apply <folder>/pieces/<piece, with - for each />.patch", "bash").rstrip("\n")]
    return lines


def issue_text(result: dict, pieces: list, limit: int = LIMIT) -> str:
    """The text of the issue. It is never longer than the limit: what does not fit is named, and is in the files of the run."""
    lines = head_of_text(result, pieces)
    left = left_out_text(pieces)
    if left:
        kept = [line if len(line) <= LEFT_OUT_WIDTH else f"{line[:LEFT_OUT_WIDTH]} [and {len(line) - LEFT_OUT_WIDTH} more characters]"
                for line in left.splitlines()[:LEFT_OUT_LINES]]
        more = len(left.splitlines()) - len(kept)
        lines += ["", "## Left out, and why", "", fenced("\n".join(kept)).rstrip("\n")]
        lines += [f"And {more} more line(s)."] if more else []
        lines += ["The whole list is in the file `left-out.txt` of the run."]
    offered = [piece for piece in pieces if piece.state == "offered"]
    if offered:
        lines += ["", "## The patches"]
        only_in_files = []
        for piece in offered:
            block = ["", f"### `{piece.name}`", "", fenced(piece.patch, "diff").rstrip("\n")]
            if len("\n".join(lines + block)) + 400 + 40 * len(offered) > limit:
                only_in_files.append(piece.name)
            else:
                lines += block
        if only_in_files:
            lines += ["", "Only in the files of the run, because the text of an issue has a size limit: "
                      + ", ".join(f"`{name}`" for name in only_in_files) + "."]
    text = "\n".join(lines) + "\n"
    if len(text) > limit:
        raise NotJudged(f"the text of the issue has {len(text)} characters, and the limit is {limit}. Fix: this is a "
                        "fault of scripts/weekly_cleanup.py; shorten what issue_text writes before the patches.")
    return text


def outside_marks(line: str) -> str:
    """A line without what stands in code marks. Code opens with a run of backticks and ends at the next run of the
    same length; a run with no such partner is plain text, and so is what follows it."""
    runs = [(found.start(), found.end()) for found in re.finditer("`+", line)]
    kept, at, n = [], 0, 0
    while n < len(runs):
        start, end = runs[n]
        partner = next((m for m in range(n + 1, len(runs)) if runs[m][1] - runs[m][0] == end - start), None)
        if partner is None:
            n += 1
            continue
        kept.append(line[at:start])
        at, n = runs[partner][1], partner + 1
    return "".join(kept) + line[at:]


def plain_part(text: str) -> str:
    """The part of a text that is in no fenced block and in no code marks: what GitHub may read as a mention or as a
    link to an issue."""
    kept, fence = [], ""
    for line in text.splitlines():
        marks = re.match(" {0,3}(`{3,})", line)
        if fence:
            fence = "" if marks and len(marks.group(1)) >= len(fence) and not line.strip().strip("`") else fence
        elif marks:
            fence = marks.group(1)
        else:
            kept.append(outside_marks(line))
    return "\n".join(kept) + ("\n@ a fenced block is not closed" if fence else "")


def page(status: int, said: str, text: str = "") -> None:
    """Write the result to the page of the run, when the run has one. `said` holds fixed words and numbers only."""
    where = os.environ.get("GITHUB_STEP_SUMMARY")
    if where:
        with open(where, "a", encoding="utf-8") as out:
            out.write(f"### The weekly cleanup\n\n{FIRST_LINE[status]} {said}\n\n{text}")


# --- make ------------------------------------------------------------------------------------------------------

def runs_address() -> str:
    """Where the runs of the workflow are, when this is a job. The address is taken only in its usual form."""
    address = f"{os.environ.get('GITHUB_SERVER_URL', '')}/{os.environ.get('GITHUB_REPOSITORY', '')}/actions/workflows/weekly-cleanup.yml"
    return address if re.fullmatch(r"https://github\.com/[\w.-]+/[\w.-]+/actions/workflows/weekly-cleanup\.yml", address) else ""


def make(root: Path, out: Path, only: str | None) -> int:
    wanted = [only] if only else all_names()
    if only and only not in all_names():
        raise NotJudged(f"`{only}` is no piece. Fix: give one of: {', '.join(all_names())}")
    if out.exists() and any(out.iterdir()):
        raise NotJudged(f"the folder {out} is not empty, and the patches of another run in it would read as this run's. "
                        "Fix: give a folder that is empty or not there yet.")
    for tool in ("git", *(["go", "gofmt"] if any(name.startswith("go/") for name in wanted) else [])):
        if not shutil.which(tool):
            raise NotJudged(f"`{tool}` is not on the PATH. Fix: install it; for Go, the version that the workflow pins.")
    with tempfile.TemporaryDirectory(prefix="weekly-cleanup-") as work:
        copy, commit = make_copy(root, Path(work))
        (Path(work) / "bin").mkdir()
        unlisted = go_fixers_on_no_list() if any(name.startswith("go/") for name in wanted) else []
        outputs = {}
        pieces = go_pieces(copy, wanted, Path(work) / "bin", outputs) + python_pieces(copy, wanted)
    result = {"date": dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d"), "commit": commit, "runs": runs_address(),
              "unlisted": unlisted, "title": TITLE, "one_piece": bool(only),
              "anything": any(piece.state != "nothing to change" for piece in pieces)}
    text = issue_text(result, pieces)
    (out / "pieces").mkdir(parents=True, exist_ok=True)
    for piece in pieces:
        if piece.patch:
            (out / "pieces" / piece.file).write_text(piece.patch, encoding="utf-8")
    for name, printed in outputs.items():
        (out / name).write_text(printed, encoding="utf-8")
    (out / "issue.md").write_text(text, encoding="utf-8")
    (out / "left-out.txt").write_text(left_out_text(pieces) + "\n", encoding="utf-8")
    kept = [{**asdict(piece), "patch": "", "state": piece.state} for piece in pieces]
    (out / "result.json").write_text(json.dumps({**result, "pieces": kept}, indent=2) + "\n", encoding="utf-8")
    counts = {state: sum(1 for piece in pieces if piece.state == state) for state in ("offered", "held", "left out", "nothing to change")}
    said = (f"{len(pieces)} piece(s): {counts['offered']} offered, {counts['held']} held, {counts['left out']} left out, "
            f"{counts['nothing to change']} with nothing to change.")
    print(f"weekly cleanup: {said}\ntext: {out / 'issue.md'}")
    page(0, said + " The text of the issue, as the job would write it, follows.", "---\n\n" + text)
    return 0


# --- write -----------------------------------------------------------------------------------------------------

class NoRedirect(urllib.request.HTTPRedirectHandler):
    """An answer that points to another address is not followed: the token would go there too."""

    def redirect_request(self, *args, **kwargs):
        return None


def github(method: str, address: str, token: str, body: dict | None = None) -> tuple:
    """One call to GitHub. Gives the decoded answer and the address of the next page, if there is one."""
    request = urllib.request.Request(address, data=json.dumps(body).encode() if body is not None else None, method=method, headers={
        "Accept": "application/vnd.github+json", "Authorization": f"Bearer {token}", "X-GitHub-Api-Version": "2022-11-28",
        "Content-Type": "application/json"})
    try:
        with urllib.request.build_opener(NoRedirect).open(request, timeout=30) as response:
            following = re.search(r'<([^>]+)>;\s*rel="next"', response.headers.get("Link", ""))
            return json.load(response), following.group(1) if following else ""
    except urllib.error.HTTPError as error:
        raise NotJudged(f"GitHub answered {error.code} to {method} {address.split('?')[0]}. Fix: read the run's log; the "
                        "job needs `issues: write` and nothing else to write the issue.") from None
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as error:
        raise NotJudged(f"GitHub gave no answer that can be read to {method} {address.split('?')[0]} ({type(error).__name__}). "
                        "Fix: run the job again.") from None


def issues_with_the_mark(repo: str, token: str) -> list:
    """The issues that the job's own account made and whose text starts with the mark. A pull request is no issue."""
    address = f"{GITHUB_API}/repos/{repo}/issues?state=all&per_page=100&creator={urllib.parse.quote(AUTHOR, safe='')}"
    found = []
    while address:
        listed, address = github("GET", address, token)
        if address and not address.startswith(GITHUB_API + "/"):
            raise NotJudged("GitHub named a next page at another address. Fix: run the job again.")
        found += [issue for issue in listed if "pull_request" not in issue and (issue.get("user") or {}).get("login") == AUTHOR
                  and (issue.get("body") or "").startswith(MARK)]
    return found


def write(folder: Path) -> int:
    text, result = (folder / "issue.md").read_text(encoding="utf-8"), json.loads((folder / "result.json").read_text(encoding="utf-8"))
    token, repo = os.environ.get("GITHUB_TOKEN", ""), os.environ.get("GITHUB_REPOSITORY", "")
    if not token or not re.fullmatch(r"[\w.-]+/[\w.-]+", repo):
        raise NotJudged("GITHUB_TOKEN or GITHUB_REPOSITORY is not set. Fix: this command is for the weekly job, which has both.")
    if result.get("one_piece") or not text.startswith(MARK) or len(text) > LIMIT:
        raise NotJudged("the text in the folder is not the weekly text: it is for one piece, has no mark, or is too "
                        "long. Fix: make it with `make --out <folder>` and no --piece.")
    if re.search(r"@|#\d", plain_part(text)):
        raise NotJudged("the text in the folder holds an `@` or a `#` with a number outside code marks and fenced "
                        "blocks, where GitHub reads a mention or a link to an issue. Nothing was written. Fix: this is "
                        "a fault of scripts/weekly_cleanup.py; put that part of issue_text into code marks.")
    found = issues_with_the_mark(repo, token)
    is_open = [issue for issue in found if issue.get("state") == "open"]
    if len(is_open) > 1:
        numbers = ", ".join(str(int(issue["number"])) for issue in is_open)
        raise Finding(f"{len(is_open)} open issues carry the mark of the weekly cleanup (numbers {numbers}), and the job "
                      "writes one. Nothing was written. Fix: close all but one.")
    if is_open:
        number = int(is_open[0]["number"])
        github("PATCH", f"{GITHUB_API}/repos/{repo}/issues/{number}", token, {"body": text})
        said = f"The text of issue {number} was replaced."
    elif found:
        said = (f"Issue {int(found[0]['number'])} is closed, so nothing was written. Closing the issue stops the weekly text; "
                "open it again to start it.")
    elif result.get("anything"):
        made, _next = github("POST", f"{GITHUB_API}/repos/{repo}/issues", token, {"title": TITLE, "body": text})
        said = f"There was no issue with the mark, so issue {int(made['number'])} was made."
    else:
        said = "There is nothing to clean and no issue with the mark, so none was made."
    print(f"weekly cleanup: {said}")
    page(0, said)
    return 0


def parse(argv: list | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    commands = parser.add_subparsers(dest="command", required=True)
    maker = commands.add_parser("make", help="run the fixers on a copy and write the text and the patches")
    maker.add_argument("--root", type=Path, default=ROOT, help="the checkout to read; it is not changed")
    maker.add_argument("--out", type=Path, required=True, help="the folder for the text and the patches")
    maker.add_argument("--piece", help="make one piece only, for example go/proxy/rangeint")
    writer = commands.add_parser("write", help="put the text into the one issue (the weekly job only)")
    writer.add_argument("--from", dest="folder", type=Path, required=True, help="the folder that `make` wrote")
    return parser.parse_args(argv)


def main(argv: list | None = None) -> int:
    args = parse(argv)
    try:
        return make(args.root.resolve(), args.out, args.piece) if args.command == "make" else write(args.folder)
    except (NotJudged, Finding) as error:
        status = 1 if isinstance(error, Finding) else 2
        print(f"weekly cleanup: {'a finding' if status == 1 else 'not judged'}: {error}", file=sys.stderr)
        page(status, "", fenced(str(error)))
        return status
    except Exception as error:  # an error of the script is "not judged": it must not end with the status of a finding
        print(f"weekly cleanup: not judged: an error of the script itself: {error!r}. Fix: this is a fault of "
              "scripts/weekly_cleanup.py; run it again with the same files to see the place.", file=sys.stderr)
        page(2, "", fenced(repr(error)))
        return 2


if __name__ == "__main__":
    sys.exit(main())
