"""The V3 service takes a request body up to MAX_REQUEST_BODY_BYTES, refuses a
larger one with 413, and a Content-Length that is not a whole number of 0 or
more with 400, before any route reads it."""

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
    (str(main.MAX_REQUEST_BODY_BYTES), 200),
    (str(main.MAX_REQUEST_BODY_BYTES + 1), 413),
    ("-1", 400),
    ("abc", 400),
])
def test_the_request_body_length_is_checked_before_any_route(monkeypatch, length, status):
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "")
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        # Only the header is sent: /health reads no body, so the bound decides alone.
        connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=10)
        connection.putrequest("POST", "/health")
        connection.putheader("Content-Length", length)
        connection.endheaders()
        assert connection.getresponse().status == status
        connection.close()
    finally:
        server.shutdown()
        server.server_close()
