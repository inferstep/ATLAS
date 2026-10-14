"""The lens sends its service token only to the model server's addresses, also after a redirect."""
import http.server
import os
import sys
import threading
import urllib.request
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from geometric_lens import auth_token


def stand_in(name, seen, moved_to=""):
    """A local server that writes down the Authorization of each request in `seen`. A path that starts with /moved
    is sent on to `moved_to` + /after + the path."""
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            seen[name + self.path] = self.headers.get("Authorization", "")
            self.send_response(302 if self.path.startswith("/moved") else 200)
            if self.path.startswith("/moved"):
                self.send_header("Location", f"{moved_to}/after{self.path}")
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_port}"


def test_the_token_goes_only_to_the_model_server_and_not_on_to_another_address(monkeypatch):
    seen = {}
    other, other_address = stand_in("other", seen)
    model_server, model_address = stand_in("model", seen, moved_to=other_address)
    # The model server is the stand-in "model". The other one is on the same host, on another port.
    monkeypatch.setenv("LLAMA_URL", model_address)
    monkeypatch.delenv("LLAMA_EMBED_URL", raising=False)
    monkeypatch.setattr(auth_token, "SERVICE_TOKEN", "a-stand-in-token")
    monkeypatch.setattr(urllib.request, "_opener", None)  # the global opener of the test goes away after it
    auth_token.install_urllib_opener()

    for address in (f"{model_address}/asked", f"{other_address}/asked", f"{model_address}/moved"):
        urllib.request.urlopen(address, timeout=10).close()
    # A call to the model server that sets the header itself, as model_headers() does.
    own_header = urllib.request.Request(f"{model_address}/moved-with-its-own-header", headers=auth_token.auth_headers())
    urllib.request.urlopen(own_header, timeout=10).close()
    model_server.shutdown()
    other.shutdown()

    assert seen == {
        "model/asked": "Bearer a-stand-in-token",
        "other/asked": "",
        "model/moved": "Bearer a-stand-in-token",
        "other/after/moved": "",
        "model/moved-with-its-own-header": "Bearer a-stand-in-token",
        "other/after/moved-with-its-own-header": "",
    }


def test_a_token_file_that_cannot_be_read_stops_the_lens_and_one_that_is_not_there_is_no_token(tmp_path, monkeypatch):
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tmp_path / "not-there"))
    assert auth_token._load() == ""

    if os.geteuid() == 0:
        pytest.skip("root reads a file of mode 000, so the case of a file that cannot be read needs a user that is not root")
    unreadable = tmp_path / "service-token"
    unreadable.write_text("a-stand-in-token\n")
    unreadable.chmod(0)
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(unreadable))
    with pytest.raises(SystemExit) as stop:
        auth_token._load()
    assert f"the service token file {unreadable} cannot be read (Permission denied), so the lens stops" in str(stop.value)
