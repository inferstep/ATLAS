"""Agreement, not the model's guess, decides when the answer key is wrong.

The self-test compares a candidate's output to an expected value the same
model produced. For a task it cannot reliably solve, producing the answer IS
the task, so the key is wrong and correct code fails its own suite.

Measured in one run: 42 self-test verifications, every one 0/N, and V3
selected a candidate in 0 of 12 sessions — every session shipped the model's
own draft. A candidate pulled from those logs passed immediately when given a
correct expected value, so the code was never the problem.

CodeT (Chen et al., 2022), already cited for this pipeline, does not use
generated tests as an oracle. It runs candidates on the generated INPUTS and
clusters them by agreement. These tests pin that behaviour.
"""

import json
import subprocess
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import pipeline

PROJECT_ROOT = Path(__file__).resolve().parents[2]

CORRECT_A = ("def main():\n"
             "    d = [int(x) for x in open('input.txt')]\n"
             "    print(sum(1 for i in range(1, len(d)) if d[i] > d[i-1]))\n"
             "main()\n")
CORRECT_B = ("def main():\n"
             "    n = [int(v) for v in open('input.txt').read().split()]\n"
             "    print(len([1 for i in range(1, len(n)) if n[i] > n[i-1]]))\n"
             "main()\n")
WRONG = ("import sys\n"
         "def main():\n"
         "    print(len([int(x) for x in sys.stdin]))\n"
         "main()\n")


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


def _case(expected="THIS KEY IS WRONG"):
    return type("TC", (), {"input_str": "199\n200\n208\n210\n200",
                           "expected_output": expected})()


def _case3(expected):
    return type("TC", (), {"input_str": "1\n2\n3",
                           "expected_output": expected})()


def _cands(*codes):
    return [{"index": i, "code": c} for i, c in enumerate(codes)]


def test_agreeing_candidates_win_despite_a_wrong_key():
    """The measured situation: the key is wrong, the code is not."""
    winners = pipeline._consensus_winners(
        _cands(CORRECT_A, WRONG, CORRECT_B), [_case()], _sandbox, lambda *a, **k: None)
    assert sorted(c["index"] for c in winners) == [0, 2]


def test_one_candidate_cannot_agree_with_itself():
    """Agreement between independent programs is evidence; a single program
    reproducing its own output is not."""
    assert pipeline._consensus_winners(
        _cands(CORRECT_A), [_case()], _sandbox, lambda *a, **k: None) == []


def test_total_disagreement_selects_nothing():
    """Three answers, three clusters of one — no majority to trust."""
    a = "print(1)\n"
    b = "print(2)\n"
    c = "print(3)\n"
    assert pipeline._consensus_winners(
        _cands(a, b, c), [_case()], _sandbox, lambda *a, **k: None) == []


def test_no_test_cases_selects_nothing():
    assert pipeline._consensus_winners(
        _cands(CORRECT_A, CORRECT_B), [], _sandbox, lambda *a, **k: None) == []


def test_a_crashing_candidate_does_not_join_a_cluster():
    boom = "raise SystemExit(1)\n"
    winners = pipeline._consensus_winners(
        _cands(CORRECT_A, boom, CORRECT_B), [_case()], _sandbox, lambda *a, **k: None)
    assert sorted(c["index"] for c in winners) == [0, 2]


def test_the_probe_reports_output_rather_than_asserting():
    probe, files = pipeline._make_output_probe(CORRECT_A, _case())
    assert pipeline._CONSENSUS_MARK in probe
    assert "SELF_TEST_PASS" not in probe
    assert "THIS KEY IS WRONG" not in probe, "the probe must not see the key"
    assert not any("THIS KEY IS WRONG" in v for v in files.values()), \
        "nor may the key be staged for it"


def test_the_consensus_stage_is_registered():
    """An unregistered stage contributes no phase row, so the run summary
    loses the reason a candidate was selected."""
    assert pipeline._STAGE_PHASE["consensus"] == "selection"


