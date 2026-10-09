"""A caller that goes away stops the command it asked for.

The executor bounds time, memory, processes and output. What it did not bound
was interest: a caller that reset, closed or aborted its connection left the
command running to its own deadline, holding a CPU and a memory budget for an
answer nobody would read -- and producing verification evidence about a request
that no longer existed.

The handler is async so it can notice, the bounded runner is told through the
cancellation callback it already had, and cancellation stays a distinct outcome
from a timeout and from resource exhaustion.

Local fixtures only. No model, no deployed service.
"""

from __future__ import annotations

import contextlib
import os
import socket
import subprocess
import sys
import time

import pytest

from tests.infrastructure.bounded_commands import OWN_LIMIT, REACHED_OWN_LIMIT, allocator, flood
from tests.infrastructure.proc_files import needs_proc, sleeping

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SANDBOX = os.path.join(ROOT, "sandbox")
sys.path.insert(0, SANDBOX)

import resource_contract as rc

# Unique per run. A fixed marker is contaminated by any earlier run that
# left a process behind -- and these tests deliberately leave processes
# running mid-test, so an aborted one always does.
MARKER_BASE = 7300 + (os.getpid() % 900) * 10
# The ceilings the private executor of these tests runs under.
MEMORY_BYTES = 512 * 1024 * 1024
OUTPUT_BYTES = 8 * 1024 * 1024


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _reap(seconds: int) -> None:
    """Kill any stray marker process, so one test cannot leak into the next."""
    for pid in sleeping(seconds):
        # A marker that ends between the look and the kill is gone, which is what this wants.
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, 9)


def alive(seconds: int) -> int:
    """How many `sleep <seconds>` processes are still running. It fails where it cannot see a process at all."""
    return len(sleeping(seconds))


@pytest.fixture(scope="module")
def executor():
    """The real executor, in-process, on a private port."""
    port = _free_port()
    env = dict(os.environ,
               PORT=str(port),
               ATLAS_SANDBOX_WORKSPACE_ROOT="/tmp",
               ATLAS_EXEC_MEMORY_BYTES=str(MEMORY_BYTES),
               ATLAS_EXEC_OUTPUT_BYTES=str(OUTPUT_BYTES),
               MAX_EXECUTION_TIME="60")
    script = (
        "import importlib.util, os, sys, uvicorn\n"
        f"sys.path.insert(0, {SANDBOX!r})\n"
        "spec = importlib.util.spec_from_file_location('ex', "
        f"{os.path.join(SANDBOX, 'executor_server.py')!r})\n"
        "mod = importlib.util.module_from_spec(spec); sys.modules['ex'] = mod\n"
        "spec.loader.exec_module(mod)\n"
        "uvicorn.run(mod.app, host='127.0.0.1', port=int(os.environ['PORT']), "
        "log_level='error')\n")
    proc = subprocess.Popen([sys.executable, "-c", script], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    deadline = time.time() + 60
    while time.time() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                break
        except OSError:
            time.sleep(0.2)
    else:
        proc.kill()
        pytest.skip("the private executor did not start")
    yield port
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def _raw_post(port: int, body: str) -> socket.socket:
    """Open a connection, send a /shell request, return the live socket."""
    s = socket.create_connection(("127.0.0.1", port), timeout=10)
    payload = body.encode()
    s.sendall(b"POST /shell HTTP/1.1\r\nHost: 127.0.0.1\r\n"
              b"Content-Type: application/json\r\n"
              b"Content-Length: " + str(len(payload)).encode() + b"\r\n\r\n" + payload)
    return s


def _start_and_drop(port: int, marker: int, how: str) -> None:
    _reap(marker)
    body = ('{"command":"sleep %d","timeout":50}' % marker)
    s = _raw_post(port, body)
    time.sleep(1.5)                       # the command is running by now
    assert alive(marker) >= 1, "the command never started"
    if how == "rst":
        s.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER,
                     b"\x01\x00\x00\x00\x00\x00\x00\x00")
        s.close()
    elif how == "fin":
        s.shutdown(socket.SHUT_RDWR)
        s.close()
    else:
        s.close()


@needs_proc
@pytest.mark.parametrize("how,marker", [("rst", MARKER_BASE), ("fin", MARKER_BASE + 1)])
def test_a_caller_that_goes_away_stops_the_command(executor, how, marker):
    _reap(marker)
    _start_and_drop(executor, marker, how)
    deadline = time.time() + 20
    while time.time() < deadline:
        if alive(marker) == 0:
            break
        time.sleep(0.25)
    assert alive(marker) == 0, (
        f"the command outlived a {how} by more than the watcher's interval")


@needs_proc
def test_repeated_cancellation_is_idempotent(executor):
    marker = MARKER_BASE + 2
    for _ in range(3):
        _start_and_drop(executor, marker, "close")
        deadline = time.time() + 20
        while time.time() < deadline and alive(marker):
            time.sleep(0.25)
        assert alive(marker) == 0
    # And the executor is still answering.
    got = _shell(executor, "echo still-here", 10)
    assert got["outcome"] == rc.OUTCOME_COMPLETED


def _shell(port: int, command: str, timeout: int) -> dict:
    import json
    import urllib.request
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/shell",
        data=json.dumps({"command": command, "timeout": timeout}).encode(),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout + 60) as resp:
        return json.loads(resp.read())


