"""The V3 service takes a request body up to MAX_REQUEST_BODY_BYTES and refuses
a larger one with 413, before any route reads it."""

import http.client
import sys
import threading
from http.server import ThreadingHTTPServer
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters
import main


@pytest.mark.parametrize("length, status", [
    (main.MAX_REQUEST_BODY_BYTES, 200),
    (main.MAX_REQUEST_BODY_BYTES + 1, 413),
])
def test_a_request_body_over_the_bound_is_refused(monkeypatch, length, status):
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "")
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        # Only the header is sent: /health reads no body, so the bound decides alone.
        connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=10)
        connection.putrequest("POST", "/health")
        connection.putheader("Content-Length", str(length))
        connection.endheaders()
        assert connection.getresponse().status == status
        connection.close()
    finally:
        server.shutdown()
        server.server_close()
