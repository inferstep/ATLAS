"""Public policy tests for backend-aware Compose command construction."""

from types import SimpleNamespace

import pytest

from atlas import compose


@pytest.mark.parametrize(
    ("backend", "expected"),
    [
        ("metal", ["-f", "docker-compose.yml", "-f", "compose/docker-compose.macos.yml"]),
        ("rocm", ["-f", "docker-compose.yml", "-f", "compose/docker-compose.rocm.yml"]),
        ("vulkan", ["-f", "docker-compose.yml", "-f", "compose/docker-compose.vulkan.yml"]),
        # The GPU-less CPU path stacks the Vulkan and CPU overlays, in merge
        # order, matching the bootstrap's compose_files_args.
        ("cpu", ["-f", "docker-compose.yml", "-f", "compose/docker-compose.vulkan.yml",
                 "-f", "compose/docker-compose.cpu.yml"]),
        # cuda and any unknown or absent backend keep Compose's default
        # discovery: no -f flags, so the base file only.
        ("cuda", []),
        ("unknown", []),
        ("", []),
        (None, []),
    ],
    ids=["metal", "rocm", "vulkan", "cpu", "cuda", "unknown", "empty", "none"],
)
def test_backend_selects_expected_compose_files(tmp_path, backend, expected):
    (tmp_path / "compose").mkdir()
    for name in ("docker-compose.yml", "compose/docker-compose.macos.yml",
                 "compose/docker-compose.rocm.yml", "compose/docker-compose.vulkan.yml",
                 "compose/docker-compose.cpu.yml"):
        (tmp_path / name).write_text("services: {}\n")
    assert compose.file_args(str(tmp_path), backend=backend,
                             environ={}) == expected


@pytest.mark.parametrize(
    ("backend", "present_files", "missing_file"),
    [
        ("metal", ["docker-compose.yml"], "compose/docker-compose.macos.yml"),
        # The CPU stack needs both of its overlays.
        ("cpu", ["docker-compose.yml", "compose/docker-compose.vulkan.yml"],
         "compose/docker-compose.cpu.yml"),
    ],
    ids=["metal-overlay-missing", "cpu-second-overlay-missing"],
)
def test_missing_overlay_fails_before_docker(tmp_path, backend, present_files,
                                             missing_file):
    (tmp_path / "compose").mkdir()
    for name in present_files:
        (tmp_path / name).write_text("services: {}\n")
    with pytest.raises(FileNotFoundError, match=missing_file):
        compose.command(str(tmp_path), ["up", "-d"], backend=backend,
                        environ={})


def test_shell_environment_wins_over_dotenv_values(tmp_path):
    (tmp_path / ".env").write_text("ATLAS_BACKEND=rocm\n")
    assert compose.resolve_backend(
        str(tmp_path),
        values={"ATLAS_BACKEND": "vulkan"},
        environ={"ATLAS_BACKEND": "metal"},
    ) == "metal"


def test_command_places_overlay_before_operation(tmp_path):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "compose").mkdir()
    (tmp_path / "compose/docker-compose.macos.yml").write_text("services: {}\n")
    cmd = compose.command(str(tmp_path), ["up", "-d", "atlas-proxy"],
                          backend="metal", environ={})
    assert cmd == [
        "docker", "compose", "-f", "docker-compose.yml", "-f",
        "compose/docker-compose.macos.yml", "up", "-d", "atlas-proxy",
    ]


def test_read_env_file_empty_value_with_inline_comment(tmp_path):
    """`KEY= # comment` parses as an empty value (compose semantics),
    not as the literal string "# comment"."""
    (tmp_path / ".env").write_text(
        "EMPTY_WITH_COMMENT= # explanation\n"
        "PORT=8080  # note\n"
        "HEX=#fff\n"
        "PLAIN=value\n"
    )
    values = compose.read_env_file(str(tmp_path))
    assert values["EMPTY_WITH_COMMENT"] == ""
    assert values["PORT"] == "8080"
    assert values["HEX"] == "#fff"  # no whitespace before '#': not a comment
    assert values["PLAIN"] == "value"


# Order: an explicit URL variable, then the port variable, then the port in
# .env (passed as `values` or read from the file), then the default port.
@pytest.mark.parametrize(
    ("service", "dotenv", "values", "environ", "expected"),
    [
        ("llama", None, {"ATLAS_LLAMA_PORT": "9001"},
         {"ATLAS_INFERENCE_URL": "http://inference.test:9000/"},
         "http://inference.test:9000"),
        ("proxy", None, {"ATLAS_PROXY_PORT": "7777"},
         {"ATLAS_PROXY_PORT": "8888"}, "http://localhost:8888"),
        ("sandbox", "ATLAS_SANDBOX_PORT=31111\n", None, {},
         "http://localhost:31111"),
        ("lens", None, None, {}, "http://localhost:8099"),
        ("v3", None, None, {}, "http://localhost:8070"),
    ],
    ids=["explicit-url-wins", "port-variable-beats-dotenv",
         "port-from-dotenv-file", "lens-default", "v3-default"],
)
def test_service_url_resolution_order(tmp_path, service, dotenv, values,
                                      environ, expected):
    if dotenv is not None:
        (tmp_path / ".env").write_text(dotenv)
    assert compose.service_url(service, atlas_root=str(tmp_path),
                               values=values, environ=environ) == expected


def test_service_port_resolution_order(tmp_path):
    (tmp_path / ".env").write_text("ATLAS_LLAMA_PORT=9002\n")
    assert compose.service_port("llama", atlas_root=str(tmp_path),
                                environ={}) == "9002"
    assert compose.service_port("llama", atlas_root=str(tmp_path),
                                environ={"ATLAS_LLAMA_PORT": "9003"}) == "9003"


def test_container_id_uses_compose_ps_output(tmp_path, monkeypatch):
    calls = []

    def fake_run(cmd, **kwargs):
        calls.append(cmd)
        return SimpleNamespace(returncode=0, stdout="abc123def\n", stderr="")

    monkeypatch.setattr(compose.subprocess, "run", fake_run)
    cid = compose.container_id(str(tmp_path), "geometric-lens", environ={},
                               fallback="atlas-geometric-lens-1")
    assert cid == "abc123def"
    assert calls == [["docker", "compose", "ps", "-q", "geometric-lens"]]


def test_container_id_falls_back_when_compose_fails(tmp_path, monkeypatch):
    def docker_missing(cmd, **kwargs):
        raise FileNotFoundError("docker not installed")

    monkeypatch.setattr(compose.subprocess, "run", docker_missing)
    cid = compose.container_id(str(tmp_path), "geometric-lens", environ={},
                               fallback="atlas-geometric-lens-1")
    assert cid == "atlas-geometric-lens-1"


def test_container_id_falls_back_when_service_down(tmp_path, monkeypatch):
    monkeypatch.setattr(
        compose.subprocess, "run",
        lambda cmd, **kwargs: SimpleNamespace(returncode=0, stdout="\n",
                                              stderr=""))
    cid = compose.container_id(str(tmp_path), "sandbox", environ={},
                               fallback="atlas-sandbox-1")
    assert cid == "atlas-sandbox-1"
