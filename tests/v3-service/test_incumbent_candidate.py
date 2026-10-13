"""#259: the caller's own bytes are a real candidate.

Isaac's decision: the model's exact bytes join the pool as one more
candidate, face the same checks, and the lens ranks them with the rest. V3
checks those exact bytes -- nothing re-types them -- and a copy that differs
only in whitespace is never delivered in their place.

Measured before the change, over 112 recorded sessions: V3 replaced the
model's file in all 33 of its deliveries. On the 23 AoC deliveries it fixed 3
wrong model files that ran cleanly (so a rule that kept the model's file on
every tie would have lost them), and never broke a right one.
"""

import sys
from pathlib import Path
from types import SimpleNamespace

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters
import pipeline as P
import scoring
import symbols

THEIRS = ("import sys\n\n\ndef solve():\n"
          "    print(sum(int(x) for x in open('input.txt')))\n\n\n"
          "if __name__ == '__main__':\n    solve()\n")
# The same program re-typed, with the drift a re-typing adds.
RETYPED = THEIRS.replace("print(sum(", "print( sum(").replace("\n\n\ndef", "\n\ndef")
BETTER = THEIRS.replace("sum(int(x)", "sum(int(x.strip())")
PROBE = THEIRS.replace("solve()", "solve( )")


class _LLM:
    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        return f"```python\n{PROBE}```", 3, 1.0


def _sandbox(failing=()):
    class _Sandbox:
        def __init__(self, project_files=None):
            pass

        def __call__(self, code, test_input="", **_):
            if code in failing:
                return False, "", "Traceback: boom"
            return True, "", ""
    return _Sandbox


def _service(monkeypatch, plan_codes, energies, failing=(), repairs=()):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setattr(adapters, "LLMAdapter", _LLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", _sandbox(failing))
    monkeypatch.setattr(adapters, "EmbedAdapter", lambda: (lambda text: []))
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate",
                        lambda code: (energies.get(code, 1.0), 0.1, False))
    monkeypatch.setattr(scoring, "score_candidate_combined",
                        lambda code: {**scoring.NEUTRAL_COMBINED,
                                      "cx_energy": energies.get(code, 1.0)})
    monkeypatch.setattr(scoring, "score_candidate_per_step",
                        lambda code: {"gx_score_min": 0.9, "gx_score_mean": 0.9,
                                      "cx_norm_max": 0.1, "first_off_rails_idx": -1,
                                      "n_tokens": 10, "thresholds": {"severe": 0.30}})
    monkeypatch.setattr(symbols, "structural_score",
                        lambda project_symbols, code: {"ok": False})
    service = P.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            (_ for _ in ()).throw(RuntimeError("unavailable")))
    asked = []

    def _generate(problem, task_id, llm, num_plans=None, budget_tier="standard"):
        asked.append(num_plans)
        return SimpleNamespace(candidates=list(plan_codes), total_tokens=0)
    service.plan_search = SimpleNamespace(generate=_generate)
    repaired = []

    def _repair(problem, code, error, llm_call, task_id):
        repaired.append(code)
        return SimpleNamespace(repairs=list(repairs), total_tokens=0)
    service.pr_cot = SimpleNamespace(repair=_repair)
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service, asked, repaired


def _run(service, baseline=THEIRS):
    return service.run("write solve.py that sums input.txt", task_id="t",
                       files={"input.txt": "1\n2\n"}, file_path="solve.py",
                       baseline_code=baseline)


def test_the_exact_bytes_are_checked_not_a_copy(monkeypatch):
    """The submitted file is evaluated as sent: its record names its bytes."""
    service, _, _ = _service(monkeypatch, [BETTER], energies={THEIRS: 0.2})
    result = _run(service)
    assert result["code"] == THEIRS
    assert result["phase_solved"] == "incumbent"
    assert result["evidence_record"]["candidate_content_hash"] == \
        P.contract.content_hash(THEIRS)
    events = [e for e in result["events"] if e["stage"] == "incumbent"]
    assert events and events[0]["data"]["passed"] is True


def test_a_candidate_the_lens_ranks_higher_wins(monkeypatch):
    """A real candidate, not a protected one: it loses to a better rank."""
    service, _, _ = _service(monkeypatch, [BETTER],
                             energies={THEIRS: 3.0, BETTER: 0.5})
    result = _run(service)
    assert result["code"] == BETTER
    assert result["phase_solved"] == "phase1"


def test_a_retyped_copy_never_wins_in_place_of_the_exact_bytes(monkeypatch):
    """The copy the lens happens to prefer is the same program re-typed:
    the exact bytes stand for it."""
    service, _, _ = _service(monkeypatch, [RETYPED],
                             energies={THEIRS: 3.0, RETYPED: 0.1, PROBE: 5.0})
    result = _run(service)
    assert result["code"] == THEIRS
    assert result["phase_solved"] == "incumbent"
    dropped = [e for e in result["events"] if e["stage"] == "retyping_dropped"]
    assert dropped, [e["stage"] for e in result["events"]]


def test_a_retyped_copy_can_repair_a_file_that_failed(monkeypatch):
    """Re-typing is how an indentation slip gets fixed: when the submitted
    bytes failed, a whitespace-only copy that passes is a real repair."""
    service, _, _ = _service(monkeypatch, [RETYPED],
                             energies={RETYPED: 0.1, PROBE: 5.0},
                             failing=(THEIRS, PROBE))
    result = _run(service)
    assert result["code"] == RETYPED
    assert result["phase_solved"] == "phase1"


def test_the_incumbent_takes_no_generation_slot(monkeypatch):
    """k counts generations: with the probe in hand, PlanSearch is asked for
    k - 1 plans whether or not the caller sent a file."""
    with_file, asked_with, _ = _service(monkeypatch, [BETTER], energies={})
    kept = _run(with_file)
    without, asked_without, _ = _service(monkeypatch, [BETTER], energies={})
    fresh = without.run("write solve.py that sums input.txt", task_id="t",
                        files={"input.txt": "1\n2\n"}, file_path="solve.py",
                        baseline_code="")
    assert asked_with and asked_with == asked_without
    # The count is of generated candidates; the caller's file is not one.
    assert kept["candidates_generated"] == fresh["candidates_generated"]


def test_a_failing_incumbent_is_repaired_like_any_candidate(monkeypatch):
    """Nothing passed. The submitted file failed its run, and it is the
    lowest-energy failing candidate, so it is the one repair works on."""
    service, _, repaired = _service(
        monkeypatch, [BETTER], energies={THEIRS: 0.1},
        failing=(THEIRS, BETTER, PROBE))
    result = _run(service)
    assert repaired == [THEIRS]
    assert result["passed"] is False
    assert result["code"] == ""


def test_the_budget_boundary_ranks_the_incumbent_too(monkeypatch):
    """Out of time with a passing incumbent in hand: the best-ranked passing
    member is returned, and that may be the caller's own file."""
    # As the budget-exit test in test_mode_semantics drives it: the lens
    # declines to select, then the clock is spent before repair.
    monkeypatch.setattr(P, "select_candidate", lambda cands, strategy="lens": None)
    monkeypatch.setattr(P, "_remaining_budget_ms", lambda start, cap_ms=None: -1.0)
    service, _, _ = _service(monkeypatch, [BETTER], energies={THEIRS: 0.1})
    result = _run(service)
    stages = [e["stage"] for e in result["events"]]
    assert "budget_exhausted" in stages, stages
    assert result["code"] == THEIRS
    assert result["phase_solved"] == "incumbent"
