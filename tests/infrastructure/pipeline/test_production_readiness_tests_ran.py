"""A test gate passes only when a test ran.

pytest exits 0 when every test was skipped, and `go test` exits 0 for a
package with no test files and for a -run pattern that matches nothing. A
gate that trusted the exit code alone would report those as passes.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[3] / "scripts" / "production-readiness.py"
PYTEST = (sys.executable, "-m", "pytest", "tests/cli", "--no-header", "-q")
GO_TEST = ("go", "test", "-race", "./...")


@pytest.fixture(scope="module")
def gates_module():
    spec = importlib.util.spec_from_file_location("production_readiness", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # the dataclasses resolve their annotations through it
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


@pytest.mark.parametrize("command, output", [
    pytest.param(PYTEST, ".                                                                        [100%]\n"
                         "1 passed in 0.00s", id="pytest: 1 passed"),
    pytest.param(PYTEST, ".s                                                                       [100%]\n"
                         "1 passed, 1 skipped in 0.00s", id="pytest: 1 passed and 1 skipped"),
    pytest.param(PYTEST, "Coverage LCOV written to file /tmp/reports/python.lcov\n"
                         "1432 passed, 3 skipped, 2 warnings in 61.07s (0:01:01)",
                 id="pytest: passed, with skips, warnings and a coverage report"),
    pytest.param(PYTEST, "======================== 12 passed in 3.41s ========================",
                 id="pytest: the summary line between rules"),
    pytest.param(GO_TEST, "ok  \texample.invalid/proxy\t279.153s", id="go test: a package that ran tests"),
    pytest.param(GO_TEST, "ok  \texample.invalid/tui\t0.142s\tcoverage: 51.7% of statements\n"
                          "?   \texample.invalid/tui/gen\t[no test files]",
                 id="go test: a package that ran tests beside one with no test files"),
    pytest.param(GO_TEST, "--- PASS: TestReadsThe(cached)Value (0.00s)\n"
                          "ok  \texample.invalid/proxy\t1.2s",
                 id="go test: a test that prints the word cached"),
    pytest.param(("go", "vet", "./..."), "", id="go vet is not a test run"),
    pytest.param((sys.executable, "-m", "ruff", "check", "."), "", id="ruff is not a test run"),
])
def test_output_with_a_passed_test_or_of_a_command_that_is_not_a_test_run_is_a_pass(gates_module, command, output):
    assert gates_module._ran_no_test(command, output) == ""


@pytest.mark.parametrize("command, output, words", [
    pytest.param(PYTEST, "s                                                                        [100%]\n"
                         "1 skipped in 0.00s", "1 skipped in 0.00s", id="pytest: 1 skipped"),
    pytest.param(PYTEST, "ss\n"
                         "2 skipped, 1 warning in 0.10s", "every test was skipped", id="pytest: 2 skipped and a warning"),
    pytest.param(PYTEST, "===================== 4 skipped, 1 xfailed in 0.20s =====================", "passed no test",
                 id="pytest: skipped and xfailed"),
    pytest.param(PYTEST, "", "no summary line", id="pytest: no output"),
    pytest.param(PYTEST, "collected 3 items", "no summary line", id="pytest: no summary line"),
    pytest.param(PYTEST, "tests/cli/test_x.py prints: 3 passed in 0.5s earlier\n"
                         "2 skipped in 0.10s", "every test was skipped",
                 id="pytest: a count inside the test output is not the summary"),
    pytest.param(GO_TEST, "?   \texample.invalid/tui/gen\t[no test files]", "go test ran no test",
                 id="go test: no test files"),
    pytest.param(GO_TEST, "ok  \texample.invalid/proxy\t0.169s [no tests to run]\n"
                          "?   \texample.invalid/proxy/gen\t[no test files]", "go test ran no test",
                 id="go test: no tests to run"),
    pytest.param(GO_TEST, "", "go test ran no test", id="go test: no output"),
])
def test_output_with_no_passed_test_is_not_a_pass(gates_module, command, output, words):
    assert words in gates_module._ran_no_test(command, output)


@pytest.mark.parametrize("output, packages", [
    pytest.param("ok  \texample.invalid/tui\t(cached)", "1 package(s), example.invalid/tui", id="one package"),
    pytest.param("ok  \texample.invalid/tui\t(cached)\tcoverage: 51.9% of statements", "1 package(s), example.invalid/tui",
                 id="one package with its coverage"),
    pytest.param("ok  \texample.invalid/a\t0.142s\nok  \texample.invalid/b\t(cached)", "1 package(s), example.invalid/b",
                 id="one package of two"),
])
def test_go_output_taken_from_the_test_cache_is_not_a_pass(gates_module, output, packages):
    reason = gates_module._ran_no_test(GO_TEST, output)
    assert f"go test ran no test for {packages}" in reason
    assert reason.endswith("Fix: run go test with -count=1")


@pytest.mark.parametrize("coverage", [False, True], ids=["without coverage", "with coverage"])
def test_the_go_test_gates_never_take_a_result_from_the_test_cache(gates_module, monkeypatch, tmp_path, coverage):
    if coverage:
        monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, str(tmp_path))
    else:
        monkeypatch.delenv(gates_module.COVERAGE_DIR_ENV, raising=False)
    gates = gates_module._with_coverage(gates_module._gates(("tests/cli",)))
    for name in ("go-proxy-test", "go-tui-test"):
        assert "-count=1" in gates[name].command, name


def test_every_test_gate_is_one_the_rule_reads(gates_module):
    gates = gates_module._with_coverage(gates_module._gates(("tests/cli",)))
    for name in ("go-proxy-test", "go-tui-test", "python-tests", "python-tests-lens"):
        assert gates_module._ran_no_test(gates[name].command, ""), name


def test_a_gate_whose_tests_were_all_skipped_fails_with_the_reason(gates_module, tmp_path):
    (tmp_path / "test_only_skipped.py").write_text(
        "import pytest\n\n\ndef test_nothing_runs():\n    pytest.skip('this suite has nothing to run here')\n",
        encoding="utf-8")
    gate = gates_module.Gate("python-tests", (sys.executable, "-m", "pytest", str(tmp_path), "--no-header", "-q",
                                              "-p", "no:cacheprovider"), cwd=tmp_path)
    result = gates_module._run_gate(gate, force_required=False)
    assert result.status == "failed"
    assert "every test was skipped" in result.reason


@pytest.mark.parametrize("test_file, status, reason", [
    pytest.param("def test_it_runs():\n    assert True\n", "passed", "", id="a test that passes"),
    pytest.param("def test_it_fails():\n    assert False\n", "failed", "exit code 1", id="a test that fails keeps its exit code"),
])
def test_a_gate_whose_tests_ran_gives_their_result(gates_module, tmp_path, test_file, status, reason):
    (tmp_path / "test_one.py").write_text(test_file, encoding="utf-8")
    gate = gates_module.Gate("python-tests", (sys.executable, "-m", "pytest", str(tmp_path), "--no-header", "-q",
                                              "-p", "no:cacheprovider"), cwd=tmp_path)
    result = gates_module._run_gate(gate, force_required=False)
    assert (result.status, result.reason) == (status, reason)
