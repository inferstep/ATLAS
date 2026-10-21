"""Tests for atlas.client seams against the real service contracts."""

import importlib

import pytest

from atlas import client


@pytest.mark.parametrize("response", [
    pytest.param({"success": True, "stdout": "hi\n", "stderr": ""}, id="executor reports success"),
    pytest.param({"passed": True, "stdout": "hi\n", "stderr": ""}, id="older one reports passed"),
])
def test_run_sandbox_reads_the_pass_result(monkeypatch, response):
    """The sandbox executor's ExecuteResponse reports `success`; older builds
    reported `passed`, which is still read when `success` is absent. The
    request must not send fields ExecuteRequest doesn't define, like stdin."""
    sent = {}

    def fake_post(url, body, timeout=120):
        sent.update(body)
        return response

    monkeypatch.setattr(client, "_post", fake_post)
    assert client.run_sandbox("print('hi')", "assert True") == (True, "hi\n", "")
    assert sent["code"] == "print('hi')"
    assert sent["test_code"] == "assert True"
    assert "stdin" not in sent


def test_run_sandbox_returns_error_on_connection_failure(monkeypatch):
    def boom(url, body, timeout=120):
        raise OSError("connection refused")

    monkeypatch.setattr(client, "_post", boom)
    passed, stdout, stderr = client.run_sandbox("code")
    assert passed is False
    assert "connection refused" in stderr


@pytest.mark.parametrize("models_reply, expected_model", [
    pytest.param({"data": [{"id": "/models/Qwen3.5-9B-Q6_K.gguf"}]}, "Qwen3.5-9B-Q6_K.gguf",
                 id="id from v1 models"),
    pytest.param(OSError("404"), "unknown", id="unknown when v1 models fails"),
])
def test_check_llama_reads_model_id_from_v1_models(monkeypatch, models_reply, expected_model):
    """llama-server's /health carries no model metadata; the id comes
    from /v1/models (fallback: "unknown")."""
    def fake_get(url, timeout=10):
        if url.endswith("/health"):
            return {"status": "ok"}
        assert url.endswith("/v1/models"), url
        if isinstance(models_reply, Exception):
            raise models_reply
        return models_reply

    monkeypatch.setattr(client, "_get", fake_get)
    assert client.check_llama() == (True, expected_model)


def test_chat_stream_bridges_reasoning_content_into_think_tags(monkeypatch):
    """reasoning_content deltas surface as literal <think>…</think> so
    callers keep a single thinking-detection path."""
    events = [
        b'data: {"choices":[{"delta":{"reasoning_content":"pondering"},'
        + b'"finish_reason":null}]}\n',
        b'data: {"choices":[{"delta":{"content":"print(42)"},'
        + b'"finish_reason":null}]}\n',
        b'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n',
        b'data: [DONE]\n',
    ]

    class Response:
        """Hands out one SSE line per read(), like a chunked HTTP body."""
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self, _n):
            return events.pop(0) if events else b""

    monkeypatch.setattr(client.urllib.request, "urlopen",
                        lambda req, timeout=900: Response())
    tokens = [text for text, _done in client.chat_stream(
        [{"role": "user", "content": "q"}])]
    assert tokens == ["<think>", "pondering", "</think>", "print(42)"]


@pytest.mark.parametrize("env, expected", [
    pytest.param({"ATLAS_LENS_URL": "http://current:1111",
                  "ATLAS_RAG_URL": "http://deprecated:2222"},
                 "http://current:1111", id="current name wins over deprecated"),
    pytest.param({"ATLAS_RAG_URL": "http://deprecated:2222"},
                 "http://deprecated:2222", id="deprecated name alone still works"),
])
def test_lens_url_env_names(monkeypatch, env, expected):
    """ATLAS_LENS_URL must win: it once lost to ATLAS_RAG_URL, the name it
    replaced. An .env carrying only ATLAS_RAG_URL keeps working. The URLs
    bind at import, so the module is reloaded under each environment."""
    monkeypatch.delenv("ATLAS_LENS_URL", raising=False)
    monkeypatch.delenv("ATLAS_RAG_URL", raising=False)
    for name, value in env.items():
        monkeypatch.setenv(name, value)
    try:
        assert importlib.reload(client).LENS_URL == expected
    finally:
        monkeypatch.undo()  # restore the real environment
        importlib.reload(client)  # and rebind the URLs to it
