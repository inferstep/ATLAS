"""What the sandbox's /execute answer says about a run that the resource contract stopped.

A program that is killed at a ceiling writes nothing about it, so its own
words cannot name the ceiling. The answer names it from the outcome of the
run: the outcome itself, the kind of error, and a first line of the message.
Its stderr stays what the program wrote.

Two kinds of test. In the first no command runs: each handler gets planned
results of its commands, so every handler and every step is held, on any
system. In the second a real command runs into each ceiling. Each such
command ends by itself (see bounded_commands), and the two that need /proc
skip where there is none.
"""
from __future__ import annotations

import os
import sys

import pytest

from tests.infrastructure.bounded_commands import OWN_LIMIT, REACHED_OWN_LIMIT, allocator, flood
from tests.infrastructure.proc_files import needs_proc

SANDBOX = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))), "sandbox")
sys.path.insert(0, SANDBOX)

import executor_server as ex
import resource_contract as rc

MiB = 1024 * 1024
OWN_WORDS = "Traceback (most recent call last):\n  File \"x\", line 1\nNameError: name 'x' is not defined\n"
CODE = {"java": "public class Main { public static void main(String[] a) {} }", "kotlin": "fun main() {}"}
# Each command of each handler, in the order the handler runs them: what an answer calls a failure of that step when
# the step ran to its own end. None: the handler reads the kind out of the words of the run. "not read": the handler
# goes on whatever the step did.
STEPS = {
    "bash": [("bash -n", "SyntaxError"), ("bash solution.sh", None)],
    "c": [("gcc -o", "CompileError"), ("program", None)],
    "cpp": [("g++ -o", "CompileError"), ("program", None)],
    "go": [("go mod", "not read"), ("go build", "CompileError"), ("program", None)],
    "java": [("javac -d", "CompileError"), ("java -cp", None)],
    "javascript": [("node --check", "SyntaxError"), ("node solution.js", None)],
    "kotlin": [("kotlinc -include-runtime", "CompileError"), ("java -jar", None)],
    "php": [("php -l", "SyntaxError"), ("php main.php", None)],
    "python": [("pip install", "DependencyError"), ("python -m", "not read"), ("python -c", None)],
    "ruby": [("ruby -c", "SyntaxError"), ("ruby main.rb", None)],
    "rust": [("rustc main.rs", "CompileError"), ("program", None)],
    "typescript": [("tsc --noEmit", "not read"), ("tsx solution.ts", None)],
}
EVERY_STEP = [(language, at) for language, steps in STEPS.items() for at in range(len(steps))]
STOPPED = {rc.OUTCOME_TIMED_OUT: "Timeout", rc.OUTCOME_MEMORY_EXHAUSTED: "MemoryLimit",
           rc.OUTCOME_PROCESS_LIMIT: "ProcessLimit", rc.OUTCOME_OUTPUT_LIMIT: "OutputLimit"}


@pytest.fixture
def small():
    contract = rc.ResourceContract(wall_seconds=20, memory_bytes=384 * MiB, max_processes=16, output_bytes=1 * MiB)
    contract.validate()
    return contract


# --- each handler, with planned results of its commands ------------------------------------------------------------

def answer(monkeypatch, tmp_path, language, plan):
    """The answer of a handler when its commands give the planned results, and the commands it ran."""
    ran = []

    def planned(cmd, timeout, cwd=None, env=None, stdin=None, cancelled=None):
        result = {"success": True, "stdout": "the output\n", "stderr": "", "returncode": 0, "timed_out": False,
                  "outcome": rc.OUTCOME_COMPLETED, "peak_memory_bytes": 0, "peak_processes": 1, "survivors": 0}
        result.update(plan.get(len(ran), {}))
        result["stopped"] = rc.stopped_words(result["outcome"], ex.EXEC_CONTRACT.for_request(timeout))
        ran.append((" ".join(str(part).split("/")[-1] for part in cmd[:2]), timeout))
        return result

    monkeypatch.setattr(ex, "_run_cmd", planned)
    got = ex.LANGUAGE_HANDLERS[language](code=CODE.get(language, "print(1)"), test_code=None, workspace=tmp_path, timeout=7,
                                         requirements=["requests"], stdin=None)
    return got, ran


def test_the_table_of_this_file_names_every_handler_and_every_command_it_runs(monkeypatch, tmp_path):
    assert sorted(STEPS) == sorted(ex.LANGUAGE_HANDLERS)
    for language, steps in STEPS.items():
        folder = tmp_path / language
        folder.mkdir()
        _, ran = answer(monkeypatch, folder, language, {})
        assert [command for command, _ in ran] == [command for command, _ in steps], language


