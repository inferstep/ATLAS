"""Two crashing candidates must never form a winning cluster.

Third-party audit reproduction: the probe swallowed exceptions and printed
the transport marker regardless, and repr('') is the two-character string
"''" — truthy — so two ordinary-exception candidates agreed on empty output
and crash consensus won (WINNERS [0, 1]). The prior regression test used
SystemExit, which inherits from BaseException, not Exception, so it passed
while missing the defect.
"""

import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import pipeline  # noqa: E402

CRASH_RUNTIME = "raise RuntimeError('boom')\n"
CRASH_KEYERROR = "x = {}\nprint(x['missing'])\n"
GOOD_A = "print(len(open('input.txt').read().split()))\n"
GOOD_B = "print(sum(1 for _ in open('input.txt').read().split()))\n"


def _sandbox(code, test_input="", files=None, **_):
    """The executor's shape: stage the request's files into a fresh
    workspace, then run from it."""
    work = tempfile.mkdtemp()
    for name, content in (files or {}).items():
        Path(work, name).write_text(content)
    Path(work, "t.py").write_text(code)
    run = subprocess.run([sys.executable, "t.py"], cwd=work,
                         capture_output=True, text=True,
                         stdin=subprocess.DEVNULL, timeout=60)
    return run.returncode == 0, run.stdout, run.stderr


def _case():
    return type("TC", (), {"input_str": "1 2 3", "expected_output": "3"})()


def _cands(*codes):
    return [{"index": i, "code": c} for i, c in enumerate(codes)]


def test_ordinary_exceptions_do_not_cluster():
    """The audit's exact reproduction, with Exception subclasses."""
    winners = pipeline._consensus_winners(
        _cands(CRASH_RUNTIME, CRASH_KEYERROR, GOOD_A),
        [_case()], _sandbox, lambda *a, **k: None, "input.txt")
    assert not any(c["index"] in (0, 1) for c in winners), winners


def test_agreeing_healthy_candidates_still_win_over_crashers():
    winners = pipeline._consensus_winners(
        _cands(CRASH_RUNTIME, GOOD_A, GOOD_B),
        [_case()], _sandbox, lambda *a, **k: None, "input.txt")
    assert sorted(c["index"] for c in winners) == [1, 2], winners


def test_partial_validity_is_not_agreement_material():
    """A candidate must answer EVERY probe case to enter clustering.

    Third-party audit follow-up: with any() a pair of candidates that
    crashed on one case but matched on the other formed the winning
    cluster — code proven broken on half the consensus inputs won it.
    """
    crash_on_empty = ("data = open('input.txt').read().split()\n"
                      "print(data[2])\n")  # IndexError when input is short
    case_long = _case()
    case_short = type("TC", (), {"input_str": "1", "expected_output": "1"})()
    winners = pipeline._consensus_winners(
        _cands(crash_on_empty, crash_on_empty, GOOD_A),
        [case_long, case_short], _sandbox, lambda *a, **k: None, "input.txt")
    assert not any(c["index"] in (0, 1) for c in winners), winners


def test_the_probe_marks_crashes_distinctly():
    body, files = pipeline._make_output_probe(CRASH_RUNTIME, _case(), "input.txt")
    ok, out, _err = _sandbox(body, files=files)
    assert "CRASH" in out
    assert "''" not in out
