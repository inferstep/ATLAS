#!/usr/bin/env python3
"""Measured reliability of the live stack: proxy + v3-service + lens + sandbox.

`tests/e2e/` drives fake llama/lens/v3 handlers, which is what makes it
deterministic enough for CI — and also what stops it from telling you whether
the real services work together. This runs real sessions against the running
stack and reports two numbers that must not be conflated:

  Harness Integrity Rate  sessions in which ATLAS's own plumbing did nothing
                          wrong. Independent of whether the model succeeded at
                          the task, and the number that should be 100%.

  Task Success Rate       sessions where the requested change actually landed.
                          A failure here can come from the model or from the
                          harness; this script does not tell them apart.

The split matters because "it built a snake game" moves with model skill and
sampling luck, so it cannot tell you whether a pipeline regression shipped. A
harness defect is instead something provable from the event stream and the
workspace: a rejection whose stated reason does not hold against the file on
disk, steering that names a remedy the target cannot accept, an exit that
escaped the gates, a corrupt write, a service fault, an orphaned tool call, an
event the TUI cannot render, or a background job left running after the session.

Usage:
    python scripts/eval/e2e-reliability.py                 # default suite, 2 reps
    python scripts/eval/e2e-reliability.py --reps 5
    python scripts/eval/e2e-reliability.py --tasks flask_pause,add_function
    python scripts/eval/e2e-reliability.py --json out.json

Requires the stack to be up (docker compose ps) and ATLAS_PROJECT_DIR to be the
workspace the proxy has mounted at /workspace.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable, Optional

REPO = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO / "scripts"))
sys.path.insert(0, str(REPO / "scripts" / "eval"))
from code_quality import analyze as analyze_quality
from reliability_detectors import (h1_protocol, h3_dead_end_steering, h4_gate_escape, h5_corrupt_write,
                                   h6_service_fault, h7_background_leak, h8_anchored_on_injected_text,
                                   h9_tier_misapplied, model_output_guards, tui_handled_types)
from reliability_records import Session, Task
from reliability_report import (Stopped, log_defects, report, sibling,
                                stop_on_signals, write_summary)
from reliability_sandbox import (_extract_script, _file_parses, _function_names, _js_parses, _quality_parser,
                                 _runtime_argv, _ws_path, evaluator_identity, use_sandbox)
from reliability_stream import PROMPT_POLICIES, answer_prompt, read_stream

# --------------------------------------------------------------------------
# Task suite
# --------------------------------------------------------------------------
#
# Each task is a deterministic fixture plus a post-condition that is decidable
# without reading the model's prose. `check` sees the workspace and returns
# (passed, detail) — it must answer "did the requested change land", never
# "did the model claim it did".


def _read_fixture(name: str) -> str:
    p = REPO / "scripts" / "fixtures" / name
    if not p.exists():
        raise SystemExit(f"missing fixture {p} — see scripts/fixtures/README.md")
    return p.read_text()


def _check_flask_pause(ws: Path) -> tuple[bool, str]:
    """A pause toggle wired into the game loop, with the JS still parsing.

    Three separate things, all required. An earlier, looser version of this
    check accepted the bare token `32` or a stray `Space` anywhere in the
    file and reported a pass against a fixture that had not been touched —
    a reliability check that can pass without the work being done is worse
    than no check, so each clause below names a distinct piece of the
    feature and the state variable has to be one the keydown handler
    actually assigns.
    """
    src = (ws / "app.py").read_text()
    js = _extract_script(src)
    if js is None:
        return False, "no <script> block survived"

    ok, err = _js_parses(js)
    if not ok:
        return False, f"embedded JS broken: {err}"

    # 1. A boolean the code flips, not merely a word that appears somewhere.
    state = None
    for m in re.finditer(r"\b(?:let|var|const)\s+(\w*[Pp]aused?\w*)\s*=", js):
        state = m.group(1)
        break
    if not state:
        return False, "no pause state variable declared"
    if not re.search(rf"\b{re.escape(state)}\s*=\s*(?:!\s*{re.escape(state)}|true|false)", js):
        return False, f"{state} is declared but never toggled"

    # 2. A keyboard branch on the spacebar specifically.
    if not re.search(r"(?:key|code)\s*===?\s*['\"](?: |Space|Spacebar)['\"]"
                     r"|keyCode\s*===?\s*32", js):
        return False, f"{state} exists but nothing binds the spacebar"

    # 3. The loop has to actually honour it, or the key toggles a dead flag.
    if not re.search(rf"if\s*\([^)]*\b{re.escape(state)}\b", js):
        return False, f"{state} is toggled but the game loop never checks it"

    return True, f"{state} declared, toggled, spacebar-bound, honoured by the loop"


def _check_add_function(ws: Path) -> tuple[bool, str]:
    src = (ws / "stats.py").read_text()
    names, err = _function_names(src)
    if err:
        return False, f"stats.py does not parse: {err}"
    if "median" not in names:
        return False, f"no median() defined (found {sorted(names)})"
    # Behaviour, not just presence.
    proc = subprocess.run(
        _runtime_argv() + [
            "-c",
            "import sys; sys.path.insert(0,%r); import stats;"
            "print(stats.median([3,1,2]), stats.median([4,1,3,2]))" % _ws_path(ws)],
        capture_output=True, text=True, timeout=30)
    if proc.returncode != 0:
        return False, f"median() raised: {proc.stderr.strip()[:160]}"
    if proc.stdout.split() != ["2", "2.5"]:
        return False, f"median() wrong: got {proc.stdout.strip()!r}, want '2 2.5'"
    return True, "median() defined and correct on odd + even input"


def _check_offbyone(ws: Path) -> tuple[bool, str]:
    proc = subprocess.run(
        _runtime_argv() + [
            "-c",
            "import sys; sys.path.insert(0,%r); import chunk;"
            "print(chunk.chunks([1,2,3,4,5], 2))" % _ws_path(ws)],
        capture_output=True, text=True, timeout=30)
    if proc.returncode != 0:
        return False, f"chunks() raised: {proc.stderr.strip()[:160]}"
    got = proc.stdout.strip()
    want = "[[1, 2], [3, 4], [5]]"
    if got != want:
        return False, f"chunks() wrong: got {got}, want {want}"
    return True, "chunks() drops no tail element"


# --- AoC-style puzzles: exact-integer answers, holdout-verified ----------
#
# The answer is a specific number, so "did it work" needs no judgement. The
# model sees input.txt; the check re-runs its program against a holdout input
# it never saw, so hardcoding the number it was shown fails. `shoal` is the
# one that separates understanding from transcription: the naive
# per-individual simulation reaches ~1.6e12 elements and cannot finish, so
# only the counting solution completes.

AOC_DIR = REPO / "scripts" / "fixtures" / "aoc"


def _aoc_answers() -> dict:
    return json.loads((AOC_DIR / "answers.json").read_text())


def _run_solution(ws: Path, timeout: int = 60) -> tuple[bool, str]:
    prog = ws / "solve.py"
    if not prog.exists():
        return False, "solve.py was never created"
    try:
        p = subprocess.run(_runtime_argv(_ws_path(ws)) + ["solve.py"],
                           cwd=str(ws),
                           capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, f"solve.py did not finish within {timeout}s"
    if p.returncode != 0:
        return False, f"solve.py failed: {p.stderr.strip()[:160]}"
    nums = re.findall(r"-?\d+", p.stdout)
    if not nums:
        return False, f"no number printed (stdout={p.stdout.strip()[:80]!r})"
    return True, nums[-1]


def _check_aoc(name: str):
    def check(ws: Path) -> tuple[bool, str]:
        want = _aoc_answers()[name]
        ok, got = _run_solution(ws)
        if not ok:
            return False, got
        if got != str(want["input"]):
            return False, f"wrong answer: got {got}, want {want['input']}"
        # Same program, an input it never saw. A hardcoded answer dies here.
        original = (ws / "input.txt").read_text()
        (ws / "input.txt").write_text((AOC_DIR / name / "holdout.txt").read_text())
        try:
            ok2, got2 = _run_solution(ws)
        finally:
            (ws / "input.txt").write_text(original)
        if not ok2:
            return False, f"correct on its own input but broke on the holdout: {got2}"
        if got2 != str(want["holdout"]):
            return False, (f"holdout mismatch: got {got2}, want {want['holdout']} "
                           f"— the answer looks hardcoded rather than computed")
        return True, f"{got} correct, and correct on the holdout input"
    return check


_AOC_PROMPTS = {
    "sonar": ("input.txt holds one integer per line: a sonar depth reading. "
              "Consider sums of three-measurement sliding windows. Write "
              "solve.py that reads input.txt and prints how many such window "
              "sums are larger than the immediately previous window sum."),
    "course": ("input.txt holds one command per line: 'forward N', 'down N' or "
               "'up N'. Track horizontal position, depth and aim, all starting "
               "at 0. 'down N' increases aim by N, 'up N' decreases aim by N, "
               "and 'forward N' increases horizontal position by N AND "
               "increases depth by aim multiplied by N. Write solve.py that "
               "reads input.txt and prints the final horizontal position "
               "multiplied by the final depth."),
    "slope": ("input.txt is a grid of '.' (open) and '#' (tree). The pattern "
              "repeats infinitely to the right. Starting at the top-left and "
              "moving by a fixed (right, down) step until past the bottom, "
              "count the trees encountered. Write solve.py that reads "
              "input.txt and prints the PRODUCT of the tree counts for these "
              "five slopes: right 1 down 1, right 3 down 1, right 5 down 1, "
              "right 7 down 1, and right 1 down 2."),
    "shoal": ("input.txt is a comma-separated list of integers, each an "
              "internal timer for one fish. Each day every timer decreases by "
              "1. A fish whose timer is 0 resets to 6 and spawns a new fish "
              "with timer 8 (the new fish does not decrease that same day). "
              "Write solve.py that reads input.txt and prints how many fish "
              "exist after 256 days. Note: the population reaches roughly "
              "1e12, so simulating each fish individually will not finish."),
}


def _aoc_task(name: str) -> Task:
    return Task(
        name=f"aoc_{name}",
        prompt=_AOC_PROMPTS[name] + " Then run it and confirm the answer.",
        files={"input.txt": (AOC_DIR / name / "input.txt").read_text()},
        check=_check_aoc(name),
        must_exist=("input.txt",),
        immutable=("input.txt",),
    )


TASKS: dict[str, Task] = {
    # Tier-2: Python file whose real logic is JS inside a template string. The
    # case ATLAS exists for, and the one that exposed D9/D10.
    "flask_pause": Task(
        name="flask_pause",
        prompt=("In app.py, the snake game has no pause. Add a pause toggle: "
                "pressing the spacebar should pause and resume the game loop. "
                "Keep the change small and targeted. Then verify the app still "
                "starts."),
        files={"app.py": None},  # filled from fixture at load time
        check=_check_flask_pause,
        must_exist=("app.py",),
    ),
    # Mid-tier: add a function to an existing module. Exercises the edit path
    # without the embedded-script complication.
    "add_function": Task(
        name="add_function",
        prompt=("In stats.py, add a median(values) function next to the "
                "existing mean(). It must return the middle value for an "
                "odd-length list and the average of the two middle values for "
                "an even-length list. Then verify it works."),
        files={"stats.py": (
            '"""Small statistics helpers."""\n'
            "\n"
            "\n"
            "def mean(values):\n"
            "    if not values:\n"
            "        raise ValueError('mean() of empty sequence')\n"
            "    return sum(values) / len(values)\n"
        )},
        check=_check_add_function,
        must_exist=("stats.py",),
    ),
    # Low-tier repair: a one-character bug with an unambiguous correct answer.
    # Isolates harness behaviour from model creativity.
    "offbyone": Task(
        name="offbyone",
        prompt=("chunk.py has a bug: chunks([1,2,3,4,5], 2) drops the last "
                "element instead of returning it as a short final chunk. Fix "
                "it and verify."),
        files={"chunk.py": (
            '"""Split a list into fixed-size chunks."""\n'
            "\n"
            "\n"
            "def chunks(values, size):\n"
            "    out = []\n"
            "    for i in range(0, len(values) - size + 1, size):\n"
            "        out.append(values[i:i + size])\n"
            "    return out\n"
        )},
        check=_check_offbyone,
        must_exist=("chunk.py",),
    ),
}


for _n in ("sonar", "course", "slope", "shoal"):
    TASKS[f"aoc_{_n}"] = _aoc_task(_n)


# --- conversational probes: answer, do not edit, do not run V3 -----------
#
# The tier system's whole point is that V3 does not run on everything. A
# question should come back from the conversational tier: an answer, no
# writes, and no multi-minute pipeline. A wrong answer is a model limit; V3
# spinning up for a question is a product defect, and costs minutes.

_QUIRKY_SRC = '''"""Order bookkeeping."""


def apply_discount(total, pct):
    """Reduce total by pct percent."""
    return total - (total * pct / 100)


def find_duplicates(items):
    """Return values that appear more than once."""
    dupes = []
    for i in range(len(items)):
        for j in range(len(items)):
            if i != j and items[i] == items[j] and items[i] not in dupes:
                dupes.append(items[i])
    return dupes
'''


def _answer_text(s: "Session") -> str:
    parts = []
    for ev in s.events:
        t, d = ev.get("type"), (ev.get("data") or {})
        if t == "text":
            parts.append(str(d.get("content") or ""))
        elif t == "done":
            parts.append(str(d.get("summary") or ""))
    return " ".join(parts).lower()


def _check_explains(terms: tuple[str, ...], any_of: tuple[tuple[str, ...], ...] = ()):
    """The answer has to contain the substance, not merely be long.

    Deliberately generous: each clause accepts synonyms, because this measures
    whether the question was understood, not whether it was phrased the way
    the check's author would have phrased it.
    """
    def check(ws: Path, s: "Session" = None) -> tuple[bool, str]:
        text = _answer_text(s) if s is not None else ""
        if len(text.strip()) < 40:
            return False, "no substantive answer was produced"
        missing = [t for t in terms if t not in text]
        if missing:
            return False, f"answer never mentions {missing}"
        for group in any_of:
            if not any(g in text for g in group):
                return False, f"answer covers none of {list(group)}"
        return True, "answer covers the substance"
    return check


TASKS["ask_explain"] = Task(
    name="ask_explain",
    prompt=("In orders.py, what does find_duplicates do, and what is its time "
            "complexity? Just explain — do not change any code."),
    files={"orders.py": _QUIRKY_SRC},
    check=_check_explains(("duplicat",),
                          any_of=(("o(n^2)", "o(n2)", "o(n²)", "quadratic",
                                   "nested loop", "n squared"),)),
    must_exist=("orders.py",),
    immutable=("orders.py",),
    conversational=True,
)

TASKS["ask_bug"] = Task(
    name="ask_bug",
    prompt=("In orders.py, apply_discount(100, 10) returns 90.0 but a "
            "colleague says it should return 90. Explain what is going on "
            "here and whether it is actually a bug. Do not change the code."),
    files={"orders.py": _QUIRKY_SRC},
    check=_check_explains((),
                          any_of=(("float", "division", "/", "decimal"),
                                  ("90.0", "90"),)),
    must_exist=("orders.py",),
    immutable=("orders.py",),
    conversational=True,
)


# --- small rung: a feature added to a real 1.7k-line file ---------------
#
# Everything above works in a ~200-line workspace. This is the first rung
# where the model must LOCATE the right place in a file too long to hold in
# one look, and where the failure that matters is not "did it work" but "did
# it break the twelve things that already worked".

_EXECUTOR = REPO / "sandbox" / "executor_server.py"
_EXISTING_LANGS = ("python", "javascript", "typescript", "go", "java",
                   "kotlin", "rust", "ruby", "php", "bash", "json", "yaml")


# Runs inside the sandbox. Prints one JSON line: {"ok": bool, "why": str}.
_TOML_PROBE = r'''
import ast, json, sys, tempfile, types
from pathlib import Path

def verdict(ok, why=""):
    print(json.dumps({"ok": ok, "why": why}))
    sys.exit(0)

new_src = open("executor_server.py").read()
old_src = open("_probe_original.py").read()

def dispatch(src):
    """The lang if/elif chain with the most branches, and its function."""
    best = None
    for fn in [n for n in ast.walk(ast.parse(src)) if isinstance(n, ast.FunctionDef)]:
        for stmt in fn.body:
            if not (isinstance(stmt, ast.If) and "lang" in (ast.get_source_segment(src, stmt.test) or "")):
                continue
            chain, node = {}, stmt
            while True:
                key = " ".join((ast.get_source_segment(src, node.test) or "").split())
                chain[key] = ast.dump(ast.Module(body=node.body, type_ignores=[]))
                if len(node.orelse) == 1 and isinstance(node.orelse[0], ast.If):
                    node = node.orelse[0]
                    continue
                if node.orelse:
                    chain["<else>"] = ast.dump(ast.Module(body=node.orelse, type_ignores=[]))
                break
            if best is None or len(chain) > len(best[1]):
                best = (fn, chain)
    return best

old = dispatch(old_src)
new = dispatch(new_src)
if new is None:
    verdict(False, "the lang dispatch chain is gone")
old_fn, old_chain = old
new_fn, new_chain = new
if new_fn.name != old_fn.name:
    verdict(False, "the dispatch moved from %s to %s" % (old_fn.name, new_fn.name))
changed = [k for k in old_chain if new_chain.get(k) != old_chain[k]]
if changed:
    verdict(False, "existing branch changed or removed: %s" % ", ".join(changed[:3]))
toml_keys = [k for k in new_chain if k not in old_chain and "toml" in k]
if not toml_keys:
    verdict(False, "no toml branch in %s's dispatch" % new_fn.name)

# The prompt permits the toml package; the sandbox ships only tomllib. Alias
# it so a permitted choice is not failed by the environment. This supplies no
# logic the agent did not write.
try:
    import toml  # noqa: F401
except ImportError:
    import tomllib
    shim = types.ModuleType("toml")
    shim.loads = tomllib.loads
    shim.TomlDecodeError = tomllib.TOMLDecodeError
    shim.TOMLDecodeError = tomllib.TOMLDecodeError
    sys.modules["toml"] = shim

ns = {}
for stmt in ast.parse(new_src).body:
    if isinstance(stmt, (ast.Import, ast.ImportFrom)):
        try:
            exec(compile(ast.Module(body=[stmt], type_ignores=[]), "executor_server.py", "exec"), ns)
        except Exception:
            pass
ns.setdefault("_safe_overlay_path", lambda f: f)
exec(compile(ast.Module(body=[new_fn], type_ignores=[]), "executor_server.py", "exec"), ns)
check = ns[new_fn.name]
tmp = Path(tempfile.mkdtemp())
try:
    good = check("toml", 'title = "ok"\n[owner]\nname = "x"\n', tmp)
except Exception as e:
    verdict(False, "valid TOML raised %s: %s" % (type(e).__name__, str(e)[:80]))
try:
    bad = check("toml", 'title = "unterminated\n[owner\n', tmp)
except Exception as e:
    verdict(False, "invalid TOML raised %s instead of appending an error: %s" % (type(e).__name__, str(e)[:80]))
if not isinstance(good, list) or good:
    verdict(False, "valid TOML reported errors: %r" % (good,)[:120])
if not isinstance(bad, list) or not bad:
    verdict(False, "invalid TOML reported no error")
verdict(True)
'''


def _check_add_toml(ws: Path) -> tuple[bool, str]:
    src_path = ws / "executor_server.py"
    src = src_path.read_text()
    _, err = _function_names(src)
    if err:
        return False, f"broke the file: {err}"

    # Regression first — this is the "did everything break" question, and it
    # matters more than the feature.
    # Must still be DISPATCHED, not merely mentioned. A first version tested
    # for the bare string and passed a file whose python branch had been
    # renamed away — "python" also appears in comments and error text.
    def dispatched(name: str) -> bool:
        return bool(re.search(rf'lang\s*==\s*[\'"]{name}[\'"]', src)
                    or re.search(rf'lang\s+in\s*\([^)]*[\'"]{name}[\'"]', src))

    lost = [name for name in _EXISTING_LANGS if not dispatched(name)]
    if lost:
        return False, f"removed existing language handling: {lost}"

    if not dispatched("toml"):
        return False, "no toml branch added"
    if not re.search(r"import\s+toml|tomllib|tomli", src):
        return False, "toml branch added but nothing parses TOML"

    # Behavioural, and against the seeded original. The comment here used to
    # promise this while the probe only checked that "toml" appeared inside
    # some function, and the success message claimed "all 12 existing
    # languages intact" having checked only that each was still dispatched.
    #
    # Measured on 84296fd smallrung_toml rep2, scored PASS by that check:
    # insert_after placed `elif lang == "toml":` inside the java branch,
    # between `stderr = result.get(...)` and the loop that turns compiler
    # output into errors. The file parsed and every language was still
    # dispatched -- yet java compile errors were no longer reported, and the
    # toml branch raised UnboundLocalError on invalid TOML instead of
    # appending an error. The prompt says "Change nothing else -- the existing
    # language branches must keep working exactly as they do now", so both
    # halves are what the user asked for, not a hidden bar.
    (ws / "_probe_original.py").write_text(_EXECUTOR.read_text())
    probe = ws / "_probe_toml.py"
    probe.write_text(_TOML_PROBE)
    # Runs under the sandbox's Python: it needs tomllib (3.11+) and it parses
    # the agent's source, which targets that interpreter.
    p = None
    try:
        p = subprocess.run(_runtime_argv(_ws_path(ws)) + ["_probe_toml.py"],
                           cwd=str(ws), capture_output=True,
                           text=True, timeout=60)
        out = p.stdout
    except subprocess.TimeoutExpired:
        out = ""
    finally:
        probe.unlink(missing_ok=True)
        (ws / "_probe_original.py").unlink(missing_ok=True)
    verdict = None
    for line in reversed(out.splitlines()):
        if line.startswith("{"):
            try:
                verdict = json.loads(line)
            except json.JSONDecodeError:
                # A garbled line is no verdict, and the check below says so.
                pass
            break
    if verdict is None:
        tail = ((p.stderr if p else "") or out or "timed out")[-160:]
        return False, f"toml probe produced no verdict: {tail}"
    if not verdict.get("ok"):
        return False, verdict.get("why", "toml probe failed")
    return True, ("toml branch accepts valid TOML and reports invalid TOML; "
                  "all existing dispatch branches unchanged")


TASKS["smallrung_toml"] = Task(
    name="smallrung_toml",
    prompt=("executor_server.py exposes a syntax-check routine that dispatches "
            "on a `lang` variable and already handles python, json, yaml and "
            "several other languages. Add support for TOML: accept lang "
            "\"toml\", parse the code with Python's tomllib (or the toml "
            "package), and append any parse error to the same `errors` list "
            "the other branches use. Change nothing else — the existing "
            "language branches must keep working exactly as they do now."),
    files={"executor_server.py": _EXECUTOR.read_text()},
    check=_check_add_toml,
    must_exist=("executor_server.py",),
)


# --- medium rung: find a seeded bug across several real files -----------
#
# The deliverable is IDENTIFYING the defect, not editing it. That is
# deliberate: verbatim-transcription failures were already observed on edits
# of this size, and would likely dominate a fix-it task (a hypothesis, not a
# measured ceiling), hiding what this actually tests —
# can it navigate ~1.3k lines across three unfamiliar files, understand a
# selection algorithm, and locate a one-character bug from a symptom alone.

_BUGFIND_SRCS = ("planning.py", "scoring.py", "adapters.py")


def _bugfind_files() -> dict:
    out = {}
    for name in _BUGFIND_SRCS:
        text = (REPO / "v3-service" / name).read_text()
        if name == "planning.py":
            # The tie-break: highest score wins, ties go to the SHORTER plan.
            # Flipped so ties go to the longer one — one character, and the
            # symptom is entirely behavioural.
            orig = "if score > best_score or (score == best_score and n_steps < best_steps):"
            assert orig in text, "seed anchor moved in planning.py"
            text = text.replace(
                orig,
                "if score > best_score or (score == best_score and n_steps > best_steps):",
                1)
        out[name] = text
    return out


def _check_bugfind(ws: Path, s: "Session" = None) -> tuple[bool, str]:
    answer = _answer_text(s) if s is not None else ""
    if len(answer.strip()) < 30:
        return False, "no substantive answer"
    if "planning.py" not in answer:
        return False, f"did not name planning.py (answer: {answer[:90]!r})"

    # Must name the actual mechanism, not merely the word "tie". An earlier
    # version accepted an answer that said the cause was "how min() is used
    # with a custom key" — there is no min() there, and the function it named
    # was the scorer, not the selection loop. It had the file and the symptom
    # right and the mechanism invented, which is exactly the answer a loose
    # check should not pass.
    mechanism = any(t in answer for t in (
        "best_steps", "n_steps", "> best", "< best", "314",
        "greater than", "less than", "comparison operator"))
    if not mechanism:
        return False, ("named planning.py and the symptom, but not the actual "
                       f"comparison (answer: {answer[:110]!r})")

    # And it must not assert a mechanism that is not in the file.
    for invented in ("min(", "max(", "sorted(", "sort("):
        if invented in answer:
            return False, f"named a mechanism the file does not use: {invented!r}"
    return True, "located the seeded tie-break comparison in planning.py"


TASKS["bugfind_tiebreak"] = Task(
    name="bugfind_tiebreak",
    prompt=("This directory holds three modules from a code-generation "
            "pipeline. Several candidate plans are scored, and the best one "
            "is selected. Symptom: when two plans tie on score, the pipeline "
            "consistently picks the one with MORE steps, though it should "
            "prefer the shorter plan. Find the cause. Tell me which file and "
            "which comparison is wrong — do not change any code."),
    files=_bugfind_files(),
    check=_check_bugfind,
    must_exist=_BUGFIND_SRCS,
    immutable=_BUGFIND_SRCS,
    conversational=True,
)


def _check_multiturn(ws: Path) -> tuple[bool, str]:
    """Both turns' work must survive.

    The multi-turn risk is not that the second request fails — it is that it
    lands and takes the first one with it, by rewriting the file from a stale
    idea of its contents. So this checks BOTH functions exist and BOTH still
    behave, not just the newest one.
    """
    src = (ws / "stats.py").read_text()
    names, err = _function_names(src)
    if err:
        return False, f"stats.py does not parse after the follow-up: {err}"
    for want in ("mean", "median", "mode"):
        if want not in names:
            return False, f"{want}() missing after both turns (have {sorted(names)})"
    proc = subprocess.run(
        _runtime_argv() + [
            "-c",
            "import sys; sys.path.insert(0,%r); import stats;"
            "print(stats.median([3,1,2]), stats.mode([1,2,2,3]))" % _ws_path(ws)],
        capture_output=True, text=True, timeout=30)
    if proc.returncode != 0:
        return False, f"a function raised: {proc.stderr.strip()[:150]}"
    got = proc.stdout.split()
    if got != ["2", "2"]:
        return False, f"wrong results: got {got}, want ['2', '2']"
    return True, "both turns' functions present and correct"


TASKS["multiturn_stats"] = Task(
    name="multiturn_stats",
    prompt=("In stats.py, add a median(values) function next to the existing "
            "mean(). Odd-length lists return the middle value; even-length "
            "lists return the average of the two middle values."),
    followups=(
        "Good. Now also add a mode(values) function that returns the most "
        "common value. Keep median() exactly as it is.",
    ),
    files={"stats.py": (
        '"""Small statistics helpers."""\n'
        "\n"
        "\n"
        "def mean(values):\n"
        "    if not values:\n"
        "        raise ValueError('mean() of empty sequence')\n"
        "    return sum(values) / len(values)\n"
    )},
    check=_check_multiturn,
    must_exist=("stats.py",),
)


def _check_go_bugfix(ws: Path) -> tuple[bool, str]:
    """A Go fix, verified by running it — not by reading it.

    Every fixture so far has been Python or JavaScript, while the sandbox
    supports twelve languages. A different language exercises a different
    syntax checker, a different runner, and a different set of gate paths.
    """
    src = ws / "chunk.go"
    if not src.exists():
        return False, "chunk.go is missing"
    go = shutil.which("go")
    if not go:
        return True, "go toolchain unavailable on the host — skipped"
    proc = subprocess.run([go, "run", "chunk.go"], cwd=str(ws),
                          capture_output=True, text=True, timeout=180)
    if proc.returncode != 0:
        return False, f"go run failed: {(proc.stderr or proc.stdout).strip()[:160]}"
    got = proc.stdout.strip()
    want = "[[1 2] [3 4] [5]]"
    if got != want:
        return False, f"wrong output: got {got!r}, want {want!r}"
    return True, "chunks() fixed and verified by running it"


TASKS["go_offbyone"] = Task(
    name="go_offbyone",
    prompt=("chunk.go has a bug: Chunks([]int{1,2,3,4,5}, 2) drops the last "
            "element instead of returning it as a short final chunk. The "
            "program should print [[1 2] [3 4] [5]]. Fix it and verify by "
            "running it."),
    files={"chunk.go": (
        "package main\n"
        "\n"
        "import \"fmt\"\n"
        "\n"
        "// Chunks splits a slice into fixed-size chunks.\n"
        "func Chunks(values []int, size int) [][]int {\n"
        "\tout := [][]int{}\n"
        "\tfor i := 0; i+size <= len(values); i += size {\n"
        "\t\tout = append(out, values[i:i+size])\n"
        "\t}\n"
        "\treturn out\n"
        "}\n"
        "\n"
        "func main() {\n"
        "\tfmt.Println(Chunks([]int{1, 2, 3, 4, 5}, 2))\n"
        "}\n"
    )},
    check=_check_go_bugfix,
    must_exist=("chunk.go",),
)


def _check_multifile(ws: Path) -> tuple[bool, str]:
    """A real multi-file program: separate modules, working CLI, passing tests.

    The interesting failure is not "it did not work" — it is one 300-line
    todo.py with a `store` class glued on and a test file that imports
    nothing. So this checks the seams: the modules exist separately, the
    tests actually run and pass, and the CLI works end to end through them.
    """
    missing = [f for f in ("todo.py", "store.py", "test_store.py")
               if not (ws / f).exists()]
    if missing:
        return False, f"missing {', '.join(missing)}"

    # The split has to be real: store.py must carry the persistence, and
    # todo.py must go through it rather than reimplementing it.
    store_src = (ws / "store.py").read_text()
    todo_src = (ws / "todo.py").read_text()
    if "import store" not in todo_src and "from store" not in todo_src:
        return False, "todo.py never imports store.py — the split is cosmetic"
    if len(store_src.splitlines()) < 5:
        return False, "store.py is a stub"

    tp = subprocess.run(_runtime_argv(_ws_path(ws)) + ["-m", "pytest", "test_store.py", "-q"],
                        cwd=str(ws), capture_output=True, text=True, timeout=120)
    if tp.returncode != 0:
        tail = (tp.stdout or tp.stderr).strip().splitlines()
        return False, f"tests fail: {tail[-1][:120] if tail else 'no output'}"

    add = subprocess.run(_runtime_argv(_ws_path(ws)) + ["todo.py", "add", "buy milk"],
                         cwd=str(ws), capture_output=True, text=True, timeout=60)
    if add.returncode != 0:
        return False, f"`todo.py add` failed: {add.stderr.strip()[:120]}"
    lst = subprocess.run(_runtime_argv(_ws_path(ws)) + ["todo.py", "list"],
                         cwd=str(ws), capture_output=True, text=True, timeout=60)
    if lst.returncode != 0:
        return False, f"`todo.py list` failed: {lst.stderr.strip()[:120]}"
    if "buy milk" not in lst.stdout:
        return False, f"added item not listed (stdout={lst.stdout.strip()[:80]!r})"
    return True, "modules separate, tests pass, CLI round-trips through store"


TASKS["multifile_cli"] = Task(
    name="multifile_cli",
    prompt=("Build a small command-line todo app in this directory, as three "
            "files. store.py holds the persistence layer: load and save a "
            "list of items as JSON in todos.json, plus functions to add an "
            "item and to mark one done. todo.py is the CLI entry point and "
            "must use store.py rather than reimplementing it; support "
            "`add <text>`, `list`, and `done <index>`. test_store.py holds "
            "pytest tests for store.py, using a temporary file so the tests "
            "do not touch real data. Keep each file focused and small. Then "
            "run the tests and confirm they pass."),
    files={},
    check=_check_multifile,
)


def task_contract(task: "Task") -> dict:
    """The task mode this harness declares for a task: question for the
    conversational probes, work for everything else."""
    if task.conversational:
        return {"task_mode": "question"}
    return {"task_mode": "work"}


def h2_false_rejection(s: Session) -> list[str]:
    """A rejection blaming the file for a defect the file does not have.

    The D9 class: V3 authored the broken content, the gate blocked it, but the
    message named the file, so the model went hunting a bug that was not there.
    Decided against the workspace as it stands after the session.
    """
    out = []
    for ev in s.of_type("tool_result"):
        d = ev.get("data") or {}
        if d.get("success"):
            continue
        err = str(d.get("error") or "")
        # "Your content for X has a syntax error — it was NOT written" is the
        # CORRECT message: it blames the model's submission, and the file on
        # disk is clean precisely because the write was refused. The defect
        # this detector exists for is the opposite — naming the FILE as
        # defective when the file is fine, which sends the model hunting a bug
        # that is not there. Matching both inflated the defect count and would
        # have had someone chasing a non-bug.
        if re.search(r"your content for", err, re.I):
            continue
        m = re.search(r"([\w./-]+\.(?:py|html|htm|js)) has a .*syntax error", err)
        if not m:
            continue
        target = s.workspace / Path(m.group(1)).name
        if not target.exists():
            continue
        ok, _ = _file_parses(target)
        if ok:
            out.append(f"H2 false rejection: {m.group(1)} was blamed for a "
                       f"syntax error it does not have")
    return out


# --------------------------------------------------------------------------
# Runner
# --------------------------------------------------------------------------

def run_session(task: Task, rep: int, url: str, workspace: Path,
                subdir: str, timeout: int, raw_sink=None, prompt_policy: str = "deny") -> Session:
    """`raw_sink`, when given, is an open file the exact SSE lines are written
    to BEFORE anything parses them. A reconstruction bug then stays visible
    instead of overwriting its own evidence -- the parsed events beside it are
    derived, and an unparseable frame is truncated in them but whole here."""
    _reset_workspace(task, workspace)

    # sandbox_subdir, NOT working_dir. The proxy deliberately overrides the
    # client's working_dir with ATLAS_WORKSPACE_DIR (agent.go): the TUI sends
    # its HOST cwd, which does not exist inside the container, and the bind
    # mount is aligned so /workspace already IS the user's directory.
    # Passing a subdir through working_dir is therefore silently ignored, and
    # an earlier version of this harness had every session operating on
    # /workspace while the checks read the subdirectory — so real successes
    # were scored as failures. sandbox_subdir is the field that scopes a run.
    payload = {
        "message": task.prompt,
        "mode": "yolo",
        "sandbox_subdir": subdir,
        "session_id": f"reliability-{task.name}-{rep}",
        # Every owned sender declares a task mode; absence is reserved for
        # external callers. The harness knows which tasks are questions, as
        # the TUI does for /ask: declaring work for them sent a question to
        # the work tier and its planner. It does NOT declare expected outputs
        # or verification: the evaluator and the holdout are offline
        # scoring, not obligations the agent was told to meet, and promoting
        # them here would invent a requirement the task never stated.
        "task_contract": task_contract(task),
    }
    body = json.dumps(payload).encode()
    req = urllib.request.Request(f"{url}/v1/agent", data=body,
                                 headers={"Content-Type": "application/json"})
    events: list[dict] = []
    prompts: list[dict] = []
    stream_ok = False
    t0 = time.time()

    def take(ev: dict) -> None:
        # Arrival time, seconds since the request went out. Saved events had no
        # timing at all, so a question as basic as "where did the 570s go --
        # generation, tool execution, or waiting?" could not be answered from
        # the recorded evidence, and latency claims made without it had to be
        # withdrawn. Inert to every detector, which read `type` and `data`.
        ev["_t"] = round(time.time() - t0, 3)
        events.append(ev)
        if ev.get("type") == "permission_request":
            prompts.append({**answer_prompt(url, payload["session_id"], ev, prompt_policy), "_t": ev["_t"]})

    history: list[dict] = [{"role": "user", "content": task.prompt}]
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            ended = read_stream(resp, take, t0 + timeout, raw_sink)
        stream_ok = ended == "done"
        if ended == "cap":
            # One observed session looped past 20 minutes against a 900s cap,
            # trying to satisfy a self-test it had written with the wrong
            # expectation.
            take({"type": "error", "data": {
                "error": f"harness cap: session exceeded {timeout}s"}})
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        take({"type": "error", "data": {"error": f"stream failed: {e}"}})

    _send_followups(task, rep, url, subdir, timeout, events, history, take, t0)
    wall = time.time() - t0

    s = Session(task=task.name, rep=rep, events=events, workspace=workspace,
                wall_s=wall, stream_ok=stream_ok, prompts=prompts)
    return _judge_outcome(task, workspace, s)


def _reset_workspace(task: Task, workspace: Path) -> None:
    """Empty the workspace, then lay down only this task's fixtures."""
    # Resetting the fixtures alone is not isolation: solve.py from a previous
    # AoC task survived into the next one, and a session that wrote nothing
    # would have been scored on the earlier task's program.
    if workspace.exists():
        for leftover in sorted(workspace.rglob("*"), reverse=True):
            try:
                if leftover.is_file() or leftover.is_symlink():
                    leftover.unlink()
                elif leftover.is_dir():
                    leftover.rmdir()
            except OSError:
                # Best-effort teardown. A leftover the harness cannot remove
                # (busy, permission, vanished under us) must not abort the
                # run — mkdir below recreates the workspace either way, and a
                # survivor shows up as a fixture mismatch in the task check.
                pass
    workspace.mkdir(parents=True, exist_ok=True)
    for name, content in task.files.items():
        (workspace / name).write_text(content)


