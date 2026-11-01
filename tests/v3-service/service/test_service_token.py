"""The V3 service's check of the service token. With a token configured, a request with no token or another token is
refused, and a request with the token reaches its route. /health stays open for the stack's probes."""
import json
import threading
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
