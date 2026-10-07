"""A test gate passes only when a test ran.

pytest exits 0 when every test was skipped, and `go test` exits 0 for a
package with no test files and for a -run pattern that matches nothing. A
gate that trusted the exit code alone would report those as passes.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "production-readiness.py"
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


@pytest.mark.parametrize("output", [
    ".                                                                        [100%]\n1 passed in 0.00s",
    ".s                                                                       [100%]\n1 passed, 1 skipped in 0.00s",
    "Coverage LCOV written to file /tmp/reports/python.lcov\n1432 passed, 3 skipped, 2 warnings in 61.07s (0:01:01)",
    "======================== 12 passed in 3.41s ========================",
])
def test_pytest_output_with_a_passed_test_is_a_pass(gates_module, output):
    assert gates_module._ran_no_test(PYTEST, output) == ""


@pytest.mark.parametrize("output, words", [
    ("s                                                                        [100%]\n1 skipped in 0.00s", "1 skipped in 0.00s"),
    ("ss\n2 skipped, 1 warning in 0.10s", "every test was skipped"),
    ("===================== 4 skipped, 1 xfailed in 0.20s =====================", "passed no test"),
    ("", "no summary line"),
    ("collected 3 items", "no summary line"),
])
def test_pytest_output_with_no_passed_test_is_not_a_pass(gates_module, output, words):
    assert words in gates_module._ran_no_test(PYTEST, output)


def test_a_count_inside_the_test_output_is_not_the_summary(gates_module):
    output = "tests/cli/test_x.py prints: 3 passed in 0.5s earlier\n2 skipped in 0.10s"
    assert "every test was skipped" in gates_module._ran_no_test(PYTEST, output)


@pytest.mark.parametrize("output", [
    "ok  \texample.invalid/proxy\t279.153s",
    "ok  \texample.invalid/tui\t0.142s\tcoverage: 51.7% of statements\n?   \texample.invalid/tui/gen\t[no test files]",
])
def test_go_output_with_a_package_that_ran_tests_is_a_pass(gates_module, output):
    assert gates_module._ran_no_test(GO_TEST, output) == ""


@pytest.mark.parametrize("output", [
    "?   \texample.invalid/tui/gen\t[no test files]",
    "ok  \texample.invalid/proxy\t0.169s [no tests to run]\n?   \texample.invalid/proxy/gen\t[no test files]",
    "",
])
def test_go_output_with_no_test_run_is_not_a_pass(gates_module, output):
    assert "go test ran no test" in gates_module._ran_no_test(GO_TEST, output)


@pytest.mark.parametrize("output, packages", [
    ("ok  \texample.invalid/tui\t(cached)", "1 package(s), example.invalid/tui"),
    ("ok  \texample.invalid/tui\t(cached)\tcoverage: 51.9% of statements", "1 package(s), example.invalid/tui"),
    ("ok  \texample.invalid/a\t0.142s\nok  \texample.invalid/b\t(cached)", "1 package(s), example.invalid/b"),
])
def test_go_output_taken_from_the_test_cache_is_not_a_pass(gates_module, output, packages):
    reason = gates_module._ran_no_test(GO_TEST, output)
    assert f"go test ran no test for {packages}" in reason
    assert reason.endswith("Fix: run go test with -count=1")


def test_a_test_that_prints_the_word_cached_is_still_a_pass(gates_module):
    output = "--- PASS: TestReadsThe(cached)Value (0.00s)\nok  \texample.invalid/proxy\t1.2s"
    assert gates_module._ran_no_test(GO_TEST, output) == ""


@pytest.mark.parametrize("coverage", [False, True])
def test_the_go_test_gates_never_take_a_result_from_the_test_cache(gates_module, monkeypatch, tmp_path, coverage):
    if coverage:
        monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, str(tmp_path))
    else:
        monkeypatch.delenv(gates_module.COVERAGE_DIR_ENV, raising=False)
    gates = gates_module._with_coverage(gates_module._gates(("tests/cli",)))
    for name in ("go-proxy-test", "go-tui-test"):
        assert "-count=1" in gates[name].command, name


def test_a_command_that_is_not_a_test_run_is_left_alone(gates_module):
    assert gates_module._ran_no_test(("go", "vet", "./..."), "") == ""
    assert gates_module._ran_no_test((sys.executable, "-m", "ruff", "check", "."), "") == ""


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


def test_a_gate_whose_tests_ran_passes(gates_module, tmp_path):
    (tmp_path / "test_one.py").write_text("def test_it_runs():\n    assert True\n", encoding="utf-8")
    gate = gates_module.Gate("python-tests", (sys.executable, "-m", "pytest", str(tmp_path), "--no-header", "-q",
                                              "-p", "no:cacheprovider"), cwd=tmp_path)
    result = gates_module._run_gate(gate, force_required=False)
    assert (result.status, result.reason) == ("passed", "")


def test_a_failing_gate_keeps_its_exit_code_as_the_reason(gates_module, tmp_path):
    (tmp_path / "test_one.py").write_text("def test_it_fails():\n    assert False\n", encoding="utf-8")
    gate = gates_module.Gate("python-tests", (sys.executable, "-m", "pytest", str(tmp_path), "--no-header", "-q",
                                              "-p", "no:cacheprovider"), cwd=tmp_path)
    result = gates_module._run_gate(gate, force_required=False)
    assert (result.status, result.reason) == ("failed", "exit code 1")
