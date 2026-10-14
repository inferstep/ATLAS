"""Static contracts for runtime configuration shared across services."""

import os
import shutil
import subprocess

import pytest

from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]

# Every compose combination a real install runs. Overlay regressions
# (bad `!reset`, dangling service key) only surface when the overlay is
# actually merged onto the base file.
COMPOSE_COMBINATIONS = [
    ("docker-compose.yml",),
    ("docker-compose.yml", "docker-compose.rocm.yml"),
    ("docker-compose.yml", "docker-compose.vulkan.yml"),
    ("docker-compose.yml", "docker-compose.vulkan.yml", "docker-compose.cpu.yml"),
    ("docker-compose.yml", "docker-compose.macos.yml"),
]


def _docker_compose_available() -> bool:
    if shutil.which("docker") is None:
        return False
    completed = subprocess.run(
        ["docker", "compose", "version"],
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    return completed.returncode == 0


@pytest.mark.parametrize(
    "files", COMPOSE_COMBINATIONS, ids=lambda files: "+".join(files)
)
def test_compose_combinations_parse(files):
    if not _docker_compose_available():
        pytest.skip("docker compose v2 not available")
    command = ["docker", "compose"]
    for f in files:
        assert (ROOT / f).exists(), f"missing compose file: {f}"
        command += ["-f", f]
    command += ["config", "-q"]
    # Placeholders for the required (:?) interpolation vars — this test
    # validates YAML structure, not runtime config.
    env = dict(
        os.environ,
        ATLAS_MODEL_FILE="placeholder.gguf",
        ATLAS_MODEL_NAME="placeholder",
    )
    completed = subprocess.run(
        command, cwd=ROOT, env=env, capture_output=True, text=True, check=False
    )
    assert completed.returncode == 0, completed.stderr


def test_all_services_declare_restart_policy():
    # Every service must survive a daemon restart / host reboot without a
    # manual `docker compose up`. Checked on the base file — overlays
    # inherit the restart policy through the compose merge.
    yaml = pytest.importorskip("yaml")
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    services = compose["services"]
    assert "llama-server" in services
    for name, service in services.items():
        assert service.get("restart") == "unless-stopped", (
            f"service {name} missing 'restart: unless-stopped'"
        )


@pytest.mark.parametrize("service", ["geometric-lens", "v3-service", "sandbox"])
def test_the_services_that_read_the_token_run_as_the_installing_user(service):
    # `atlas init` makes the token file 0600 for the installing user and
    # writes that user's ids for the proxy and the sandbox. A service that
    # runs as its image's own user cannot read the file, and stops.
    yaml = pytest.importorskip("yaml")
    services = yaml.safe_load((ROOT / "docker-compose.yml").read_text())["services"]
    proxy = services["atlas-proxy"]["user"]
    assert proxy == "${ATLAS_PROXY_UID:-1000}:${ATLAS_PROXY_GID:-1000}"
    assert services[service]["user"] == (proxy if service != "sandbox" else proxy.replace("PROXY", "SANDBOX"))


@pytest.mark.parametrize("service", ["geometric-lens", "v3-service"])
def test_the_lens_and_v3_have_a_home_that_their_user_can_write(service):
    # The images' HOME belongs to their own user (1001); /tmp is 1777.
    yaml = pytest.importorskip("yaml")
    services = yaml.safe_load((ROOT / "docker-compose.yml").read_text())["services"]
    assert "HOME=/tmp" in services[service]["environment"]


@pytest.mark.parametrize("service, dockerfile, folder", [
    ("v3-service", "v3-service/Dockerfile", "/data/telemetry"),
])
def test_a_folder_that_a_service_writes_on_a_named_volume_is_writable_for_any_user(service, dockerfile, folder):
    # A new named volume takes the owner and mode of the image's folder, and
    # the service runs as the installing user, not as the image's own user.
    yaml = pytest.importorskip("yaml")
    volumes = yaml.safe_load((ROOT / "docker-compose.yml").read_text())["services"][service]["volumes"]
    assert [v for v in volumes if v.endswith(f":{folder}") and not v.startswith((".", "/", "$"))]
    assert f"chmod 1777 {folder}" in (ROOT / dockerfile).read_text()


def test_legacy_runtime_keys_are_upgrade_fallbacks_only():
    compose = (ROOT / "docker-compose.yml").read_text()

    assert (
        "PARALLEL_SLOTS=${ATLAS_PARALLEL_SLOTS:-${PARALLEL_SLOTS:-4}}"
        in compose
    )
    assert (
        "KV_CACHE_TYPE_K=${ATLAS_KV_TYPE_K:-${KV_CACHE_TYPE_K:-f16}}"
        in compose
    )
    assert (
        "KV_CACHE_TYPE_V=${ATLAS_KV_TYPE_V:-${KV_CACHE_TYPE_V:-f16}}"
        in compose
    )
    assert (
        "ATLAS_PARALLEL_SLOTS=${ATLAS_PARALLEL_SLOTS:-${PARALLEL_SLOTS:-4}}"
        in compose
    )


def _sandbox(compose):
    return compose["services"]["sandbox"]


def test_go_run_has_an_exec_permitted_link_directory():
    """`go run` links the program into GOTMPDIR and then executes it.

    Docker mounts every tmpfs noexec unless told otherwise, and the sandbox
    runs on a read-only rootfs, so GOTMPDIR's default (/tmp) made `go run`
    fail with "fork/exec ...: permission denied" on every invocation.
    Measured across four benchmark runs: 22 permission-denied results,
    every one of them a Go build, each followed by the model re-sending the
    identical command.

    So GOTMPDIR must point at a tmpfs that actually permits exec.
    """
    yaml = pytest.importorskip("yaml")
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    sandbox = _sandbox(compose)

    env = dict(
        item.split("=", 1) for item in sandbox["environment"] if "=" in item
    )
    gotmpdir = env.get("GOTMPDIR")
    assert gotmpdir, "sandbox must set GOTMPDIR away from the noexec default"

    matching = [t for t in sandbox["tmpfs"] if t.split(":", 1)[0] == gotmpdir]
    assert matching, f"GOTMPDIR={gotmpdir} has no tmpfs declaration"
    assert "exec" in matching[0].split(":", 1)[1].split(","), (
        f"GOTMPDIR={gotmpdir} is mounted noexec, so `go run` cannot execute "
        f"what it just linked: {matching[0]}"
    )


def test_tmp_stays_noexec():
    """The exec grant is scoped to one path on purpose.

    /workspace is a bind mount and already permits exec, so this adds no
    capability the sandbox lacked; it makes the Go toolchain's own default
    path work. /tmp, which is where untrusted downloads and build scratch
    land, must not pick up exec along the way.
    """
    yaml = pytest.importorskip("yaml")
    compose = yaml.safe_load((ROOT / "docker-compose.yml").read_text())
    for entry in _sandbox(compose)["tmpfs"]:
        path, _, opts = entry.partition(":")
        if path == "/tmp":
            assert "exec" not in opts.split(","), f"/tmp must stay noexec: {entry}"
            return
    raise AssertionError("sandbox declares no /tmp tmpfs")
