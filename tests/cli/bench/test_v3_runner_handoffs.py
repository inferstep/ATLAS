"""What the benchmark runner hands from step to step.

V3Pipeline.run_task goes probe, allocation, candidate pool, sandbox, then
selection or phase 3 repair, and each step hands the next what it found: the
probe's verdict, the k and tier the CxGx gate allocates, PlanSearch's
constraints, the failing candidates, the tokens each repair spent. Each test
drives one run with the model, the sandbox, the lens and the V3 stages faked,
and checks what arrives on the other side. run_v3_benchmark's tests check the
service checks before a run, the tasks it runs and the summary it keeps.
"""

import io
import json
from types import SimpleNamespace

import pytest

from atlas.bench import v3_runner
from atlas.bench.models import BenchmarkTask

TASK = BenchmarkTask(task_id="sum", prompt="Read two ints and print their sum.",
                     canonical_solution="", test_code="", entry_point="",
                     eval_mode="stdio", test_inputs=["1 2\n"], test_outputs=["3\n"])


def fenced(code):
    return f"```python\n{code}\n```"


def answer(code, tokens):
    """One chat completion, as the shared client returns it."""
    return {"content": fenced(code), "tokens": tokens, "time_ms": 1.0, "logprobs": []}


@pytest.fixture
def pipeline(tmp_path, monkeypatch):
    """A V3Pipeline whose lens, embedding server and stages are fakes: the
    lens gives each candidate the same score, and the stages find nothing
    until a test gives them something to find. The model and the sandbox are
    each test's own."""
    monkeypatch.setattr(v3_runner, "extract_embedding_urllib", lambda text, url: [0.5, 0.5])
    monkeypatch.setattr(v3_runner, "score_candidate_combined",
                        lambda code, url: dict(v3_runner.NEUTRAL_COMBINED))
    monkeypatch.setattr(v3_runner, "score_candidate", lambda code, url: (1.0, 0.5))
    runner = v3_runner.V3Pipeline(tmp_path)
    runner.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm_call, num_plans:
            SimpleNamespace(constraint_sets=[], candidates=[]))
    runner.div_sampling = SimpleNamespace(
        apply=lambda prompt, candidate_index, task_id: f"variant {candidate_index}")
    runner.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm_call, task_id: SimpleNamespace(test_cases=[]),
        config=SimpleNamespace(majority_threshold=0.6))
    runner.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id: SimpleNamespace(repairs=[]))
    runner.refinement_loop = SimpleNamespace(
        run=lambda **kwargs: SimpleNamespace(solved=False, winning_code="", total_iterations=1),
        config=SimpleNamespace(max_time_ms=600_000))
    return runner


def test_a_probe_that_passes_the_sandbox_is_the_answer(pipeline, tmp_path, monkeypatch):
    """The probe passes, so the gate allocates one candidate, nothing else is
    generated, the probe is not tested again, and selection returns it."""
    sandbox_runs = []

    def chat_completion(url, messages, max_tokens, **kwargs):
        return answer("print(sum(map(int, input().split())))", 7)

    def execute_code_stdio(code, inputs, outputs, timeout_sec, memory_mb):
        sandbox_runs.append(code)
        return True, "3\n", "", None

    monkeypatch.setattr(v3_runner, "chat_completion", chat_completion)
    monkeypatch.setattr(v3_runner, "execute_code_stdio", execute_code_stdio)

    result = pipeline.run_task(TASK)

    assert result["passed"] is True
    assert result["phase_solved"] == "phase1"
    assert result["code"] == "print(sum(map(int, input().split())))\n"
    assert result["candidates_generated"] == 1
    assert result["telemetry"]["probe_early_exit"] is True
    assert result["telemetry"]["adaptive_k"] == 1
    assert result["total_tokens"] == 7
    assert len(sandbox_runs) == 1
    events = (tmp_path / "v3_events.jsonl").read_text().splitlines()
    assert [json.loads(event)["phase_solved"] for event in events] == ["phase1"]


