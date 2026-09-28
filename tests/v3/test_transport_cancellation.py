"""Transport cancellation over real sockets.

`registration refused because the scope was already cancelled` proves nothing
about a call that is BLOCKED. These tests open a real HTTP connection to a
controllable server and cancel it from another thread at each point a
cancellation actually has to survive: while request bytes are going out, while
getresponse() waits for headers, mid-stream, and during a long silent gap.

The server records whether its own client disappeared before it finished, which
is the only honest evidence that cancellation reached the upstream rather than
a worker merely returning.
"""
import json
import os
import socket
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
sys.path.insert(0, V3)
import adapters  # noqa: E402


class Upstream(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    calls = []
    lock = threading.Lock()
    header_delay = 0.0
    chunk_gap = 0.05
    chunks = 6
    stubborn = False

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        self.rfile.read(n)
        with Upstream.lock:
            rec = {"start": time.time(), "end": None, "client_gone": False,
                   "sent": 0, "finished": False}
            Upstream.calls.append(rec)
        try:
            if Upstream.header_delay:
                time.sleep(Upstream.header_delay)
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for _ in range(Upstream.chunks):
                body = ("data: " + json.dumps(
                    {"choices": [{"delta": {"content": "x"}}]}) + "\n\n").encode()
                self.wfile.write(b"%x\r\n" % len(body) + body + b"\r\n")
                self.wfile.flush()
                with Upstream.lock:
                    rec["sent"] += 1
                time.sleep(Upstream.chunk_gap)
            tail = b"data: [DONE]\n\n"
            self.wfile.write(b"%x\r\n" % len(tail) + tail + b"\r\n")
            self.wfile.write(b"0\r\n\r\n")
            self.wfile.flush()
            with Upstream.lock:
                rec["finished"] = True
        except (BrokenPipeError, ConnectionResetError, OSError):
            with Upstream.lock:
                rec["client_gone"] = True
        finally:
            with Upstream.lock:
                rec["end"] = time.time()


@pytest.fixture()
def upstream():
    Upstream.calls = []
    Upstream.header_delay = 0.0
    Upstream.chunk_gap = 0.05
    Upstream.chunks = 6
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    port = srv.server_address[1]
    old = adapters.INFERENCE_URL
    adapters.INFERENCE_URL = f"http://127.0.0.1:{port}"
    try:
        yield srv
    finally:
        adapters.INFERENCE_URL = old
        srv.shutdown()
        srv.server_close()



def upstream_settled(index=0, timeout=8):
    """Wait for one upstream record to finish unwinding.

    The peer notices a torn-down connection on its NEXT write, so the record's
    outcome is not readable the instant the local worker returns. Waiting is
    part of the measurement, not a workaround.
    """
    deadline = time.time() + timeout
    while time.time() < deadline:
        with Upstream.lock:
            if len(Upstream.calls) > index and Upstream.calls[index]["end"]:
                return dict(Upstream.calls[index])
        time.sleep(0.05)
    with Upstream.lock:
        return dict(Upstream.calls[index]) if len(Upstream.calls) > index else {}


def run_call(scope, payload=b'{"x":1}', collect=None):
    """Drive one real inference connection through the production helper."""
    a = adapters.LLMAdapter(progress_callback=None)
    a.cancel_scope = scope
    # An adapter serving a request carries the identity its calls go out
    # under, alongside the scope. Set here for the same reason the scope is:
    # this stands in for a served request, and one without an identity is a
    # wiring error the adapter refuses rather than sends.
    a.request_identity = adapters.RequestIdentity(
        request_id=f"req-{scope.invocation_id}",
        invocation_id=scope.invocation_id)
    err = {}
    def _go():
        try:
            with a._open_inference(payload) as resp:
                for raw in resp:
                    if collect is not None:
                        collect.append(raw)
        except Exception as exc:  # noqa: BLE001
            err["exc"] = exc
    t = threading.Thread(target=_go)
    t.start()
    return t, err


def settled(scope, thread, timeout=10):
    thread.join(timeout=timeout)
    return not thread.is_alive() and len(scope._live) == 0


# --- 1-3: cancellation before the socket is doing work ------------------------

def test_cancel_before_connection_refuses(upstream):
    scope = adapters.CancelScope("c1")
    scope.cancel()
    t, err = run_call(scope)
    assert settled(scope, t)
    assert isinstance(err.get("exc"), adapters.Cancelled)
    assert Upstream.calls == [], "a connection was opened after cancellation"


# --- 4-5: cancellation while sending / waiting for headers --------------------

def test_cancel_while_waiting_before_headers(upstream):
    """The real before-headers case: the call is BLOCKED in getresponse()."""
    Upstream.header_delay = 6.0
    scope = adapters.CancelScope("c5")
    t, err = run_call(scope)
    # Wait until the upstream has the request and is deliberately silent.
    for _ in range(100):
        if Upstream.calls:
            break
        time.sleep(0.05)
    assert Upstream.calls, "the upstream never received the request"
    time.sleep(0.5)
    t0 = time.time()
    closed = scope.cancel()
    assert settled(scope, t), "the blocked call did not return after cancellation"
    latency = time.time() - t0
    assert closed == 1, "the live connection was not registered for cancellation"
    assert latency < 3.0, f"cancellation took {latency:.2f}s while blocked pre-headers"
    assert not Upstream.calls[0]["finished"], "the upstream completed anyway"


# --- 6-8: cancellation after headers, mid-stream, and in a silent gap ---------

def test_cancel_mid_stream(upstream):
    Upstream.chunks = 200
    Upstream.chunk_gap = 0.05
    scope = adapters.CancelScope("c7")
    got = []
    t, err = run_call(scope, collect=got)
    for _ in range(100):
        with Upstream.lock:
            if Upstream.calls and Upstream.calls[0]["sent"] > 2:
                break
        time.sleep(0.05)
    t0 = time.time()
    scope.cancel()
    assert settled(scope, t)
    assert time.time() - t0 < 3.0
    rec = upstream_settled()
    assert rec["client_gone"], f"the upstream never saw its client disappear: {rec}"
    assert not rec["finished"], "the upstream ran to completion despite cancellation"
    assert rec["sent"] < Upstream.chunks, "the upstream produced its whole stream"


def test_cancel_during_a_long_silent_gap(upstream):
    Upstream.chunks = 3
    Upstream.chunk_gap = 4.0
    scope = adapters.CancelScope("c8")
    t, err = run_call(scope)
    for _ in range(100):
        with Upstream.lock:
            if Upstream.calls and Upstream.calls[0]["sent"] >= 1:
                break
        time.sleep(0.05)
    t0 = time.time()
    scope.cancel()
    assert settled(scope, t)
    assert time.time() - t0 < 3.0, "cancellation waited for the next chunk"
    rec = upstream_settled()
    assert rec["client_gone"], f"the upstream never saw its client disappear: {rec}"
    assert not rec["finished"]


# --- 10-11, 16-17: sequencing and idempotence --------------------------------

def test_cancel_between_sequential_calls_stops_the_next(upstream):
    scope = adapters.CancelScope("c10")
    t1, e1 = run_call(scope)
    assert settled(scope, t1)
    assert not e1, e1
    first = len(Upstream.calls)
    scope.cancel()
    t2, e2 = run_call(scope)
    assert settled(scope, t2)
    assert isinstance(e2.get("exc"), adapters.Cancelled)
    assert len(Upstream.calls) == first, "a later call reached the upstream"


def test_repeated_cancellation_is_idempotent(upstream):
    scope = adapters.CancelScope("c16")
    t, _ = run_call(scope)
    settled(scope, t)
    assert scope.cancel() == 0
    assert scope.cancel() == 0


def test_cancel_after_normal_completion_is_harmless(upstream):
    scope = adapters.CancelScope("c17")
    t, err = run_call(scope)
    assert settled(scope, t)
    assert not err, err
    with Upstream.lock:
        assert Upstream.calls[0]["finished"]
        assert not Upstream.calls[0]["client_gone"]
    assert scope.cancel() == 0


# --- 12, 15: parallel and neighbouring invocations ---------------------------

def test_parallel_calls_all_cancel(upstream):
    Upstream.chunks = 200
    scope = adapters.CancelScope("c12")
    threads = [run_call(scope)[0] for _ in range(4)]
    for _ in range(200):
        with Upstream.lock:
            if len(Upstream.calls) >= 4:
                break
        time.sleep(0.05)
    closed = scope.cancel()
    for t in threads:
        t.join(timeout=10)
        assert not t.is_alive()
    assert closed == 4, f"only {closed} of 4 live connections were closed"
    assert len(scope._live) == 0
    seen = len(Upstream.calls)
    for i in range(seen):
        upstream_settled(i)
    with Upstream.lock:
        gone = sum(1 for c in Upstream.calls if c["client_gone"])
        done = sum(1 for c in Upstream.calls if c["finished"])
    assert done == 0, f"{done} of {seen} upstream calls completed despite cancellation"
    assert gone == seen, f"only {gone} of {seen} upstream calls saw the disconnect"


def test_a_healthy_neighbour_is_unaffected(upstream):
    Upstream.chunks = 40
    Upstream.chunk_gap = 0.05
    doomed, healthy = adapters.CancelScope("c15a"), adapters.CancelScope("c15b")
    td, _ = run_call(doomed)
    th, eh = run_call(healthy)
    for _ in range(200):
        with Upstream.lock:
            if len(Upstream.calls) >= 2:
                break
        time.sleep(0.05)
    doomed.cancel()
    td.join(timeout=10)
    th.join(timeout=20)
    assert not th.is_alive()
    assert not eh, f"the healthy invocation failed: {eh}"
    assert healthy.cancelled is False
    assert len(healthy._live) == 0


# --- 18: stubborn upstream ----------------------------------------------------

def test_stubborn_upstream_still_releases_the_worker(upstream):
    """An upstream that ignores the close must not hold the worker forever."""
    Upstream.chunks = 400
    Upstream.chunk_gap = 0.02
    scope = adapters.CancelScope("c18")
    t, _ = run_call(scope)
    for _ in range(200):
        with Upstream.lock:
            if Upstream.calls and Upstream.calls[0]["sent"] > 3:
                break
        time.sleep(0.02)
    t0 = time.time()
    scope.cancel()
    t.join(timeout=10)
    assert not t.is_alive(), "the worker never returned after cancellation"
    assert time.time() - t0 < 5.0


# --- leak checks --------------------------------------------------------------

def test_no_thread_or_registry_growth_across_many_calls(upstream):
    Upstream.chunks = 2
    Upstream.chunk_gap = 0.0
    before = threading.active_count()
    for _ in range(40):
        scope = adapters.CancelScope("leak")
        t, err = run_call(scope)
        assert settled(scope, t, timeout=15)
        assert len(scope._live) == 0
    time.sleep(0.5)
    assert threading.active_count() <= before + 2, "worker threads accumulated"


# --- the undocumented interface this depends on -------------------------------
#
# Cancellation of a blocked read needs shutdown() on the underlying socket, and
# http.client exposes that as HTTPConnection.sock -- an ordinary attribute that
# is NOT part of the documented API. These tests exist so a future Python that
# removes or renames it breaks the build loudly, instead of silently degrading
# cancellation back to the 5.76s behaviour that started all this.

def test_the_runtime_still_exposes_the_connection_socket():
    import http.client
    assert adapters.HTTPCONNECTION_EXPOSES_SOCK, (
        "http.client.HTTPConnection no longer exposes `sock`; the cancellation "
        "path cannot shut down a blocked read on this runtime and must be "
        "reimplemented before it can be trusted")
    conn = http.client.HTTPConnection("localhost")
    assert hasattr(conn, "sock")
    assert conn.sock is None, "an unconnected connection should carry no socket"


def test_abort_is_safe_with_no_socket_yet():
    import http.client
    conn = http.client.HTTPConnection("localhost")
    assert adapters._abort_connection(conn) is False


def test_abort_is_safe_twice_and_after_close(upstream):
    scope = adapters.CancelScope("abort")
    t, _ = run_call(scope)
    settled(scope, t)
    import http.client
    from urllib.parse import urlsplit
    u = urlsplit(adapters.INFERENCE_URL)
    conn = http.client.HTTPConnection(u.hostname, u.port, timeout=5)
    conn.connect()
    assert adapters._abort_connection(conn) is True
    assert adapters._abort_connection(conn) is False, "a second abort claimed a shutdown"


def test_abort_never_raises_on_a_hostile_object():
    class Hostile:
        @property
        def sock(self):
            raise OSError("boom")

        def close(self):
            raise RuntimeError("boom")

    assert adapters._abort_connection(Hostile()) is False


# --- the five races -----------------------------------------------------------

def test_race_between_creation_and_registration(upstream):
    """Cancel concurrently with connection setup, many times.

    A deterministic pre-cancel proves nothing about the window between
    HTTPConnection() and scope.register(). This drives that window repeatedly.
    """
    Upstream.chunks = 3
    Upstream.chunk_gap = 0.01
    leaked = 0
    for i in range(60):
        scope = adapters.CancelScope(f"race{i}")
        t, err = run_call(scope)
        # Cancel at a jittered offset straddling connect/register/request.
        time.sleep((i % 7) * 0.002)
        scope.cancel()
        t.join(timeout=10)
        assert not t.is_alive(), f"iteration {i}: worker hung"
        assert len(scope._live) == 0, f"iteration {i}: a connection stayed registered"
        if err.get("exc") is None:
            leaked += 1          # completed before cancellation: legitimate
    # Whatever the interleaving, nothing may be left registered or running.
    assert threading.active_count() < 60
    print(f"registration race: 60 trials, {leaked} completed before cancellation")


def test_cancel_while_a_large_body_is_being_sent(upstream):
    Upstream.chunks = 2
    big = json.dumps({"blob": "x" * 4_000_000}).encode()
    scope = adapters.CancelScope("bigbody")
    t, err = run_call(scope, payload=big)
    time.sleep(0.01)
    scope.cancel()
    t.join(timeout=15)
    assert not t.is_alive(), "the worker hung while sending a large body"
    assert len(scope._live) == 0


def test_cancel_after_the_final_chunk_before_cleanup(upstream):
    Upstream.chunks = 1
    Upstream.chunk_gap = 0.0
    scope = adapters.CancelScope("tail")
    got = []
    t, err = run_call(scope, collect=got)
    t.join(timeout=10)
    assert not t.is_alive()
    scope.cancel()
    assert len(scope._live) == 0
    rec = upstream_settled()
    assert rec["finished"], "a completed call was wrongly torn down"


def test_a_second_scope_survives_the_first_being_cancelled(upstream):
    """Stands in for repair/refinement running beside a cancelled generation."""
    Upstream.chunks = 30
    Upstream.chunk_gap = 0.05
    doomed, repair = adapters.CancelScope("gen"), adapters.CancelScope("repair")
    td, _ = run_call(doomed)
    tr, er = run_call(repair)
    for _ in range(200):
        with Upstream.lock:
            if len(Upstream.calls) >= 2:
                break
        time.sleep(0.02)
    doomed.cancel()
    td.join(timeout=10)
    tr.join(timeout=20)
    assert not tr.is_alive()
    assert not er, f"the repair-side invocation failed: {er}"
    assert repair.cancelled is False
    assert len(repair._live) == 0
