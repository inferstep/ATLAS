"""The live orchestrator allocates candidates through the CxGx gate.

Covers the live-only half of the feature: the probe is scored with the
combined C(x)+G(x) call, the escalation reaches `phase2_allocated` and the
generator, and the ATLAS_V3_TIMEOUT wall-clock the bench never had caps
the tier instead of buying candidates the clock cannot generate.
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
from stages.cxgx_gate import FLOOR_TIER, K_FLOOR  # noqa: E402


# C(x) says "hard" (0.25), G(x) says "well below severe" (+2) -> extreme/k=8.
ESCALATING_SCORES = {
    "cx_energy": 8.0, "cx_normalized": 0.25, "cx_calibrated": True,
    "gx_score": 0.20, "gx_available": True, "verdict": "likely_incorrect",
}


PROBE_RESPONSE = "```python\nprint(sum(map(int, input().split())))\n```"


class FakeLLM:
    """Produces a probe the sandbox will fail; reports a per-call latency."""

    avg_call_ms = 1.0

    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return PROBE_RESPONSE, 3, 1.0


class SlowLLM(FakeLLM):
    avg_call_ms = 20_000.0


class FailingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return False, "", "boom"


class FakeEmbed:
    def __call__(self, text):
        return []


class RecordingPlanSearch:
    """Records the k and tier the allocator asked generation for."""

    def __init__(self):
        self.num_plans = None
        self.budget_tier = None

    def generate(self, problem, task_id, llm, num_plans=None,
                 budget_tier="standard"):
        self.num_plans = num_plans
        self.budget_tier = budget_tier
        return SimpleNamespace(candidates=["def a():\n    pass\n"],
                               total_tokens=0)


def _run(monkeypatch, scores, llm_cls=FakeLLM):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setattr(adapters, "LLMAdapter", llm_cls)
    monkeypatch.setattr(adapters, "SandboxAdapter", FailingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate_combined",
                        lambda code: dict(scores))
    monkeypatch.setattr(scoring, "score_candidate_per_step", lambda code: None)

    service = v3pipeline.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            SimpleNamespace(test_cases=[], generation_tokens=0))
    plan_search = RecordingPlanSearch()
    service.plan_search = plan_search
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    result = service.run("sum two ints from stdin", task_id="cxgx")
    alloc = next(e for e in result["events"]
                 if e["stage"] == "phase2_allocated")
    return result, alloc["data"], plan_search


def test_gx_escalation_reaches_allocation_and_generation(monkeypatch):
    # A generous cap: the escalation survives the budget check.
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "3600")
    _, data, plan_search = _run(monkeypatch, ESCALATING_SCORES)

    assert data["base_tier"] == "hard"
    assert data["gx_escalation"] == 2
    assert data["tier"] == "extreme"
    assert data["k"] == 8
    assert data["capped_from"] == ""
    assert data["reason"] == "gated"
    # The allocation is what generation actually runs on, not a label:
    # the probe already holds slot 0, so PlanSearch fills k-1 — at the
    # allocator's tier, not the signature default (the tier was silently
    # dropped at the production call before; third-party audit finding).
    assert plan_search.num_plans == 7
    assert plan_search.budget_tier == "extreme"


def test_short_wall_clock_caps_the_tier(monkeypatch):
    """20s per observed call against a 60s cap buys almost nothing.

    The cap used to stop at the floor, which meant it could not act at all:
    it priced tiers by what they add BEYOND k=3, so the floor itself looked
    free and the affordability walk exited immediately. Measured across 43
    pipeline runs, capped_from was empty in all 33 allocations while sessions
    spent a median 207s of a 180s budget on generation alone and phase-3
    repair was skipped 19 times with 7-9s left.

    The floor still stops the LENS starving generation. It no longer
    overrides the clock.
    """
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "60")
    _, data, plan_search = _run(monkeypatch, ESCALATING_SCORES,
                                llm_cls=SlowLLM)

    assert data["base_tier"] == "hard"
    assert data["gx_escalation"] == 2
    assert data["capped_from"] == "extreme"
    assert data["reason"] == "budget_capped"
    assert data["k"] < K_FLOOR, "a clock that affords nothing must not buy k=3"
    # k=1 is the baseline alone, so PlanSearch is asked for nothing at all
    # (never invoked) rather than for zero plans.
    assert plan_search.num_plans in (None, data["k"] - 1)


def test_disabled_cap_leaves_the_escalation_alone(monkeypatch):
    # ATLAS_V3_TIMEOUT=0 disables the wall-clock cap (the bench posture),
    # so the live path allocates exactly what the bench arm measured.
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    _, data, _ = _run(monkeypatch, ESCALATING_SCORES, llm_cls=SlowLLM)

    assert data["tier"] == "extreme"
    assert data["k"] == 8
    assert data["capped_from"] == ""


def test_uncalibrated_lens_allocates_exactly_the_floor(monkeypatch):
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "3600")
    _, data, plan_search = _run(monkeypatch, scoring.NEUTRAL_COMBINED)

    assert data["base_tier"] == FLOOR_TIER
    assert data["gx_escalation"] == 0
    assert data["tier"] == FLOOR_TIER
    assert data["k"] == K_FLOOR
    assert data["reason"] == "uncalibrated"
    assert plan_search.num_plans == K_FLOOR - 1


def test_probe_scored_event_carries_the_gx_signal(monkeypatch):
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "3600")
    result, _, _ = _run(monkeypatch, ESCALATING_SCORES)

    scored = next(e for e in result["events"] if e["stage"] == "probe_scored")
    assert scored["data"]["gx_score"] == 0.20
    assert scored["data"]["gx_available"] is True
    assert scored["data"]["verdict"] == "likely_incorrect"