def _send_followups(task: Task, rep: int, url: str, subdir: str, timeout: int, events: list[dict],
                    history: list[dict], take: Callable[[dict], None], t0: float) -> None:
    """Send each follow-up message of the task in the same session, with the
    exchange so far as its history."""
    # Follow-ups: same session, prior exchange replayed as history. The
    # assistant turn is reconstructed from what it actually emitted.
    for follow in task.followups:
        reply = " ".join(
            str((e.get("data") or {}).get("summary") or (e.get("data") or {}).get("content") or "")
            for e in events if e.get("type") in ("done", "text"))
        history.append({"role": "assistant", "content": reply[:2000] or "(done)"})
        history.append({"role": "user", "content": follow})
        fbody = json.dumps({
            "message": follow, "mode": "yolo", "sandbox_subdir": subdir,
            "session_id": f"reliability-{task.name}-{rep}",
            "history": history[:-1],
            # The same declaration as the first turn. Sent without one, a
            # follow-up was a contractless request: no V3 candidate, and the
            # tier fell back to reading the message.
            "task_contract": task_contract(task),
        }).encode()
        freq = urllib.request.Request(f"{url}/v1/agent", data=fbody,
                                      headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(freq, timeout=timeout) as resp:
                read_stream(resp, take, t0 + timeout)
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            take({"type": "error", "data": {"error": f"followup failed: {e}"}})


def _judge_outcome(task: Task, workspace: Path, s: Session) -> Session:
    """Record whether the session did the task, and the quality of what it
    wrote. A session that changed a fixture it was handed has failed."""
    # Fixture integrity first. A model that rewrites the input it was given is
    # not solving the task, and without this the symptom surfaces as a
    # confusing "wrong answer" — one session overwrote a single-line puzzle
    # input, which write_file allows because the surgical-edit gate only
    # protects existing files over five lines.
    tampered = [n for n in task.immutable
                if (workspace / n).exists()
                and (workspace / n).read_text() != task.files.get(n)]
    if tampered:
        s.task_passed = False
        s.task_detail = (f"modified the fixture it was given: "
                         f"{', '.join(sorted(tampered))}")
        try:
            s.quality = analyze_quality(workspace, set(task.files), _quality_parser(), evaluator_identity()["python_parser"]).as_dict()
        except Exception as e:
            s.quality = {"error": str(e)}
        return s
    try:
        if task.conversational:
            s.task_passed, s.task_detail = task.check(workspace, s)
        else:
            s.task_passed, s.task_detail = task.check(workspace)
    except Exception as e:  # a check that explodes is a failed task, not a crash
        s.task_passed, s.task_detail = False, f"check raised: {e}"
    # Quality of what the agent wrote, excluding the fixtures it was handed.
    try:
        s.quality = analyze_quality(workspace, set(task.files), _quality_parser(), evaluator_identity()["python_parser"]).as_dict()
    except Exception as e:
        s.quality = {"error": str(e)}
    return s


def preflight(sandbox: str, subdir: str, project: str = "atlas", run=subprocess.run) -> list[str]:
    """Refuse to measure a stack that is misconfigured. `project` is that stack's compose project.

    The proxy and the sandbox each bind a host directory at /workspace, and
    nothing in a session fails loudly when those differ: file tools write
    through the proxy's mount while every run_command executes against the
    sandbox's. A whole run then produces confident numbers about an
    environment where edits and verification never met. That happened here —
    proxy on ~/demo, sandbox on ~/demo2 — and cost a full run, so the harness
    now checks before it spends an hour. `atlas doctor` reports the same thing
    under workspace_mounts.
    """
    problems: list[str] = []
    if not sandbox:
        return problems

    def mount_of(container: str) -> str:
        p = run(
            ["docker", "inspect", container, "--format",
             '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'],
            capture_output=True, text=True, timeout=30)
        return (p.stdout or "").strip()

    proxy_mount = mount_of(f"{project or 'atlas'}-atlas-proxy-1")
    sandbox_mount = mount_of(sandbox)
    if proxy_mount and sandbox_mount and proxy_mount != sandbox_mount:
        problems.append(
            f"proxy and sandbox bind DIFFERENT host dirs at /workspace "
            f"(proxy={proxy_mount} sandbox={sandbox_mount}). Edits and "
            f"verification would run on separate filesystems. Fix: set "
            f"ATLAS_PROJECT_DIR in .env, then "
            f"`docker compose up -d --force-recreate atlas-proxy sandbox`")

    # The subdir has to be visible to the sandbox too, or every verification
    # command fails with "cwd does not exist" and the task looks unsolvable.
    if subdir:
        p = run(["docker", "exec", sandbox, "test", "-d",
                 f"/workspace/{subdir}"], capture_output=True, timeout=30)
        if p.returncode != 0:
            problems.append(
                f"/workspace/{subdir} does not exist inside {sandbox} — every "
                f"run_command would fail with 'cwd does not exist'")
    return problems


def container_states(project: str, run=subprocess.run) -> dict | None:
    """Each container of the compose project, by name: its restart count,
    whether it was OOM-killed, and when it last started.

    None when docker cannot be asked (the runner may point at a stack it
    cannot inspect). `run` is subprocess.run, injectable for tests."""
    try:
        ps = run(["docker", "ps", "-a", "--filter",
                  f"label=com.docker.compose.project={project}", "--format", "{{.Names}}"],
                 capture_output=True, text=True, timeout=30)
        names = (ps.stdout or "").split()
        if ps.returncode != 0 or not names:
            return None
        p = run(["docker", "inspect", "--format",
                 "{{.Name}} {{.RestartCount}} {{.State.OOMKilled}} {{.State.StartedAt}}", *names],
                capture_output=True, text=True, timeout=30)
        if p.returncode != 0:
            return None
    except (OSError, subprocess.SubprocessError):
        return None
    out = {}
    for line in (p.stdout or "").splitlines():
        parts = line.split()
        if len(parts) != 4 or not parts[1].isdigit():
            continue
        out[parts[0].lstrip("/")] = {"restarts": int(parts[1]), "oom": parts[2] == "true",
                                     "started": parts[3]}
    return out or None


def stack_changes(before: dict | None, after: dict | None) -> list[str]:
    """What happened to the stack's containers between two snapshots.

    A restart by the restart policy raises the restart count; a manual
    restart or a recreate only moves the start time, so both are read."""
    if before is None and after is None:
        return []
    if before is None or after is None:
        return ["container state could not be read at the "
                + ("start" if before is None else "end") + " of the session"]
    out = []
    for name in sorted(before.keys() | after.keys()):
        b, a = before.get(name), after.get(name)
        if a is None:
            out.append(f"{name} is gone")
            continue
        if b is None:
            out.append(f"{name} appeared")
            continue
        if a["restarts"] > b["restarts"]:
            n = a["restarts"] - b["restarts"]
            out.append(f"{name} restarted {n} time{'s' if n > 1 else ''}")
        elif a["started"] != b["started"]:
            out.append(f"{name} was restarted or recreated")
        if a["oom"] and not (b["oom"] and a["started"] == b["started"]):
            out.append(f"{name} was OOM-killed")
    return out


# The five services a stack runs, as scripts/deploy-gated.sh names them.
STACK_SERVICES = ("llama-server", "geometric-lens", "v3-service", "sandbox", "atlas-proxy")


def running_images(project: str, run=subprocess.run) -> dict | None:
    """The image id each running service of the compose project uses.

    None when docker cannot be asked. `run` is subprocess.run, injectable for
    tests."""
    try:
        ps = run(["docker", "ps", "--filter",
                  f"label=com.docker.compose.project={project}", "--format", "{{.Names}}"],
                 capture_output=True, text=True, timeout=30)
        names = (ps.stdout or "").split()
        if ps.returncode != 0 or not names:
            return None
        p = run(["docker", "inspect", "--format",
                 '{{index .Config.Labels "com.docker.compose.service"}} {{.Image}}', *names],
                capture_output=True, text=True, timeout=30)
        if p.returncode != 0:
            return None
    except (OSError, subprocess.SubprocessError):
        return None
    out = {}
    for line in (p.stdout or "").splitlines():
        parts = line.split()
        if len(parts) == 2:
            out[parts[0]] = parts[1]
    return out or None


def _same_commit(a: str, b: str) -> bool:
    """Two commit ids name one commit when the shorter is a prefix of the
    longer and at least 7 characters long."""
    a, b = (a or "").strip(), (b or "").strip()
    short = min(len(a), len(b))
    return short >= 7 and a[:short] == b[:short]


def deployed_identity(project: str, deploy_dir: Path, commit: str,
                      run=subprocess.run) -> dict:
    """The commit and the five images this run measures (#241).

    A result is evidence only for the stack that produced it. The gated deploy
    records the commit it deployed (DEPLOYED_SHA) and, in
    deployed/running-<sha>.json, the image each of the five services runs for
    it. llama-server is kept, not rebuilt, when inference/ did not change, so
    an image's own build commit is not the test: the record is. The stack is
    that commit's only when the stated commit is the deployed one and every
    running service uses the image recorded for it.

    Returns {"commit", "images", "verified", "mismatch", "problems"}. A
    mismatch means a record exists and the stack differs from it: a run is
    refused. With no record there is nothing to check against (a stack this
    script did not deploy): the run is unverified, and says so."""
    out = {"commit": commit, "images": {}, "verified": False, "mismatch": False,
           "problems": []}
    try:
        deployed = (deploy_dir / "DEPLOYED_SHA").read_text().strip()
    except OSError:
        out["problems"].append(f"no gated deploy record in {deploy_dir}: the stack is unverified")
        live = running_images(project, run) if project else None
        out["images"] = live or {}
        return out
    out["commit"] = deployed
    if commit and not _same_commit(commit, deployed):
        out["problems"].append(f"the checkout is at {commit}, but the stack was deployed from {deployed}")
    try:
        record = json.loads((deploy_dir / "deployed" / f"running-{deployed}.json").read_text())
        recorded = {svc: (v or {}).get("image_id", "") for svc, v in (record.get("running") or {}).items()}
    except (OSError, ValueError):
        recorded = {}
        out["problems"].append(f"no record of the images deployed for {deployed}")
    live = running_images(project, run) if project else None
    out["images"] = live or {}
    if live is None:
        out["problems"].append("docker could not be asked which images are running")
    else:
        for svc in STACK_SERVICES:
            if svc not in live:
                out["problems"].append(f"{svc} is not running")
            elif recorded and live[svc] != recorded.get(svc):
                out["problems"].append(
                    f"{svc} runs {live[svc][:19]}, not the {recorded.get(svc, '?')[:19]} deployed for {deployed}")
    out["mismatch"] = bool(out["problems"])
    out["verified"] = not out["problems"]
    return out


def result_row(s: Session, evaluator, stack) -> dict:
    """One session in the run's JSON result."""
    return {
        "task": s.task, "rep": s.rep, "task_passed": s.task_passed,
        "task_detail": s.task_detail, "defects": s.defects,
        "task_mode": task_contract(TASKS[s.task])["task_mode"] if s.task in TASKS else None,
        "turns": len(s.of_type("turn_start")),
        "tools": len(s.of_type("tool_call")), "wall_s": round(s.wall_s, 1),
        "v3": s.v3,
        "quality": s.quality,
        "stack_changes": s.stack_changes,
        "prompts": s.prompts,
        "evaluator": evaluator,
        "stack": stack,
    }


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default=os.environ.get("ATLAS_PROXY_URL",
                                                    "http://127.0.0.1:8090"))
    ap.add_argument("--workspace", default=os.environ.get("ATLAS_PROJECT_DIR", ""),
                    help="host path of the subdirectory named by --subdir")
    ap.add_argument("--subdir", default="_reliability",
                    help="workspace subdirectory to confine each run to "
                         "(sent as sandbox_subdir); --workspace must be the "
                         "host path of this same subdirectory")
    ap.add_argument("--sandbox-container", default="atlas-sandbox-1",
                    help="'' to skip the background-leak check")
    ap.add_argument("--compose-project", default="atlas",
                    help="compose project whose containers are checked for restarts "
                         "and OOM kills around each session; '' to skip")
    ap.add_argument("--deploy-dir", default=os.environ.get(
                        "ATLAS_DEPLOY_DIR", str(Path.home() / "atlas-ralph")),
                    help="the gated deploy's record directory (scripts/deploy-gated.sh)")
    ap.add_argument("--commit", default="",
                    help="the commit this run claims to measure; default: this checkout's HEAD")
    ap.add_argument("--tasks", default=",".join(TASKS))
    ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--timeout", type=int, default=900)
    ap.add_argument("--json", dest="json_out", default="",
                    help="write the results here; each defect is also appended to "
                         "<name>.defects.jsonl as its session ends, and a SIGINT or "
                         "SIGTERM writes the partial results and <name>.summary.txt")
    ap.add_argument("--save-events", default="",
                    help="directory to write each session's raw event stream "
                         "to; without it a failure can only be diagnosed from "
                         "container logs, which roll")
    ap.add_argument("--prompts", choices=sorted(PROMPT_POLICIES), default="deny",
                    help="what the runner answers when a session asks for a permission (a deletion "
                         "always asks): " + "; ".join(f"{k}: {v}" for k, v in sorted(PROMPT_POLICIES.items()))
                         + ". The policy and each answer are kept in the results")
    return ap.parse_args(argv)


