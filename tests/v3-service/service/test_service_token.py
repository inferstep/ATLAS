"""The V3 service's check of the service token. With a token configured, a request with no token or another token is
refused, and a request with the token reaches its route. /health stays open for the stack's probes."""
import json
import socket
import threading
import time
import urllib.error
import urllib.request
from http.server import ThreadingHTTPServer

import pytest

import adapters
import main

TOKEN = "a-stand-in-token"
SOURCE = json.dumps({"path": "app.py", "source": "def f():\n    return 1\n"}).encode()


@pytest.mark.parametrize("method, path, header, status, says", [
    pytest.param("POST", "/internal/outline", None, 401, "unauthorized", id="no token is refused"),
    pytest.param("POST", "/internal/outline", "Bearer another-token", 401, "unauthorized", id="another token is refused"),
    pytest.param("POST", "/internal/outline", f"Bearer {TOKEN}", 200, '"symbols"', id="the token reaches the route"),
    pytest.param("GET", "/health", None, 200, '"status": "ok"', id="the health probe stays open"),
])
def test_with_a_token_configured_only_a_request_with_the_token_reaches_a_route(monkeypatch, method, path, header, status, says):
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", TOKEN)
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    headers = {"Content-Type": "application/json", **({"Authorization": header} if header else {})}
    request = urllib.request.Request(f"http://127.0.0.1:{server.server_port}{path}", method=method, headers=headers,
                                     data=SOURCE if method == "POST" else None)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            got, text = response.status, response.read().decode("utf-8")
    except urllib.error.HTTPError as error:
        got, text = error.code, error.read().decode("utf-8")
    finally:
        server.shutdown()
        server.server_close()
    assert (got, says in text) == (status, True), text
    # An answer never holds the token.
    assert TOKEN not in text


def test_a_refused_request_has_its_body_read_before_the_connection_closes(monkeypatch):
    # A connection that closes on a body still unread is reset, and the reset can reach the client
    # before it reads the answer. So the service reads the body it was told of, then closes.
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", TOKEN)
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with socket.create_connection(("127.0.0.1", server.server_port), timeout=10) as client:
            client.sendall(b"POST /internal/outline HTTP/1.1\r\nHost: v3\r\nContent-Length: 5\r\n\r\n")
            answer = b""
            while not answer.endswith(b"}"):
                answer += client.recv(65536)
            client.settimeout(0.3)
            with pytest.raises(socket.timeout):
                client.recv(1)  # still open: the service waits for the body
            client.settimeout(10)
            client.sendall(b"12345")
            after_the_body = client.recv(1)
    finally:
        server.shutdown()
        server.server_close()
    assert answer.startswith(b"HTTP/1.0 401 "), answer[:80]
    assert b"Connection: close" in answer
    assert after_the_body == b""  # the body read, the service closed


def test_a_refused_client_that_sends_its_body_slowly_is_cut_off_at_the_time_limit(monkeypatch):
    monkeypatch.setattr(adapters, "SERVICE_TOKEN", TOKEN)
    server = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    closed = False
    try:
        with socket.create_connection(("127.0.0.1", server.server_port), timeout=10) as client:
            client.sendall(b"POST /internal/outline HTTP/1.1\r\nHost: v3\r\nContent-Length: 1000\r\n\r\n")
            answer = b""
            while not answer.endswith(b"}"):
                answer += client.recv(65536)
            answered = time.monotonic()
            client.settimeout(0.2)
            while not closed and time.monotonic() - answered < 5:
                try:
                    client.sendall(b"x")  # one byte at a time, each well inside the limit
                    closed = client.recv(1) == b""
                except socket.timeout:
                    pass
                except OSError:
                    closed = True
            took = time.monotonic() - answered
    finally:
        server.shutdown()
        server.server_close()
    assert answer.startswith(b"HTTP/1.0 401 "), answer[:80]
    assert closed, "the service kept the refused connection open for 5 s"
    assert took < main.REFUSED_BODY_DRAIN_SECONDS + 1, took
