"""ATLAS_COVERAGE_DIR makes the test gates write coverage reports.

The switch has to change the test gates and nothing else, and has to leave
every gate alone when it is not set: the same script is the local quality
gate and the CI test step.
"""
import importlib.util
import json
import sys
from pathlib import Path
from xml.etree import ElementTree

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
        assert command[:3] == ("go", "test", "-json")
        assert [a for a in command if not a.startswith("-cover") and a != "-json"] == list(plain[name].command)
        assert gates[name].junit == out.resolve() / f"{name[:-len('-test')]}.junit.xml"
    for name, label in (("python-tests", "python"), ("python-tests-lens", "python-lens")):
        gate = gates[name]
        assert gate.command[:len(plain[name].command)] == plain[name].command
        assert "--cov" in gate.command
        assert f"--cov-report=lcov:{out.resolve() / (label + '.lcov')}" in gate.command
        assert f"--junitxml={out.resolve() / (label + '.junit.xml')}" in gate.command
        assert gate.command[-2:] == ("-o", "junit_family=legacy")
        assert gate.junit is None
        assert gate.env["COVERAGE_FILE"] == str(out.resolve() / f".coverage.{label}")


def test_only_the_test_gates_change(gates_module, monkeypatch, tmp_path):
    monkeypatch.setenv(gates_module.COVERAGE_DIR_ENV, str(tmp_path))
    plain = gates_module._gates(("tests/cli",))
    gates = gates_module._with_coverage(dict(plain))
    assert set(gates) == set(plain)
    for name in set(plain) - set(TEST_GATES):
        assert gates[name] == plain[name], name


def events(*rows):
    return "\n".join(json.dumps(row) for row in rows)


PKG = "example.invalid/tui"
STREAM = events(
    {"Action": "run", "Package": PKG, "Test": "TestOpens"},
    {"Action": "output", "Package": PKG, "Test": "TestOpens", "Output": "=== RUN   TestOpens\n"},
    {"Action": "pass", "Package": PKG, "Test": "TestOpens", "Elapsed": 0.02},
    {"Action": "output", "Package": PKG, "Test": "TestCloses", "Output": "=== RUN   TestCloses\n"},
    {"Action": "output", "Package": PKG, "Test": "TestCloses", "Output": "    x_test.go:7: want 1, got 2\n"},
    {"Action": "fail", "Package": PKG, "Test": "TestCloses", "Elapsed": 0.5},
    {"Action": "skip", "Package": PKG, "Test": "TestNeedsATerminal", "Elapsed": 0},
    {"Action": "output", "Package": PKG, "Output": "FAIL\n"},
    {"Action": "output", "Package": PKG, "Output": f"FAIL\t{PKG}\t1.221s\n"},
    {"Action": "fail", "Package": PKG, "Elapsed": 1.2},
)


def test_the_go_report_gives_the_text_go_test_prints_and_the_output_of_a_failed_test(gates_module):
    text, _ = gates_module._go_test_report(STREAM)
    assert text.splitlines() == ["=== RUN   TestCloses", "    x_test.go:7: want 1, got 2", "FAIL", f"FAIL\t{PKG}\t1.221s"]


def test_the_go_report_holds_each_test_with_its_result(gates_module):
    _, xml = gates_module._go_test_report(STREAM)
    suite = ElementTree.fromstring(xml).find("testsuite")
    assert suite.attrib == {"name": PKG, "tests": "3", "failures": "1", "skipped": "1"}
    cases = {case.get("name"): case for case in suite}
    assert sorted(cases) == ["TestCloses", "TestNeedsATerminal", "TestOpens"]
    assert [child.tag for child in cases["TestOpens"]] == []
    assert [child.tag for child in cases["TestNeedsATerminal"]] == ["skipped"]
    assert cases["TestCloses"].find("failure").text == "=== RUN   TestCloses\n    x_test.go:7: want 1, got 2"
    assert cases["TestCloses"].get("time") == "0.500"


def test_the_go_report_keeps_what_a_package_says_for_itself(gates_module):
    passed = events({"Action": "pass", "Package": PKG, "Test": "TestOpens", "Elapsed": 0.02},
                    {"Action": "output", "Package": PKG, "Output": f"ok  \t{PKG}\t0.142s\tcoverage: 51.7% of statements\n"})
    text, _ = gates_module._go_test_report(passed)
    assert text == f"ok  \t{PKG}\t0.142s\tcoverage: 51.7% of statements"
    assert gates_module._ran_no_test(("go", "test", "-json", "./..."), text) == ""


def test_the_go_report_keeps_a_build_error_and_a_line_that_is_no_event(gates_module):
    broken = "\n".join([
        json.dumps({"ImportPath": PKG, "Action": "build-output", "Output": "x_test.go:3:14: expected ')', found '{'\n"}),
        "panic: something go test printed outside its events",
        json.dumps({"Action": "output", "Package": PKG, "Output": f"FAIL\t{PKG} [setup failed]\n"}),
    ])
    text, xml = gates_module._go_test_report(broken)
    assert text.splitlines() == ["x_test.go:3:14: expected ')', found '{'",
                                 "panic: something go test printed outside its events", f"FAIL\t{PKG} [setup failed]"]
    assert list(ElementTree.fromstring(xml)) == []


def test_a_run_that_ran_no_test_is_still_seen_in_the_text(gates_module):
    nothing = events({"Action": "output", "Package": PKG, "Output": f"?   \t{PKG}\t[no test files]\n"},
                     {"Action": "skip", "Package": PKG, "Elapsed": 0})
    text, _ = gates_module._go_test_report(nothing)
    assert "go test ran no test" in gates_module._ran_no_test(("go", "test", "-json", "./..."), text)


def test_the_go_report_is_valid_xml_whatever_a_test_prints(gates_module):
    stream = events({"Action": "output", "Package": PKG, "Test": "TestPrints", "Output": "a\x00b\x1b[31m<&>\n"},
                    {"Action": "fail", "Package": PKG, "Test": "TestPrints", "Elapsed": 0.1})
    _, xml = gates_module._go_test_report(stream)
    assert ElementTree.fromstring(xml).find("testsuite/testcase/failure").text == "ab[31m<&>"


def test_a_gate_with_a_result_file_writes_it_and_is_judged_by_the_text(gates_module, tmp_path):
    target = tmp_path / "go-x.junit.xml"
    for code, status in ((0, "passed"), (1, "failed")):
        gate = gates_module.Gate("fake", (sys.executable, "-c", f"print({STREAM!r}); raise SystemExit({code})"),
                                 junit=target)
        result = gates_module._run_gate(gate, force_required=False)
        assert result.status == status
        assert result.output.splitlines()[-1] == f"FAIL\t{PKG}\t1.221s"
        assert ElementTree.fromstring(target.read_text(encoding="utf-8")).find("testsuite").get("tests") == "3"
        target.unlink()

