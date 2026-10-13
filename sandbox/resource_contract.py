"""One resource contract for every untrusted command this executor runs.

A staged verification command took the host down. `pilot_failing_range_step`
seeds a loop that appends without bound, its own failing test calls it, and its
client-declared verification is `python3 -m pytest -q test_steps.py`. pytest
reached 5.9 GB in seconds, the kernel ran a GLOBAL out-of-memory kill, and it
chose the largest resident process on the box -- the inference server, which had
no cgroup limit and 7.6 GB resident. The executor bounded TIME at sixty seconds
per command and nothing else, so the time bound never got a chance.

Two things were missing, and only one of them is a limit.

The first is a memory ceiling that covers the whole descendant tree. The second
is a truthful answer. RLIMIT_AS alone gives the wrong one: a python process that
hits it raises MemoryError and exits 1, which is byte-for-byte what a failing
test looks like. A verification that never completed would then be recorded as a
behavioural failure of the candidate -- the machine asserting something it did
not observe. So the ceiling is enforced twice: a kernel rlimit as a hard
backstop, and a sampler that watches the process group's own resident set and
kills it deliberately, because a limit WE applied is a fact we can report.

Nothing here decides anything about a candidate. It reports how an execution
ended; whether that ending can support evidence is the proxy's decision, and
every non-`completed` outcome is defined so that it cannot.
"""

from __future__ import annotations

import os
import resource
import sys
import signal
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional

# --- the closed outcome vocabulary ------------------------------------------
#
# Exactly one member describes an execution that ran to its own conclusion.
# Every other member means the command did not finish saying what it wanted to
# say, and no reader may treat one as evidence about the code under test.

OUTCOME_COMPLETED = "completed"
OUTCOME_TIMED_OUT = "timed_out"
OUTCOME_MEMORY_EXHAUSTED = "memory_exhausted"
OUTCOME_PROCESS_LIMIT = "process_limit_exceeded"
OUTCOME_OUTPUT_LIMIT = "output_limit_exceeded"
OUTCOME_CANCELLED = "cancelled"
OUTCOME_SPAWN_FAILED = "spawn_failed"
OUTCOME_UNCLASSIFIED = "internal_unclassified"

ALL_OUTCOMES = (
    OUTCOME_COMPLETED, OUTCOME_TIMED_OUT, OUTCOME_MEMORY_EXHAUSTED,
    OUTCOME_PROCESS_LIMIT, OUTCOME_OUTPUT_LIMIT, OUTCOME_CANCELLED,
    OUTCOME_SPAWN_FAILED, OUTCOME_UNCLASSIFIED,
)

# The outcomes under which the command reached its own end. Only this one.
COMPLETED_OUTCOMES = frozenset({OUTCOME_COMPLETED})


# ru_maxrss is in kilobytes on Linux and in bytes on macOS. Scaling a macOS
# figure by 1024 put every command at gigabytes, so each one -- `print(1)`
# included -- ended memory_exhausted on a developer's machine. Production runs
# on Linux; this only makes a local run of the sandbox mean what it says.
_MAXRSS_UNIT = 1 if sys.platform == "darwin" else 1024


def outcome_is_complete(outcome: str) -> bool:
    """Whether the command ran to its own conclusion.

    Everything else -- including the fail-closed member -- is an execution that
    was stopped, and a stopped execution demonstrates nothing.
    """
    return outcome in COMPLETED_OUTCOMES


# What an answer with a field for the kind of error calls a command that was
# stopped at a ceiling. The kind comes from the outcome, never from a phrase in
# what the command wrote: a command that is killed writes nothing about it.
STOPPED_KINDS = {
    OUTCOME_TIMED_OUT: "Timeout", OUTCOME_MEMORY_EXHAUSTED: "MemoryLimit",
    OUTCOME_PROCESS_LIMIT: "ProcessLimit", OUTCOME_OUTPUT_LIMIT: "OutputLimit",
}


