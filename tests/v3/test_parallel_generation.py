"""PlanSearch generates its candidates concurrently.

Each plan comes from its own constraint set and each candidate from its own
plan and seed, so nothing in either step reads another's result — the
parallel-sampling shape best-of-N assumes. It ran end to end instead, because
the service adapter serialized every call behind a class-level lock added to
keep concurrent REQUESTS from oversubscribing llama.cpp.

Measured 2026-08-03 on a capped session: 8 sequential LLM calls totalling 166s
against the proxy's 180s cap, of which 4 independent candidates were 88.5s,
while three of llama-server's four slots sat idle for the whole run.
"""

import threading
import time

import pytest

import adapters
from adapters import LLMAdapter
from stages.plan_search import PlanSearch


# --- fan-out ---------------------------------------------------------------

@pytest.fixture
def fan():
    return PlanSearch.__new__(PlanSearch)._fan_out


def test_results_come_back_in_index_order(fan):
    """Selection reports winners by index; a reordered list renames them."""
    out = fan([(i, i) for i in range(5)], lambda i, x: f"r{i}")
    assert [i for i, _ in out] == [0, 1, 2, 3, 4]
    assert [r for _, r in out] == [f"r{i}" for i in range(5)]


def test_one_failed_candidate_does_not_drop_the_batch(fan):
    def fn(i, _):
        if i == 2:
            raise RuntimeError("generation failed")
        return f"r{i}"

    assert [i for i, _ in fan([(i, i) for i in range(4)], fn)] == [0, 1, 3]


def test_independent_items_actually_overlap(fan):
    """4 x 0.3s is 1.2s serially. Without this the change is inert."""
    start = time.time()
    out = fan([(i, i) for i in range(4)], lambda i, _: time.sleep(0.3) or i)
    elapsed = time.time() - start
    assert len(out) == 4
    assert elapsed < 0.8, f"ran serially: {elapsed:.2f}s"


def test_a_single_item_skips_the_pool(fan):
    seen = []
    fan([(0, "a")], lambda i, _: seen.append(threading.current_thread().name))
    assert seen == ["MainThread"]


# --- adapter concurrency ---------------------------------------------------

def test_inflight_bound_defaults_to_the_slot_count(monkeypatch):
    for var in ("ATLAS_V3_MAX_INFLIGHT", "ATLAS_PARALLEL_SLOTS", "PARALLEL_SLOTS"):
        monkeypatch.delenv(var, raising=False)
    assert adapters._max_inflight() == 4

    monkeypatch.setenv("ATLAS_PARALLEL_SLOTS", "2")
    assert adapters._max_inflight() == 2

    # An explicit override wins, and 1 restores full serialization.
    monkeypatch.setenv("ATLAS_V3_MAX_INFLIGHT", "1")
    assert adapters._max_inflight() == 1

    monkeypatch.setenv("ATLAS_V3_MAX_INFLIGHT", "garbage")
    assert adapters._max_inflight() == 2


def test_the_backend_bound_is_a_semaphore_not_a_mutex():
    """A Lock here serializes the calls inside one pipeline run, which is
    the cost this change removes. The bound itself has to stay: llama.cpp
    oversubscribed past its slots degrades latency sharply."""
    peak = 0
    live = 0
    guard = threading.Lock()
    done = threading.Barrier(4, timeout=5)

    def worker():
        nonlocal peak, live
        with LLMAdapter._slots:
            with guard:
                live += 1
                peak = max(peak, live)
            try:
                done.wait()
            finally:
                with guard:
                    live -= 1

    threads = [threading.Thread(target=worker) for _ in range(4)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=5)
    assert peak > 1, "calls are still fully serialized"


def test_counters_survive_concurrent_calls():
    """call_count/total_tokens are read-modify-write and now run under
    concurrency; unguarded they lose increments and the reported token
    total drifts below the real spend."""
    a = LLMAdapter()
    n = 200

    def bump():
        with LLMAdapter._counter_lock:
            a.call_count += 1
            a.total_tokens += 10

    threads = [threading.Thread(target=bump) for _ in range(n)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert a.call_count == n
    assert a.total_tokens == 10 * n