# --- verification holds candidates to the ENVIRONMENT's contract -----------
#
# The self-test used to pick its shape from the candidate's own code, so a
# stdin-reading candidate was handed a stdin the real run never provides. It
# passed verification and was selected in 3 sessions; all 3 then failed the
# task, printing 0 because no stdin arrived. The caller runs the program with
# the task's files on disk and no stdin, and verification has to do the same.

STDIN_CANDIDATE = "import sys\nprint(len([x for x in sys.stdin]))\n"
FILE_CANDIDATE = "print(len(open('input.txt').read().split()))\n"


def _run(built):
    """`built` is a self-test (wrapper, files) pair or a bare probe body."""
    body, files = built if isinstance(built, tuple) else (built, {})
    work = tempfile.mkdtemp()
    for name, content in (files or {}).items():
        Path(work, name).write_text(content)
    Path(work, "t.py").write_text(body)
    return subprocess.run([sys.executable, "t.py"], cwd=work,
                          capture_output=True, text=True,
                          stdin=subprocess.DEVNULL, timeout=60)


def test_the_task_input_file_is_recognised():
    assert pipeline._task_input_file({"input.txt": "1\n"}) == "input.txt"
    assert pipeline._task_input_file({"solve.py": "x=1"}) == ""
    assert pipeline._task_input_file(None) == ""


def test_a_stdin_candidate_fails_the_task_contract():
    """The measured regression: it passed on a stdin the caller never gives."""
    body = pipeline._make_self_test(STDIN_CANDIDATE, _case3("3"), "input.txt")
    assert "SELF_TEST_PASS" not in _run(body).stdout


def test_a_file_candidate_passes_the_task_contract():
    body = pipeline._make_self_test(FILE_CANDIDATE, _case3("3"), "input.txt")
    assert "SELF_TEST_PASS" in _run(body).stdout


def test_without_a_task_input_file_the_old_shapes_stand():
    """Tasks that really are stdin-driven must keep working."""
    body, _files = pipeline._make_self_test(STDIN_CANDIDATE, _case3("3"))
    assert "_s.stdin=" in body


def test_the_probe_uses_the_same_contract():
    probe, files = pipeline._make_output_probe(
        STDIN_CANDIDATE, _case(), "input.txt")
    # The case input is staged for the program, exactly as the self-test
    # stages it; stdin is attached-and-empty under the task contract, and
    # the case input must never ride it.
    assert files["input.txt"] == "199\n200\n208\n210\n200"
    assert "_s.stdin=_o.StringIO('')" in probe
    assert "199" not in probe, "case input stays out of the wrapper entirely"


# --- the probe runs a candidate the way the self-test does -----------------
#
# `_make_self_test` was repaired to stage the case input through the
# sandbox's files map and run the candidate once as `__main__`. The output
# probe kept the old shape: it wrote `input.txt` from executable code, which
# raises under the hardened sandbox before the candidate runs, and it exec'd
# the source inside the imported `solution` module, where `__name__` is
# `'solution'` and a main-guarded body never executes. Same contract, so the
# same staging and the same entry point.

PROBE_MAIN_GUARDED = ("def main():\n"
                      "    print(len(open('input.txt').read().split()))\n"
                      "\n"
                      "if __name__ == '__main__':\n"
                      "    main()\n")


def _run_probe(built, work=None):
    body, files = built
    work = work or tempfile.mkdtemp()
    for name, content in (files or {}).items():
        Path(work, name).write_text(content)
    Path(work, "solution.py").write_text(body)
    return subprocess.run(
        [sys.executable, "-c",
         f"import sys; sys.path.insert(0,{work!r}); import solution"],
        cwd=work, capture_output=True, text=True,
        stdin=subprocess.DEVNULL, timeout=60), work


def test_the_probe_stages_its_case_input():
    body, files = pipeline._make_output_probe(
        FILE_CANDIDATE, _case3("3"), "input.txt")
    assert "'w'" not in body, "the probe creates no files of its own"
    assert files["input.txt"] == "1\n2\n3"
    assert files[pipeline._CANDIDATE_FILE] == FILE_CANDIDATE


