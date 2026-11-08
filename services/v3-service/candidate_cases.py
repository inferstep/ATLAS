"""The candidate case wrappers and the CodeT consensus over them.
_make_self_test runs a candidate against one self-test case, and
_make_output_probe runs it on an input that has no expected output.
_evaluate_candidate checks a candidate against its contract, and
_consensus_winners picks the candidates whose outputs agree. The run steps
and pipeline.py call these functions."""

import re
from typing import Optional

from stages.self_test_gen import PROVENANCE_TRUSTED

import adapters
import contract
from candidate_records import _task_identity


def _entry_function(code: str) -> Optional[str]:
    """Name the function a self-test should call.

    The first `def` is the entry point only when the solution defines no
    helpers above it. With `def parse(...)` followed by `def solve(...)`,
    every test would call parse with the case input and compare it to the
    final answer — a guaranteed failure that says nothing about the code.

    The entry point is the top-level function nothing else in the file
    calls. Where several qualify a conventional name wins, then the last
    one, since helpers are conventionally defined above their caller.
    Unparseable code falls back to the first def.
    """
    import ast as _ast
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        m = re.search(r'^def (\w+)\(', code, re.MULTILINE)
        return m.group(1) if m else None
    top = [n for n in tree.body if isinstance(n, _ast.FunctionDef)]
    if not top:
        return None
    names = {n.name for n in top}
    called = set()
    for node in _ast.walk(tree):
        if isinstance(node, _ast.Call) and isinstance(node.func, _ast.Name):
            if node.func.id in names:
                called.add(node.func.id)
    roots = [n.name for n in top if n.name not in called]
    for preferred in ("solve", "main", "run"):
        if preferred in roots:
            return preferred
    if roots:
        return roots[-1]
    return top[-1].name


def _entry_takes_case_input(code: str, name: str) -> bool:
    """Whether the case input can be passed to `name` and its answer read
    back as a return value.

    The two self-test shapes are not interchangeable. A function-shaped
    solution takes the case input as arguments and returns the answer. A
    script-shaped one reads stdin and prints — calling it with the case
    input raises TypeError, and there is no return value to compare.

    Measured on aoc_sonar: candidates are `def main():` reading sys.stdin
    and printing, so the function path called main(case) and every case
    failed. The stdin/stdout path below handles exactly this and was never
    reached, because the choice keyed on a function merely existing.

    Requires all three: the function takes at least one parameter, it
    returns a value somewhere, and the file does not read stdin itself.
    """
    import ast as _ast
    if "sys.stdin" in code or "input()" in code:
        return False
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        return True  # unparseable code keeps the function-shaped path
    for node in _ast.walk(tree):
        if isinstance(node, _ast.FunctionDef) and node.name == name:
            args = node.args
            takes = bool(args.args or args.posonlyargs or args.kwonlyargs
                         or args.vararg)
            returns = any(isinstance(n, _ast.Return) and n.value is not None
                          for n in _ast.walk(node))
            return takes and returns
    return False


def _reads_input_file(code: str):
    """The literal filename a candidate opens for reading, if it opens one.

    Returns None when the program takes no named file, which is the case the
    stdin path below already covers.

    Only string literals count. A computed path cannot be materialised without
    running the program, and guessing one wrong is worse than falling through
    to the existing behaviour.
    """
    import ast as _ast
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        return None
    for node in _ast.walk(tree):
        if not isinstance(node, _ast.Call):
            continue
        func = node.func
        name = getattr(func, "id", None) or getattr(func, "attr", None)
        if name not in ("open", "read_text", "read_bytes", "Path"):
            continue
        if not node.args:
            continue
        target = node.args[0]
        if not (isinstance(target, _ast.Constant) and isinstance(target.value, str)):
            continue
        # A write/append target is an output file, not the program's input.
        mode = node.args[1] if len(node.args) > 1 else None
        if isinstance(mode, _ast.Constant) and isinstance(mode.value, str):
            if any(ch in mode.value for ch in ("w", "a", "x")):
                continue
        for kw in node.keywords:
            if kw.arg == "mode" and isinstance(kw.value, _ast.Constant):
                if any(ch in str(kw.value.value) for ch in ("w", "a", "x")):
                    break
        else:
            if "/" not in target.value and "\\" not in target.value:
                return target.value
    return None


