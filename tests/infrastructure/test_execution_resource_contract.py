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

import contextlib
import glob
import os
import sys
import time

import pytest

SANDBOX = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(
    os.path.abspath(__file__)))), "sandbox")
sys.path.insert(0, SANDBOX)

import resource_contract as rc  # noqa: E402

MiB = 1024 * 1024


@pytest.fixture
def small():
    c = rc.ResourceContract(wall_seconds=20, memory_bytes=384 * MiB,
                            max_processes=16, output_bytes=1 * MiB)
    c.validate()
    return c


def run(script, contract, **kw):
    return rc.run_bounded(["bash", "-c", script], contract, **kw)


def alive(seconds: int) -> int:
    """How many `sleep <seconds>` processes are still running."""
    want = ("sleep\x00%d\x00" % seconds).encode()
    n = 0
    for d in glob.glob("/proc/[0-9]*"):
        # A pid that vanishes between the glob and the open is the ordinary
        # race in scanning /proc, and skipping it is the whole handling. It is
        # named rather than swallowed: anything else raises.
        with contextlib.suppress(OSError):
            with open(d + "/cmdline", "rb") as fh:
                if fh.read() == want:
                    n += 1
    return n


# --- the operator's budget --------------------------------------------------


def test_a_budget_that_cannot_be_enforced_fails_closed():
    base = dict(wall_seconds=10, memory_bytes=256 * MiB, max_processes=8,
                output_bytes=1 * MiB)
    rc.ResourceContract(**base).validate()
    for field, bad in [("wall_seconds", 0), ("wall_seconds", -1),
                       ("memory_bytes", 0), ("memory_bytes", 1024),
                       ("max_processes", 0), ("max_processes", -3),
                       ("output_bytes", 0), ("output_bytes", -1)]:
        with pytest.raises(rc.ResourceContractError):
            rc.ResourceContract(**{**base, field: bad}).validate()
    # Internally inconsistent: the output buffer is held by the executor, so a
    # cap at or above the per-command memory ceiling kills the server first.
    with pytest.raises(rc.ResourceContractError):
        rc.ResourceContract(**{**base, "output_bytes": 256 * MiB}).validate()
    # 0 means the optional address-space backstop is off; anything between 0
    # and 1 would be a ceiling below the resident contract, which is not a
    # narrowing anyone can act on.
    rc.ResourceContract(**{**base, "address_space_multiple": 0}).validate()
    rc.ResourceContract(**{**base, "address_space_multiple": 2.0}).validate()
    with pytest.raises(rc.ResourceContractError):
        rc.ResourceContract(**{**base, "address_space_multiple": 0.5}).validate()


def test_a_malformed_operator_value_fails_at_startup(monkeypatch):
    monkeypatch.setenv("ATLAS_EXEC_MEMORY_BYTES", "lots")
    with pytest.raises(rc.ResourceContractError):
        rc.contract_from_env()
    monkeypatch.setenv("ATLAS_EXEC_MEMORY_BYTES", "-1")
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


