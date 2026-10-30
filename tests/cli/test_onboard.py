"""Model onboarding tests for backend-specific inference lifecycles."""

import pytest

from atlas.commands import fit as fit_module
from atlas.commands import model as model_cmd
from atlas.commands import onboard


def test_metal_onboarding_never_starts_cuda_container(monkeypatch, tmp_path):
    monkeypatch.setattr(onboard, "_serving_this", lambda url, model: (False, False))

    def unexpected_run(*args, **kwargs):
        raise AssertionError("Metal onboarding must not invoke docker compose")

    monkeypatch.setattr(onboard, "_run", unexpected_run)
    ready, detail = onboard._arch_supported(
        str(tmp_path), {"ATLAS_BACKEND": "metal"}, "model.gguf",
        start=True, color=False,
    )
    assert ready is False
    assert "atlas-llama-macos.sh" in detail
    assert "will not start the CUDA container" in detail


def test_nonmetal_log_inspection_uses_backend_overlay(monkeypatch, tmp_path):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "docker-compose.rocm.yml").write_text("services: {}\n")
    monkeypatch.setattr(onboard, "_serving_this", lambda url, model: (False, False))
    calls = []

    def capture(cmd, **kwargs):
        calls.append(cmd)
        return 0, "", ""

    monkeypatch.setattr(onboard, "_run", capture)
    ready, _ = onboard._arch_supported(
        str(tmp_path), {"ATLAS_BACKEND": "rocm"}, "model.gguf",
        start=False, color=False,
    )
    assert ready is False
    assert calls == [[
        "docker", "compose", "-f", "docker-compose.yml", "-f",
        "docker-compose.rocm.yml", "logs", "--tail=200", "llama-server",
    ]]


@pytest.mark.parametrize("flags, expected_env_writes, expected_texts", [
    (["--apply"],
     [{"ATLAS_MODEL_FILE": "My-Model-Q4_K_M.gguf", "ATLAS_MODEL_NAME": "My-Model-Q4_K_M"}],
     ["to /fake/.env", "Re-run `atlas onboard`"]),
    ([],
     [],
     ["set ATLAS_MODEL_FILE + ATLAS_MODEL_NAME in .env", "pass --apply"]),
], ids=["apply-writes-env", "no-apply-keeps-hand-edit-instructions"])
def test_url_download_writes_env_only_with_apply(
        monkeypatch, capsys, flags, expected_env_writes, expected_texts):
    """After `atlas onboard --url ...` downloads the model, --apply writes
    ATLAS_MODEL_FILE + ATLAS_MODEL_NAME into .env. Without it (declined or
    not interactive) .env is left alone and the hand-edit steps are printed."""
    monkeypatch.setattr(model_cmd, "main", lambda argv: 0)
    env_writes = []

    def fake_write_env(values):
        env_writes.append(values)
        return "/fake/.env"
    monkeypatch.setattr(fit_module, "_write_env", fake_write_env)

    rc = onboard.main(["--url", "https://example.com/My-Model-Q4_K_M.gguf",
                       *flags, "--no-color"])
    assert rc == 0
    assert env_writes == expected_env_writes
    out = capsys.readouterr().out
    for text in expected_texts:
        assert text in out


@pytest.mark.parametrize("loaded, model_file, expected", [
    pytest.param("/models/Qwen3-14B-Q4_K_M.gguf", "Qwen3-14B-Q4_K_M.gguf", True,
                 id="path-form-is-the-same-model"),
    # ATLAS_MODEL_NAME is the file name without .gguf; the dot in "2.5" stays.
    pytest.param("Qwen2.5-Coder-14B-Q4_K_M", "Qwen2.5-Coder-14B-Q4_K_M.gguf", True,
                 id="name-without-gguf-is-the-same-model"),
    pytest.param("Qwen3-14B-Q4_K_M-abliterated.gguf", "Qwen3-14B-Q4_K_M.gguf", False,
                 id="longer-name-is-another-model"),
])
def test_names_match(loaded, model_file, expected):
    """onboard asks llama-server which model it serves and compares that
    name with ATLAS_MODEL_FILE. A wrong "no" restarts the server and
    reports a rebuild; a wrong "yes" passes a model that was never loaded."""
    assert onboard._names_match(loaded, model_file) is expected
