import importlib.util
import json
import sys
from pathlib import Path

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


def test_the_prompt_build_sends_the_installs_token_and_not_to_a_redirect(tmp_path, monkeypatch):
    module = _load("build_cvector_prompts.py")
    token_file = tmp_path / "service-token"
    token_file.write_text("the-token\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(token_file))
    sent = []
    monkeypatch.setattr(module, "urlopen", _recording_urlopen(sent))

    module.render({"user": "edit a function", "assistant_prefix": ""}, "http://llama-server:8080")

    assert sent[0].unredirected_hdrs["Authorization"] == "Bearer the-token"
    assert "Authorization" not in sent[0].headers


def test_the_prompt_build_sends_no_token_on_an_install_without_one(tmp_path, monkeypatch):
    module = _load("build_cvector_prompts.py")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tmp_path / "no-such-file"))
    sent = []
    monkeypatch.setattr(module, "urlopen", _recording_urlopen(sent))

    module.render({"user": "edit a function", "assistant_prefix": ""}, "http://llama-server:8080")

    assert not sent[0].has_header("Authorization")
