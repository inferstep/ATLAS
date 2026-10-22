"""The queue of the heavy runs on the development server (scripts/server/atlas_queue.py).

Most cases run the queue as the server does, as a process, with a queue folder of its own in tmp_path; the
commands it runs are short shell lines that write down what they saw. A poll is a tenth of a second.

No case lets the queue signal a process group named by a note: the cases of the nightly's stop call the helper
in this process, with a stand-in for os.killpg, and every note names a throwaway sleeping group, never group 1.
The only real signals are the ones a queue run sends to its own command's group, which it made itself.
"""
import fcntl
import json
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "scripts" / "server"))
import atlas_queue

QUEUE = Path(atlas_queue.__file__)
# A command that writes "started", and "stopped" when SIGTERM comes, and otherwise runs on.
RUNS_UNTIL_STOPPED = 'trap "echo stopped > stopped; exit 0" TERM; touch started; while true; do sleep 0.1; done'


def start_queue(tmp_path, role, *options, command):
    return subprocess.Popen([sys.executable, str(QUEUE), role, "--queue", str(tmp_path / "q"),
                             "--poll-seconds", "0.1", *options, "--", "sh", "-c", command],
                            cwd=tmp_path, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)


@pytest.fixture
def sleeping_group():
    """A throwaway process group that only sleeps, for a note to name."""
    sleeper = subprocess.Popen(["sleep", "60"], start_new_session=True)
    yield sleeper
    sleeper.kill()
    sleeper.wait()


def hold_the_lock(tmp_path, note):
    """Hold the queue's lock in this test, beside the note that a holder writes. Gives the open lock file."""
    (tmp_path / "q").mkdir(exist_ok=True)
    handle = open(tmp_path / "q" / "lock", "a")
    fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    (tmp_path / "q" / "holder.json").write_text(json.dumps(note))
    return handle


