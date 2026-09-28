"""Syntax-passing replacements must preserve observed import viability.

The pipeline tests use its real orchestration and deterministic service
doubles. These are development regressions, not acceptance evidence.
"""

import sys
from pathlib import Path
from types import SimpleNamespace

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters
import pipeline as P
import scoring
from test_mode_semantics import _service
from runtime_verification import IMPORT_COMPLETE, PythonImportComparison


BASELINE = "value = 1\n"
BROKEN = "value = missing_name\n"
GOOD = "value = 2\n"


class RuntimeSandbox:
    calls = []
    outcomes = {}

    def __init__(self, project_files=None):
        pass

    def syntax_check(self, code, language, filename=""):
        compile(code, filename or "candidate.py", "exec")
        return True, "", ""

    def __call__(self, code, **kw):
        return True, "", ""

    def run_command(self, command, files=None, cwd="/workspace", timeout=60):
        code = next(iter(files.values()))
        type(self).calls.append((code, command, files, cwd, timeout))
        error = type(self).outcomes.get(code, "")
        return not error, "" if error else IMPORT_COMPLETE + "\n", error, {"exit_code": 1 if error else 0, "elapsed_ms": 2,
                                       "outcome": "completed"}


def make_service(monkeypatch, candidates):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    service, _ = _service(monkeypatch, task_type="interactive", code=BROKEN)
    monkeypatch.setattr(RuntimeSandbox, "calls", [])
    monkeypatch.setattr(RuntimeSandbox, "outcomes", {BROKEN: "NameError: missing_name"})
    monkeypatch.setattr(adapters, "SandboxAdapter", RuntimeSandbox)
    monkeypatch.setattr(scoring, "score_candidate_combined", lambda code: dict(scoring.NEUTRAL_COMBINED))
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (1.0 if code == BROKEN else 4.0, 0.1, False))
    service.plan_search = SimpleNamespace(generate=lambda *a, **kw:
        SimpleNamespace(candidates=candidates, total_tokens=0))
    return service


def test_pipeline_does_not_select_a_runtime_regression(monkeypatch):
    service = make_service(monkeypatch, [BROKEN, GOOD, GOOD])
    result = service.run("build a web application", file_path="app.py", baseline_code=BASELINE)
    assert result["passed"] is True
    assert result["code"] == GOOD, "Lens preference must not promote an observed import failure"
    assert any(e["stage"] == "runtime_compare" for e in result["events"])
    assert result["evidence_record"]["closure_eligible"] is False


def test_pipeline_returns_no_replacement_when_every_candidate_regresses(monkeypatch):
    service = make_service(monkeypatch, [BROKEN, BROKEN, BROKEN])
    result = service.run("build a web application", file_path="app.py", baseline_code=BASELINE)
    assert not result["passed"]
    assert not result["code"], "the existing proxy fallback must retain its submitted baseline"


def test_pipeline_does_not_call_environment_failure_a_regression(monkeypatch):
    service = make_service(monkeypatch, [BROKEN, GOOD, GOOD])
    RuntimeSandbox.outcomes[BASELINE] = "ModuleNotFoundError: an_uninstalled_framework"
    result = service.run("build a web application", file_path="app.py", baseline_code=BASELINE)
    # The old syntax-level decision remains available, explicitly without a
    # comparable runtime observation. This does NOT establish runnable code.
    assert result["passed"]
    assert result["evidence_record"]["closure_eligible"] is False
    runtime = [e for e in result["verification_evidence"] if e["verifier"] == "python_import_comparison"]
    assert runtime and runtime[0]["status"] == "unavailable"


@pytest.fixture
def comparison(monkeypatch):
    monkeypatch.setattr(RuntimeSandbox, "calls", [])
    monkeypatch.setattr(RuntimeSandbox, "outcomes", {BROKEN: "NameError: missing_name"})
    return PythonImportComparison(RuntimeSandbox(), BASELINE, "/workspace/pkg/app.py", "/workspace")


