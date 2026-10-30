"""Proxy runtime tests — compose lifecycle behind ensure_proxy()."""

import io
import os
import urllib.request
from types import SimpleNamespace

import pytest

from atlas import runtime


def _metal_root(tmp_path):
    """A checkout on the Metal backend, so compose needs the macOS overlay."""
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "docker-compose.macos.yml").write_text("services: {}\n")
    (tmp_path / ".env").write_text("ATLAS_BACKEND=metal\n")
    return str(tmp_path)


def test_workspace_recreate_keeps_macos_overlay(monkeypatch, tmp_path):
    root = _metal_root(tmp_path)
    calls = []

    def capture(cmd, **kwargs):
        calls.append(cmd)
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setattr(runtime.subprocess, "run", capture)
    monkeypatch.setattr(runtime, "_check_url", lambda *args, **kwargs: True)
    assert runtime._recreate_docker_proxy(root, str(tmp_path / "project")) is True
    assert calls[0][:6] == [
        "docker", "compose", "-f", "docker-compose.yml", "-f",
        "docker-compose.macos.yml",
    ]
    assert "llama-server" not in calls[0]


def test_compose_ownership_check_keeps_macos_overlay(monkeypatch, tmp_path):
    root = _metal_root(tmp_path)
    calls = []

    def capture(cmd, **kwargs):
        calls.append(cmd)
        return SimpleNamespace(returncode=0, stdout="atlas-proxy\n", stderr="")

    monkeypatch.setattr(runtime.shutil, "which", lambda name: "/usr/bin/docker")
    monkeypatch.setattr(runtime.subprocess, "run", capture)
    assert runtime._docker_compose_owns_proxy(root) is True
    assert "docker-compose.macos.yml" in calls[0]


@pytest.mark.parametrize(
    ("health_body", "capabilities", "supports_demo"),
    [
        # A proxy older than the capability contract.
        (b'{"status":"ok"}', set(), False),
        (b"not json", set(), False),
        (b'{"capabilities":["demo_raw_completion_v1"]}',
         {"demo_raw_completion_v1"}, True),
    ],
    ids=["old-proxy", "malformed-answer", "demo-contract"],
)
def test_proxy_capabilities_come_from_its_health_answer(
        monkeypatch, health_body, capabilities, supports_demo):
    class HealthAnswer(io.BytesIO):  # BytesIO gives read() and `with`
        status = 200

    monkeypatch.setattr(urllib.request, "urlopen",
                        lambda *args, **kwargs: HealthAnswer(health_body))
    assert runtime._proxy_capabilities("http://proxy") == capabilities
    assert runtime._proxy_supports_capability(
        runtime.DEMO_RAW_CAPABILITY, "http://proxy") is supports_demo


def test_ensure_proxy_repairs_reachable_stale_proxy(monkeypatch, tmp_path):
    repaired = []
    aligned = []
    monkeypatch.setattr(runtime, "_find_atlas_dir", lambda: str(tmp_path))
    monkeypatch.setattr(runtime, "_check_url", lambda *args, **kwargs: True)
    monkeypatch.setattr(
        runtime,
        "_repair_proxy_capability",
        lambda root, capability: repaired.append((root, capability)) or True,
    )
    monkeypatch.setattr(runtime, "_align_workspace", lambda root: aligned.append(root))

    assert runtime.ensure_proxy(runtime.DEMO_RAW_CAPABILITY)
    assert repaired == [(str(tmp_path), runtime.DEMO_RAW_CAPABILITY)]
    assert aligned == [str(tmp_path)]


def test_compose_proxy_rebuild_uses_checkout_source_and_overlay(monkeypatch, tmp_path):
    root = _metal_root(tmp_path)
    calls = []

    def capture(cmd, **kwargs):
        calls.append((cmd, kwargs))
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setattr(runtime.subprocess, "run", capture)
    monkeypatch.setattr(runtime, "_proxy_supports_capability", lambda *args: True)

    assert runtime._rebuild_docker_proxy_for_capability(
        root, str(tmp_path / "project"), runtime.DEMO_RAW_CAPABILITY
    )
    command, kwargs = calls[0]
    assert command[:6] == [
        "docker", "compose", "-f", "docker-compose.yml", "-f",
        "docker-compose.macos.yml",
    ]
    assert "--build" in command
    assert command[-1] == "atlas-proxy"
    assert kwargs["env"]["ATLAS_PROJECT_DIR"] == str(tmp_path / "project")


def test_a_stale_installed_proxy_is_rebuilt_not_launched(monkeypatch):
    """A binary older than the checkout's proxy source would run old code
    after an update."""
    monkeypatch.setattr(runtime, "_find_proxy_binary",
                        lambda atlas_dir: "/installed/atlas-proxy-v2")
    monkeypatch.setattr(runtime, "go_binary_is_current", lambda binary, source_dir: False)
    monkeypatch.setattr(runtime, "_find_go", lambda: "/usr/bin/go")
    monkeypatch.setattr(runtime, "_build_proxy", lambda atlas_dir: "/rebuilt/atlas-proxy-v2")

    assert runtime._select_proxy_binary("/atlas") == "/rebuilt/atlas-proxy-v2"


def test_a_stale_compose_proxy_is_rebuilt_in_docker_not_replaced_locally(monkeypatch, tmp_path):
    """A local proxy would collide with the compose stack's container on the
    proxy port (#118)."""
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr(runtime, "_proxy_supports_capability", lambda capability: False)
    monkeypatch.setattr(runtime, "_docker_compose_owns_proxy", lambda atlas_dir: True)
    monkeypatch.setattr(runtime, "_find_go", lambda: "/usr/bin/go")
    docker_rebuilds, local_builds = [], []
    monkeypatch.setattr(runtime, "_rebuild_docker_proxy_for_capability",
                        lambda atlas_dir, project_dir, capability:
                        docker_rebuilds.append(project_dir) or True)
    monkeypatch.setattr(runtime, "_build_proxy", lambda atlas_dir: local_builds.append(atlas_dir))

    assert runtime._repair_proxy_capability("/atlas", runtime.DEMO_RAW_CAPABILITY) is True
    assert docker_rebuilds == [str(tmp_path)]
    assert local_builds == []


@pytest.mark.parametrize("proxy_bind, sandbox_bind, recreated", [
    pytest.param("project", "project", False, id="cwd-inside-both-binds"),
    pytest.param("project", "elsewhere", True, id="sandbox-bound-elsewhere"),
])
def test_align_workspace_recreates_only_when_a_bind_misses_the_cwd(
        monkeypatch, tmp_path, proxy_bind, sandbox_bind, recreated):
    cwd = tmp_path / "project" / "src"
    cwd.mkdir(parents=True)
    monkeypatch.chdir(cwd)
    monkeypatch.delenv("ATLAS_AUTO_WORKSPACE", raising=False)
    monkeypatch.setattr(runtime, "_docker_proxy_workspace", lambda: str(tmp_path / proxy_bind))
    monkeypatch.setattr(runtime, "_docker_sandbox_workspace", lambda: str(tmp_path / sandbox_bind))
    recreates = []
    monkeypatch.setattr(runtime, "_recreate_docker_proxy",
                        lambda atlas_dir, project_dir: recreates.append(project_dir) or True)

    runtime._align_workspace("/atlas")

    assert recreates == ([os.path.realpath(cwd)] if recreated else [])
