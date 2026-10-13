"""Commands for the tests of the sandbox's limits. Each one ends by itself.

A test of a guard starts the thing the guard must stop. If that thing has no
end of its own, the test is only safe while the guard works, and the day the
guard has a fault is the day the test takes what the host will give. So each
command here stops at a fixed size above the limit under test and ends with
OWN_LIMIT as its status. A guard that works ends the command long before. A
guard that does not gives a test that fails with that as its reason.
"""
from __future__ import annotations

MiB = 1024 * 1024
# The status of a command that reached its own limit. No guard ends a command
# with it: a command the sandbox stops is killed by a signal.
OWN_LIMIT = 91
# How far above the limit under test a command goes before it stops by
# itself. Far enough that a guard that samples has time to act; near enough
# that a host is not harmed when the guard does not act.
TIMES_THE_LIMIT = 4
REACHED_OWN_LIMIT = ("the command reached its own limit and ended by itself (status %d), so the guard under test did "
                     "not stop it" % OWN_LIMIT)


def allocator(block_mib: int, ceiling_bytes: int, pause: float = 0.0, each_block: str = "") -> str:
    """A shell command that takes memory block by block, up to TIMES_THE_LIMIT times the ceiling.

    `pause` is a sleep after each block, in seconds. `each_block` is one more
    Python statement to run after each block.
    """
    blocks = TIMES_THE_LIMIT * ceiling_bytes // (block_mib * MiB)
    body = f" a.append(bytearray({block_mib}<<20))"
    if pause:
        body += f"\n time.sleep({pause})"
    if each_block:
        body += f"\n {each_block}"
    return f'python3 -c "import sys, time\na=[]\nfor _ in range({blocks}):\n{body}\nsys.exit({OWN_LIMIT})"'


def with_an_address_space_limit(command: str, ceiling_bytes: int) -> str:
    """A shell command that runs `command` under a limit of its own, for a program whose text must stay as it is.

    The limit is on the address space (`ulimit -v`), TIMES_THE_LIMIT times
    the ceiling. Linux holds a process to it; a program that reaches it ends,
    and the shell then ends with OWN_LIMIT.
    """
    kib = TIMES_THE_LIMIT * ceiling_bytes // 1024
    return f"ulimit -v {kib}; {command}; exit {OWN_LIMIT}"


def flood(line: str, output_limit_bytes: int) -> str:
    """A shell command that writes the same line again and again, up to TIMES_THE_LIMIT times the output limit."""
    return f"yes {line} | head -c {TIMES_THE_LIMIT * output_limit_bytes}"
