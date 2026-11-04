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
    assert "scripts/install/atlas-llama-macos.sh" in detail
    assert "will not start the CUDA container" in detail


def test_nonmetal_log_inspection_uses_backend_overlay(monkeypatch, tmp_path):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "deploy" / "compose").mkdir(parents=True)
    (tmp_path / "deploy/compose/docker-compose.rocm.yml").write_text("services: {}\n")
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
        "deploy/compose/docker-compose.rocm.yml", "logs", "--tail=200", "llama-server",
    ]]


def test_metal_names_the_model_a_healthy_server_serves_instead(monkeypatch, tmp_path):
    monkeypatch.setattr(onboard, "_serving_this", lambda url, model: (True, False))
    monkeypatch.setattr(onboard, "_loaded_model_name", lambda url: "other.gguf")
    ready, detail = onboard._arch_supported(
        str(tmp_path), {"ATLAS_BACKEND": "metal"}, "model.gguf", start=True, color=False)
    assert ready is False
    assert "serves 'other.gguf', not 'model.gguf'" in detail


UP = ["docker", "compose", "up", "-d", "llama-server", "--no-deps"]
RECREATE = ["docker", "compose", "up", "-d", "--force-recreate", "llama-server", "--no-deps"]
LOGS = ["docker", "compose", "logs", "--tail=200", "llama-server"]
ARCH_ERROR = "llama_model_load: error loading model: unknown architecture 'novel'"


# answers: what each health check says in turn, (healthy, serving this model).
@pytest.mark.parametrize("start, answers, logs, ready, detail, commands", [
    pytest.param(True, [(True, False), (True, True)], "", True, "", [RECREATE],
                 id="a-server-with-another-model-is-recreated-and-serves"),
    pytest.param(True, [(False, False), (False, False)], ARCH_ERROR, False, "unknown architecture", [UP, LOGS],
                 id="a-stopped-server-is-started-and-its-load-error-read"),
    pytest.param(True, [(False, False)] * 32, "", False, "may still be loading", [UP] + [LOGS] * 31,
                 id="a-server-that-never-serves-is-polled-30-times"),
    pytest.param(False, [(False, False)], ARCH_ERROR, False, "unknown architecture", [LOGS],
                 id="not-started-the-logs-say-why"),
    pytest.param(False, [(False, False), (True, True)], "", True, "", [LOGS],
                 id="not-started-it-came-up-meanwhile"),
    pytest.param(False, [(False, False), (False, False)], "", False, "may still be loading", [LOGS],
                 id="not-started-and-nothing-says-why"),
])
def test_a_container_server_is_started_waited_for_and_its_logs_read(
        monkeypatch, tmp_path, start, answers, logs, ready, detail, commands):
    told = iter(answers)
    monkeypatch.setattr(onboard, "_serving_this", lambda url, model: next(told))
    monkeypatch.setattr(onboard.time, "sleep", lambda seconds: None)
    calls = []

    def run(cmd, **kwargs):
        calls.append(cmd)
        return 0, logs, ""

    monkeypatch.setattr(onboard, "_run", run)
    got_ready, got_detail = onboard._arch_supported(
        str(tmp_path), {"ATLAS_BACKEND": "cuda"}, "model.gguf", start=start, color=False)
    assert got_ready is ready
    assert detail in got_detail
    assert calls == commands


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
