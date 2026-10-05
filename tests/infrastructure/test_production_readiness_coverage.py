"""ATLAS_COVERAGE_DIR makes the test gates write coverage reports.

The switch has to change the test gates and nothing else, and has to leave
every gate alone when it is not set: the same script is the local quality
gate and the CI test step.
"""
import importlib.util
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "production-readiness.py"
TEST_GATES = ("go-proxy-test", "go-tui-test", "python-tests", "python-tests-lens")


@pytest.fixture(scope="module")
def gates_module():
    spec = importlib.util.spec_from_file_location("production_readiness", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # the dataclasses resolve their annotations through it
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


def test_without_the_variable_no_gate_changes(gates_module, monkeypatch):
    monkeypatch.delenv(gates_module.COVERAGE_DIR_ENV, raising=False)
    plain = gates_module._gates(("tests/cli",))
    assert gates_module._with_coverage(dict(plain)) == plain


def test_an_empty_value_is_not_a_directory(gates_module, monkeypatch):
    monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, "  ")
    plain = gates_module._gates(("tests/cli",))
    assert gates_module._with_coverage(dict(plain)) == plain


def test_the_test_gates_write_reports_into_the_directory(gates_module, monkeypatch, tmp_path):
    out = tmp_path / "reports"
    monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, str(out))
    plain = gates_module._gates(("tests/cli",))
    gates = gates_module._with_coverage(dict(plain))

    assert out.is_dir()
    for name in ("go-proxy-test", "go-tui-test"):
        command = gates[name].command
        profile = out.resolve() / f"{name[:-len('-test')]}.out"
        assert f"-coverprofile={profile}" in command
        assert "-covermode=atomic" in command
        # The package pattern stays last, after every flag.
        assert command[-1] == "./..."
        assert [a for a in command if not a.startswith("-cover")] == list(plain[name].command)
    for name, label in (("python-tests", "python"), ("python-tests-lens", "python-lens")):
        gate = gates[name]
        assert gate.command[:len(plain[name].command)] == plain[name].command
        assert "--cov" in gate.command
        assert f"--cov-report=lcov:{out.resolve() / (label + '.lcov')}" in gate.command
        assert gate.env["COVERAGE_FILE"] == str(out.resolve() / f".coverage.{label}")


def test_only_the_test_gates_change(gates_module, monkeypatch, tmp_path):
    monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, str(tmp_path))
    plain = gates_module._gates(("tests/cli",))
    gates = gates_module._with_coverage(dict(plain))
    assert set(gates) == set(plain)
    for name in set(plain) - set(TEST_GATES):
        assert gates[name] == plain[name], name