def test_comparison_checks_exact_overlay_and_caches_only_this_invocation(comparison):
    assert not comparison.check(BROKEN)[0]
    assert comparison.check(GOOD)[0]
    assert not comparison.check(BROKEN)[0]
    assert [c[0] for c in RuntimeSandbox.calls] == [BASELINE, BROKEN, GOOD]
    assert all(c[2] == {"pkg/app.py": c[0]} and c[3] == "/workspace" for c in RuntimeSandbox.calls)
    assert all(1 <= c[4] <= 15 for c in RuntimeSandbox.calls)
    assert "pkg.app" in RuntimeSandbox.calls[0][1]
    assert BROKEN not in RuntimeSandbox.calls[1][1], "source is data, never shell interpolation"
    other = PythonImportComparison(RuntimeSandbox(), BASELINE, "pkg/app.py", "/workspace")
    other.check(GOOD)
    assert len(RuntimeSandbox.calls) == 5


def test_import_success_is_not_a_fulfillment_claim(comparison):
    admitted, diagnostic, evidence = comparison.check(GOOD)
    assert admitted and not diagnostic
    assert evidence["status"] == "passed"
    assert evidence["environment"] == "existing_sandbox_snapshot"
    assert evidence["exit_code"] == 0 and evidence["duration_ms"] == 2
    assert evidence["candidate_hash"] != evidence["baseline_hash"]
    assert "closure_eligible" not in evidence


@pytest.mark.parametrize("outcome", ["timed_out", "cancelled", "memory_exhausted", "spawn_failed"])
def test_interrupted_candidate_is_not_promoted_as_success(comparison, monkeypatch, outcome):
    comparison.check(BASELINE)
    monkeypatch.setattr(comparison.sandbox, "run_command", lambda *a, **kw:
        (True, "", "stopped", {"exit_code": 0, "outcome": outcome}))
    admitted, _, ev = comparison.check(GOOD)
    assert not admitted and ev["status"] == "unavailable"


def test_no_budget_means_no_probe_and_no_runtime_claim(comparison):
    comparison.remaining_ms = lambda: 10_000
    admitted, _, ev = comparison.check(GOOD)
    assert admitted and ev["status"] == "unavailable"
    assert not RuntimeSandbox.calls


def test_budget_lost_after_baseline_does_not_admit_unchecked_replacement(comparison):
    comparison.check(BASELINE)
    comparison.remaining_ms = lambda: 10_000
    admitted, _, ev = comparison.check(GOOD)
    assert not admitted and ev["status"] == "unavailable"
    assert len(RuntimeSandbox.calls) == 1


def test_cancellation_propagates_without_execution(comparison):
    def cancelled():
        raise adapters.Cancelled("cancelled")
    comparison.check_cancel = cancelled
    with pytest.raises(adapters.Cancelled):
        comparison.check(GOOD)
    assert not RuntimeSandbox.calls


@pytest.mark.parametrize("path", ["/tmp/app.py", "../app.py", "app.js", "bad-name.py", "__init__.py"])
def test_unsupported_paths_are_explicitly_unavailable(monkeypatch, path):
    monkeypatch.setattr(RuntimeSandbox, "calls", [])
    check = PythonImportComparison(RuntimeSandbox(), BASELINE, path, "/workspace")
    admitted, _, evidence = check.check(GOOD)
    assert admitted and evidence["status"] == "unavailable"
    assert not RuntimeSandbox.calls


def test_candidate_runner_unavailable_is_not_a_pass(comparison, monkeypatch):
    comparison.check(BASELINE)
    monkeypatch.setattr(comparison.sandbox, "run_command", lambda *a, **kw:
        (False, "", "connection refused", {"exit_code": None}))
    admitted, diagnostic, evidence = comparison.check(GOOD)
    assert not admitted and evidence["status"] == "unavailable"
    assert "connection refused" in diagnostic


def test_nested_working_directory_uses_workspace_relative_overlay(monkeypatch):
    monkeypatch.setattr(RuntimeSandbox, "calls", [])
    check = PythonImportComparison(RuntimeSandbox(), BASELINE, "/workspace/project/pkg/app.py", "/workspace/project")
    assert check.check(GOOD)[0]
    assert all(call[2] == {"project/pkg/app.py": call[0]} for call in RuntimeSandbox.calls)
    assert all(call[3] == "/workspace/project" for call in RuntimeSandbox.calls)
    assert "pkg.app" in check.command