def main() -> int:
    args = parse_args()

    if not args.workspace:
        print("error: --workspace (or ATLAS_PROJECT_DIR) must be set — it has "
              "to be the host path the proxy mounts.", file=sys.stderr)
        return 2
    ws = Path(args.workspace)
    if not ws.is_dir():
        print(f"error: workspace {ws} is not a directory", file=sys.stderr)
        return 2

    TASKS["flask_pause"].files["app.py"] = _read_fixture("snake_app.py")

    selected = [TASKS[n] for n in args.tasks.split(",") if n in TASKS]
    if not selected:
        print(f"error: no known tasks in {args.tasks!r}", file=sys.stderr)
        return 2

    # What the proxy says it runs, read once before any session and kept
    # with every result.
    STACK = stack_identity(args.url)
    # And which commit and images it is (#241). A stack that differs from its
    # gated deploy record is not measured at all.
    commit = args.commit
    if not commit:
        head = subprocess.run(["git", "-C", str(REPO), "rev-parse", "--short", "HEAD"],
                              capture_output=True, text=True)
        commit = head.stdout.strip() if head.returncode == 0 else ""
    identity = deployed_identity(args.compose_project, Path(args.deploy_dir), commit)
    if identity["mismatch"]:
        for line in identity["problems"]:
            print(f"error: {line}", file=sys.stderr)
        print("error: refusing to measure a stack that is not the one deployed "
              "for this commit", file=sys.stderr)
        return 2
    for line in identity["problems"]:
        print(f"warning: {line}", file=sys.stderr)
    STACK.update(commit=identity["commit"], images=identity["images"],
                 identity_verified=identity["verified"], prompt_policy=args.prompts)

    use_sandbox(args)
    if problems := preflight(args.sandbox_container, args.subdir, args.compose_project):
        for line in problems:
            print(f"error: {line}", file=sys.stderr)
        return 2

    known = tui_handled_types()
    if not known:
        print("error: could not read the TUI event dispatcher", file=sys.stderr)
        return 2
    # Types the harness itself synthesises are not TUI concerns.
    known |= {"__unparseable__"}

    sessions: list[Session] = []
    total = len(selected) * args.reps
    defects_log = sibling(args.json_out, ".defects.jsonl")
    stopped = 0
    with stop_on_signals():
        try:
            for rep in range(1, args.reps + 1):
                for task in selected:
                    print(f"[{len(sessions) + 1}/{total}] {task.name} rep {rep} ...", flush=True)
                    sessions.append(_scored_session(task, rep, args, ws, known, defects_log))
        except Stopped as e:
            stopped = e.signum
            print(f"\nstopped by {signal.Signals(stopped).name} after {len(sessions)} "
                  f"of {total} session(s)", flush=True)
    return _finish(sessions, known, args, STACK, stopped)


