"""The resource contract every untrusted command runs under.

A staged verification command took the deployed inference server down.
`pilot_failing_range_step` seeds a loop that appends without bound, its own
failing test calls it with a negative step, and its client-declared
verification is `python3 -m pytest -q test_steps.py`. pytest reached 5.9 GB in
seconds; the kernel ran a host-global out-of-memory kill and chose the largest
resident process on the box. The executor bounded time at sixty seconds per
command and nothing else, so the time bound never got a chance.

Every case here uses local fixtures. Nothing contacts a model, a deployed
service, or a container.
"""

from __future__ import annotations

import dataclasses
import os
import subprocess
import sys
import time

import pytest

from tests.infrastructure.bounded_commands import (OWN_LIMIT, REACHED_OWN_LIMIT, allocator, flood,
                                                   with_an_address_space_limit)
from tests.infrastructure.proc_files import needs_proc, sleeping

SANDBOX = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__)))), "sandbox")
sys.path.insert(0, SANDBOX)

import resource_contract as rc

MiB = 1024 * 1024
# The loop of the staged verification command, as it was seeded: it appends
# and never reaches its stop. Its text stays as it is.
RUNAWAY = ("python3 -c \"out=[]\ncurrent=0\nstop=5\nstep=-1\n"
           "while current < stop:\n out.append(current)\n current += step\"")


@pytest.fixture
def small():
    """20 seconds, 384 MiB, 16 processes and 1 MiB of output."""
    c = rc.ResourceContract(wall_seconds=20, memory_bytes=384 * MiB,
                            max_processes=16, output_bytes=1 * MiB)
    c.validate()
    return c


def run(script, contract, **kw):
    return rc.run_bounded(["bash", "-c", script], contract, **kw)


def alive(seconds: int) -> int:
    """How many `sleep <seconds>` processes are still running. It fails where it cannot see a process at all."""
    return len(sleeping(seconds))


# --- the operator's budget --------------------------------------------------

# A budget that can be enforced. Each case below changes one field of it.
BUDGET = dict(wall_seconds=10, memory_bytes=256 * MiB, max_processes=8, output_bytes=1 * MiB)


@pytest.mark.parametrize("field, value", [
    pytest.param("wall_seconds", 0, id="no time"),
    pytest.param("wall_seconds", -1, id="negative time"),
    pytest.param("memory_bytes", 0, id="no memory"),
    pytest.param("memory_bytes", 1024, id="too little memory for an interpreter"),
    pytest.param("max_processes", 0, id="no processes"),
    pytest.param("max_processes", -3, id="a negative number of processes"),
    pytest.param("output_bytes", 0, id="no output"),
    pytest.param("output_bytes", -1, id="negative output"),
    # The output buffer is held by the executor, so a cap at or above the
    # per-command memory ceiling kills the server first.
    pytest.param("output_bytes", 256 * MiB, id="an output cap as large as the memory ceiling"),
    # 0 means the optional address-space backstop is off; anything between 0
    # and 1 would be a ceiling below the resident contract, which is not a
    # narrowing anyone can act on.
    pytest.param("address_space_multiple", 0.5, id="an address-space ceiling below the memory ceiling"),
])
def test_a_budget_that_cannot_be_enforced_fails_closed(field, value):
    rc.ResourceContract(**BUDGET).validate()
    with pytest.raises(rc.ResourceContractError):
        rc.ResourceContract(**{**BUDGET, field: value}).validate()


@pytest.mark.parametrize("multiple", [0, 2.0], ids=["off", "twice the memory ceiling"])
def test_the_address_space_backstop_is_off_or_at_least_the_memory_ceiling(multiple):
    rc.ResourceContract(**BUDGET, address_space_multiple=multiple).validate()


@pytest.mark.parametrize("value", ["lots", "-1"], ids=["not a number", "negative"])
def test_a_malformed_operator_value_fails_at_startup(monkeypatch, value):
    monkeypatch.setenv("ATLAS_EXEC_MEMORY_BYTES", value)
    with pytest.raises(rc.ResourceContractError):
        rc.contract_from_env()