def test_zero_exit_without_completed_import_cannot_promote(comparison, monkeypatch):
    comparison.check(BASELINE)
    monkeypatch.setattr(comparison.sandbox, "run_command", lambda *a, **kw:
        (True, "", "", {"exit_code": 0, "outcome": "completed"}))
    admitted, _, evidence = comparison.check(GOOD)
    assert not admitted and evidence["status"] == "unavailable"


@pytest.mark.parametrize("cause", ["transport", "budget"])
def test_unavailable_candidate_diagnostic_does_not_claim_failed_import(comparison, monkeypatch, cause):
    comparison.check(BASELINE)
    if cause == "transport":
        monkeypatch.setattr(comparison.sandbox, "run_command", lambda *a, **kw:
            (False, "", "connection refused", {"exit_code": None}))
    else:
        comparison.remaining_ms = lambda: 10_000
    admitted, diagnostic, evidence = comparison.check(GOOD)
    assert not admitted and evidence["status"] == "unavailable"
    assert "could not be observed" in diagnostic
    assert "candidate does not" not in diagnostic
    assert evidence["reason"] == diagnostic


def test_failed_candidate_diagnostic_reports_the_observed_failure(comparison):
    admitted, diagnostic, evidence = comparison.check(BROKEN)
    assert not admitted and evidence["status"] == "failed"
    assert "candidate does not import" in diagnostic
    assert "NameError: missing_name" in diagnostic


@pytest.mark.parametrize("exit_code", [0, 7])
def test_executor_completed_outcome_is_understood_by_import_comparison(comparison, monkeypatch, tmp_path, exit_code):
    from test_sandbox_syntax import _load_sandbox_module

    executor = _load_sandbox_module()
    # Only controlled fixture code executes on the test host. Candidate code
    # continues to execute exclusively through sandbox snapshots in product.
    actual = executor._run_cmd(
        [sys.executable, "-c", f"print({IMPORT_COMPLETE!r}); raise SystemExit({exit_code})"],
        timeout=3, cwd=tmp_path)
    assert actual["outcome"] == executor.OUTCOME_COMPLETED
    comparison.check(BASELINE)
    monkeypatch.setattr(comparison.sandbox, "run_command", lambda *a, **kw:
        (actual["success"], actual["stdout"], actual["stderr"],
         {"exit_code": actual["returncode"], "outcome": executor.OUTCOME_COMPLETED}))
    admitted, _, evidence = comparison.check(GOOD)
    assert admitted is (exit_code == 0)
    assert evidence["status"] == ("passed" if exit_code == 0 else "failed")


def test_pipeline_reports_why_a_project_path_disables_import_comparison(monkeypatch):
    service = make_service(monkeypatch, [BROKEN, GOOD, GOOD])
    result = service.run("build a web application", file_path="my-app/app.py", baseline_code=BASELINE)
    events = [event for event in result["events"] if event["stage"] == "runtime_compare"]
    assert events and all("not an importable Python module path" in event["data"]["reason"] for event in events)
    assert not RuntimeSandbox.calls


@pytest.mark.parametrize("remaining_ms, execution_timeout", [
    (25_000, 15), (24_999, 14), (11_000, 1), (10_999, None),
])
def test_late_budget_reduces_timeout_before_declining_observation(comparison, remaining_ms, execution_timeout):
    comparison.check(BASELINE)
    comparison.remaining_ms = lambda: remaining_ms
    before = len(RuntimeSandbox.calls)
    admitted, _, evidence = comparison.check(GOOD)
    if execution_timeout is None:
        assert not admitted and evidence["status"] == "unavailable"
        assert len(RuntimeSandbox.calls) == before
    else:
        assert admitted and evidence["status"] == "passed"
        assert RuntimeSandbox.calls[-1][4] == execution_timeout
