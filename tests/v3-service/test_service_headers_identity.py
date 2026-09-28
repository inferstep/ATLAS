"""V3 forwards its bound request and invocation identity on service calls.

The Lens forwards whatever pair it receives to the model server, so V3 must
hand it the invocation id it already binds per /v3/generate. Attribution only.
"""
import os
import sys
import threading

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "v3-service"))

import adapters  # noqa: E402
import structured_log as sl  # noqa: E402


def _reset():
    sl.bind_identity("", "")


def test_no_bound_identity_gives_only_content_type():
    _reset()
    assert adapters._service_headers() == {"Content-Type": "application/json"}


def test_bound_pair_is_forwarded_from_the_contextvars():
    _reset()
    sl.bind_identity("req-9", "inv-9")
    try:
        h = adapters._service_headers()
        assert h[adapters.REQUEST_ID_HEADER] == "req-9"
        assert h[adapters.INVOCATION_ID_HEADER] == "inv-9"
    finally:
        _reset()


def test_explicit_arguments_win_over_the_contextvars():
    _reset()
    sl.bind_identity("req-ctx", "inv-ctx")
    try:
        h = adapters._service_headers("req-arg", "inv-arg")
        assert (h[adapters.REQUEST_ID_HEADER], h[adapters.INVOCATION_ID_HEADER]) == ("req-arg", "inv-arg")
        h = adapters._service_headers("req-arg")
        assert (h[adapters.REQUEST_ID_HEADER], h[adapters.INVOCATION_ID_HEADER]) == ("req-arg", "inv-ctx")
    finally:
        _reset()


def test_partial_binding_stays_partial():
    _reset()
    sl.bind_identity("req-only", "")
    try:
        h = adapters._service_headers()
        assert h[adapters.REQUEST_ID_HEADER] == "req-only" and adapters.INVOCATION_ID_HEADER not in h
    finally:
        _reset()


def test_worker_thread_inherits_nothing():
    _reset()
    sl.bind_identity("req-main", "inv-main")
    out = {}

    def worker():
        out["headers"] = adapters._service_headers()
    t = threading.Thread(target=worker)
    t.start()
    t.join()
    _reset()
    assert adapters.REQUEST_ID_HEADER not in out["headers"] and adapters.INVOCATION_ID_HEADER not in out["headers"]


def test_headers_carry_no_content(monkeypatch):
    _reset()
    sl.bind_identity("r", "i")
    try:
        assert set(adapters._service_headers()) == {"Content-Type", adapters.REQUEST_ID_HEADER, adapters.INVOCATION_ID_HEADER}
    finally:
        _reset()