def _scored_session(task: Task, rep: int, args, ws: Path, known: set[str],
                    defects_log: Path | None) -> Session:
    """Run one session, apply the harness-defect detectors, and report it as it ends."""
    if args.sandbox_container:
        subprocess.run(["docker", "exec", args.sandbox_container,
                        "pkill", "-f", "python app"],
                       capture_output=True, timeout=30)
    before = container_states(args.compose_project) if args.compose_project else None
    s = run_session(task, rep, args.url, ws,
                    args.subdir, args.timeout, prompt_policy=args.prompts)
    if args.compose_project:
        s.stack_changes = stack_changes(before, container_states(args.compose_project))
    s.defects += h1_protocol(s, known)
    s.defects += h2_false_rejection(s)
    s.defects += h3_dead_end_steering(s)
    s.defects += h4_gate_escape(s, task)
    s.defects += h5_corrupt_write(s, task)
    s.defects += h6_service_fault(s)
    s.defects += h8_anchored_on_injected_text(s)
    s.defects += h9_tier_misapplied(s, task)
    s.defects += h7_background_leak(args.sandbox_container, s)
    if args.save_events:
        d = Path(args.save_events)
        d.mkdir(parents=True, exist_ok=True)
        (d / f"{task.name}-rep{rep}.jsonl").write_text(
            "\n".join(json.dumps(e) for e in s.events))
    log_defects(defects_log, task.name, rep, s.defects)
    turns = len(s.of_type("turn_start"))
    print(f"      task={'PASS' if s.task_passed else 'fail'} "
          f"harness={'clean' if not s.defects else str(len(s.defects)) + ' defect(s)'} "
          f"turns={turns} {s.wall_s:.0f}s — {s.task_detail[:70]}",
          flush=True)
    for d in s.defects:
        print(f"      ! {d}", flush=True)
    for c in s.stack_changes:
        print(f"      ! stack: {c}", flush=True)
    for p in s.prompts:
        print(f"      ? asked to approve {p['tool']}: answered {p['answer'] or 'nothing'} "
              f"(policy {p['policy']}, delivered: {p['delivered']})", flush=True)
    return s