def stopped_words(outcome: str, contract: "ResourceContract") -> str:
    """One line that names the ceiling a command was stopped at; empty for every other outcome."""
    return {
        OUTCOME_TIMED_OUT: f"Execution timed out after {contract.wall_seconds}s",
        OUTCOME_MEMORY_EXHAUSTED: f"Execution stopped at the memory limit of {contract.memory_bytes // (1024 * 1024)} MiB",
        OUTCOME_PROCESS_LIMIT: f"Execution stopped at the limit of {contract.max_processes} processes",
        OUTCOME_OUTPUT_LIMIT: f"Execution stopped at the output limit of {contract.output_bytes} bytes",
    }.get(outcome, "")


def why_it_failed(run: dict, kind, classify) -> dict:
    """The fields of an answer that say why a run failed, and how the run ended.

    `run` is the result of one bounded command, with `stopped` holding the
    line of stopped_words. A run that was stopped at a ceiling is named by
    its outcome, and that line is the first line of its message. Its stderr
    stays what the command itself wrote. For a run that reached its own end,
    `kind` is the kind of a failure of this step (a compile error); with no
    `kind`, `classify` reads the kind out of what the run wrote.
    """
    stderr, failed = run["stderr"], not run["success"]
    message = "\n".join(part for part in (run.get("stopped", ""), stderr) if part)
    return {
        "stderr": stderr, "outcome": run["outcome"], "timed_out": run["outcome"] == OUTCOME_TIMED_OUT,
        "error_type": (STOPPED_KINDS.get(run["outcome"]) or kind or classify(stderr)) if failed else None,
        "error_message": message[:500] if failed else None,
    }


class ResourceContractError(ValueError):
    """An operator budget that cannot be enforced as written."""


@dataclass(frozen=True)
class ResourceContract:
    """The operator's ceiling for one untrusted command.

    Every field is a hard maximum. A caller may ask for less and never for
    more: `for_request` takes the minimum of the two, so no request body, task
    contract, tool argument, model output or workspace file can raise it.
    """

    wall_seconds: int
    memory_bytes: int
    max_processes: int
    output_bytes: int
    sample_interval: float = 0.05
    # An OPTIONAL kernel backstop on ADDRESS SPACE, off by default.
    #
    # Address space is not memory, and treating it as memory broke every
    # managed runtime in the sandbox. A JVM reserves tens of gigabytes of
    # virtual space before touching any of it: javac exited with an empty
    # CompileError and kotlinc with `OutOfMemoryError: Map failed` from
    # UnixFileDispatcherImpl.map0, while ruby and php -- which reserve almost
    # nothing -- passed the same smoke test. The limit was refusing a mapping,
    # not a memory demand.
    #
    # The resident ceiling above is the contract, and the sampler enforces it
    # across the whole descendant tree from VmHWM, which is a high-water mark:
    # any sample taken after growth still sees the peak, so a burst between
    # two samples is caught at the next one rather than missed. That is what
    # bounds a runaway. A multiple set here re-enables the rlimit at
    # memory_bytes x this value, for a deployment that wants a second wall and
    # runs no managed runtime; 0 leaves it off.
    address_space_multiple: float = 0.0
    # How long a killed group has to die before SIGKILL follows SIGTERM.
    grace_seconds: float = 2.0

    def validate(self) -> None:
        if self.wall_seconds <= 0:
            raise ResourceContractError(
                f"wall_seconds must be positive, got {self.wall_seconds}")
        if self.memory_bytes < 64 * 1024 * 1024:
            raise ResourceContractError(
                "memory_bytes must be at least 64 MiB for an interpreter to "
                f"start at all, got {self.memory_bytes}")
        if self.max_processes <= 0:
            raise ResourceContractError(
                f"max_processes must be positive, got {self.max_processes}")
        if self.output_bytes <= 0:
            raise ResourceContractError(
                f"output_bytes must be positive, got {self.output_bytes}")
        if self.address_space_multiple and self.address_space_multiple < 1.0:
            raise ResourceContractError(
                "address_space_multiple must be 0 (off) or at least 1.0, got "
                f"{self.address_space_multiple}")
        if self.sample_interval <= 0:
            raise ResourceContractError(
                f"sample_interval must be positive, got {self.sample_interval}")
        # Internal consistency: the output buffer lives in THIS process, so a
        # cap above the per-command memory ceiling means the executor dies
        # before the command does.
        if self.output_bytes >= self.memory_bytes:
            raise ResourceContractError(
                f"output_bytes ({self.output_bytes}) must be below memory_bytes "
                f"({self.memory_bytes}): the buffer is held by the executor")

    def for_request(self, wall_seconds: Optional[int]) -> "ResourceContract":
        """This contract, narrowed by a caller's own shorter deadline.

        Narrowing only. A request asking for longer gets the operator's value,
        which is the whole point of the ceiling being operator-owned.
        """
        if wall_seconds is None:
            return self
        wall = max(1, min(int(wall_seconds), self.wall_seconds))
        return ResourceContract(
            wall_seconds=wall, memory_bytes=self.memory_bytes,
            max_processes=self.max_processes, output_bytes=self.output_bytes,
            sample_interval=self.sample_interval,
            address_space_multiple=self.address_space_multiple,
            grace_seconds=self.grace_seconds)


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return int(raw)
    except ValueError as exc:
        raise ResourceContractError(f"{name}={raw!r} is not an integer") from exc


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or raw.strip() == "":
        return default
    try:
        return float(raw)
    except ValueError as exc:
        raise ResourceContractError(f"{name}={raw!r} is not a number") from exc