def lock_is_free(tmp_path):
    with open(tmp_path / "q" / "lock", "a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            return False
        return True


def wait_for(path, seconds=10):
    end = time.monotonic() + seconds
    while not path.exists():
        assert time.monotonic() < end, f"{path.name} did not appear"
        time.sleep(0.05)


def test_a_job_runs_holding_the_lock_with_its_note_and_then_frees_both(tmp_path):
    job = start_queue(tmp_path, "dev", "--what", "a fault run", command=(
        'cat q/holder.json > note.json; echo "$ATLAS_QUEUE_CPUS $(nice)" > limits.txt; exit 3'))

    assert job.wait(timeout=20) == 3
    note = json.loads((tmp_path / "note.json").read_text())
    assert note["what"] == "a fault run" and note["yields"] is True
    assert note["command"][:2] == ["sh", "-c"] and note["process_group"] > 1
    # Six of the server's eight CPUs for its containers, and nice 10 for the job itself.
    assert (tmp_path / "limits.txt").read_text() == "6 10\n"
    assert lock_is_free(tmp_path) and not (tmp_path / "q" / "holder.json").exists()


def test_a_job_waits_while_another_run_holds_the_queue(tmp_path, sleeping_group):
    held = hold_the_lock(tmp_path, {"what": "the nightly run", "command": ["sleep", "60"],
                                    "process_group": sleeping_group.pid, "yields": False})
    job = start_queue(tmp_path, "dev", command="touch ran")

    time.sleep(1)
    assert job.poll() is None and not (tmp_path / "ran").exists()
    held.close()
    assert job.wait(timeout=20) == 0 and (tmp_path / "ran").exists()


def test_the_nightly_asks_a_job_to_yield_and_the_job_stops_its_command(tmp_path):
    job = start_queue(tmp_path, "dev", command=RUNS_UNTIL_STOPPED)
    wait_for(tmp_path / "started")

    nightly = start_queue(tmp_path, "nightly", command='echo "$ATLAS_QUEUE_LATE_MINUTES" > late.txt')

    assert nightly.wait(timeout=20) == 0
    assert "asked for it" in nightly.stdout.read()
    assert job.wait(timeout=20) == 0 and (tmp_path / "stopped").read_text() == "stopped\n"
    assert (tmp_path / "late.txt").read_text() == "0\n"
    # The nightly's request ends with it.
    assert lock_is_free(tmp_path) and not (tmp_path / "q" / "yield").exists()


def test_a_job_stops_its_command_when_it_is_told_to_stop(tmp_path):
    # SIGHUP is what a dropped ssh sends; the command runs in a group of its own and must not run on unqueued.
    job = start_queue(tmp_path, "dev", command=RUNS_UNTIL_STOPPED)
    wait_for(tmp_path / "started")

    job.send_signal(signal.SIGHUP)

    assert job.wait(timeout=20) == 0 and (tmp_path / "stopped").read_text() == "stopped\n"
    assert lock_is_free(tmp_path)


def test_no_job_takes_the_queue_while_the_nightly_runs_beside_a_stuck_holder(tmp_path, sleeping_group):
    # The note names a command the group does not run, so the nightly refuses to signal it and, after its waits,
    # runs beside the holder, without the lock.
    held = hold_the_lock(tmp_path, {"what": "a stuck job", "command": ["not", "this"],
                                    "process_group": sleeping_group.pid, "yields": True})
    nightly = start_queue(tmp_path, "nightly", "--yield-wait-minutes", "0.01", "--stop-wait-minutes", "0.01",
                          command="touch nightly-started; sleep 2; touch nightly-done")
    wait_for(tmp_path / "nightly-started")
    job = start_queue(tmp_path, "dev", command="test -e nightly-done && touch ran-after-the-nightly")

    held.close()

    assert job.wait(timeout=20) == 0 and (tmp_path / "ran-after-the-nightly").exists()
    assert nightly.wait(timeout=20) == 0
    output = nightly.stdout.read()
    assert "does not run the noted command" in output and "the nightly runs beside it" in output


def test_a_yield_request_older_than_12_hours_is_ignored(tmp_path):
    (tmp_path / "q").mkdir()
    asked = datetime.now(timezone.utc) - timedelta(hours=13)
    (tmp_path / "q" / "yield").write_text(asked.isoformat(timespec="seconds") + "\n")

    job = start_queue(tmp_path, "dev", command="sleep 0.5; touch ran")

    assert job.wait(timeout=20) == 0 and (tmp_path / "ran").exists()


def test_the_nightly_stops_a_holder_that_does_not_yield_after_its_wait(tmp_path, sleeping_group):
    held = hold_the_lock(tmp_path, {"what": "a stuck job", "command": ["sleep", "60"],
                                    "process_group": sleeping_group.pid, "yields": True})
    sent = []

    handle = atlas_queue.wait_for_the_lock(tmp_path / "q", "nightly", None, poll_s=0.05, yield_wait_s=0.2,
                                           stop_wait_s=0.2, kill=lambda group, signum: sent.append((group, signum)))

    assert handle is None and sent == [(sleeping_group.pid, signal.SIGTERM)]
    held.close()


def test_the_nightly_never_stops_a_smoke_run_and_goes_before_a_waiting_job(tmp_path, sleeping_group):
    held = hold_the_lock(tmp_path, {"what": "the smoke run", "command": ["sleep", "60"],
                                    "process_group": sleeping_group.pid, "yields": False})
    nightly = start_queue(tmp_path, "nightly", "--yield-wait-minutes", "0.01", command="sleep 1; touch nightly-done")
    wait_for(tmp_path / "q" / "yield")
    job = start_queue(tmp_path, "dev", command="test -e nightly-done && touch ran-after-the-nightly")

    time.sleep(1.5)
    assert sleeping_group.poll() is None and nightly.poll() is None and job.poll() is None
    held.close()

    assert nightly.wait(timeout=20) == 0 and (tmp_path / "nightly-done").exists()
    assert job.wait(timeout=20) == 0 and (tmp_path / "ran-after-the-nightly").exists()


@pytest.mark.parametrize("group, command, refused_because", [
    (1, ["init"], "no job runs in it"),
    (0, ["x"], "no job runs in it"),
    (None, ["x"], "no job runs in it"),
    ("own", ["x"], "this run's own"),
    ("sleeper", ["another", "command"], "does not run the noted command"),
    ("ended", ["sleep", "60"], "does not run the noted command"),
])
def test_the_nightly_refuses_to_signal_a_group_the_note_cannot_name_safely(sleeping_group, group, command,
                                                                          refused_because):
    if group == "own":
        group = os.getpgrp()
        # In a container the tests can run in group 1, and the first rule refuses it.
        refused_because = refused_because if group > 1 else "no job runs in it"
    elif group == "sleeper":
        group = sleeping_group.pid
    elif group == "ended":
        ended = subprocess.Popen(["sleep", "60"], start_new_session=True)
        ended.kill()
        ended.wait()
        group = ended.pid
    sent = []

    done = atlas_queue.stop_the_holder({"process_group": group, "command": command},
                                       kill=lambda group, signum: sent.append((group, signum)))

    assert done.startswith("refused") and refused_because in done and sent == []


@pytest.mark.parametrize("role, options", [
    ("smoke", ["--wait-minutes", "0"]),
    ("dev", ["--floor-gib", "1000000"]),
], ids=["a smoke run whose wait has ended", "a job when / is too full"])
def test_a_run_that_cannot_start_runs_nothing_and_ends_with_status_75(tmp_path, sleeping_group, role, options):
    held = hold_the_lock(tmp_path, {"what": "a job", "command": ["sleep", "60"],
                                    "process_group": sleeping_group.pid, "yields": True})

    run = start_queue(tmp_path, role, *options, command="touch ran")

    assert run.wait(timeout=20) == 75
    assert "not started" in run.stdout.read() and not (tmp_path / "ran").exists()
    held.close()