@pytest.mark.parametrize("language, at", EVERY_STEP)
@pytest.mark.parametrize("outcome", sorted(STOPPED))
def test_a_step_that_was_stopped_at_a_ceiling_is_named_by_its_outcome(monkeypatch, tmp_path, language, at, outcome):
    stopped = {"success": False, "returncode": -9, "timed_out": outcome == rc.OUTCOME_TIMED_OUT, "outcome": outcome}
    got, ran = answer(monkeypatch, tmp_path, language, {at: stopped})
    if STEPS[language][at][1] == "not read":
        assert got.success is True
        assert got.outcome == rc.OUTCOME_COMPLETED
        return
    words = rc.stopped_words(outcome, ex.EXEC_CONTRACT.for_request(ran[at][1]))
    assert len(ran) == at + 1, "the handler went on after a step that was stopped"
    assert got.success is False
    assert got.error_type == STOPPED[outcome]
    assert got.outcome == outcome
    assert got.timed_out is (outcome == rc.OUTCOME_TIMED_OUT)
    assert got.error_message.split("\n")[0] == words
    # The error stream is the command's own: PHP's check gives its output there when the stream is empty.
    assert got.stderr == ("the output\n" if (language, at) == ("php", 0) else "")


@pytest.mark.parametrize("language, at", EVERY_STEP)
@pytest.mark.parametrize("own_words", [OWN_WORDS, ""])
def test_a_step_that_failed_by_itself_is_named_as_before(monkeypatch, tmp_path, language, at, own_words):
    kind = STEPS[language][at][1]
    got, _ = answer(monkeypatch, tmp_path, language, {at: {"success": False, "stderr": own_words, "returncode": 1}})
    if kind == "not read":
        assert got.success is True
        return
    assert got.success is False
    assert got.outcome == rc.OUTCOME_COMPLETED
    assert got.timed_out is False
    # PHP's check writes its words to the output when the error stream is empty.
    said = own_words or ("the output\n" if (language, at) == ("php", 0) else "")
    assert got.stderr == said
    assert got.error_message == said[:500]
    assert got.error_type == (kind or ("NameError" if own_words else None))


@pytest.mark.parametrize("language", sorted(STEPS))
def test_a_run_that_passed_has_no_kind_of_error_and_reached_its_own_end(monkeypatch, tmp_path, language):
    got, _ = answer(monkeypatch, tmp_path, language, {})
    assert got.success is True
    assert (got.error_type, got.error_message, got.stderr) == (None, None, "")
    assert (got.outcome, got.timed_out) == (rc.OUTCOME_COMPLETED, False)
    assert got.stdout == "the output\n"


# --- the words and the fields ------------------------------------------------------------------------------------

def test_the_words_for_each_ceiling_give_the_ceiling(small):
    assert rc.stopped_words(rc.OUTCOME_TIMED_OUT, small) == "Execution timed out after 20s"
    assert rc.stopped_words(rc.OUTCOME_MEMORY_EXHAUSTED, small) == "Execution stopped at the memory limit of 384 MiB"
    assert rc.stopped_words(rc.OUTCOME_PROCESS_LIMIT, small) == "Execution stopped at the limit of 16 processes"
    assert rc.stopped_words(rc.OUTCOME_OUTPUT_LIMIT, small) == "Execution stopped at the output limit of 1048576 bytes"
    for outcome in (rc.OUTCOME_COMPLETED, rc.OUTCOME_CANCELLED, rc.OUTCOME_SPAWN_FAILED, rc.OUTCOME_UNCLASSIFIED):
        assert rc.stopped_words(outcome, small) == ""
    assert sorted(rc.STOPPED_KINDS) == sorted(STOPPED)


def run_result(**other):
    return {"success": False, "stderr": "", "outcome": rc.OUTCOME_COMPLETED, "stopped": "", **other}


def test_the_ceiling_decides_the_kind_before_the_step_and_before_the_words_of_the_run():
    # The program wrote the name of an error of its own, and then the contract stopped it.
    stopped = run_result(stderr=OWN_WORDS, outcome=rc.OUTCOME_TIMED_OUT, stopped="Execution timed out after 5s")
    for kind in (None, "CompileError"):
        got = rc.why_it_failed(stopped, kind, ex._classify_error)
        assert got["error_type"] == "Timeout"
        assert got["stderr"] == OWN_WORDS
        assert got["error_message"] == "Execution timed out after 5s\n" + OWN_WORDS
        assert got["timed_out"] is True
    assert rc.why_it_failed(run_result(stderr=OWN_WORDS), "CompileError", ex._classify_error)["error_type"] == "CompileError"
    assert rc.why_it_failed(run_result(stderr=OWN_WORDS), None, ex._classify_error)["error_type"] == "NameError"


def test_the_words_of_a_program_that_only_speaks_of_a_timeout_do_not_make_it_one():
    spoke = run_result(stderr="requests.exceptions.ReadTimeout: the read timed out\n")
    got = rc.why_it_failed(spoke, None, ex._classify_error)
    assert got["outcome"] == rc.OUTCOME_COMPLETED
    assert got["timed_out"] is False