def contract_from_env() -> ResourceContract:
    """The operator's contract, validated at import.

    A malformed, zero, negative or internally inconsistent budget raises here,
    at startup, rather than being clamped into something plausible at the
    moment an untrusted command is about to run.
    """
    c = ResourceContract(
        wall_seconds=_env_int("MAX_EXECUTION_TIME", 60),
        memory_bytes=_env_int("ATLAS_EXEC_MEMORY_BYTES", 2 * 1024 * 1024 * 1024),
        max_processes=_env_int("ATLAS_EXEC_MAX_PROCESSES", 256),
        output_bytes=_env_int("ATLAS_EXEC_OUTPUT_BYTES", 32 * 1024 * 1024),
        address_space_multiple=_env_float("ATLAS_EXEC_ADDRESS_SPACE_MULTIPLE", 0.0),
    )
    c.validate()
    return c


@dataclass
class BoundedResult:
    outcome: str
    returncode: int
    stdout: str
    stderr: str
    peak_memory_bytes: int
    peak_processes: int
    output_bytes: int
    elapsed_seconds: float
    truncated: bool
    # Processes still alive after every stop this owner knows how to make. It
    # is zero on every path the tests cover and is carried rather than assumed,
    # because a platform where it is not zero must say so.
    survivors: int = 0

    @property
    def success(self) -> bool:
        """Exit zero AND a command that reached its own end.

        A memory-killed pytest exits non-zero, and so does a failing one. The
        difference is the outcome, never the code, so success is defined over
        both.
        """
        return self.returncode == 0 and outcome_is_complete(self.outcome)


# --- process-group accounting ------------------------------------------------


def _pids_in_group(pgid: int) -> List[int]:
    """Every live process in the group, read from /proc.

    The group is the unit because the thing that kills a host is rarely one
    process: it is a test runner with workers, or a build with a linker. A
    parent that exits leaving allocating children behind still has them here.
    """
    out = []
    try:
        entries = os.listdir("/proc")
    except OSError:
        return out
    for name in entries:
        if not name.isdigit():
            continue
        try:
            with open(f"/proc/{name}/stat", "rb") as fh:
                fields = fh.read().rsplit(b")", 1)[1].split()
            # After the comm field: state, ppid, pgrp, ...
            if int(fields[2]) == pgid:
                out.append(int(name))
        except (OSError, IndexError, ValueError):
            continue
    return out


