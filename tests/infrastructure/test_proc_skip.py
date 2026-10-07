"""The tests that read /proc skip where there is none, and no other test does."""
import ast
import os
from pathlib import Path

import pytest

from tests.infrastructure import proc_files

HERE = Path(__file__).parent
READ_PROC = {
    "test_http_cancellation.py": {
        "test_a_caller_that_goes_away_stops_the_command", "test_repeated_cancellation_is_idempotent",
        "test_a_healthy_neighbour_is_unaffected", "test_no_late_evidence_and_no_descendants_survive",
        "test_shutdown_drains_within_the_bound"},
    "test_execution_resource_contract.py": {
        "test_a_gradual_allocator_is_stopped_and_named", "test_the_runaway_that_took_the_host_down",
        "test_a_fork_tree_of_allocators_is_stopped", "test_process_limit"},
}


def marked(path: Path) -> set[str]:
    """The test functions of a file that carry the skip."""
    tree = ast.parse(path.read_text(encoding="utf-8"))
    return {node.name for node in ast.walk(tree) if isinstance(node, ast.FunctionDef)
            and any(isinstance(d, ast.Name) and d.id == "needs_proc" for d in node.decorator_list)}


def test_the_skip_holds_exactly_where_proc_is_missing():
    assert proc_files.proc_is_there() == os.path.isdir("/proc/self")
    assert proc_files.needs_proc.args == (not proc_files.proc_is_there(),)


def test_the_reason_is_the_cause_and_not_the_name_of_a_system_that_fails():
    reason = proc_files.needs_proc.kwargs["reason"]
    assert reason == proc_files.NEEDS_PROC_REASON
    assert "finds a command's processes in /proc" in reason
    assert "mac" not in reason.lower()
    assert "darwin" not in reason.lower()


@pytest.mark.parametrize("name", sorted(READ_PROC))
def test_the_tests_that_read_proc_carry_the_skip_and_no_other_test_does(name):
    assert marked(HERE / name) == READ_PROC[name]
