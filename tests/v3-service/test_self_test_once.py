"""D2: self-tests are generated once and reused by phase-3 repair.

Phase 0 generates the verification self-tests; ``verified_sandbox``
closes over them. Phase 3 used to regenerate an identical set — burning
tokens — and a retry failure there downgraded a good phase-0 set to
None, silently weakening repair verification. Phase 3 may only generate
when phase 0 produced nothing.
"""

import sys
from pathlib import Path
from types import SimpleNamespace

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402
import pipeline as v3pipeline  # noqa: E402
import scoring  # noqa: E402


class FakeLLM:
    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        # Unclosed <think> extracts to no code: the probe fails and the
        # pipeline runs phase 1 → sandbox → phase 3.
        return "<think>still thinking", 3, 1.0


class FailingSandbox:
    """Every candidate fails so the pipeline reaches phase-3 repair."""

    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return False, "", "boom"


class FakeEmbed:
    def __call__(self, text):
        return []


class CountingSelfTestGen:
    def __init__(self, results):
        # One entry per expected generate() call; an Exception instance
        # is raised instead of returned.
        self.results = list(results)
        self.calls = 0

    def generate(self, problem, llm, task_id):
        self.calls += 1
        outcome = self.results.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome


def _tests(n=2):
    cases = [SimpleNamespace(input_str="1", expected_output="1")
             for _ in range(n)]
    return SimpleNamespace(test_cases=cases, generation_tokens=5)


def _run_pipeline(monkeypatch, self_test_gen):
    # Keep the test hermetic: never probe the host's /data/telemetry.
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setattr(adapters, "LLMAdapter", FakeLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", FailingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (5.0, 0.5, False))
    monkeypatch.setattr(scoring, "score_candidate_per_step", lambda code: None)

    service = v3pipeline.V3PipelineService()
    service.self_test_gen = self_test_gen
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None:
            SimpleNamespace(candidates=["def a():\n    pass\n",
                                        "def b():\n    pass\n",
                                        "def c():\n    pass\n"],
                            total_tokens=0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service.run("sum two ints from stdin", task_id="d2")


def test_phase3_reuses_phase0_self_tests(monkeypatch):
    gen = CountingSelfTestGen([_tests()])
    result = _run_pipeline(monkeypatch, gen)

    # One generation, in phase 0 — phase 3 reused the set.
    assert gen.calls == 1
    stages = [e["stage"] for e in result["events"]]
    assert "phase3" in stages
    assert stages.count("self_test_gen") == 1


def test_phase3_generates_only_when_phase0_produced_none(monkeypatch):
    gen = CountingSelfTestGen([RuntimeError("llm hiccup"), _tests()])
    result = _run_pipeline(monkeypatch, gen)

    # Phase 0 failed, phase 3 retried: exactly two attempts.
    assert gen.calls == 2
    stages = [e["stage"] for e in result["events"]]
    assert stages.count("self_test_error") == 1
    assert stages.count("self_test_done") == 1