def _rss_bytes(pid: int) -> int:
    """This process's PEAK resident set, not its current one.

    VmHWM is a kernel-maintained high-water mark that survives until the
    process exits, so a sample taken after a spike still sees the spike. An
    instantaneous reading misses anything that grows and dies between two
    samples, which is precisely the allocator that took the host down.
    """
    try:
        with open(f"/proc/{pid}/status", "rb") as fh:
            for line in fh:
                if line.startswith(b"VmHWM:"):
                    return int(line.split()[1]) * 1024
    except (OSError, IndexError, ValueError):
        return 0
    return 0


def group_usage(pgid: int, token: str = "") -> tuple:
    """(peak resident bytes, process count) for everything this run started.

    The union of the process group and the token holders: the group is the
    cheap common case, the token catches whatever left it.
    """
    pids = set(_pids_in_group(pgid))
    if token:
        pids.update(_token_holders(token))
    return sum(_rss_bytes(p) for p in pids), len(pids)


EXEC_TOKEN_VAR = "ATLAS_EXEC_TOKEN"


def _token_holders(token: str) -> List[int]:
    """Every live process carrying this execution's token.

    The process group is not enough. `(cmd &)`, a double fork, or an explicit
    `setsid` all leave the group, and once the direct child is reaped the
    orphan reparents away, so neither a group signal nor a parent walk finds
    it. The token is in the environment the child was started with, it is
    inherited by everything the command spawns however it spawns it, and
    /proc/<pid>/environ is readable for our own uid -- so the set is exact and
    needs no privilege, namespace or cgroup.
    """
    marker = f"{EXEC_TOKEN_VAR}={token}".encode()
    found = []
    try:
        entries = os.listdir("/proc")
    except OSError:
        return found
    for name in entries:
        if not name.isdigit():
            continue
        try:
            with open(f"/proc/{name}/environ", "rb") as fh:
                if marker in fh.read():
                    found.append(int(name))
        except (OSError, ValueError):
            continue
    return found


def _kill_token(token: str, grace: float) -> int:
    """End everything carrying the token. Returns how many were still alive."""
    victims = _token_holders(token)
    if not victims:
        return 0
    for sig in (signal.SIGTERM, signal.SIGKILL):
        for pid in victims:
            try:
                os.kill(pid, sig)
            except (ProcessLookupError, PermissionError, OSError):
                continue
        deadline = time.time() + (grace if sig == signal.SIGTERM else 0.5)
        while time.time() < deadline:
            victims = _token_holders(token)
            if not victims:
                return 0
            time.sleep(0.02)
    return len(_token_holders(token))


def _kill_group(pgid: int, grace: float) -> None:
    """End the group and everything in it, politely then not.

    Called on every path including the ordinary one: a command that exits
    normally after forking a child into the background has left something
    running, and the request it belonged to is over.
    """
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(pgid, sig)
        except (ProcessLookupError, PermissionError, OSError):
            return
        deadline = time.time() + (grace if sig == signal.SIGTERM else 0.5)
        while time.time() < deadline:
            if not _pids_in_group(pgid):
                return
            time.sleep(0.02)


def _apply_child_limits(contract: ResourceContract):
    """The kernel backstop, installed in the child between fork and exec.

    In the CHILD, not the parent: the executor reads command output into its
    own memory, and a parent that shares the child's ceiling dies of its own
    success. RLIMIT_AS is inherited across fork and exec, so every descendant
    carries it whatever the command spawns.
    """

    ceiling = int(contract.memory_bytes * contract.address_space_multiple) \
        if contract.address_space_multiple else 0

    def _limits():
        if ceiling:
            resource.setrlimit(resource.RLIMIT_AS, (ceiling, ceiling))
        # A file the command writes is not the concern here; the executor's own
        # buffer is bounded separately. This bounds a single runaway file that
        # would fill the container's writable layer.
        try:
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        except (ValueError, OSError):
            pass
        os.setsid()

    return _limits