def test_the_probe_runs_a_main_guarded_candidate():
    """Its output is the whole point of the probe; under the old module
    identity it was empty and clustered with every other empty result."""
    run, _ = _run_probe(pipeline._make_output_probe(
        PROBE_MAIN_GUARDED, _case3("3"), "input.txt"))
    assert pipeline._CONSENSUS_MARK + repr("3") in run.stdout, (
        run.stdout, run.stderr[:300])


def test_the_probe_executes_the_candidate_once():
    counter = ("open('runs.txt','a').write('x')\n"
               "if __name__ == '__main__':\n"
               "    print(len(open('runs.txt').read()))\n")
    run, work = _run_probe(pipeline._make_output_probe(
        counter, _case3("3"), "input.txt"))
    assert pipeline._CONSENSUS_MARK + repr("1") in run.stdout, run.stdout
    assert Path(work, "runs.txt").read_text() == "x"


def test_each_probe_case_stages_its_own_input():
    _, f1 = pipeline._make_output_probe(FILE_CANDIDATE, _case3("3"), "input.txt")
    other = type("TC", (), {"input_str": "9\n9", "expected_output": "2"})()
    _, f2 = pipeline._make_output_probe(FILE_CANDIDATE, other, "input.txt")
    assert f1["input.txt"] == "1\n2\n3"
    assert f2["input.txt"] == "9\n9"


def test_the_probe_body_reaches_the_sandbox_with_its_files(monkeypatch):
    """Through the real caller and the real adapter: the serialized
    /execute body carries the staged case, not a wrapper that writes it."""
    import json as _json
    import adapters as _adapters

    captured = []

    class _Resp:
        def __init__(self, payload):
            self._p = payload

        def read(self):
            return _json.dumps(self._p).encode()

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        captured.append(_json.loads(req.data.decode()))
        return _Resp({"success": True,
                      "stdout": pipeline._CONSENSUS_MARK + repr("3") + "\n",
                      "stderr": ""})

    monkeypatch.setattr(_adapters.urllib.request, "urlopen", fake_urlopen)
    sandbox = _adapters.SandboxAdapter()
    pipeline._consensus_winners(
        [{"index": 0, "code": FILE_CANDIDATE},
         {"index": 1, "code": PROBE_MAIN_GUARDED}],
        [_case3("3")], sandbox, lambda *a, **k: None, "input.txt")

    assert captured, "the consensus stage sent no sandbox request"
    for body in captured:
        assert body["files"]["input.txt"] == "1\n2\n3"
        assert "'w'" not in body["code"]


# --- agreement is counted in programs, and is not a pass ----------------------
#
# Audit V-pipeline/INTEGRITY#1: winners were marked passed, byte-identical
# copies counted as agreement, and a candidate that failed the project's build
# command could be promoted by consensus.

def test_identical_copies_are_one_program():
    """Two copies of a wrong program do not outvote one correct program."""
    winners = pipeline._consensus_winners(
        _cands(WRONG, WRONG, CORRECT_A), [_case()], _sandbox, lambda *a, **k: None)
    assert winners == []


def test_distinct_agreeing_programs_still_win():
    winners = pipeline._consensus_winners(
        _cands(CORRECT_A, CORRECT_A, CORRECT_B), [_case()], _sandbox, lambda *a, **k: None)
    assert sorted(c["index"] for c in winners) == [0, 1, 2]


def test_a_function_shaped_probe_returns_its_pair():
    code = "def solve(n):\n    return n * 2\n"
    probe, files = pipeline._make_output_probe(code, _case3("6"))
    assert pipeline._CONSENSUS_MARK in probe and files == {}


def test_a_candidate_that_failed_a_project_check_cannot_agree_its_way_in():
    failed_build = {"verification_evidence": [{"verifier": "build_command", "status": "failed"}]}
    failed_import = {"verification_evidence": [{"verifier": "python_import_comparison",
                                                "status": "failed"}]}
    unavailable = {"verification_evidence": [{"verifier": "build_command", "status": "unavailable"}]}
    assert pipeline._failed_a_project_check(failed_build)
    assert pipeline._failed_a_project_check(failed_import)
    assert not pipeline._failed_a_project_check(unavailable)
    assert not pipeline._failed_a_project_check({})