def _finish(sessions: list[Session], known: set[str], args, stack: dict, stopped: int) -> int:
    """The summary and the JSON result. After a stop, both cover the sessions
    that finished, and the summary is also written to <name>.summary.txt."""
    if sessions:
        report(sessions, known, model_output_guards)
    EVAL_ID = evaluator_identity()
    print(f"evaluator: {EVAL_ID}")
    print(f"stack: {stack}")
    if args.json_out:
        Path(args.json_out).write_text(json.dumps([result_row(s, EVAL_ID, stack) for s in sessions], indent=2))
        print(f"\nwrote {args.json_out}")
    if stopped:
        summary = sibling(args.json_out, ".summary.txt")
        write_summary(summary, sessions, known, model_output_guards,
                      [f"stopped by {signal.Signals(stopped).name} after {len(sessions)} session(s)",
                       f"evaluator: {EVAL_ID}", f"stack: {stack}"])
        if summary and sessions:
            print(f"wrote {summary}")
        return 128 + stopped
    return 0 if all(not s.defects for s in sessions) else 1


def stack_identity(url: str) -> dict:
    """What the proxy reports it is running: the grammar mode (/version) and
    the lens and steering state (/v1/calibration/status). Every recorded
    dev-server run was steered and ran loose, and nothing in the evidence
    said so; a measurement that cannot say which configuration it ran
    against cannot be compared with another."""
    raw = {}
    for key, path in (("version", "/version"), ("calibration", "/v1/calibration/status")):
        try:
            with urllib.request.urlopen(f"{url}{path}", timeout=10) as r:
                raw[key] = json.loads(r.read().decode("utf-8", "replace"))
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as e:
            raw[key] = {"error": str(e)[:200]}
    version, calib = raw["version"], raw["calibration"]
    asa = calib.get("asa") or {}
    lens = calib.get("lens") or {}
    return {
        "api_version": version.get("api_version"),
        "grammar_mode": version.get("grammar_mode"),
        "asa": asa.get("verdict"),
        "asa_detail": asa.get("hint"),
        "lens": lens.get("verdict"),
        "errors": {k: v["error"] for k, v in raw.items() if "error" in v},
    }


if __name__ == "__main__":
    raise SystemExit(main())
