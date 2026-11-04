"""Tests for atlas.client seams against the real service contracts."""

import importlib

import pytest

from atlas import client


@pytest.mark.parametrize("health_reply, embedding_reply, expected", [
    pytest.param({"status": "ok"}, [{"embedding": [0.1, 0.2, 0.3]}],
                 (True, 3, False), id="pooled vector"),
    pytest.param({"status": "ok"}, [{"embedding": [[0.1, 0.2, 0.3, 0.4],
                                                   [0.5, 0.6, 0.7, 0.8]]}],
                 (True, 4, False), id="one vector per token"),
    pytest.param({"status": "ok"},
                 [{"embedding": [0.1, 0.2, 0.3], "hidden_states_dim": 3}],
                 (True, 3, True), id="hidden-states patch present"),
    pytest.param(None, None, (False, 0, False), id="server not reachable"),
])
def test_probe_llama_reads_reachability_dim_and_patch(monkeypatch, health_reply,
                                                      embedding_reply, expected):
    """`atlas lens check` and `atlas asa check` judge the loaded model by
    these fields: an embedding dim that differs from the artifact's is a
    mismatch, and lens training needs the hidden-states patch."""
    monkeypatch.setattr(client, "get_json_or_none",
                        lambda url, timeout: health_reply if url.endswith("/health") else {})
    monkeypatch.setattr(client, "post_json_or_none",
                        lambda url, body, timeout: embedding_reply)

    probe = client.probe_llama("http://llama:8080")

    got = (probe.reachable, probe.embedding_dim, probe.has_hidden_states_patch)
    assert got == expected


@pytest.mark.parametrize("props_reply, expected", [
    pytest.param({"n_layer": 48, "model_path": "/models/a.gguf"}, (48, "/models/a.gguf"), id="n_layer-and-path"),
    pytest.param({"default_generation_settings": {"n_layer": 32}, "model_name": "b"}, (32, "b"),
                 id="older-field-names"),
    pytest.param({"model": "c"}, (0, "c"), id="no-layer-count"),
    pytest.param(None, (0, ""), id="no-props"),
])
def test_probe_llama_reads_the_layer_count_and_model_name(monkeypatch, props_reply, expected):
    monkeypatch.setattr(client, "get_json_or_none",
                        lambda url, timeout: {"status": "ok"} if url.endswith("/health") else props_reply)
    monkeypatch.setattr(client, "post_json_or_none",
                        lambda url, body, timeout: [{"embedding": [0.1]}])

    probe = client.probe_llama("http://llama:8080")

    assert (probe.n_layers, probe.model_name) == expected


@pytest.mark.parametrize("embedding_reply", [
    pytest.param([], id="empty-list"),
    pytest.param({"error": "embeddings disabled"}, id="an-object"),
    pytest.param([{"embedding": []}], id="empty-vector"),
])
def test_a_server_without_an_embedding_is_reachable_but_named(monkeypatch, embedding_reply):
    monkeypatch.setattr(client, "get_json_or_none",
                        lambda url, timeout: {"status": "ok"} if url.endswith("/health") else {})
    monkeypatch.setattr(client, "post_json_or_none", lambda url, body, timeout: embedding_reply)

    probe = client.probe_llama("http://llama:8080")

    assert probe.reachable
    assert probe.embedding_dim == 0
    assert "didn't return an embedding" in probe.error


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