def test_the_gates_k_and_tier_shape_phase_1_and_pr_cot_repairs_the_lowest_energy_failure(
        pipeline, monkeypatch):
    """No candidate passes. The gate's k=4 at the "light" tier reaches phase 1:
    PlanSearch is asked for the three after the probe and writes one, and
    DivSampling writes the other two within the light tier's token budget.
    PR-CoT gets the failure with the lowest energy, with its error, and its
    repair passes the real sandbox."""
    monkeypatch.setattr(v3_runner.cxgx_gate, "allocate", lambda **scores: SimpleNamespace(
        k=4, tier="light", base_tier="light", gx_escalation=False, reason="test"))
    monkeypatch.setattr(v3_runner, "score_candidate_combined",
                        lambda code, url: {**v3_runner.NEUTRAL_COMBINED, "cx_energy": 3.0})
    energies = {"planned\n": 1.0, "div 2\n": 2.0, "div 3\n": 4.0}
    monkeypatch.setattr(v3_runner, "score_candidate", lambda code, url: (energies[code], 0.5))
    max_tokens_of = {}

    def chat_completion(url, messages, max_tokens, **kwargs):
        prompt = messages[-1]["content"]
        max_tokens_of[prompt] = max_tokens
        code = {TASK.prompt: "probe", "variant 2": "div 2", "variant 3": "div 3",
                "fix it": "repaired"}[prompt]
        return answer(code, 5)

    def execute_code_stdio(code, inputs, outputs, timeout_sec, memory_mb):
        if code == "repaired\n":
            return True, "3\n", "", None
        return False, "", f"wrong answer from {code.strip()}", None

    plan_requests = []

    def generate(problem, task_id, llm_call, num_plans):
        plan_requests.append(num_plans)
        return SimpleNamespace(constraint_sets=[SimpleNamespace(constraints=["add them"])],
                               candidates=["planned\n"])

    repairs_asked = []

    def repair(problem, code, error, llm_call, task_id):
        repairs_asked.append((code, error))
        llm_call("fix it", 0.2, 100, None)
        return SimpleNamespace(repairs=["repaired\n"])

    monkeypatch.setattr(v3_runner, "chat_completion", chat_completion)
    monkeypatch.setattr(v3_runner, "execute_code_stdio", execute_code_stdio)
    pipeline.plan_search = SimpleNamespace(generate=generate)
    pipeline.pr_cot = SimpleNamespace(repair=repair)

    result = pipeline.run_task(TASK)

    assert plan_requests == [3]
    assert max_tokens_of["variant 2"] == 1024 + 4096
    assert max_tokens_of["variant 3"] == 1024 + 4096
    assert result["candidates_generated"] == 4
    assert result["telemetry"]["budget_tier"] == "light"
    assert repairs_asked == [("planned\n", "wrong answer from planned")]
    assert result["phase_solved"] == "pr_cot"
    assert result["code"] == "repaired\n"
    assert result["telemetry"]["phase3_strategies_tried"] == ["pr_cot"]
    assert result["telemetry"]["self_test_fallback"] is True
    # The probe, the two DivSampling candidates and the repair, 5 tokens each.
    assert result["total_tokens"] == 20


def test_the_refinement_loop_gets_the_constraints_and_a_sandbox_that_runs_the_self_tests(
        pipeline, monkeypatch):
    """PR-CoT's repair fails too, so the refinement loop runs. It gets the
    failing candidates, PlanSearch's constraints and a sandbox that checks
    against the self-tests; its winning code passes the real sandbox."""
    monkeypatch.setattr(v3_runner.cxgx_gate, "allocate", lambda **scores: SimpleNamespace(
        k=2, tier="standard", base_tier="standard", gx_escalation=False, reason="test"))

    def chat_completion(url, messages, max_tokens, **kwargs):
        code = {TASK.prompt: "probe", "refine": "refined"}[messages[-1]["content"]]
        return answer(code, 5)

    def execute_code_stdio(code, inputs, outputs, timeout_sec, memory_mb):
        return code == "refined\n", "", "wrong answer", None

    self_tests = [SimpleNamespace(input_str="1 2", expected_output="3")]
    loop_calls = []

    def run(**kwargs):
        loop_calls.append(kwargs)
        kwargs["llm_call"]("refine", 0.2, 100, None)
        return SimpleNamespace(solved=True, winning_code="refined\n", total_iterations=2)

    monkeypatch.setattr(v3_runner, "chat_completion", chat_completion)
    monkeypatch.setattr(v3_runner, "execute_code_stdio", execute_code_stdio)
    pipeline.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm_call, num_plans: SimpleNamespace(
            constraint_sets=[SimpleNamespace(constraints=["add them"])], candidates=["planned\n"]))
    pipeline.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm_call, task_id: SimpleNamespace(test_cases=self_tests),
        config=SimpleNamespace(majority_threshold=0.6))
    pipeline.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=["still wrong\n"]))
    pipeline.refinement_loop = SimpleNamespace(run=run, config=SimpleNamespace(max_time_ms=600_000))

    result = pipeline.run_task(TASK)

    assert len(loop_calls) == 1
    assert [c.code for c in loop_calls[0]["failing_candidates"]] == ["probe\n", "planned\n"]
    assert loop_calls[0]["original_constraints"] == ["add them"]
    assert loop_calls[0]["sandbox_run"].self_verify_mode is True
    assert loop_calls[0]["sandbox_run"].custom_test_cases == self_tests
    assert result["phase_solved"] == "refinement"
    assert result["code"] == "refined\n"
    assert result["telemetry"]["refinement_iterations"] == 2
    assert result["telemetry"]["phase3_strategies_tried"] == ["pr_cot", "refinement"]
    # The probe's 5 tokens and the refinement loop's 5.
    assert result["total_tokens"] == 10


