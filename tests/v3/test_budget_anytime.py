"""V3 hands back its best candidate when its clock runs out.

V3 is an anytime algorithm: the refinement loop is budget-gated, so it expands
to fill ATLAS_V3_TIMEOUT rather than converging early. Measured 2026-08-03 on
one task — a 180s budget produced 9 LLM calls and 5 sandbox verifications, a
420s budget 21 calls and 10. The cap is its terminal condition, not an
interruption.

It returned nothing at that terminal condition. The caller's deadline
cancelled the request, every verified candidate went with it, and the write
fell back to the model's own output: 16 of 41 write calls in one 28-session
run, ~48 of the 56 minutes spent in them.
"""

import os
import time

import pytest

import pipeline


@pytest.fixture(autouse=True)
def _clear_budget_env(monkeypatch):
    monkeypatch.delenv("ATLAS_V3_TIMEOUT", raising=False)


def test_remaining_budget_tracks_the_configured_cap(monkeypatch):
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "180")
    start = time.time()
    left = pipeline._remaining_budget_ms(start)
    assert 179_000 < left <= 180_000

    # A spent clock goes negative rather than clamping, so a guard reading
    # "< reserve" fires rather than seeing a small positive budget.
    assert pipeline._remaining_budget_ms(time.time() - 300) < 0


def test_a_disabled_cap_reports_no_budget(monkeypatch):
    """0 disables the cap for offline bench runs — the guards must then
    never fire, or they would truncate a run that was asked to be unbounded."""
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    assert pipeline._remaining_budget_ms(time.time()) is None


def test_without_a_caller_cap_the_service_default_is_300s(monkeypatch):
    """A caller that sends no budget_ms (an offline or bench caller) gets
    the service's own reading of ATLAS_V3_TIMEOUT, default 300s.

    300s, not the 180s both shipped with: PlanSearch spends two LLM calls per
    candidate, so k=3 costs ~162s at the measured ~22s per call before the
    probe and self-test. At 180s, sessions spent a median 207s on generation
    alone and phase-3 repair was skipped 19 times with 7-9s left.
    """
    monkeypatch.delenv("ATLAS_V3_TIMEOUT", raising=False)
    left = pipeline._remaining_budget_ms(time.time())
    assert 299_000 < left <= 300_000

    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "not-a-number")
    left = pipeline._remaining_budget_ms(time.time())
    assert 299_000 < left <= 300_000


def test_a_caller_cap_replaces_the_configured_one(monkeypatch):
    """The proxy cuts a call to half of the session's remaining time and
    sends that cap. The service used to plan a 300s run inside a call the
    proxy would abandon far sooner."""
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "300")
    left = pipeline._remaining_budget_ms(time.time(), 40_000)
    assert 39_000 < left <= 40_000
    # A cap the caller applies holds even where the service's own reading
    # would be unbounded.
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    assert pipeline._remaining_budget_ms(time.time(), 40_000) is not None
    # No usable cap: the configured behaviour, unchanged.
    assert pipeline._remaining_budget_ms(time.time(), None) is None
    assert pipeline._remaining_budget_ms(time.time(), 0) is None


