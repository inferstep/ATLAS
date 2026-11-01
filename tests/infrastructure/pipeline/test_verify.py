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

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "verify.py"
ALWAYS = ["test-integrity", "dockerfile-sources"]
PYTHON = ["python-compile", "min-python", "ruff"]
SENDERS = "tests/contracts/test_api_version_contract.py"
CHANGED_TEST = "tests/v3-service/structural_edit/test_structural_edit.py"


@pytest.fixture(scope="module")
def verify():
    spec = importlib.util.spec_from_file_location("atlas_verify", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


# Each row: the changed files and whether --full is given; then the gates it picks besides ALWAYS, the pytest
# paths it runs, and whether the lens suite runs in its own process.
@pytest.mark.parametrize("changed, full, gates, suites, lens", [
    pytest.param(["README.md"], False, [], [], False, id="a document runs only the checks of the whole tree"),
    pytest.param(["proxy/agent.go"], False, ["go-proxy-vet", "go-proxy-staticcheck"], ["tests/replay"], False,
                 id="a proxy change runs the Go gates and leaves the slow suite to --full"),
    pytest.param(["proxy/agent.go"], True, ["go-proxy-vet", "go-proxy-staticcheck", "go-proxy-test"],
                 ["tests/replay", "tests/e2e"], False,
                 id="a proxy change with --full runs the proxy tests and the slow suite"),
    pytest.param(["tui/chat.go"], False, ["go-tui-vet", "go-tui-staticcheck", "go-tui-test"], [], False,
                 id="a TUI change runs its tests too"),
    pytest.param(["v3-service/symbols.py"], False, PYTHON, ["tests/v3-service", "tests/v3"], False,
                 id="a Python change runs the Python gates and the suites of its folder"),
    pytest.param(["atlas/commands/doctor.py"], False, PYTHON, ["tests/cli", "tests/contracts"], False,
                 id="a change in atlas runs the suites of atlas"),
    pytest.param(["sandbox/executor_server.py"], False, PYTHON, [], False, id="a slow suite waits for --full"),
    pytest.param(["sandbox/executor_server.py"], True, PYTHON, ["tests/infrastructure"], False,
                 id="a slow suite runs with --full"),
    pytest.param([CHANGED_TEST], False, PYTHON, [CHANGED_TEST], False, id="a changed test file is run itself"),
    pytest.param(["v3-service/symbols.py", CHANGED_TEST], False, PYTHON, ["tests/v3-service", "tests/v3"], False,
                 id="a changed test file is not run again when its suite runs"),
    pytest.param(["tests/replay/stage.py"], False, PYTHON, [SENDERS], False,
                 id="a file that sends to the agent runs the test that lists the senders"),
    pytest.param(["tests/replay/recording.py"], False, PYTHON, [], False,
                 id="a file that does not send to the agent does not run that test"),
    pytest.param(["atlas/cli.py", "tests/replay/stage.py"], False, PYTHON, ["tests/cli", "tests/contracts"], False,
                 id="the test that lists the senders is not added when its suite already runs"),
    pytest.param(["geometric-lens/main.py"], False, PYTHON, [], True,
                 id="a lens change runs the lens suite in its own process"),
    pytest.param(["scripts/install.sh"], False, ["shellcheck"], [], False, id="a shell script picks shellcheck"),
    pytest.param([".github/workflows/test.yml"], False, ["workflow-yaml"], [], False, id="a workflow picks the YAML check"),
    pytest.param(["compose/docker-compose.rocm.yml"], False,
                 ["compose", "compose-rocm", "compose-vulkan", "compose-cpu", "compose-macos"], [], False,
                 id="a compose file picks every compose check"),
])
def test_a_change_picks_the_gates_and_suites_that_cover_its_files(verify, changed, full, gates, suites, lens):
    assert verify.select(changed, full) == (ALWAYS + gates, suites, lens)


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


def test_a_dockerfile_change_asks_for_hadolint_when_it_is_not_installed(verify, monkeypatch):
    monkeypatch.setattr(verify.shutil, "which", lambda name: None)
    assert verify.hadolint_notes(["inference/Dockerfile.vulkan"]) == [
        "skip hadolint: it is not installed (https://github.com/hadolint/hadolint/releases)"]


def test_a_dockerfile_change_shows_the_findings_of_its_own_files(verify, monkeypatch):
    monkeypatch.setattr(verify.shutil, "which", lambda name: "/usr/bin/" + name)
    listed = "proxy/Dockerfile:2: DL3003 warning: Use WORKDIR\nsandbox/Dockerfile:9: DL3008 warning: Pin\nhadolint: 2 finding(s)\n"
    monkeypatch.setattr(verify.subprocess, "run", lambda *args, **kwargs: verify.subprocess.CompletedProcess(args, 0, listed, ""))
    assert verify.hadolint_notes(["proxy/Dockerfile", "docs/API.md"]) == [
        "note hadolint (reports, does not fail):", "  proxy/Dockerfile:2: DL3003 warning: Use WORKDIR"]


def test_a_change_with_no_dockerfile_does_not_run_hadolint(verify):
    assert verify.hadolint_notes(["proxy/agent.go", "scripts/check_dockerfile_sources.py"]) == []

