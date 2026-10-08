"""What v3-service makes of an /execute answer for a run that the sandbox stopped at a limit.

The sandbox adapter keeps three things of the answer: whether the run passed,
its output, and an error text. That text goes into the repair prompts (PR-CoT
and the failure analysis), and the record of a self-test is sorted by it. A
program that is killed at a limit writes nothing, so the adapter takes the
limit from the answer's `outcome` field and puts it first in the text.
"""
import json
import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))
sys.path.insert(0, str(PROJECT_ROOT / "sandbox"))

import adapters  # noqa: E402
import pipeline  # noqa: E402
import resource_contract  # noqa: E402
from stages import failure_analysis, pr_cot  # noqa: E402

CASE = SimpleNamespace(input_str="3", expected_output="7")


def stopped(outcome, message, stderr="", kind="Timeout"):
    """An /execute answer for a run that the sandbox stopped, as the sandbox gives it."""
    return {"success": False, "compile_success": True, "tests_run": 1, "tests_passed": 0, "lint_score": None, "stdout": "",
            "stderr": stderr, "error_type": kind, "error_message": "\n".join(part for part in (message, stderr) if part),
            "execution_time_ms": 15012, "timed_out": outcome == "timed_out", "outcome": outcome}


# The answer of a sandbox from before the answer said how a run ended: no word and no field for the time limit.
BEFORE = {"success": False, "compile_success": True, "tests_run": 1, "tests_passed": 0, "lint_score": None, "stdout": "",
          "stderr": "", "error_type": None, "error_message": "", "execution_time_ms": 15012}


def through_the_adapter(monkeypatch, answer):
    """What the adapter returns for this answer of the sandbox."""
    class Response:
        def read(self):
            return json.dumps(answer).encode()

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(adapters.urllib.request, "urlopen", lambda req, timeout=None: Response())
    return adapters.SandboxAdapter()("while True:\n    pass\n")


def test_a_run_at_its_time_limit_gives_the_repair_step_a_text_that_says_so(monkeypatch):
    ok, out, err = through_the_adapter(monkeypatch, stopped("timed_out", "Execution timed out after 15s"))
    assert (ok, out, err) == (False, "", "Execution timed out after 15s")
    prompt = pr_cot.PRCoT(pr_cot.PRCoTConfig())._build_analysis_prompt("the problem", "the code", err, "logical_error")
    assert "Error output: Execution timed out after 15s\n" in prompt
    failing = [failure_analysis.FailingCandidate(code="the code", error_output=err, index=0)]
    assert failure_analysis.format_candidates_with_errors(failing).endswith("Error: Execution timed out after 15s")


def test_a_self_test_at_its_time_limit_is_recorded_as_a_timeout(monkeypatch):
    ok, out, err = through_the_adapter(monkeypatch, stopped("timed_out", "Execution timed out after 15s"))
    assert pipeline._capture_case(0, CASE, ok, out, err)["outcome"] == "timeout"


def test_what_the_program_wrote_before_the_limit_stays_after_the_line_for_the_limit(monkeypatch):
    wrote = 'Traceback (most recent call last):\n  File "solution.py", line 9, in solve\nRecursionError: too deep\n'
    _, _, err = through_the_adapter(monkeypatch, stopped("timed_out", "Execution timed out after 15s", stderr=wrote))
    assert err == "Execution timed out after 15s\n" + wrote


@pytest.mark.parametrize("outcome, kind, message", [
    ("memory_exhausted", "MemoryLimit", "Execution stopped at the memory limit of 2048 MiB"),
    ("process_limit_exceeded", "ProcessLimit", "Execution stopped at the limit of 256 processes"),
    ("output_limit_exceeded", "OutputLimit", "Execution stopped at the output limit of 33554432 bytes"),
])
def test_each_other_limit_is_the_first_line_of_the_text_too(monkeypatch, outcome, kind, message):
    _, _, err = through_the_adapter(monkeypatch, stopped(outcome, message, stderr="its own words\n", kind=kind))
    assert err == message + "\nits own words\n"


def test_the_field_decides_and_not_a_word_that_the_program_wrote(monkeypatch):
    # The program reached its own end, and its words speak of a timeout of its own.
    own = {**BEFORE, "stderr": "requests.exceptions.ReadTimeout: the read timed out\n", "timed_out": False, "outcome": "completed",
           "error_type": "RuntimeError", "error_message": "requests.exceptions.ReadTimeout: the read timed out\n"}
    _, _, err = through_the_adapter(monkeypatch, own)
    assert err == "requests.exceptions.ReadTimeout: the read timed out\n"
    # The sandbox stopped the run, and the answer brings no message: the field alone gives the line.
    bare = {**BEFORE, "timed_out": True, "outcome": "timed_out"}
    assert through_the_adapter(monkeypatch, bare)[2] == "Execution timed out"


def test_the_outcomes_that_the_adapter_reads_are_the_ones_that_the_sandbox_names_as_stopped_at_a_limit():
    assert sorted(adapters._STOPPED_AT_A_LIMIT) == sorted(resource_contract.STOPPED_KINDS)


def test_an_answer_with_no_field_for_the_outcome_is_read_as_before(monkeypatch):
    # An older sandbox beside this v3-service. Nothing in its answer says that the run was stopped, so nothing can be
    # said: the text is the empty error stream, and the record sorts the case as an error of the run.
    ok, out, err = through_the_adapter(monkeypatch, BEFORE)
    assert (ok, out, err) == (False, "", "")
    assert pipeline._capture_case(0, CASE, ok, out, err)["outcome"] == "execution_error"
    prompt = pr_cot.PRCoT(pr_cot.PRCoTConfig())._build_analysis_prompt("the problem", "the code", err, "logical_error")
    assert "Error output: \n" in prompt


def test_a_run_that_passed_and_one_that_failed_by_itself_are_read_as_before(monkeypatch):
    passed = {**BEFORE, "success": True, "tests_passed": 1, "stdout": "7\n", "timed_out": False, "outcome": "completed"}
    assert through_the_adapter(monkeypatch, passed) == (True, "7\n", "")
    failed = {**BEFORE, "stderr": "NameError: name 'x' is not defined\n", "error_type": "NameError",
              "error_message": "NameError: name 'x' is not defined\n", "timed_out": False, "outcome": "completed"}
    assert through_the_adapter(monkeypatch, failed) == (False, "", "NameError: name 'x' is not defined\n")