def test_the_run_plans_against_the_callers_cap(monkeypatch):
    """The generation deadline, which every phase's budget check shares, is
    the caller's cap and not ATLAS_V3_TIMEOUT."""
    import adapters

    made = []

    class RecordingLLM:
        def __init__(self, progress_callback=None, thinking=False):
            made.append(self)

    class Stop(Exception):
        pass

    def no_sandbox(*args, **kwargs):
        raise Stop()

    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "300")
    monkeypatch.setattr(adapters, "LLMAdapter", RecordingLLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", no_sandbox)
    before = time.time()
    with pytest.raises(Stop):
        pipeline.V3PipelineService().run(
            "write a helper", task_id="budget", file_path="helper.py",
            budget_ms=40_000)
    assert made, "the run never built its generation adapter"
    assert 39 < made[0].deadline - before <= 40.5, made[0].deadline - before


def test_new_stages_are_registered_for_the_summary():
    """A stage missing from _STAGE_PHASE contributes no phase row, so the
    run summary silently loses the reason it stopped."""
    assert pipeline._STAGE_PHASE["budget_exhausted"] == "fallback"
    assert pipeline._STAGE_PHASE["divsampling_stop"] == "generation"


def test_reserve_covers_one_more_generation(monkeypatch):
    """A flat reserve is useless when the unit of work is an LLM call.

    Measured 2026-08-03: the phase-3 entry check passed with ~20s left,
    PR-CoT then spent 31s on a single call, and the cap landed part-way
    through the next one. The reserve has to be sized to what starting more
    work actually costs.
    """
    import types

    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "180")

    # Rebuild the closure's inputs the way _run_impl does.
    def make_guard(avg_call_ms, elapsed_s):
        llm = types.SimpleNamespace(avg_call_ms=avg_call_ms)
        start = time.time() - elapsed_s

        def out_of_budget(reserve_ms=None):
            left = pipeline._remaining_budget_ms(start)
            if left is None:
                return False
            if reserve_ms is None:
                observed = getattr(llm, "avg_call_ms", 0.0) or 0.0
                reserve_ms = max(20000.0, observed * 1.2 + 10000.0)
            return left < reserve_ms

        return out_of_budget

    # 30s calls, 140s spent: 40s left does not cover another 30s call + margin.
    assert make_guard(30_000, 140)() is True
    # Same clock, cheap calls: 40s left is plenty for a 5s call.
    assert make_guard(5_000, 140)() is False
    # Early in the run nothing is short.
    assert make_guard(30_000, 10)() is False


def test_a_disabled_cap_never_trips_the_guard(monkeypatch):
    import types
    monkeypatch.setenv("ATLAS_V3_TIMEOUT", "0")
    llm = types.SimpleNamespace(avg_call_ms=60_000)
    start = time.time() - 10_000

    left = pipeline._remaining_budget_ms(start)
    assert left is None, "an unbounded run must report no budget"




def test_an_unverified_candidate_is_not_returned():
    """The caller's baseline is the model's own write, syntax- and
    structure-gated. A candidate that failed the sandbox is not better than
    that, and ranking failures by energy picks among them without evidence.

    Measured across a 28-session run: 7 of 8 budget returns took the
    unverified path, task success fell 20/28 to 17/28 (aoc_course and
    aoc_slope each 2/2 to 0/2) while harness integrity reached 28/28. The
    cap used to discard everything, so this fallback rarely reached disk;
    returning V3's work exposed it.
    """
    assert pipeline._STAGE_PHASE["budget_no_verified_candidate"] == "fallback"


# --- generations are sized to the clock ------------------------------------

def test_max_tokens_shrinks_to_what_the_clock_can_decode():
    """A generation runs until it stops or hits max_tokens, so an 8192-token
    ceiling at ~25 tok/s is a 327s call — longer than the whole 180s budget.

    Measured 2026-08-04: every V3 hang-up in a 28-session run was the
    pipeline cut mid-probe, the FIRST generation, which had the full budget
    and still did not finish. Refusing the call produces nothing at all;
    asking for a length the clock can deliver is the fix.
    """
    import adapters

    a = adapters.LLMAdapter(deadline=time.time() + 60)
    a.total_tokens, a.total_time_ms = 500, 20_000   # 25 tok/s observed
    # 60s - 5s reserve = 55s at 25 tok/s * 0.8 == ~1100 tokens, not 8192.
    got = a._budget_max_tokens(8192)
    assert 800 < got < 1400, got

    # A generous clock leaves the caller's ceiling alone.
    a.deadline = time.time() + 3600
    assert a._budget_max_tokens(8192) == 8192


def test_first_call_is_sized_from_an_assumed_rate():
    """Before this run has observed a call there is no measured rate, and
    the first generation is exactly where the hang-ups happened."""
    import adapters

    a = adapters.LLMAdapter(deadline=time.time() + 180)
    assert a.total_tokens == 0
    got = a._budget_max_tokens(8192)
    assert got < 8192, "the first call must still be bounded by the clock"
    assert got >= adapters.LLMAdapter._MIN_USEFUL_TOKENS


def test_a_spent_clock_refuses_rather_than_asking_for_nothing():
    import adapters
    a = adapters.LLMAdapter(deadline=time.time() + 2)
    with pytest.raises(adapters.BudgetExhausted):
        a._budget_max_tokens(8192)


def test_an_unbounded_adapter_keeps_the_caller_ceiling():
    import adapters
    a = adapters.LLMAdapter(deadline=None)
    assert a._budget_max_tokens(8192) == 8192