def test_the_product_default_is_explicit(monkeypatch):
    for name in ("MAX_EXECUTION_TIME", "ATLAS_EXEC_MEMORY_BYTES",
                 "ATLAS_EXEC_MAX_PROCESSES", "ATLAS_EXEC_OUTPUT_BYTES"):
        monkeypatch.delenv(name, raising=False)
    c = rc.contract_from_env()
    assert c.wall_seconds == 60
    assert c.memory_bytes == 2 * 1024 * MiB
    assert c.max_processes == 256
    assert c.output_bytes == 32 * MiB


def test_no_caller_can_raise_the_ceiling(small):
    # A request may ask for less time and never for more, and no request field
    # touches memory, processes or output at all.
    assert small.for_request(5).wall_seconds == 5
    assert small.for_request(9999).wall_seconds == small.wall_seconds
    assert small.for_request(0).wall_seconds == 1
    assert small.for_request(-7).wall_seconds == 1
    for asked in (None, 1, 5, 10 ** 9):
        got = small.for_request(asked)
        assert got.memory_bytes == small.memory_bytes
        assert got.max_processes == small.max_processes
        assert got.output_bytes == small.output_bytes


# --- memory -----------------------------------------------------------------


@needs_proc
def test_a_gradual_allocator_is_stopped_and_named(small):
    r = run(allocator(1, small.memory_bytes, pause=0.001), small)
    assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success
    # The sampler is the enforcement now that the address-space rlimit is
    # off by default, and the bound it holds to is unchanged: 1 MiB per
    # millisecond can add at most ~50 MiB inside one 0.05s interval, which is
    # a tenth of this ceiling. Measured overshoot here is 1.04-1.10x.
    assert r.peak_memory_bytes <= small.memory_bytes * 1.5


@needs_proc
def test_a_rapid_allocator_is_stopped_and_named(small):
    # Large blocks can cross the ceiling between two samples. VmHWM is a
    # high-water mark, so the next sample still sees the peak and stops the
    # tree; what the interval bounds is the overshoot, not whether it is
    # noticed.
    r = run(allocator(64, small.memory_bytes), small)
    assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success
    assert r.elapsed_seconds < small.wall_seconds  # stopped for its memory, long before its time is up