_SOURCE_SUFFIXES = (".py", ".js", ".ts", ".go", ".java", ".rb", ".php", ".rs",
                    ".c", ".cpp", ".h", ".html", ".css", ".md", ".json",
                    ".yaml", ".yml", ".toml")


def _task_input_file(project_files) -> str:
    """The data file the task supplies for the program to read, if any.

    Verification has to hold a candidate to the environment's contract, not
    to whichever contract the candidate happens to prefer. The caller runs
    `python solve.py` with this file on disk and NO stdin, so a candidate
    that reads stdin must fail here rather than be handed a stdin the real
    run will never provide.

    Measured: with the self-test choosing its shape from the candidate's own
    code, stdin-reading candidates passed verification and were selected in 3
    sessions — every one of which then failed the task, printing 0 because no
    stdin arrived.
    """
    if not project_files:
        return ""
    for name in project_files:
        if not name.lower().endswith(_SOURCE_SUFFIXES):
            return name
    return ""


# The candidate is staged beside its input rather than spliced into the
# wrapper, so the bytes that run are the bytes that were generated.
_CANDIDATE_FILE = "candidate.py"


def _trusted_oracle(self_tests) -> bool:
    """Whether every case in this suite declares trusted provenance.

    Model-generated cases are not trusted: the same model wrote the code and
    the answer, from the problem statement alone, and for these tasks
    producing the expected output IS solving the problem. Measured on the
    captured pool: 21 of 36 valid generated keys disagreed with the task's
    own reference, and a correct candidate scored 2/5 against its own suite.

    The answer decides only whether verified_sandbox labels the score
    `self_test_untrusted`; no suite rejects a candidate. Nothing in this
    service produces a trusted case -- self_test_gen marks every case it
    writes as generated -- so every score here carries that label.

    Unknown provenance fails closed. A case that cannot say where it came
    from is not trusted, so a future producer must opt in deliberately
    rather than inherit trust by omission.
    """
    cases = getattr(self_tests, "test_cases", None) or []
    if not cases:
        return False
    return all(getattr(tc, "provenance", None) == PROVENANCE_TRUSTED
               for tc in cases)


def _staged_candidate_run(code: str, inp: str, infile: str):
    """How a case runs a candidate, for both builders that need it.

    Returns ``(header, run, files)``: the files the sandbox stages, the
    source that attaches stdin and captures stdout, and the one statement
    that executes the candidate. The self-test and the output probe differ
    only in what they do with the captured output and with an exception, so
    they share this and nothing else -- two copies of "how to run a
    candidate" drift apart when only one gets fixed.

    The case's input is staged, never written by executable code: that write
    would land in the candidate's working directory, which is read-only in
    the sandbox, and raise before the candidate ran. The candidate executes
    once, from a file, under the name a program is really run with; exec'd
    inside an imported module, ``__name__`` would be the module name and an
    ``if __name__ == "__main__":`` body would never run.
    """
    files = {_CANDIDATE_FILE: code}
    if infile:
        # Empty stdin, not absent stdin. The caller runs the program with
        # stdin at EOF, so a candidate that reads sys.stdin must terminate
        # immediately and fail fast. With no stdin attached it BLOCKS until
        # the sandbox timeout instead: measured live, one stdin candidate
        # turned the probe into a 300s hang.
        files[infile] = inp
        stdin_setup = "_s.stdin=_o.StringIO('')\n"
    else:
        stdin_setup = f"_s.stdin=_o.StringIO({repr(inp)})\n"
    header = ("import sys as _s,io as _o,runpy as _rp\n" + stdin_setup
              + "_c=_o.StringIO()\n_old=_s.stdout\n_s.stdout=_c\n")
    run = f"_rp.run_path({repr(_CANDIDATE_FILE)},run_name='__main__')\n"
    return header, run, files


