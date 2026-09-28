"""The executor must be able to say what the workspace looked like.

Candidate staging runs a client's declared command against a snapshot holding
the candidate bytes, and then has to answer one question the caller cannot
answer for itself: did the command change the thing it was testing? The
snapshot is deleted before the response returns, so nothing outside the
executor can look.

/shell already snapshotted, overlaid and deleted -- that mechanism has existed
for V3 build verification. What it could not do was describe the state either
side of the run, so a command that quietly rewrote the candidate was
indistinguishable from one that left it alone.

These tests pin the observation and, just as importantly, pin that it reports
FACTS: hashes, a count, a truncation flag and whether the run timed out. It
draws no conclusion about whether a change was permitted, because it has no
way to know what the client declared.
"""
import hashlib
import importlib.util
import os
import sys
from pathlib import Path

import pytest

from tests.conftest import run_shell_sync

SANDBOX_DIR = Path(__file__).resolve().parents[2] / "sandbox"


@pytest.fixture(scope="module")
def executor(tmp_path_factory):
    root = tmp_path_factory.mktemp("ws-staging")
    (root / "baseline.py").write_text("BASE = 1\n")
    (root / "input.txt").write_text("7\n")
    base = tmp_path_factory.mktemp("sandbox-base")
    os.environ["ATLAS_SANDBOX_WORKSPACE_ROOT"] = str(root)
    os.environ["WORKSPACE_BASE"] = str(base)
    sys.path.insert(0, str(SANDBOX_DIR))
    spec = importlib.util.spec_from_file_location(
        "executor_server_staging", SANDBOX_DIR / "executor_server.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod
    try:
        spec.loader.exec_module(mod)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise
    mod._TEST_WORKSPACE_ROOT = root
    return mod


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


CANDIDATE = "print(7)\n"


def shell(executor, command, files=None, observe=None, timeout=30):
    return run_shell_sync(executor, executor.ShellRequest(
        command=command, files=files, observe_paths=observe, timeout=timeout))


# --- the observation exists and is about the staged bytes --------------------

def test_a_staged_candidate_is_observed_at_its_own_hash(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE},
                observe=["solve.py"])
    assert res.observation is not None
    assert res.observation.target_before["solve.py"] == sha(CANDIDATE)
    assert res.observation.target_after["solve.py"] == sha(CANDIDATE)
    assert res.observation.workspace_before == res.observation.workspace_after
    assert res.observation.workspace_files > 0
    assert res.observation.digest_truncated is False


def test_a_command_that_rewrites_the_candidate_is_visible(executor):
    res = shell(executor, "echo 'print(8)' > solve.py",
                files={"solve.py": CANDIDATE}, observe=["solve.py"])
    obs = res.observation
    assert obs.target_before["solve.py"] == sha(CANDIDATE)
    assert obs.target_after["solve.py"] != obs.target_before["solve.py"]
    # And the workspace digest moves with it: the executor states both and
    # leaves the conclusion to the caller.
    assert obs.workspace_after != obs.workspace_before


def test_a_command_that_changes_another_input_is_visible(executor):
    res = shell(executor, "echo 9 > input.txt",
                files={"solve.py": CANDIDATE}, observe=["solve.py"])
    obs = res.observation
    assert obs.target_after["solve.py"] == obs.target_before["solve.py"], \
        "the candidate itself was untouched"
    assert obs.workspace_after != obs.workspace_before, \
        "a changed input must be visible even when the target is not"


def test_an_absent_path_is_reported_as_absent_not_omitted(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE},
                observe=["solve.py", "never_written.py"])
    obs = res.observation
    assert obs.target_before["never_written.py"] == ""
    assert obs.target_after["never_written.py"] == ""


def test_a_command_creating_the_observed_path_moves_it_off_absent(executor):
    res = shell(executor, "echo hi > made.py", files={"solve.py": CANDIDATE},
                observe=["made.py"])
    obs = res.observation
    assert obs.target_before["made.py"] == ""
    assert obs.target_after["made.py"] != ""


# --- nothing is observed unless asked ----------------------------------------

def test_no_observation_without_a_request(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE})
    assert res.observation is None, "observing is opt-in; existing callers pay nothing"


def test_an_empty_observe_list_still_reports_the_workspace(executor):
    """An empty list is a real request: observe nothing in particular, but do
    tell me whether the workspace moved."""
    res = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=[])
    assert res.observation is not None
    assert res.observation.target_before == {}
    assert res.observation.workspace_before == res.observation.workspace_after


# --- timeout is structural, not parsed out of prose --------------------------

