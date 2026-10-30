"""The steering scripts call llama-server's /apply-template, which refuses a call without the token."""
import importlib.util
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def load_script(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "geometric-lens" / "asa_calibration" / f"{name}.py")
    script = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(script)
    return script


class Recorder(BaseHTTPRequestHandler):
    authorization = []

    def log_message(self, *args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers["Content-Length"]))
        Recorder.authorization.append(self.headers.get("Authorization"))
        body = json.dumps({"prompt": "rendered"}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


@pytest.fixture
def llama_url():
    Recorder.authorization = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), Recorder)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{server.server_address[1]}"
    server.shutdown()
    server.server_close()


def test_the_steering_build_sends_the_token(llama_url, monkeypatch):
    pytest.importorskip("gguf")
    monkeypatch.syspath_prepend(str(ROOT / "geometric-lens"))
    monkeypatch.setenv("LLAMA_EMBED_URL", llama_url)
    monkeypatch.setattr("geometric_lens.auth_token.SERVICE_TOKEN", "the-token")

    assert load_script("build_steering_vector")._render_user_context("hi") == "rendered"

    assert Recorder.authorization == ["Bearer the-token"]


def test_the_prompt_build_sends_the_hosts_token(llama_url, tmp_path, monkeypatch):
    token_file = tmp_path / "service-token"
    token_file.write_text("the-token\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(token_file))

    rendered = load_script("build_cvector_prompts").render({"user": "hi", "assistant_prefix": ""}, llama_url)

    assert rendered == "rendered"
    assert Recorder.authorization == ["Bearer the-token"]


def test_the_prompt_build_sends_no_token_on_an_install_without_one(llama_url, tmp_path, monkeypatch):
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tmp_path / "no-such-file"))

    load_script("build_cvector_prompts").render({"user": "hi", "assistant_prefix": ""}, llama_url)

    assert Recorder.authorization == [None]