def test_without_phase_1_a_direct_generation_is_the_one_candidate_and_phase_3_off_returns_it(
        tmp_path, monkeypatch):
    """Phase 1 off: no probe, no PlanSearch or DivSampling, one direct
    generation. It fails, and with phase 3 off the run ends there, keeping
    the failed candidate's code."""
    calls = []

    def chat_completion(url, messages, max_tokens, **kwargs):
        calls.append(max_tokens)
        return answer("print(0)", 4)

    monkeypatch.setattr(v3_runner, "chat_completion", chat_completion)
    monkeypatch.setattr(v3_runner, "execute_code_stdio",
                        lambda code, inputs, outputs, timeout_sec, memory_mb:
                            (False, "0\n", "", None))
    monkeypatch.setattr(v3_runner, "extract_embedding_urllib", lambda text, url: [0.5, 0.5])
    monkeypatch.setattr(v3_runner, "score_candidate", lambda code, url: (1.0, 0.5))
    pipeline = v3_runner.V3Pipeline(tmp_path, enable_phase1=False, enable_phase3=False)

    result = pipeline.run_task(TASK)

    assert calls == [v3_runner.MAX_TOKENS]
    assert "probe_cx_normalized" not in result["telemetry"]
    assert result["candidates_generated"] == 1
    assert result["passed"] is False
    assert result["phase_solved"] == "none"
    assert result["code"] == "print(0)\n"
    assert "latency" in result["telemetry"]
    assert "phase3_strategies_tried" not in result["telemetry"]


def test_a_benchmark_run_keeps_its_summary_in_the_run_metadata(tmp_path, monkeypatch):
    """The services answer; a smoke run takes the first 10 of 12 tasks; the
    pass rate and the phase breakdown of its results go into run_meta.json."""
    monkeypatch.setattr(v3_runner.config, "_root", tmp_path)
    monkeypatch.setattr(v3_runner.urllib.request, "urlopen",
                        lambda req, timeout: io.BytesIO(b'{"status": "ok", "energy": 1.0}'))
    tasks = [SimpleNamespace(task_id=f"t{i}") for i in range(12)]
    monkeypatch.setattr(v3_runner, "load_lcb_tasks", lambda: tasks)
    tasks_run = []

    class Runner:
        def __init__(self, run_dir, enable_phase1, enable_phase3, selection_strategy):
            pass

        def run_lcb(self, run_tasks):
            tasks_run.extend(run_tasks)
            return {"t0": {"passed": True, "phase_solved": "phase1"},
                    "t1": {"passed": True, "phase_solved": "pr_cot"},
                    "t2": {"passed": False, "phase_solved": "none"},
                    "t3": {"passed": False, "phase_solved": "none"}}

    monkeypatch.setattr(v3_runner, "V3BenchmarkRunner", Runner)

    run_dir = v3_runner.run_v3_benchmark(run_id="smoke", smoke_only=True)

    assert tasks_run == tasks[:10]
    meta = json.loads((run_dir / "run_meta.json").read_text())
    assert meta["total_tasks"] == 4
    assert meta["passed_tasks"] == 2
    assert meta["pass_rate"] == 0.5
    assert meta["phase_breakdown"] == {"phase1": 1, "pr_cot": 1, "none": 2}


def test_a_benchmark_run_stops_before_its_tasks_when_llama_server_does_not_answer(
        tmp_path, monkeypatch):
    monkeypatch.setattr(v3_runner.config, "_root", tmp_path)

    def urlopen(req, timeout):
        raise OSError("connection refused")

    loaded = []

    def load_lcb_tasks():
        loaded.append(True)
        return []

    monkeypatch.setattr(v3_runner.urllib.request, "urlopen", urlopen)
    monkeypatch.setattr(v3_runner, "load_lcb_tasks", load_lcb_tasks)

    with pytest.raises(SystemExit) as stop:
        v3_runner.run_v3_benchmark(run_id="down")

    assert stop.value.code == 1
    assert loaded == []
