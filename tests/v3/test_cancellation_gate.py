"""No V3 inference may START after the client is gone.

Measured on a real acquisition: case v3c-b-walk4 issued 23 serial inference
calls, and the 23rd STARTED at +569.8s -- the work deadline -- then ran 39.8s
to completion after the agent request had already returned. The relay recorded
it `completed`, never cancelled, and the acquisition's 30s drain expired with
that one call still in flight.

V3 is a synchronous ThreadingHTTPServer. Its only disconnect signal is a
BrokenPipeError on the next SSE write, which sets `disconnected` on the
progress callback, and the pipeline consults that flag at PHASE boundaries.
Nothing consulted it at the point that actually spends GPU: the dispatch of an
inference call. These tests pin that gate at the one chokepoint every V3 call
passes through.
"""
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "v3-service"))
import adapters  # noqa: E402


class Callback:
    """Stands in for the handler's emit_progress, which carries the flag."""

    def __init__(self, disconnected=False):
        self.disconnected = disconnected
        self.events = []

    def __call__(self, stage, detail, **kw):
        self.events.append((stage, detail))


def adapter(cb, sent):
    a = adapters.LLMAdapter(progress_callback=cb)

    def _send(*args, **kwargs):
        sent.append(1)
        return {"choices": [{"text": "```python\nx = 1\n```"}],
                "usage": {"total_tokens": 10}}

    a._send = _send
    return a


def test_dispatch_is_refused_once_the_client_is_gone():
    sent = []
    cb = Callback(disconnected=True)
    a = adapter(cb, sent)
    with pytest.raises(adapters.ClientDisconnected):
        a("prompt", 0.7, 128, 42)
    assert sent == [], "an inference call was dispatched after the client disconnected"


def test_a_live_client_still_dispatches():
    sent = []
    cb = Callback(disconnected=False)
    a = adapter(cb, sent)
    out = a("prompt", 0.7, 128, 42)
    assert sent == [1]
    assert out[0]


def test_the_gate_is_evaluated_per_call_not_once():
    sent = []
    cb = Callback(disconnected=False)
    a = adapter(cb, sent)
    a("prompt", 0.7, 128, 1)
    cb.disconnected = True          # the SSE write fails between calls
    with pytest.raises(adapters.ClientDisconnected):
        a("prompt", 0.7, 128, 2)
    assert sent == [1], "a second call was dispatched after mid-run disconnect"


def test_no_callback_means_no_gate():
    """A non-agent caller (bench, CLI) has no SSE client and must be unaffected."""
    sent = []
    a = adapters.LLMAdapter(progress_callback=None)

    def _send(*args, **kwargs):
        sent.append(1)
        return {"choices": [{"text": "```python\nx = 1\n```"}],
                "usage": {"total_tokens": 10}}

    a._send = _send
    assert a("prompt", 0.7, 128, 42)[0]
    assert sent == [1]


def test_the_walk4_shape_starts_no_late_call():
    """23 serial calls; the client goes at the 22nd. The 23rd must not start."""
    sent = []
    cb = Callback()
    a = adapter(cb, sent)
    for i in range(22):
        a("prompt", 0.7, 128, i)
    assert len(sent) == 22
    cb.disconnected = True
    with pytest.raises(adapters.ClientDisconnected):
        a("prompt", 0.7, 128, 22)
    assert len(sent) == 22, "the late call was dispatched anyway"


# --- request-scoped cancellation ----------------------------------------------
#
# The dispatch guard above only helps when something already noticed the client
# left. While a generation is in flight nothing is written, so nothing notices.
# Measured against a real V3 service over a real socket: an in-flight call ran
# its full 25s with the upstream still seeing a connected client, and a further
# call started afterwards. The scope is the signal that does not wait for a
# broken write, and closing the registered connection is what actually stops
# the upstream.


class FakeConn:
    def __init__(self):
        self.closed = 0

    def close(self):
        self.closed += 1


def test_cancel_closes_every_live_connection():
    scope = adapters.CancelScope("inv-1")
    a, b = FakeConn(), FakeConn()
    assert scope.register(a) and scope.register(b)
    assert scope.cancel() == 2
    assert a.closed == 1 and b.closed == 1
    assert scope.cancelled


def test_cancel_is_idempotent():
    scope = adapters.CancelScope("inv-1")
    c = FakeConn()
    scope.register(c)
    scope.cancel()
    scope.cancel()
    assert c.closed == 1, "a second cancel re-closed a connection it no longer owns"


def test_register_after_cancel_is_refused():
    scope = adapters.CancelScope("inv-1")
    scope.cancel()
    assert scope.register(FakeConn()) is False, "a call opened after cancellation"


def test_one_scope_never_touches_another():
    mine, theirs = adapters.CancelScope("a"), adapters.CancelScope("b")
    ours, yours = FakeConn(), FakeConn()
    mine.register(ours)
    theirs.register(yours)
    mine.cancel()
    assert ours.closed == 1 and yours.closed == 0
    assert theirs.cancelled is False


def test_dispatch_refused_on_a_cancelled_scope_without_any_sse_signal():
    sent = []
    cb = Callback(disconnected=False)      # nothing ever noticed a broken write
    a = adapter(cb, sent)
    a.cancel_scope = adapters.CancelScope("inv-1")
    a.cancel_scope.cancel()
    with pytest.raises(adapters.Cancelled):
        a("prompt", 0.7, 128, 42)
    assert sent == []


def test_no_scope_means_no_cancellation_path():
    """Bench and CLI callers have no request; they must be unaffected."""
    sent = []
    a = adapter(Callback(), sent)
    assert a.cancel_scope is None
    assert a("prompt", 0.7, 128, 42)[0]
    assert sent == [1]


def test_unregister_leaves_the_scope_usable():
    scope = adapters.CancelScope("inv-1")
    c = FakeConn()
    scope.register(c)
    scope.unregister(c)
    assert scope.cancel() == 0
    assert c.closed == 0, "an unregistered connection was closed by cancel"


def test_concurrent_register_and_cancel_never_leaks():
    import threading as _t
    scope = adapters.CancelScope("inv-1")
    conns, leaked = [], []

    def worker():
        c = FakeConn()
        if scope.register(c):
            conns.append(c)
        else:
            leaked.append(c)

    threads = [_t.Thread(target=worker) for _ in range(32)]
    for t in threads[:16]:
        t.start()
    for t in threads[:16]:
        t.join()
    scope.cancel()
    for t in threads[16:]:
        t.start()
    for t in threads[16:]:
        t.join()
    assert all(c.closed == 1 for c in conns), "a registered connection survived cancel"
    assert all(c.closed == 0 for c in leaked)
    assert len(conns) + len(leaked) == 32
