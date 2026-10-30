"""A count of processes is an answer only where a process that is alive can be seen.

The sandbox tests assert that nothing is left running. Their count reads
/proc. Where that place shows nothing, a count of 0 is not "nothing is left",
so the count first looks for this very process and fails when it is not there.
"""
import os
import subprocess
import time
from pathlib import Path

import pytest

from tests.infrastructure import proc_files
from tests.infrastructure.proc_files import needs_proc

HERE = Path(__file__).parent
COUNTING_TESTS = ("test_execution_resource_contract.py", "test_http_cancellation.py")


def a_place(root: Path, processes: dict[int, bytes]) -> str:
    """A folder laid out as /proc is, with the command line of each given process."""
    for pid, command in processes.items():
        (root / str(pid)).mkdir(parents=True)
        (root / str(pid) / "cmdline").write_bytes(command)
    (root / "meminfo").write_text("not a process\n", encoding="utf-8")
    return str(root)


def test_a_place_that_shows_this_process_is_counted_in(tmp_path):
    root = a_place(tmp_path, {os.getpid(): b"python\x00-m\x00pytest\x00", 4242: b"sleep\x00777\x00",
                              4243: b"sleep\x007777\x00", 4244: b"sleep\x00777\x00extra\x00"})
    assert proc_files.sleeping(777, root) == [4242]
    assert proc_files.sleeping(7777, root) == [4243]
    assert proc_files.sleeping(55, root) == []


@pytest.mark.parametrize("processes", [
    pytest.param({}, id="no-process"),
    pytest.param({4242: b"sleep\x00777\x00"}, id="only-another-process"),
    pytest.param({os.getpid(): b""}, id="this-process-with-no-command-line"),
])
def test_a_place_that_does_not_show_this_process_cannot_be_counted_in(tmp_path, processes):
    root = a_place(tmp_path, processes)
    with pytest.raises(proc_files.CannotLook, match="cannot look: this process") as refused:
        proc_files.sleeping(777, root)
    assert "would say 0 whatever runs" in str(refused.value)
    assert "Fix:" in str(refused.value)
    assert isinstance(refused.value, AssertionError)


def test_a_place_that_is_not_there_cannot_be_counted_in(tmp_path):
    with pytest.raises(proc_files.CannotLook, match="cannot look"):
        proc_files.sleeping(777, str(tmp_path / "no-such-place"))


@needs_proc
def test_a_process_that_runs_is_seen_and_is_gone_when_it_ends():
    marker = 6100 + os.getpid() % 800
    running = subprocess.Popen(["sleep", str(marker)])
    try:
        # For a moment after a process starts, /proc shows it with no command line yet. The wait ends by the clock.
        deadline = time.monotonic() + 5
        while not proc_files.sleeping(marker) and time.monotonic() < deadline:
            time.sleep(0.01)
        assert proc_files.sleeping(marker) == [running.pid]
    finally:
        running.kill()
        running.wait(timeout=10)
    assert proc_files.sleeping(marker) == []


@pytest.mark.parametrize("name", COUNTING_TESTS)
def test_the_sandbox_tests_count_processes_only_through_the_helper(name):
    source = (HERE / name).read_text(encoding="utf-8")
    for call in ("glob.glob(", "os.listdir(\"/proc\")"):
        assert call not in source, (
            f"{name} reads /proc by itself. Fix: count with `sleeping` of tests/infrastructure/proc_files.py, "
            "which fails where it cannot see a process at all.")
    assert "return len(sleeping(seconds))" in source