def test_a_timeout_is_reported_structurally(executor):
    res = shell(executor, "sleep 5", files={"solve.py": CANDIDATE},
                observe=["solve.py"], timeout=1)
    assert res.timed_out is True
    assert res.success is False
    # And the observation still exists: a command that timed out may well have
    # changed something before it was killed, and the caller has to know.
    assert res.observation is not None


def test_a_clean_run_is_not_flagged_as_a_timeout(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    assert res.timed_out is False
    assert res.success is True


# --- isolation ---------------------------------------------------------------

def test_the_candidate_never_reaches_the_production_workspace(executor):
    root = executor._TEST_WORKSPACE_ROOT
    shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    assert not (root / "solve.py").exists(), \
        "candidate bytes were written to the real workspace"


def test_a_mutating_command_does_not_touch_the_production_workspace(executor):
    root = executor._TEST_WORKSPACE_ROOT
    before = (root / "input.txt").read_text()
    shell(executor, "echo 9 > input.txt; rm -f baseline.py",
          files={"solve.py": CANDIDATE}, observe=["solve.py"])
    assert (root / "input.txt").read_text() == before, \
        "a staged command changed a real input"
    assert (root / "baseline.py").exists(), \
        "a staged command deleted a real file"


def test_two_stagings_do_not_share_mutable_state(executor):
    first = shell(executor, "echo marker > shared.txt",
                  files={"solve.py": CANDIDATE}, observe=["shared.txt"])
    second = shell(executor, "true", files={"solve.py": CANDIDATE},
                   observe=["shared.txt"])
    assert first.observation.target_after["shared.txt"] != ""
    assert second.observation.target_before["shared.txt"] == "", \
        "one staging's writes were visible to the next"


def test_the_snapshot_is_destroyed_after_success_and_after_failure(executor):
    base = Path(executor.WORKSPACE_BASE)
    before = {p.name for p in base.glob("shell-*")}
    shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    shell(executor, "exit 3", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    shell(executor, "sleep 5", files={"solve.py": CANDIDATE},
          observe=["solve.py"], timeout=1)
    after = {p.name for p in base.glob("shell-*")}
    assert after == before, f"staging snapshots survived: {sorted(after - before)}"


# --- the observation carries no content --------------------------------------

def test_the_observation_carries_no_bytes_or_command_text(executor):
    secret = "TOKEN = 'hunter2'\nprint(7)\n"
    res = shell(executor, "grep -c TOKEN solve.py > /dev/null",
                files={"solve.py": secret}, observe=["solve.py"])
    blob = res.observation.model_dump_json() if hasattr(res.observation, "model_dump_json") \
        else res.observation.json()
    for needle in ("hunter2", "TOKEN", "print(7)", "grep"):
        assert needle not in blob, f"the observation carries {needle!r}"


def test_the_observation_fields_are_hashes_and_counts(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    obs = res.observation
    for name, value in (("workspace_before", obs.workspace_before),
                        ("workspace_after", obs.workspace_after)):
        assert len(value) == 64 and all(c in "0123456789abcdef" for c in value), \
            f"{name} is not a sha256"
    assert isinstance(obs.workspace_files, int)
    assert isinstance(obs.digest_truncated, bool)


# --- the digest is honest about its own limit --------------------------------

def test_the_digest_reports_truncation_rather_than_a_partial_answer(executor, monkeypatch):
    monkeypatch.setattr(executor, "SHELL_OBSERVE_MAX_FILES", 1)
    res = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    assert res.observation.digest_truncated is True, \
        "a workspace the executor cannot describe exactly must say so"


def test_an_untruncated_digest_is_stable_across_identical_runs(executor):
    a = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    b = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    assert a.observation.workspace_after == b.observation.workspace_after, \
        "the same tree digested two different ways"


def test_different_candidates_digest_differently(executor):
    a = shell(executor, "true", files={"solve.py": CANDIDATE}, observe=["solve.py"])
    b = shell(executor, "true", files={"solve.py": "print(8)\n"}, observe=["solve.py"])
    assert a.observation.workspace_after != b.observation.workspace_after


# --- overlay containment still holds -----------------------------------------

def test_an_escaping_overlay_path_is_refused(executor):
    for bad in ("../escape.py", "/etc/passwd", "a/../../escape.py"):
        with pytest.raises(Exception):
            shell(executor, "true", files={bad: CANDIDATE}, observe=["solve.py"])


def test_an_escaping_observe_path_reports_absent_rather_than_reading_it(executor):
    res = shell(executor, "true", files={"solve.py": CANDIDATE},
                observe=["../../etc/passwd"])
    assert res.observation.target_before["../../etc/passwd"] == ""
    assert res.observation.target_after["../../etc/passwd"] == ""