# --- the one bounded runner --------------------------------------------------


class _Pump(threading.Thread):
    """Reads one pipe into a bounded buffer.

    subprocess.communicate() reads until EOF into unbounded memory, which is
    the same host-exhaustion shape one level up: a command that prints forever
    kills the EXECUTOR rather than itself. This stops at the cap and says so.
    """

    def __init__(self, stream, limit: int):
        super().__init__(daemon=True)
        self.stream, self.limit = stream, limit
        self.chunks: List[bytes] = []
        self.total = 0
        self.overflowed = False

    def run(self) -> None:
        try:
            while True:
                chunk = self.stream.read(65536)
                if not chunk:
                    return
                self.total += len(chunk)
                if self.total > self.limit:
                    self.overflowed = True
                    room = self.limit - sum(len(c) for c in self.chunks)
                    if room > 0:
                        self.chunks.append(chunk[:room])
                    return
                self.chunks.append(chunk)
        except (OSError, ValueError):
            return
        finally:
            try:
                self.stream.close()
            except (OSError, ValueError):
                pass

    def text(self) -> str:
        return b"".join(self.chunks).decode("utf-8", "replace")


def _exit_code(status) -> int:
    """A wait status as the exit code the rest of the system speaks.

    Negative for a signal, mirroring subprocess, so one convention survives.
    """
    if status is None:
        return -1
    if os.WIFSIGNALED(status):
        return -os.WTERMSIG(status)
    if os.WIFEXITED(status):
        return os.WEXITSTATUS(status)
    return -1


