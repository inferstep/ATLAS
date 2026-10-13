"""One skip for the tests that need the /proc file system.

The sandbox finds the processes of a command there: the process group in
/proc/<pid>/stat, and a command's orphans by a token in /proc/<pid>/environ.
Some tests count processes the same way. On a system without /proc these
tests cannot say anything, so they skip with that cause as the reason. Where
/proc is there the skip does nothing and the tests run.
"""
import contextlib
import os

import pytest

NEEDS_PROC_REASON = ("the sandbox finds a command's processes in /proc, which this system does not have "
                     "(Linux has it)")


def proc_is_there() -> bool:
    return os.path.isdir("/proc/self")


needs_proc = pytest.mark.skipif(not proc_is_there(), reason=NEEDS_PROC_REASON)

CANNOT_LOOK = ("cannot look: this process ({pid}) is alive and {root} does not show it, so a count of processes there "
               "would say 0 whatever runs. Fix: run this test where {root} shows the processes of the system, or put "
               "`needs_proc` on the test.")


class CannotLook(AssertionError):
    """The place where processes are counted does not show a process that is certainly alive."""


def command_lines(root: str = "/proc") -> dict[int, bytes]:
    """The command line of every process that `root` shows, by process number.

    It looks for this very process too, by the same route. "No such process"
    is an answer only where a process that is alive can be seen; where this
    one cannot, the count fails and does not say 0.
    """
    seen: dict[int, bytes] = {}
    for entry in os.listdir(root) if os.path.isdir(root) else []:
        if entry.isdigit():
            # A process that ends between the listing and the read is the ordinary race of this place.
            with contextlib.suppress(OSError):
                with open(os.path.join(root, entry, "cmdline"), "rb") as handle:
                    seen[int(entry)] = handle.read()
    if not seen.get(os.getpid()):
        raise CannotLook(CANNOT_LOOK.format(pid=os.getpid(), root=root))
    return seen


def sleeping(seconds: int, root: str = "/proc") -> list[int]:
    """The numbers of the processes that run exactly `sleep <seconds>`: the marker these tests leave running."""
    want = ("sleep\x00%d\x00" % seconds).encode()
    return sorted(pid for pid, command in command_lines(root).items() if command == want)