@needs_proc
def test_the_runaway_that_took_the_host_down(small):
    """`stepped(0, 5, -1)` exactly as the frozen family seeds it."""
    # The loop has no end. Before anything of the sandbox runs it: under a
    # limit of its own, and nothing else, it ends by itself.
    alone = subprocess.run(["bash", "-c", with_an_address_space_limit(RUNAWAY, 64 * MiB)],
                           capture_output=True, timeout=60, check=False)
    assert alone.returncode == OWN_LIMIT
    r = run(with_an_address_space_limit(RUNAWAY, small.memory_bytes), small)
    assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success
    # And under the previous owner it is not stopped at all: an unbounded
    # Popen with only a timeout reaches whatever the host will give it.
    # Nothing of the sandbox bounds this one, which is the point. It has the
    # limit of its own all the same, so that it ends when this test cannot
    # see its growth.
    unbounded = subprocess.Popen(
        ["bash", "-c", with_an_address_space_limit(RUNAWAY, small.memory_bytes)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        # Watched for long enough that a loaded machine does not turn a real
        # demonstration into a flaky one: the point is that nothing STOPS it,
        # not how fast it gets there.
        grew = 0
        deadline = time.time() + 30
        while time.time() < deadline:
            time.sleep(0.05)
            grew = max(grew, rc.group_usage(unbounded.pid)[0])
            if grew > small.memory_bytes:
                break
            if unbounded.poll() is not None:
                break
        assert grew > small.memory_bytes, (
            "the previous owner should allow growth past the ceiling; "
            f"reached {grew} of {small.memory_bytes}")
    finally:
        rc._kill_group(unbounded.pid, 0.2)
        unbounded.wait(timeout=5)


@needs_proc
def test_a_fork_tree_of_allocators_is_stopped(small):
    r = run("for i in 1 2 3 4 5; do python3 -c \"import time;a=bytearray(120<<20);"
            "time.sleep(30)\" & done; wait", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success


@pytest.mark.parametrize("script, returncode, success", [
    pytest.param("python3 -c \"a=bytearray(200<<20); print(len(a))\"", 0, True,
                 id="200 MiB under a ceiling of 384 MiB"),
    pytest.param("exit 3", 3, False, id="an ordinary failure"),
])
def test_a_command_that_ends_by_itself_is_completed(small, script, returncode, success):
    r = run(script, small)
    assert r.outcome == rc.OUTCOME_COMPLETED
    assert r.returncode == returncode
    assert r.success is success


# The optional address-space ceiling (address_space_multiple) is a limit the
# kernel holds the child to. The child asks for address space with a mapping
# it never touches, so it uses no memory even when the ceiling is not there.
@pytest.mark.skipif(sys.platform != "linux", reason="Linux holds a process to RLIMIT_AS; macOS does not")
def test_the_address_space_ceiling_stops_a_child_above_it():
    contract = rc.ResourceContract(wall_seconds=20, memory_bytes=64 * MiB, max_processes=16,
                                   output_bytes=1 * MiB, address_space_multiple=1.0)
    contract.validate()
    mapping = "python3 -c \"import mmap; mmap.mmap(-1, {} << 20); print('mapped')\""
    below = run(mapping.format(16), contract)
    assert below.returncode == 0 and "mapped" in below.stdout, below.stderr
    above = run(mapping.format(128), contract)
    assert above.returncode != 0 and "Cannot allocate memory" in above.stderr, (above.returncode, above.stdout)


# --- time, output, processes -------------------------------------------------


def test_a_command_past_its_time_is_timed_out(small):
    r = run("sleep 60", dataclasses.replace(small, wall_seconds=2))
    assert r.outcome == rc.OUTCOME_TIMED_OUT
    assert r.elapsed_seconds < 20


def test_output_overflow_is_named_not_silently_truncated(small):
    r = run(flood("ABCDEFGHIJKLMNOP", small.output_bytes), small)
    assert r.outcome == rc.OUTCOME_OUTPUT_LIMIT
    assert r.truncated
    assert not r.success
    assert r.output_bytes <= small.output_bytes


def test_output_overflow_while_allocating(small):
    r = run(allocator(1, small.memory_bytes, each_block="sys.stdout.write('x'*100000)"), small)
    assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
    assert r.outcome in (rc.OUTCOME_OUTPUT_LIMIT, rc.OUTCOME_MEMORY_EXHAUSTED)
    assert not r.success


@needs_proc
def test_process_limit(small):
    r = run("for i in $(seq 1 40); do sleep 12 & done; wait",
            dataclasses.replace(small, wall_seconds=15, max_processes=8))
    assert r.outcome == rc.OUTCOME_PROCESS_LIMIT
    assert not r.success


# --- cancellation and cleanup ------------------------------------------------


@pytest.mark.parametrize("script, cancel_after", [
    pytest.param("sleep 30", 0, id="before it runs"),
    pytest.param(allocator(1, 384 * MiB, pause=0.01), 0.4, id="while it allocates"),
])
def test_a_cancelled_command_is_named_cancelled(small, script, cancel_after):
    started = time.time()
    r = run(script, small, cancelled=lambda: time.time() - started >= cancel_after)
    assert r.outcome == rc.OUTCOME_CANCELLED
    assert not r.success


@needs_proc
@pytest.mark.parametrize("script", [
    pytest.param("python3 -c \"import signal,time\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\ntime.sleep(120)\"",
                 id="it ignores termination"),
    pytest.param("python3 -c \"import os,time\nos.close(1)\ntime.sleep(120)\"",
                 id="it closes stdout and keeps running"),
])
def test_a_child_that_runs_past_its_time_still_dies(small, script):
    r = run(script, dataclasses.replace(small, wall_seconds=2))
    assert r.outcome == rc.OUTCOME_TIMED_OUT
    assert r.survivors == 0


# Each case sleeps a number of seconds of its own, so its processes can be
# counted apart from those of any other case.
@needs_proc
@pytest.mark.parametrize("script, secs", [
    pytest.param("(sleep %d &) ; exit 0", 4211, id="subshell background"),
    pytest.param("bash -c 'sleep %d &' ; exit 0", 4212, id="double fork"),
    pytest.param("setsid sleep %d </dev/null >/dev/null 2>&1 & exit 0", 4213, id="explicit setsid"),
    pytest.param("nohup sleep %d >/dev/null 2>&1 & disown; exit 0", 4214, id="nohup disown"),
    pytest.param("python3 -c \"import subprocess;subprocess.Popen(['sleep','%d'])\"", 4215,
                 id="grandchild via python"),
])
def test_nothing_the_command_started_outlives_the_request(small, script, secs):
    assert alive(secs) == 0, "a previous run leaked into this one"
    r = run(script % secs, small)
    time.sleep(0.4)
    assert r.survivors == 0
    assert alive(secs) == 0


@needs_proc
def test_a_healthy_sibling_survives_a_memory_kill(small):
    """One command's ceiling is its own. Nothing else on the box is touched."""
    sibling = subprocess.Popen(
        ["bash", "-c", "python3 -c \"import time;a=bytearray(80<<20);time.sleep(20)\""],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        time.sleep(0.6)
        assert sibling.poll() is None
        r = run(allocator(8, small.memory_bytes), small)
        assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
        assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
        assert sibling.poll() is None, "a sibling died with the memory-killed command"
    finally:
        rc._kill_group(sibling.pid, 0.2)
        sibling.wait(timeout=5)


@needs_proc
def test_repeated_memory_failures_stay_bounded(small):
    for _ in range(4):
        r = run(allocator(8, small.memory_bytes), small)
        assert r.returncode != OWN_LIMIT, REACHED_OWN_LIMIT
        assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
        assert r.survivors == 0


# --- the executor speaks the same contract ------------------------------------


def test_the_executor_runs_everything_through_the_owner():
    src = open(os.path.join(SANDBOX, "executor_server.py")).read()
    assert "EXEC_CONTRACT = contract_from_env()" in src
    assert "run_bounded(cmd, EXEC_CONTRACT.for_request(timeout)" in src
    # No execution site may spawn around the owner.
    body = "\n".join(line for line in src.splitlines()
                     if not line.strip().startswith("#"))
    spawns = body.count("subprocess.Popen(")
    assert spawns == 1, (
        f"{spawns} direct Popen sites in the executor; untrusted execution "
        "belongs to run_bounded and the one background-job spawner")
    assert "preexec_fn=_apply_child_limits(EXEC_CONTRACT)" in body, (
        "the background job spawner does not install the contract's limits")
    for banned in ("os.system(", "subprocess.call(", "subprocess.run(cmd,",
                   "os.popen("):
        assert banned not in body, f"the executor reaches {banned}"


def test_the_executor_result_carries_the_outcome():
    sys.path.insert(0, SANDBOX)
    import executor_server as ex
    got = ex._run_cmd(["bash", "-c", "echo ok"], timeout=5)
    assert got["outcome"] == rc.OUTCOME_COMPLETED
    assert got["success"] is True
    assert got["survivors"] == 0


@needs_proc
def test_the_executor_result_names_a_command_that_took_too_much_memory(monkeypatch, small):
    sys.path.insert(0, SANDBOX)
    import executor_server as ex
    # The small contract of this file, not the product's own ceiling: the
    # command's own limit is a multiple of the ceiling it runs under.
    monkeypatch.setattr(ex, "EXEC_CONTRACT", small)
    killed = ex._run_cmd(["bash", "-c", allocator(64, small.memory_bytes)], timeout=30)
    assert killed["returncode"] != OWN_LIMIT, REACHED_OWN_LIMIT
    assert killed["outcome"] == rc.OUTCOME_MEMORY_EXHAUSTED
    assert killed["success"] is False
