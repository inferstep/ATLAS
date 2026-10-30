import importlib.util
import json
import sys
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))


def _load(name):
    path = ROOT / "asa_calibration" / name
    spec = importlib.util.spec_from_file_location(name.replace(".py", ""), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class _Response:
    def __init__(self, prompt):
        self.prompt = prompt

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self):
        return json.dumps({"prompt": self.prompt}).encode()


def _recording_urlopen(sent):
    def urlopen(request, timeout):
        sent.append(request)
        return _Response("TEMPLATE")
    return urlopen


def test_hidden_state_builder_uses_loaded_models_template(monkeypatch):
    module = _load("build_steering_vector.py")
    module._render_user_context.cache_clear()
    monkeypatch.setattr("geometric_lens.model_transport.urlopen",
                        lambda *_a, **_k: _Response("MODEL_TEMPLATE:"))
    rendered = module.render_prompt({
        "user": "edit a function",
        "assistant_prefix": '{"name":"structural_edit"',
    })
    assert rendered == 'MODEL_TEMPLATE:{"name":"structural_edit"'
    assert "<|im_start|>" not in rendered


def test_legacy_cvector_builder_uses_loaded_models_template(monkeypatch):
    module = _load("build_cvector_prompts.py")
    monkeypatch.setattr(module, "urlopen", lambda *_a, **_k: _Response("MODEL\nTEMPLATE\n"))
    rendered = module.render({
        "user": "edit a function",
        "assistant_prefix": '{"name":"structural_edit"',
    }, "http://llama-server:8080")
    assert rendered == 'MODEL\\nTEMPLATE\\n{"name":"structural_edit"'
    assert "<|im_start|>" not in rendered


def test_the_steering_build_sends_the_token(monkeypatch):
    module = _load("build_steering_vector.py")
    monkeypatch.setattr("geometric_lens.auth_token.SERVICE_TOKEN", "the-token")
    sent = []
    monkeypatch.setattr("geometric_lens.model_transport.urlopen", _recording_urlopen(sent))

    module._render_user_context("edit a function")

    assert sent[0].get_header("Authorization") == "Bearer the-token"


def test_the_prompt_build_sends_the_installs_token(tmp_path, monkeypatch):
    module = _load("build_cvector_prompts.py")
    token_file = tmp_path / "service-token"
    token_file.write_text("the-token\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(token_file))
    sent = []
    monkeypatch.setattr(module, "urlopen", _recording_urlopen(sent))

    module.render({"user": "edit a function", "assistant_prefix": ""}, "http://llama-server:8080")

    assert sent[0].get_header("Authorization") == "Bearer the-token"


def test_the_prompt_build_sends_no_token_on_an_install_without_one(tmp_path, monkeypatch):
    module = _load("build_cvector_prompts.py")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tmp_path / "no-such-file"))
    sent = []
    monkeypatch.setattr(module, "urlopen", _recording_urlopen(sent))

    module.render({"user": "edit a function", "assistant_prefix": ""}, "http://llama-server:8080")

    assert not sent[0].has_header("Authorization")


@pytest.fixture
def redirect_to_another_address():
    """A server that redirects every call to a second address, which records the token it is sent."""
    seen = []

    class Other(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            seen.append(self.headers.get("Authorization"))
            body = json.dumps({"prompt": "TEMPLATE"}).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    other = ThreadingHTTPServer(("127.0.0.1", 0), Other)

    class Redirect(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            self.rfile.read(int(self.headers["Content-Length"]))
            self.send_response(302)
            self.send_header("Location", f"http://127.0.0.1:{other.server_address[1]}/")
            self.send_header("Content-Length", "0")
            self.end_headers()

    redirect = ThreadingHTTPServer(("127.0.0.1", 0), Redirect)
    for server in (other, redirect):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{redirect.server_address[1]}", seen
    for server in (other, redirect):
        server.shutdown()
        server.server_close()
    urllib.request.install_opener(None)


def test_the_steering_builds_token_does_not_follow_a_redirect(redirect_to_another_address, monkeypatch):
    url, seen = redirect_to_another_address
    monkeypatch.setenv("LLAMA_URL", url)
    monkeypatch.setenv("LLAMA_EMBED_URL", url)
    monkeypatch.setattr("geometric_lens.auth_token.SERVICE_TOKEN", "the-token")
    module = _load("build_steering_vector.py")

    module._render_user_context("edit a function")

    assert seen == [None]


def test_the_prompt_builds_token_does_not_follow_a_redirect(redirect_to_another_address, tmp_path, monkeypatch):
    url, seen = redirect_to_another_address
    token_file = tmp_path / "service-token"
    token_file.write_text("the-token\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(token_file))
    module = _load("build_cvector_prompts.py")

    module.render({"user": "edit a function", "assistant_prefix": ""}, url)

    assert seen == [None]
