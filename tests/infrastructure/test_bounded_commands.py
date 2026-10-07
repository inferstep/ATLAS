"""The commands of the sandbox limit tests end by themselves, and none is written without an end."""
import ast
import re
import subprocess
from pathlib import Path

import pytest

from tests.infrastructure import bounded_commands as bounded

HERE = Path(__file__).parent
LIMIT_TESTS = ("test_execution_resource_contract.py", "test_http_cancellation.py")
# A loop or a writer with no end of its own, in the text of a command.
NO_END = re.compile(r"while True|while 1\b|while :|\byes \S")
MiB = bounded.MiB


def shell(command: str) -> subprocess.CompletedProcess:
    return subprocess.run(["bash", "-c", command], capture_output=True, timeout=120, check=False)


def test_an_allocator_that_nothing_stops_ends_by_itself_with_its_own_status():
    done = shell(bounded.allocator(1, 8 * MiB))
    assert done.returncode == bounded.OWN_LIMIT


def test_an_allocator_stops_at_a_fixed_multiple_of_the_ceiling():
    command = bounded.allocator(64, 384 * MiB)
    assert f"for _ in range({bounded.TIMES_THE_LIMIT * 384 // 64}):" in command
    assert "bytearray(64<<20)" in command
    assert "while" not in command
    paused = bounded.allocator(1, 8 * MiB, pause=0.01, each_block="sys.stdout.write('x')")
    assert "\n time.sleep(0.01)\n sys.stdout.write('x')\n" in paused


def test_a_flood_that_nothing_stops_ends_by_itself_at_a_fixed_multiple_of_the_limit():
    done = shell(bounded.flood("AB", 1024))
    assert done.returncode == 0
    assert len(done.stdout) == bounded.TIMES_THE_LIMIT * 1024


def test_a_command_under_its_own_address_space_limit_ends_with_the_own_status():
    command = bounded.with_an_address_space_limit("true", 384 * MiB)
    assert command == f"ulimit -v {bounded.TIMES_THE_LIMIT * 384 * 1024}; true; exit {bounded.OWN_LIMIT}"
    assert shell(command).returncode == bounded.OWN_LIMIT


def test_the_own_status_is_not_one_a_signal_or_a_shell_gives():
    # A command the sandbox stops is killed by a signal (a negative status in
    # Python, 128 + the signal number in a shell). 126 and 127 are the shell's own.
    assert 2 < bounded.OWN_LIMIT < 126


@pytest.mark.parametrize("name", LIMIT_TESTS)
def test_no_command_of_the_limit_tests_is_written_without_an_end(name):
    source = (HERE / name).read_text(encoding="utf-8")
    texts = [node.value for node in ast.walk(ast.parse(source))
             if isinstance(node, ast.Constant) and isinstance(node.value, str)]
    no_end = [text for text in texts if NO_END.search(text)]
    assert no_end == [], (
        f"{name} holds a command with no end of its own: {no_end}. A test of a guard must be harmless when the "
        "guard fails. Fix: build the command with tests/infrastructure/bounded_commands.py (allocator, flood, "
        "with_an_address_space_limit), so that it ends by itself above the limit under test.")
    # The one program whose text must stay as it is runs only under a limit of its own.
    bare = re.findall(r"(?<!with_an_address_space_limit\()\bRUNAWAY\b(?! =)", source)
    assert bare == [], (
        f"{name} runs the runaway loop without `with_an_address_space_limit(...)`. Fix: wrap it, so that the "
        "loop ends when the guard under test does not stop it.")