def run_bounded(cmd: List[str], contract: ResourceContract,  # noqa: C901
                cwd: Optional[Path] = None, env: Optional[Dict[str, str]] = None,
                stdin: Optional[str] = None,
                cancelled=None) -> BoundedResult:
    """Run one untrusted command under the contract, and say how it ended.

    Every limit is installed before the command starts, applies to the whole
    descendant tree, and cannot be raised by anything the command does. The
    group is ended on every exit path, so a fork-and-exit cannot outlive the
    request that asked for it.
    """
    run_env = os.environ.copy()
    if env:
        run_env.update(env)
    token = uuid.uuid4().hex
    run_env[EXEC_TOKEN_VAR] = token
    started = time.time()
    try:
        proc = subprocess.Popen(
            cmd,
            stdin=subprocess.PIPE if stdin is not None else subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            cwd=str(cwd) if cwd else None,
            env=run_env,
            preexec_fn=_apply_child_limits(contract),
        )
    except (OSError, ValueError) as exc:
        return BoundedResult(
            outcome=OUTCOME_SPAWN_FAILED, returncode=-1, stdout="",
            stderr=str(exc), peak_memory_bytes=0, peak_processes=0,
            output_bytes=0, elapsed_seconds=time.time() - started, truncated=False)

    pgid = proc.pid  # setsid in the child makes the pid the group id
    half = max(1, contract.output_bytes // 2)
    out_pump, err_pump = _Pump(proc.stdout, half), _Pump(proc.stderr, half)
    out_pump.start()
    err_pump.start()
    if stdin is not None:
        try:
            proc.stdin.write(stdin.encode())
        except (OSError, ValueError):
            pass
        finally:
            try:
                proc.stdin.close()
            except (OSError, ValueError):
                pass

    ceiling = contract.memory_bytes
    deadline = started + contract.wall_seconds
    peak_mem, peak_procs = 0, 0
    outcome = OUTCOME_UNCLASSIFIED
    status = None
    child_peak = 0

    # Reaped here rather than through Popen, because wait4 carries the child's
    # own rusage -- and ru_maxrss is the kernel's peak for that child and every
    # descendant it reaped. A command that allocates a gigabyte and dies
    # between two samples leaves no trace in /proc by the time anyone looks;
    # it leaves an exact number here.
    while True:
        try:
            done, st, usage = os.wait4(proc.pid, os.WNOHANG)
        except ChildProcessError:
            done, st, usage = proc.pid, 0, None
        if done:
            status = st
            if usage is not None:
                child_peak = usage.ru_maxrss * _MAXRSS_UNIT
            outcome = OUTCOME_COMPLETED
            break
        rss, procs = group_usage(pgid, token)
        peak_mem, peak_procs = max(peak_mem, rss), max(peak_procs, procs)
        if out_pump.overflowed or err_pump.overflowed:
            outcome = OUTCOME_OUTPUT_LIMIT
            break
        if cancelled is not None and cancelled():
            outcome = OUTCOME_CANCELLED
            break
        if peak_mem >= ceiling:
            # OUR decision, which is what makes it reportable.
            outcome = OUTCOME_MEMORY_EXHAUSTED
            break
        if procs > contract.max_processes:
            outcome = OUTCOME_PROCESS_LIMIT
            break
        if time.time() >= deadline:
            outcome = OUTCOME_TIMED_OUT
            break
        time.sleep(contract.sample_interval)

    if outcome != OUTCOME_COMPLETED:
        _kill_group(pgid, contract.grace_seconds)
        _kill_token(token, contract.grace_seconds)
        try:
            done, st, usage = os.wait4(proc.pid, 0)
            status = st
            if usage is not None:
                child_peak = usage.ru_maxrss * _MAXRSS_UNIT
        except (ChildProcessError, OSError):
            pass
    out_pump.join(timeout=2)
    err_pump.join(timeout=2)
    # Nothing outlives the request, including on the ordinary path: a command
    # that exited after forking a child into the background left it running.
    _kill_group(pgid, 0.1)
    survivors = _kill_token(token, contract.grace_seconds)
    proc.returncode = _exit_code(status)
    peak_mem = max(peak_mem, child_peak)

    returncode = proc.returncode

    # A child that died on its own before a sample saw it -- on the optional
    # address-space rlimit, or on the container's own cgroup limit -- exits in
    # a way that is indistinguishable from a failing test. ru_maxrss is the
    # kernel's own peak for that child and everything it reaped, so it tells
    # them apart by measurement rather than by reading the child's words.
    if outcome == OUTCOME_COMPLETED and (out_pump.overflowed or err_pump.overflowed):
        # Closing the pipe at the cap sends SIGPIPE, so the command frequently
        # dies before the sampler reads the overflow flag and lands on the
        # completed branch. Truncated output means it did not get to say
        # everything it had, which is not a command that reached its own end.
        outcome = OUTCOME_OUTPUT_LIMIT
    if outcome == OUTCOME_COMPLETED and child_peak >= ceiling:
        outcome = OUTCOME_MEMORY_EXHAUSTED
    if outcome == OUTCOME_COMPLETED and returncode < 0:
        # Killed by a signal nobody here sent.
        if -returncode in (signal.SIGKILL, signal.SIGSEGV, signal.SIGBUS) \
                and peak_mem >= ceiling // 2:
            outcome = OUTCOME_MEMORY_EXHAUSTED
        else:
            outcome = OUTCOME_UNCLASSIFIED
    if outcome not in ALL_OUTCOMES:
        outcome = OUTCOME_UNCLASSIFIED

    return BoundedResult(
        outcome=outcome, returncode=returncode,
        stdout=out_pump.text(), stderr=err_pump.text(),
        peak_memory_bytes=peak_mem, peak_processes=peak_procs,
        output_bytes=out_pump.total + err_pump.total,
        elapsed_seconds=time.time() - started,
        truncated=out_pump.overflowed or err_pump.overflowed,
        survivors=survivors)