def test_the_message_is_the_first_500_characters_and_starts_with_the_words_for_the_ceiling():
    long = run_result(stderr="x" * 2000, outcome=rc.OUTCOME_MEMORY_EXHAUSTED, stopped="Execution stopped at the memory limit of 384 MiB")
    got = rc.why_it_failed(long, None, ex._classify_error)
    assert len(got["error_message"]) == 500
    assert got["error_message"].startswith("Execution stopped at the memory limit of 384 MiB\nxxx")


def test_the_result_of_a_bounded_command_keeps_the_commands_own_error_stream_for_the_shell_call():
    got = ex._run_cmd(["bash", "-c", "echo its own words >&2; sleep 30; exit %d" % OWN_LIMIT], timeout=1)
    assert got["returncode"] != OWN_LIMIT, REACHED_OWN_LIMIT
    assert got["outcome"] == rc.OUTCOME_TIMED_OUT
    # /shell gives this text to its caller as it is, with the outcome beside it.
    assert got["stderr"] == "its own words\n"
    assert got["stopped"] == "Execution timed out after 1s"
    assert ex._run_cmd(["bash", "-c", "echo ok"], timeout=5)["stopped"] == ""


# --- a real command at each ceiling -------------------------------------------------------------------------------

def bash(tmp_path, code, timeout=30):
    return ex.execute_bash(code=code, test_code=None, workspace=tmp_path, timeout=timeout)


def stopped_at(got, outcome):
    """Hold that the contract stopped the run, and not the command's own limit."""
    assert got.outcome != rc.OUTCOME_COMPLETED, REACHED_OWN_LIMIT + f" (stderr: {got.stderr[-200:]!r})"
    assert got.outcome == outcome
    assert got.success is False
    assert got.error_type == STOPPED[outcome]
    return got


def test_a_program_that_runs_into_its_time_limit(tmp_path):
    got = stopped_at(bash(tmp_path, "echo before the limit >&2\nsleep 30\nexit %d\n" % OWN_LIMIT, timeout=1), rc.OUTCOME_TIMED_OUT)
    assert got.timed_out is True
    assert got.stderr == "before the limit\n"
    assert got.error_message == "Execution timed out after 1s\nbefore the limit\n"


def test_a_program_that_writes_more_than_the_output_limit(monkeypatch, tmp_path, small):
    monkeypatch.setattr(ex, "EXEC_CONTRACT", small)
    got = stopped_at(bash(tmp_path, flood("ABCDEFGHIJKLMNOP", small.output_bytes)), rc.OUTCOME_OUTPUT_LIMIT)
    assert got.error_message.split("\n")[0] == "Execution stopped at the output limit of 1048576 bytes"
    assert got.timed_out is False


@needs_proc
def test_a_program_that_takes_more_than_the_memory_limit(monkeypatch, tmp_path, small):
    monkeypatch.setattr(ex, "EXEC_CONTRACT", small)
    got = stopped_at(bash(tmp_path, allocator(64, small.memory_bytes)), rc.OUTCOME_MEMORY_EXHAUSTED)
    assert got.error_message.split("\n")[0] == "Execution stopped at the memory limit of 384 MiB"


@needs_proc
def test_a_program_that_starts_more_processes_than_the_limit(monkeypatch, tmp_path):
    few = rc.ResourceContract(wall_seconds=15, memory_bytes=384 * MiB, max_processes=8, output_bytes=1 * MiB)
    few.validate()
    monkeypatch.setattr(ex, "EXEC_CONTRACT", few)
    got = stopped_at(bash(tmp_path, "for i in $(seq 1 40); do sleep 12 & done; wait"), rc.OUTCOME_PROCESS_LIMIT)
    assert got.error_message.split("\n")[0] == "Execution stopped at the limit of 8 processes"


def test_a_program_that_fails_by_itself_and_one_that_passes(tmp_path):
    failed = bash(tmp_path, "echo 'ValueError: no' >&2\nexit 3\n")
    assert (failed.success, failed.outcome, failed.timed_out) == (False, rc.OUTCOME_COMPLETED, False)
    assert (failed.error_type, failed.stderr, failed.error_message) == ("ValueError", "ValueError: no\n", "ValueError: no\n")
    passed = bash(tmp_path, "echo ok\n")
    assert (passed.success, passed.outcome, passed.error_type, passed.error_message) == (True, rc.OUTCOME_COMPLETED, None, None)
    assert passed.stdout == "ok\n"


def test_the_execute_call_itself_gives_the_fields_to_its_caller(monkeypatch, tmp_path):
    monkeypatch.setattr(ex, "WORKSPACE_BASE", str(tmp_path))
    got = ex.execute_code(ex.ExecuteRequest(code="sleep 30\nexit %d\n" % OWN_LIMIT, language="bash", timeout=1))
    said = got.model_dump()
    assert said["outcome"] != rc.OUTCOME_COMPLETED, REACHED_OWN_LIMIT
    assert (said["success"], said["error_type"], said["timed_out"], said["outcome"]) == (False, "Timeout", True, "timed_out")
    assert said["stderr"] == ""
    assert said["error_message"] == "Execution timed out after 1s"
