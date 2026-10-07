"""One skip for the tests that need the /proc file system.

The sandbox finds the processes of a command there: the process group in
/proc/<pid>/stat, and a command's orphans by a token in /proc/<pid>/environ.
Some tests count processes the same way. On a system without /proc these
tests cannot say anything, so they skip with that cause as the reason. Where
/proc is there the skip does nothing and the tests run.
"""
import os

import pytest

NEEDS_PROC_REASON = ("the sandbox finds a command's processes in /proc, which this system does not have "
                     "(Linux has it)")


def proc_is_there() -> bool:
    return os.path.isdir("/proc/self")


needs_proc = pytest.mark.skipif(not proc_is_there(), reason=NEEDS_PROC_REASON)