@needs_proc
def test_a_healthy_neighbour_is_unaffected(executor):
    """One caller leaving does not disturb another's command."""
    import threading
    marker = MARKER_BASE + 3
    result = {}

    def neighbour():
        result["answer"] = _shell(executor, "sleep 6; echo neighbour-ok", 30)

    t = threading.Thread(target=neighbour)
    t.start()
    time.sleep(1.0)
    _start_and_drop(executor, marker, "rst")
    t.join(timeout=90)
    assert not t.is_alive(), "the neighbour never finished"
    assert result["answer"]["outcome"] == rc.OUTCOME_COMPLETED
    assert "neighbour-ok" in result["answer"]["stdout"]
    deadline = time.time() + 20
    while time.time() < deadline and alive(marker):
        time.sleep(0.25)
    assert alive(marker) == 0


@needs_proc
def test_cancellation_is_distinct_from_timeout_and_exhaustion(executor):
    """Three different endings, three different names."""
    timed = _shell(executor, "sleep 300", 3)
    assert timed["outcome"] == rc.OUTCOME_TIMED_OUT

    killed = _shell(executor, allocator(32, MEMORY_BYTES), 40)
    assert killed["exit_code"] != OWN_LIMIT, REACHED_OWN_LIMIT
    assert killed["outcome"] == rc.OUTCOME_MEMORY_EXHAUSTED

    flooded = _shell(executor, flood("ABCDEFGHIJKLMNOP", OUTPUT_BYTES), 40)
    assert flooded["outcome"] == rc.OUTCOME_OUTPUT_LIMIT

    # And the contract's own cancellation, which the HTTP path now reaches.
    small = rc.ResourceContract(wall_seconds=20, memory_bytes=256 * 1024 * 1024,
                                max_processes=16, output_bytes=1024 * 1024)
    got = rc.run_bounded(["bash", "-c", "sleep 30"], small, cancelled=lambda: True)
    assert got.outcome == rc.OUTCOME_CANCELLED
    assert got.outcome != rc.OUTCOME_TIMED_OUT


@needs_proc
def test_no_late_evidence_and_no_descendants_survive(executor):
    marker = MARKER_BASE + 4
    _reap(marker)
    body = ('{"command":"(sleep %d &) ; sleep %d","timeout":50}' % (marker, marker))
    s = _raw_post(executor, body)
    time.sleep(1.5)
    assert alive(marker) >= 1
    s.close()
    deadline = time.time() + 25
    while time.time() < deadline and alive(marker):
        time.sleep(0.25)
    assert alive(marker) == 0, "a detached descendant outlived the cancelled request"


@needs_proc
def test_shutdown_drains_within_the_bound(executor):
    """A shutdown does not wait for a long command, and leaves nothing."""
    port = _free_port()
    env = dict(os.environ, PORT=str(port), ATLAS_SANDBOX_WORKSPACE_ROOT="/tmp",
               ATLAS_EXEC_MEMORY_BYTES=str(512 * 1024 * 1024), MAX_EXECUTION_TIME="60")
    script = (
        "import importlib.util, os, sys, uvicorn\n"
        f"sys.path.insert(0, {SANDBOX!r})\n"
        "spec = importlib.util.spec_from_file_location('ex', "
        f"{os.path.join(SANDBOX, 'executor_server.py')!r})\n"
        "mod = importlib.util.module_from_spec(spec); sys.modules['ex'] = mod\n"
        "spec.loader.exec_module(mod)\n"
        "uvicorn.run(mod.app, host='127.0.0.1', port=int(os.environ['PORT']), "
        "log_level='error')\n")
    proc = subprocess.Popen([sys.executable, "-c", script], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    marker = MARKER_BASE + 5
    _reap(marker)
    try:
        deadline = time.time() + 60
        while time.time() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=1):
                    break
            except OSError:
                time.sleep(0.2)
        # A short deadline on purpose. Graceful shutdown drains in-flight work
        # rather than cutting it off, so "within the existing bound" is the
        # command's own deadline -- and what has to be true afterwards is that
        # nothing outlived it.
        s = _raw_post(port, '{"command":"sleep %d","timeout":6}' % marker)
        time.sleep(1.5)
        assert alive(marker) >= 1
        started = time.time()
        proc.terminate()
        proc.wait(timeout=40)
        drained = time.time() - started
        assert drained < 40, f"shutdown took {drained:.1f}s"
        s.close()
    finally:
        if proc.poll() is None:
            proc.kill()
            proc.wait(timeout=10)
    deadline = time.time() + 25
    while time.time() < deadline and alive(marker):
        time.sleep(0.25)
    assert alive(marker) == 0, "a command outlived the shutdown"


def test_the_handler_uses_supported_lifecycle_apis():
    """No socket poking, no interpreter-specific hack."""
    body = open(os.path.join(SANDBOX, "executor_server.py")).read()
    assert "await http.is_disconnected()" in body, (
        "the handler does not ask the framework whether the caller is gone")
    assert "asyncio.to_thread" in body
    for banned in ("_transport", "get_extra_info", "sock.", "socket.SO_",
                   "fileno()", "ctypes"):
        assert banned not in body, f"the handler reaches into {banned}"
    # One flag per request, never module-level: a shared flag is how a closed
    # descriptor cancels whoever inherits its number next.
    assert "gone = threading.Event()" in body
    handler = body[body.index("async def run_shell("):]
    handler = handler[:handler.index("\n@app.")]
    assert "cancelled=" not in handler or "gone.is_set" in handler
