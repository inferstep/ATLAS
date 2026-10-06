"""`make verify` picks the gates that cover a change and prints what failed.

The selection is the part that can go wrong silently: a gate that is not
picked reports nothing. These tests pin which files pick which gates, and
that every gate it can pick exists and has a fix message.
"""
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "verify.py"
ALWAYS = ["test-integrity", "dockerfile-sources"]


@pytest.fixture(scope="module")
def verify():
    spec = importlib.util.spec_from_file_location("atlas_verify", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


def test_a_document_change_runs_only_the_checks_of_the_whole_tree(verify):
    assert verify.select(["README.md"], full=False) == (ALWAYS, [], False)


def test_a_proxy_change_runs_the_go_gates_and_leaves_the_slow_suite_to_full(verify):
    gates, suites, lens = verify.select(["proxy/agent.go"], full=False)
    assert gates == ALWAYS + ["go-proxy-vet", "go-proxy-staticcheck"]
    assert suites == ["tests/replay"]
    assert not lens
    gates, suites, _ = verify.select(["proxy/agent.go"], full=True)
    assert "go-proxy-test" in gates
    assert suites == ["tests/replay", "tests/e2e"]


def test_a_file_that_sends_to_the_agent_runs_the_test_that_lists_the_senders(verify):
    contract = "tests/contracts/test_api_version_contract.py"
    assert contract in verify.select(["tests/replay/stage.py"], full=False)[1]
    assert contract not in verify.select(["tests/replay/recording.py"], full=False)[1]
    assert verify.select(["atlas/cli.py", "tests/replay/stage.py"], full=False)[1].count(contract) == 0


def test_a_tui_change_runs_its_tests_too(verify):
    gates, _, _ = verify.select(["tui/chat.go"], full=False)
    assert gates == ALWAYS + ["go-tui-vet", "go-tui-staticcheck", "go-tui-test"]


def test_a_python_change_runs_the_python_gates_and_the_suites_of_its_folder(verify):
    gates, suites, lens = verify.select(["v3-service/symbols.py"], full=False)
    assert gates == ALWAYS + ["python-compile", "min-python", "ruff"]
    assert suites == ["tests/v3-service", "tests/v3"]
    assert not lens
    assert verify.select(["atlas/commands/doctor.py"], full=False)[1] == ["tests/cli", "tests/contracts"]


def test_a_slow_suite_waits_for_full(verify):
    assert verify.select(["sandbox/executor_server.py"], full=False)[1] == []
    assert verify.select(["sandbox/executor_server.py"], full=True)[1] == ["tests/infrastructure"]


def test_a_changed_test_file_is_run_itself_once(verify):
    path = "tests/v3-service/test_structural_edit.py"
    assert verify.select([path], full=False)[1] == [path]
    # Its suite already runs it when the code it covers changed too.
    assert verify.select(["v3-service/symbols.py", path], full=False)[1] == ["tests/v3-service", "tests/v3"]


def test_the_lens_suite_runs_in_its_own_process(verify):
    assert verify.select(["geometric-lens/main.py"], full=False)[2] is True


@pytest.mark.parametrize("path, gate", [
    ("scripts/install.sh", "shellcheck"),
    (".github/workflows/test.yml", "workflow-yaml"),
    ("docker-compose.rocm.yml", "compose-rocm"),
])
def test_other_file_types_pick_their_gate(verify, path, gate):
    assert gate in verify.select([path], full=False)[0]


def test_every_gate_it_can_pick_exists_and_has_a_fix(verify):
    known = set(verify.load_gates_module()._gates(("tests/contracts",)))
    picked = {name for _, names in verify.GATES_BY_PATH + verify.SLOW_GATES_BY_PATH for name in names}
    assert picked <= known, sorted(picked - known)
    for name in picked | {"python-tests", "python-tests-lens", "go-proxy-changed-tests", "code-health"}:
        assert verify.FIXES.get(name, "").strip(), name


def test_changed_go_test_files_give_a_run_pattern(verify):
    pattern = verify.changed_go_tests(["proxy/context_test.go", "proxy/agent.go", "tui/chat_test.go"])
    assert pattern.startswith("^(Test")
    assert pattern.endswith(")$")
    assert verify.changed_go_tests(["proxy/agent.go"]) == ""


def test_a_short_output_is_shown_whole_and_a_long_one_by_its_failing_lines(verify):
    assert verify.failing_lines("one\ntwo") == ["one", "two"]
    long = "\n".join(["ok  line"] * 100 + ["--- FAIL: TestX (0.00s)", "agent_test.go:12: got 1, want 2"])
    assert verify.failing_lines(long) == ["--- FAIL: TestX (0.00s)", "agent_test.go:12: got 1, want 2"]
    assert len(verify.failing_lines("\n".join(f"Error {i}" for i in range(500)))) == 40


def test_the_base_reaches_git_as_a_revision_and_never_as_an_option(verify, monkeypatch):
    calls = []

    def record(argv, **_):
        calls.append(list(argv))
        return subprocess.CompletedProcess(argv, 0, stdout="0000000\n", stderr="")

    monkeypatch.setattr(verify.subprocess, "run", record)
    verify.changed_files("--not-a-branch")
    merge_base = calls[0]
    assert merge_base[merge_base.index("--not-a-branch") - 1] == "--end-of-options"


def test_a_base_that_reads_as_an_option_is_refused():
    done = subprocess.run([sys.executable, str(SCRIPT), "--base=--not-a-branch"], capture_output=True, text=True, check=False)
    assert done.returncode == 2
    assert "Not a valid object name" in done.stderr
    assert "fix:" in done.stderr


def test_a_base_it_cannot_read_is_an_error_with_a_fix(verify):
    done = subprocess.run([sys.executable, str(SCRIPT), "--base", "no-such-branch"], capture_output=True, text=True, check=False)
    assert done.returncode == 2
    assert "fix:" in done.stderr