def _make_self_test(code: str, tc, task_input_file: str = ""):
    """Build one case's executable check, and the files the sandbox stages.

    Returns ``(wrapper_source, files)``. The wrapper creates nothing: the
    case's input file is staged through the sandbox's request files map, the
    same mechanism that already carries project context. Written from
    executable code, it would land in the candidate's working directory,
    which is read-only in the sandbox container, and the case would die with
    ``OSError: [Errno 30]`` before the candidate ran.

    The candidate then runs through ``runpy.run_path(..., run_name="__main__")``
    -- once, from a file, with the name a program is actually run under, so
    an ``if __name__ == "__main__":`` body executes.

    Uses ast.literal_eval (safe — only parses Python literals) to convert
    I/O string representations to actual values for comparison.
    All code runs inside the sandboxed container.
    """
    inp = tc.input_str.strip()
    exp = tc.expected_output.strip()
    name = _entry_function(code)
    if name and _entry_takes_case_input(code, name):
        return (code + "\nimport ast as _a\n"
            + f"_i={repr(inp)}\n_e={repr(exp)}\n"
            + "try:\n _p=_a.literal_eval(_i)\nexcept:\n _p=_i\n"  # bare except inside generated user code, intentional
            + f"_r={name}(*_p) if isinstance(_p,tuple) else {name}(_p) if isinstance(_p,list) else {name}(_p)\n"
            + "try:\n _ev=_a.literal_eval(_e)\nexcept:\n _ev=_e\n"  # bare except inside generated user code, intentional
            + "assert str(_r)==str(_ev) or _r==_ev,f'got {_r}'\nprint('SELF_TEST_PASS')\n"), {}
    # A program that reads a named file has to be given that file. Feeding it
    # stdin instead tests a contract the task never stated, and the verdict
    # comes out backwards: a candidate that correctly reads input.txt finds no
    # such file in the sandbox and FAILS, while one that reads stdin passes.
    # Verification then selects for the shape that cannot work when the caller
    # runs `python solve.py` with no stdin.
    #
    # Measured on the two AoC tasks whose input is a file: 9 of 12 candidates
    # ATLAS wrote read stdin, against a prompt that says "reads input.txt",
    # and both tasks sat at 7/26. The same model asked directly, with no
    # pipeline, wrote input.txt readers and scored 12/12.
    infile = task_input_file or _reads_input_file(code)
    header, run, files = _staged_candidate_run(code, inp, infile)
    # A SystemExit the program raised itself is ordinary termination when its
    # code is 0 or None, and an execution failure otherwise; anything else
    # propagates and the process exits non-zero.
    return (
        header
        + "try:\n"
        + "    " + run
        + "except SystemExit as _e:\n"
        "    if _e.code not in (0,None):\n"
        "        raise\n"
        "finally:\n"
        " _s.stdout=_old\n"
        f"assert _c.getvalue().strip()=={repr(exp)},f'got {{_c.getvalue().strip()}}'\n"
        "print('SELF_TEST_PASS')\n"), files


_CONSENSUS_MARK = "V3_OUT:"


def _evaluate_candidate(file_path, code, smoke_passed, emit, *, task=None):
    """THE canonical contract record for ONE artifact.

    An unsupported candidate stays available as a fallback but is never
    represented as verified.

    One record, bound to the exact bytes it describes. No parallel strength,
    score or coverage field is kept beside it: a second authoritative copy is
    how two answers about one candidate start to disagree.
    """
    adapter = adapters.select_adapter(file_path, code)
    task = task or _task_identity("", "")
    return adapters.contract_record(
        adapter=adapter, accepted=bool(smoke_passed),
        contract_id=task["contract_id"], contract_version=task["contract_version"],
        artifact_scope=task["artifact_scope"],
        evaluation_context_hash=task["evaluation_context_hash"],
        candidate_content_hash=contract.content_hash(code))


