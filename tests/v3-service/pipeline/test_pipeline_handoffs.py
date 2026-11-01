"""What one pipeline run hands from step to step.

V3PipelineService.run goes through its phases in a fixed order, and each
phase hands the next what it found: a budget that is spent, an error a phase
must not swallow, the candidates a veto kept, the context repair is given, a
check's verdict. Each test here makes one such hand-off happen and checks
what arrives on the other side. They drive the whole run with every service
adapter faked; the `service` fixture fails every candidate, so a run goes
through every phase unless a test changes that.
"""

from types import SimpleNamespace

import pytest

import adapters
import graph
import pipeline as v3pipeline
import scoring
from graph.extract import available as graph_extraction_available


class NoCodeLLM:
    """Answers with an unclosed think block, which extracts to no code: the
    probe fails, so the run goes on to generate candidates."""

    avg_call_ms = 0.0

    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return "<think>still thinking", 3, 1.0


class FailingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return False, "", "boom"


class PassingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return True, "ok", ""


class NoEmbed:
    def __call__(self, text):
        return []


@pytest.fixture
def service(monkeypatch):
    """A service whose adapters and stages are fakes. PlanSearch writes one
    candidate, the sandbox fails it, there are no self-test cases, PR-CoT
    repairs nothing and refinement solves nothing."""
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.delenv("ATLAS_V3_TIMEOUT", raising=False)
    monkeypatch.delenv(v3pipeline.CAPTURE_ENV, raising=False)
    monkeypatch.setattr(adapters, "LLMAdapter", NoCodeLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", FailingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", NoEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda problem: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (5.0, 0.5, False))
    monkeypatch.setattr(scoring, "score_candidate_per_step", lambda code: None)
    fake = v3pipeline.V3PipelineService()
    fake.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id: SimpleNamespace(test_cases=[], generation_tokens=0))
    fake.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=["def a():\n    pass\n"], total_tokens=0))
    fake.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id: SimpleNamespace(repairs=[], total_tokens=0))
    fake.refinement_loop = SimpleNamespace(
        run=lambda **kwargs: SimpleNamespace(solved=False, total_tokens=0, total_iterations=1, winning_code=""))
    return fake


def test_every_recorded_event_reaches_the_progress_callback(service):
    streamed = []

    def progress(stage, detail, **data):
        streamed.append(stage)

    result = service.run("sum two ints", task_id="progress", progress_callback=progress)

    assert streamed == [event["stage"] for event in result["events"]]


@pytest.mark.parametrize("client_gone, request_cancelled, error", [
    pytest.param(True, False, adapters.ClientDisconnected, id="client-gone"),
    pytest.param(False, True, adapters.Cancelled, id="request-cancelled"),
])
def test_a_gone_client_or_a_cancelled_request_stops_the_run_at_the_next_phase(
        service, client_gone, request_cancelled, error):
    """The probe fails, so the next phase boundary is the allocation."""
    stages = []

    def progress(stage, detail, **data):
        stages.append(stage)

    progress.disconnected = client_gone
    scope = adapters.CancelScope()
    if request_cancelled:
        scope.cancel()

    with pytest.raises(error):
        service.run("sum two ints", task_id="stop", progress_callback=progress, cancel_scope=scope)

    assert "phase2" not in stages


def test_a_budget_spent_before_pr_cot_returns_the_best_so_far_without_repairing(service, monkeypatch):
    """Phase 3 regenerates the self-tests (phase 0 produced none), and that
    call spends the rest of the clock."""
    clock = {"spent": False}
    monkeypatch.setattr(v3pipeline, "_remaining_budget_ms",
                        lambda start, cap_ms=None: -1.0 if clock["spent"] else None)
    generations = []

    def generate(problem, llm, task_id):
        generations.append(task_id)
        clock["spent"] = len(generations) == 2
        return SimpleNamespace(test_cases=[], generation_tokens=0)

    repairs = []

    def repair(problem, code, error, llm_call, task_id):
        repairs.append(code)
        return SimpleNamespace(repairs=[], total_tokens=0)

    service.self_test_gen = SimpleNamespace(generate=generate)
    service.pr_cot = SimpleNamespace(repair=repair)

    result = service.run("sum two ints", task_id="budget-pr-cot")

    assert len(generations) == 2
    assert repairs == []
    assert [e["detail"] for e in result["events"] if e["stage"] == "budget_exhausted"] == [
        "budget spent before PR-CoT repair"]


@pytest.mark.parametrize("stage, call", [
    pytest.param("div_sampling", "apply", id="divsampling"),
    pytest.param("pr_cot", "repair", id="pr-cot"),
    pytest.param("refinement_loop", "run", id="refinement"),
])
def test_a_lens_that_cannot_score_stops_the_run_from_any_phase(service, stage, call):
    """PlanSearch leaves two of the three slots to DivSampling, and its one
    candidate fails, so both repair strategies run."""
    def lens_down(*args, **kwargs):
        raise scoring.LensUnavailable("lens_unreachable: URLError")

    setattr(service, stage, SimpleNamespace(**{call: lens_down}))

    with pytest.raises(scoring.LensUnavailable):
        service.run("sum two ints", task_id="lens-down")