def test_a_gradual_allocator_is_stopped_and_named(small):
    r = run("python3 -c \"import time\na=[]\nwhile True:\n a.append(bytearray(1<<20))\n"
            " time.sleep(0.001)\"", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success
    # The sampler is the enforcement now that the address-space rlimit is
    # off by default, and the bound it holds to is unchanged: 1 MiB per
    # millisecond can add at most ~50 MiB inside one 0.05s interval, which is
    # a tenth of this ceiling. Measured overshoot here is 1.04-1.10x.
    assert r.peak_memory_bytes <= small.memory_bytes * 1.5


def test_a_rapid_allocator_is_stopped_and_named(small):
    # Large blocks can cross the ceiling between two samples. VmHWM is a
    # high-water mark, so the next sample still sees the peak and stops the
    # tree; what the interval bounds is the overshoot, not whether it is
    # noticed.
    r = run("python3 -c \"a=[]\nwhile True: a.append(bytearray(64<<20))\"", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success


def test_the_runaway_that_took_the_host_down(small):
    """`stepped(0, 5, -1)` exactly as the frozen family seeds it."""
    r = run("python3 -c \"out=[]\ncurrent=0\nstop=5\nstep=-1\n"
            "while current < stop:\n out.append(current)\n current += step\"", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success
    # And under the previous owner it is not stopped at all: an unbounded
    # Popen with only a timeout reaches whatever the host will give it.
    import subprocess
    unbounded = subprocess.Popen(
        ["bash", "-c", "python3 -c \"out=[]\ncurrent=0\nwhile current < 5:\n"
         " out.append(current)\n current += -1\""],
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


def test_a_fork_tree_of_allocators_is_stopped(small):
    r = run("for i in 1 2 3 4 5; do python3 -c \"import time;a=bytearray(120<<20);"
            "time.sleep(30)\" & done; wait", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert not r.success


def test_a_command_near_the_limit_completes(small):
    r = run("python3 -c \"a=bytearray(200<<20); print(len(a))\"", small)
    assert r.outcome == rc.OUTCOME_COMPLETED
    assert r.success and r.returncode == 0


def test_an_ordinary_failure_is_not_a_resource_event(small):
    r = run("exit 3", small)
    assert r.outcome == rc.OUTCOME_COMPLETED
    assert not r.success and r.returncode == 3


# --- time, output, processes -------------------------------------------------


def test_timeout_before_memory(small):
    c = rc.ResourceContract(wall_seconds=2, memory_bytes=384 * MiB,
                            max_processes=16, output_bytes=1 * MiB)
    r = run("sleep 60", c)
    assert r.outcome == rc.OUTCOME_TIMED_OUT
    assert r.elapsed_seconds < 20


def test_memory_before_timeout(small):
    r = run("python3 -c \"import time\na=[]\nwhile True:\n a.append(bytearray(4<<20))\"", small)
    assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
    assert r.elapsed_seconds < small.wall_seconds


def test_output_overflow_is_named_not_silently_truncated(small):
    r = run("yes ABCDEFGHIJKLMNOP", small)
    assert r.outcome == rc.OUTCOME_OUTPUT_LIMIT
    assert r.truncated
    assert not r.success
    assert r.output_bytes <= small.output_bytes


def test_output_overflow_while_allocating(small):
    r = run("python3 -c \"a=[]\nimport sys\nwhile True:\n a.append(bytearray(1<<20))\n"
            " sys.stdout.write('x'*100000)\"", small)
    assert r.outcome in (rc.OUTCOME_OUTPUT_LIMIT, rc.OUTCOME_MEMORY_EXHAUSTED)
    assert not r.success


def test_process_limit(small):
    c = rc.ResourceContract(wall_seconds=15, memory_bytes=384 * MiB,
                            max_processes=8, output_bytes=1 * MiB)
    r = run("for i in $(seq 1 40); do sleep 12 & done; wait", c)
    assert r.outcome == rc.OUTCOME_PROCESS_LIMIT
    assert not r.success


# --- cancellation and cleanup ------------------------------------------------


def test_cancellation_before_execution(small):
    r = rc.run_bounded(["bash", "-c", "sleep 30"], small, cancelled=lambda: True)
    assert r.outcome == rc.OUTCOME_CANCELLED
    assert not r.success


def test_cancellation_during_allocation(small):
    started = time.time()
    r = run("python3 -c \"import time\na=[]\nwhile True:\n a.append(bytearray(1<<20))\n"
            " time.sleep(0.01)\"", small,
            cancelled=lambda: time.time() - started > 0.4)
    assert r.outcome == rc.OUTCOME_CANCELLED
    assert not r.success


def test_a_child_that_ignores_termination_still_dies(small):
    c = rc.ResourceContract(wall_seconds=2, memory_bytes=384 * MiB,
                            max_processes=16, output_bytes=1 * MiB)
    r = run("python3 -c \"import signal,time\n"
            "signal.signal(signal.SIGTERM, signal.SIG_IGN)\ntime.sleep(120)\"", c)
    assert r.outcome == rc.OUTCOME_TIMED_OUT
    assert r.survivors == 0


def test_a_child_that_closes_stdout_and_keeps_running_still_dies(small):
    c = rc.ResourceContract(wall_seconds=2, memory_bytes=384 * MiB,
                            max_processes=16, output_bytes=1 * MiB)
    r = run("python3 -c \"import os,time\nos.close(1)\ntime.sleep(120)\"", c)
    assert r.outcome == rc.OUTCOME_TIMED_OUT
    assert r.survivors == 0


@pytest.mark.parametrize("name,script,secs", [
    ("subshell background", "(sleep %d &) ; exit 0", 4211),
    ("double fork", "bash -c 'sleep %d &' ; exit 0", 4212),
    ("explicit setsid", "setsid sleep %d </dev/null >/dev/null 2>&1 & exit 0", 4213),
    ("nohup disown", "nohup sleep %d >/dev/null 2>&1 & disown; exit 0", 4214),
    ("grandchild via python",
     "python3 -c \"import subprocess;subprocess.Popen(['sleep','%d'])\"", 4215),
])
def test_nothing_the_command_started_outlives_the_request(small, name, script, secs):
    assert alive(secs) == 0, "a previous run leaked into this one"
    r = run(script % secs, small)
    time.sleep(0.4)
    assert r.survivors == 0, name
    assert alive(secs) == 0, name


def test_a_healthy_sibling_survives_a_memory_kill(small):
    """One command's ceiling is its own. Nothing else on the box is touched."""
    import subprocess
    sibling = subprocess.Popen(
        ["bash", "-c", "python3 -c \"import time;a=bytearray(80<<20);time.sleep(20)\""],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
    try:
        time.sleep(0.6)
        assert sibling.poll() is None
        r = run("python3 -c \"a=[]\nwhile True: a.append(bytearray(8<<20))\"", small)
        assert r.outcome == rc.OUTCOME_MEMORY_EXHAUSTED
        assert sibling.poll() is None, "a sibling died with the memory-killed command"
    finally:
        rc._kill_group(sibling.pid, 0.2)
        sibling.wait(timeout=5)


def test_repeated_memory_failures_stay_bounded(small):
    for _ in range(4):
        r = run("python3 -c \"a=[]\nwhile True: a.append(bytearray(8<<20))\"", small)
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
    killed = ex._run_cmd(
        ["bash", "-c", "python3 -c \"a=[]\nwhile True: a.append(bytearray(64<<20))\""],
        timeout=30)
    assert killed["outcome"] == rc.OUTCOME_MEMORY_EXHAUSTED
    assert killed["success"] is False
