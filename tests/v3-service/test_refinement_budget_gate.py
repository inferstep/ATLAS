"""Refinement budget gate: the loop runs only when it can afford an iteration.

H200 join: 453/487 refinement entries exhausted their budget with zero
completed iterations while burning ~6 minutes each. Both orchestrators now
estimate one-iteration cost (~3 sequential LLM calls at the observed
per-call latency, conservative 120s floor with no observation) before
entering the loop, and skip straight to the fallback when unaffordable.

Covers the shared helpers (estimate/afford) and the live orchestrator
decision: an exhausted ATLAS_V3_TIMEOUT budget emits ``refinement_skip``
and never calls the loop; a disabled cap (0) always enters.
"""

import json
import sys
from pathlib import Path
from types import SimpleNamespace

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402
import pipeline as v3pipeline  # noqa: E402
import scoring  # noqa: E402
from stages.refinement_loop import (  # noqa: E402
    ITERATION_LLM_CALLS,
    MIN_ITERATION_MS,
    can_afford_iteration,
    estimate_iteration_ms,
)


# --- Shared helpers -----------------------------------------------------------

def test_estimate_scales_with_observed_call_latency():
    assert estimate_iteration_ms(10_000.0) == ITERATION_LLM_CALLS * 10_000.0


def test_estimate_falls_back_to_floor_without_observation():
    assert estimate_iteration_ms(0.0) == MIN_ITERATION_MS
    assert estimate_iteration_ms(-5.0) == MIN_ITERATION_MS


def test_afford_boundary():
    assert can_afford_iteration(120_000.0, 120_000.0) is True
    assert can_afford_iteration(119_999.0, 120_000.0) is False
    assert can_afford_iteration(200_000.0, 30_000.0) is True


# --- Live orchestrator decision -----------------------------------------------

class FakeLLM:
    """Never produces code, and reports a slow observed call latency."""

    avg_call_ms = 60_000.0  # one iteration estimate: 3 * 60s = 180s

    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return "<think>still thinking", 3, 1.0


class FailingSandbox:
    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        return False, "", "boom"


class FakeEmbed:
    def __call__(self, text):
        return []


class RecordingRefinement:
    def __init__(self):
        self.calls = 0

    def run(self, **kw):
        self.calls += 1
        return SimpleNamespace(solved=False, total_tokens=0,
                               total_iterations=1, winning_code="")


def _run_pipeline(monkeypatch):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setattr(adapters, "LLMAdapter", FakeLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", FailingSandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", FakeEmbed)
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (5.0, 0.5, False))
    monkeypatch.setattr(scoring, "score_candidate_per_step", lambda code: None)

    service = v3pipeline.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            SimpleNamespace(test_cases=[], generation_tokens=0))
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=["def a():\n    pass\n"],
                            total_tokens=0))
    service.pr_cot = SimpleNamespace(
        repair=lambda problem, code, error, llm_call, task_id:
            SimpleNamespace(repairs=[], total_tokens=0))
    refinement = RecordingRefinement()
    service.refinement_loop = refinement
    result = service.run("sum two ints from stdin", task_id="gate")
    return result, refinement


def test_exhausted_budget_returns_before_the_repair_phase(monkeypatch):
    """1-second total budget: the run must not start repair, and must still
    hand back a result.

    This used to assert the run reached phase 3 and closed through
    `refinement_skip` + `fallback`. It now stops earlier — entering the
    repair phase with a spent clock is work whose output the caller would
    never receive, so the pipeline returns its best candidate at the phase
    boundary instead. The invariant the test exists for is unchanged: an
    exhausted budget runs no refinement and still returns.
    """
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "1")
    result, refinement = _run_pipeline(monkeypatch)

    assert refinement.calls == 0
    stages = [e["stage"] for e in result["events"]]
    assert "refinement" not in stages
    assert "budget_exhausted" in stages
    # Returning early is only correct if it still returns.
    assert result["total_time_ms"] > 0
    assert "code" in result

    spent = next(e for e in result["events"] if e["stage"] == "budget_exhausted")
    assert spent["data"]["remaining_ms"] <= 1000


def test_budget_exit_captures_scored_candidates_before_sandbox(
        monkeypatch, tmp_path):
    """A candidate scored before the budget boundary remains attributable.

    The capture record is deliberately unverified: recording the observation
    must not invent a sandbox result or alter the anytime return decision.
    """
    sink = tmp_path / "pool.jsonl"
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "1")
    monkeypatch.setenv(v3pipeline.CAPTURE_ENV, str(sink))

    result, refinement = _run_pipeline(monkeypatch)

    assert refinement.calls == 0
    assert any(e["stage"] == "budget_exhausted" for e in result["events"])
    records = [json.loads(line) for line in sink.read_text().splitlines()
               if line.strip()]
    generated = [r for r in records
                 if r.get("type") == "candidate_evaluation"
                 and r.get("role") == "generated"]
    assert len(generated) == 1
    assert generated[0]["lens"]["energy"] == 5.0


def test_disabled_cap_always_enters_refinement(monkeypatch):
    # ATLAS_V3_TIMEOUT=0 disables the cap (offline bench posture): the
    # gate must not fire, however slow the observed serving speed.
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    result, refinement = _run_pipeline(monkeypatch)

    assert refinement.calls == 1
    stages = [e["stage"] for e in result["events"]]
    assert "refinement_skip" not in stages
    assert "refinement" in stages