def test_a_budget_exhausted_mid_pipeline_returns_the_best_so_far(service, monkeypatch):
    """Every generation in phases 1 to 3 sits inside its phase's own except,
    so the stub raises BudgetExhausted from the sandbox: the first call of
    phase one that nothing else catches."""
    def out_of_time(code, test_input="", **_):
        raise adapters.BudgetExhausted("2s left decodes ~0 tokens at 20 tok/s")

    monkeypatch.setattr(adapters, "SandboxAdapter", lambda project_files=None: out_of_time)

    result = service.run("sum two ints", task_id="budget-mid-run")

    assert [e["detail"] for e in result["events"] if e["stage"] == "budget_exhausted"] == [
        "budget exhausted mid-pipeline (2s left decodes ~0 tokens at 20 tok/s)"]
    assert result["code"] == ""


@pytest.mark.skipif(not graph_extraction_available(), reason="tree-sitter Python grammar not installed")
def test_the_call_graph_veto_takes_a_candidate_out_of_selection(service, monkeypatch):
    """Both candidates execute. The first calls a project function it never
    imports, which the structural veto credits and the call graph does not."""
    project = {"utils.py": "def helper():\n    return 1\n"}
    unimported = "def solve():\n    return helper()\n"
    imported = "from utils import helper\n\ndef solve():\n    return helper()\n"
    monkeypatch.setattr(adapters, "SandboxAdapter", PassingSandbox)
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=[unimported, imported], total_tokens=0))

    result = service.run("write solve()", task_id="call-graph", file_path="main.py", files=project)

    assert [e["data"]["index"] for e in result["events"] if e["stage"] == "call_graph_veto"] == [0]
    assert [c["index"] for c in result["evidence_selection"]["candidates"]] == [1]
    assert result["code"] == imported


@pytest.mark.skipif(not graph_extraction_available(), reason="tree-sitter Python grammar not installed")
def test_the_call_chain_of_the_failing_function_reaches_both_repair_strategies(service, monkeypatch):
    project = {"svc.py": ("def read():\n    return []\n\n"
                          "def load(data):\n    return clean(data)\n\n"
                          "def clean(data):\n    return data\n")}
    traceback = ("Traceback (most recent call last):\n"
                 '  File "svc.py", line 5, in load\n'
                 "    return clean(data)\n"
                 "NameError: name 'clean' is not defined")
    monkeypatch.setattr(adapters, "SandboxAdapter",
                        lambda project_files=None: lambda code, test_input="", **_: (False, "", traceback))
    pr_cot_errors = []
    refinement_errors = []

    def repair(problem, code, error, llm_call, task_id):
        pr_cot_errors.append(error)
        return SimpleNamespace(repairs=[], total_tokens=0)

    def refine(**kwargs):
        refinement_errors.extend(c.error_output for c in kwargs["failing_candidates"])
        return SimpleNamespace(solved=False, total_tokens=0, total_iterations=1, winning_code="")

    service.pr_cot = SimpleNamespace(repair=repair)
    service.refinement_loop = SimpleNamespace(run=refine)

    service.run("fix load()", task_id="call-chain", file_path="svc.py", files=project)

    enriched = traceback + "\n\n" + graph.repair_context(project, "load", transitive=True)
    assert pr_cot_errors == [enriched]
    assert refinement_errors == [enriched]


@pytest.mark.parametrize("file_path, build_command, check, verdict", [
    pytest.param("solve.py", "make test", "verify_build_command",
                 (False, "", "make: *** [test] Error 1", {"verifier": "build_command", "status": "failed"}),
                 id="build-command-fails"),
    pytest.param("app.js", "", "smoke_compile_check",
                 (False, "", "SyntaxError: Unexpected token"),
                 id="javascript-syntax-check-fails"),
])
def test_a_failed_build_or_syntax_check_is_the_verdict_on_the_submitted_file(
        service, monkeypatch, file_path, build_command, check, verdict):
    """The submitted file executes; the check after execution fails it."""
    monkeypatch.setattr(adapters, "SandboxAdapter", PassingSandbox)
    monkeypatch.setattr(scoring, check, lambda *args, **kwargs: verdict)

    result = service.run("keep the file working", task_id="verdict", file_path=file_path,
                         build_command=build_command, baseline_code="print(1)\n")

    assert [e["detail"] for e in result["events"] if e["stage"] == "incumbent"] == [
        f"The submitted file: passed=False stderr={verdict[2]}"]


def test_a_repair_is_checked_against_the_self_tests_phase_three_generated(service, monkeypatch):
    """Phase 0 produced no cases, phase 3 generated two. Only the repair
    executes, so only its verification runs the cases."""
    repaired = "def solve(x):\n    return x + 1\n"
    two_cases = SimpleNamespace(test_cases=[SimpleNamespace(input_str="1", expected_output="2"),
                                            SimpleNamespace(input_str="2", expected_output="3")],
                                generation_tokens=0)
    suites = [SimpleNamespace(test_cases=[], generation_tokens=0), two_cases]
    monkeypatch.setattr(adapters, "SandboxAdapter",
                        lambda project_files=None: lambda code, test_input="", **_: (repaired in code, "", "boom"))
    service.self_test_gen = SimpleNamespace(generate=lambda problem, llm, task_id: suites.pop(0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id: SimpleNamespace(repairs=[repaired], total_tokens=0))

    result = service.run("add one", task_id="regenerated-cases")

    assert [e["detail"] for e in result["events"] if e["stage"] == "self_test_verify"] == ["0/2 passed"]
    assert result["phase_solved"] == "pr_cot"