def _make_output_probe(code: str, tc, task_input_file: str = ""):
    """Run a candidate on a case's INPUT and report what it printed.

    Returns ``(wrapper_source, files)`` — the same staged shape the self-test
    uses, through the same ``_staged_candidate_run``.

    The self-test compares that output to the model's predicted answer. For a
    problem the model cannot reliably solve, producing the answer IS the
    problem, so the prediction can be wrong and correct code fail its own
    suite. Uniform zero is a broken answer key, not broken code.

    CodeT (Chen et al., 2022), which this pipeline already cites, does not use
    generated tests as an oracle, for exactly this reason. It runs candidates
    on the generated INPUTS and clusters them by agreement, so the signal is
    the answer candidates converge on rather than the answer the model
    guessed. This probe produces the raw material for that.
    """
    inp = tc.input_str.strip()
    name = _entry_function(code)
    if name and _entry_takes_case_input(code, name):
        # The pair every caller unpacks. A bare string makes the unpack
        # raise, the caller's except turns that into "no answer", and a
        # function-shaped candidate can never join a cluster.
        return (code + "\nimport ast as _a\n"
                + f"_i={repr(inp)}\n"
                + "try:\n _p=_a.literal_eval(_i)\nexcept:\n _p=_i\n"
                + f"_r={name}(*_p) if isinstance(_p,tuple) else {name}(_p)\n"
                + f"print({repr(_CONSENSUS_MARK)}+repr(str(_r).strip()))\n"), {}
    infile = task_input_file or _reads_input_file(code)
    header, run, files = _staged_candidate_run(code, inp, infile)
    # The marker is emitted ONLY on clean completion, and only for non-empty
    # output. Printed regardless, it would let two CRASHING candidates agree:
    # repr('') is the two-character string "''", which is truthy, so their
    # empty outputs would cluster and crash consensus would win. A crash prints
    # a CRASH line the clustering explicitly refuses, and silence emits nothing.
    #
    # The probe reports rather than fails, so unlike the self-test it catches
    # what the candidate raised instead of letting it end the process. A
    # SystemExit the program raised itself is ordinary termination at code 0
    # or None -- a candidate that prints its answer and calls sys.exit() has
    # answered, and is not a crash.
    return (header
            + "_crashed=False\n"
            + "try:\n"
            + "    " + run
            + "except SystemExit as _e:\n    _crashed=_e.code not in (0,None)\n"
            + "except BaseException:\n    _crashed=True\n"
            + "finally:\n _s.stdout=_old\n"
            + "_out=_c.getvalue().strip()\n"
            + f"if _crashed:\n    print({repr(_CONSENSUS_MARK)}+'CRASH')\n"
            + f"elif _out:\n    print({repr(_CONSENSUS_MARK)}+repr(_out))\n"), files


def _failed_a_project_check(candidate) -> bool:
    """The candidate failed a check the generated cases did not write: the
    project's own build command, importing where the submitted baseline
    imports, or keeping the top-level names the submitted file binds.
    Candidates agreeing with each other does not outweigh that."""
    return any(ev.get("verifier") in ("build_command", "python_import_comparison",
                                      "incumbent_role")
               and ev.get("status") == "failed"
               for ev in candidate.get("verification_evidence") or [])


def _consensus_winners(candidates, test_cases, sandbox, emit,
                       task_input_file=""):
    """CodeT agreement: candidates whose outputs match the largest cluster.

    Returns [] when there is nothing to agree on — fewer than two candidates,
    no case produced output, or every candidate disagreed with every other.
    Agreement between independently generated programs is evidence; one
    program agreeing with itself is not, so a lone cluster does not win.

    A candidate enters clustering only if it produced a real answer on EVERY
    probe case. Partial validity is not agreement material: two candidates
    that crashed on most cases but matched on one must not form the winning
    cluster.

    A cluster is sized by its DISTINCT programs. Byte-identical candidates
    are one program agreeing with itself, and counting them twice would let
    two copies of a wrong program outvote one correct program.

    Agreement is not verification: the caller marks winners `consensus`, not
    `passed`.
    """
    if len(candidates) < 2 or not test_cases:
        return []
    sigs = {}
    for c in candidates:
        outs = []
        for tc in test_cases:
            try:
                probe_code, probe_files = _make_output_probe(
                    c["code"], tc, task_input_file)
                ok, out, _ = sandbox(probe_code, files=probe_files)
            except Exception:
                ok, out = False, ""
            marker = ""
            if ok and _CONSENSUS_MARK in (out or ""):
                marker = out.split(_CONSENSUS_MARK)[-1].strip()
            if marker == "CRASH" or marker == "''":
                # A crash or empty output is not an answer to agree on.
                marker = ""
            outs.append(marker)
        if all(outs):
            sigs.setdefault(tuple(outs), []).append(c)
    if not sigs:
        return []

    def distinct(cluster):
        return len({contract.content_hash(c["code"]) for c in cluster})

    best = max(sigs.values(), key=distinct)
    if distinct(best) < 2:
        return []
    emit("consensus", f"{distinct(best)}/{len(candidates)} distinct candidates agree",
         cluster=distinct(best), clusters=len(sigs))
    return best
