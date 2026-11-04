"""Proxy runtime tests — compose lifecycle behind ensure_proxy()."""

import io
import os
import signal
import urllib.request
from types import SimpleNamespace

import pytest

from atlas import runtime


def _metal_root(tmp_path):
    """A checkout on the Metal backend, so compose needs the macOS overlay."""
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "deploy" / "compose").mkdir(parents=True)
    (tmp_path / "deploy/compose/docker-compose.macos.yml").write_text("services: {}\n")
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
        "deploy/compose/docker-compose.macos.yml",
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
    assert "deploy/compose/docker-compose.macos.yml" in calls[0]


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


def test_a_proxy_the_compose_stack_owns_is_waited_for_then_repaired_and_aligned(monkeypatch, tmp_path):
    waits, repaired, aligned = [], [], []
    monkeypatch.setattr(runtime, "_find_atlas_dir", lambda: str(tmp_path))
    monkeypatch.setattr(runtime, "_check_url", lambda *args, **kwargs: False)
    monkeypatch.setattr(runtime, "_docker_compose_owns_proxy", lambda atlas_dir: True)
    monkeypatch.setattr(runtime, "_wait_for_proxy", lambda timeout: waits.append(timeout) or True)
    monkeypatch.setattr(runtime, "_repair_proxy_capability",
                        lambda root, capability: repaired.append((root, capability)) or True)
    monkeypatch.setattr(runtime, "_align_workspace", lambda root: aligned.append(root))

    assert runtime.ensure_proxy(runtime.DEMO_RAW_CAPABILITY) is True
    assert waits == [60]
    assert repaired == [(str(tmp_path), runtime.DEMO_RAW_CAPABILITY)]
    assert aligned == [str(tmp_path)]


def test_a_compose_stack_that_never_answers_is_not_replaced_by_a_local_proxy(monkeypatch, tmp_path, capsys):
    root = _metal_root(tmp_path)  # its start command needs the macOS overlay
    chosen = []
    monkeypatch.setattr(runtime, "_find_atlas_dir", lambda: root)
    monkeypatch.setattr(runtime, "_check_url", lambda *args, **kwargs: False)
    monkeypatch.setattr(runtime, "_docker_compose_owns_proxy", lambda atlas_dir: True)
    monkeypatch.setattr(runtime, "_wait_for_proxy", lambda timeout: False)
    monkeypatch.setattr(runtime, "_select_proxy_binary", lambda atlas_dir: chosen.append(atlas_dir))

    assert runtime.ensure_proxy() is False
    assert chosen == []
    assert runtime.compose_config.format_command(root, ["up", "-d"]) in capsys.readouterr().out


@pytest.mark.parametrize("proxy_bin, launched, supports, ready", [
    pytest.param("/opt/atlas/atlas-proxy-v2", True, True, True, id="started-with-the-capability"),
    pytest.param("/opt/atlas/atlas-proxy-v2", True, False, False, id="started-without-the-capability"),
    pytest.param("/opt/atlas/atlas-proxy-v2", False, True, False, id="did-not-start"),
    pytest.param(None, True, True, False, id="no-proxy-binary"),
])
def test_a_local_proxy_is_ready_only_when_it_started_with_the_capability(
        monkeypatch, tmp_path, proxy_bin, launched, supports, ready):
    monkeypatch.setattr(runtime, "_find_atlas_dir", lambda: str(tmp_path))
    monkeypatch.setattr(runtime, "_check_url", lambda *args, **kwargs: False)
    monkeypatch.setattr(runtime, "_docker_compose_owns_proxy", lambda atlas_dir: False)
    monkeypatch.setattr(runtime, "_select_proxy_binary", lambda atlas_dir: proxy_bin)
    monkeypatch.setattr(runtime, "_launch_local_proxy", lambda binary: launched)
    monkeypatch.setattr(runtime, "_proxy_supports_capability", lambda capability: supports)

    assert runtime.ensure_proxy(runtime.DEMO_RAW_CAPABILITY) is ready


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
        "deploy/compose/docker-compose.macos.yml",
    ]
    assert "--build" in command
    assert command[-1] == "atlas-proxy"
    assert kwargs["env"]["ATLAS_PROJECT_DIR"] == str(tmp_path / "project")


def test_a_proxy_built_by_hand_in_the_checkout_is_found(monkeypatch, tmp_path):
    """`go build -o atlas-proxy-v2` in services/proxy/ leaves the binary there."""
    monkeypatch.setattr(runtime.shutil, "which", lambda name: None)
    monkeypatch.setenv("HOME", str(tmp_path / "home"))
    binary = tmp_path / "services" / "proxy" / "atlas-proxy-v2"
    binary.parent.mkdir(parents=True)
    binary.write_text("")
    binary.chmod(0o755)

    assert runtime._find_proxy_binary(str(tmp_path)) == str(binary)


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


# The CLI knows the checkout it runs from by the proxy's source. A path that is
# no longer there gives "", and the CLI then runs whatever proxy binary is
# installed instead of rebuilding it from the checkout, so it has to fail here.
def test_the_cli_finds_the_checkout_it_runs_from(monkeypatch, tmp_path):
    checkout = os.path.dirname(os.path.dirname(os.path.abspath(runtime.__file__)))
    monkeypatch.chdir(tmp_path)
    assert runtime._find_atlas_dir() == checkout, "not from the folder the CLI is installed in"
    monkeypatch.setattr(runtime, "__file__", str(tmp_path / "atlas" / "runtime.py"))
    monkeypatch.chdir(checkout)
    assert runtime._find_atlas_dir() == checkout, "not from the folder the CLI runs in"


class FakeOs:
    """The os calls the stale-proxy reaper makes, answered from a fixed table
    of processes. kill() only records a signal, so nothing real is stopped; a
    pid in `stubborn` outlives SIGTERM, one in `refused` belongs to another
    user, one in `gone` has exited since the scan, and one in
    `exits_before_sigkill` exits during the grace."""
    path = os.path

    def __init__(self, exes, own_pid, stubborn=(), refused=(), gone=(), exits_before_sigkill=()):
        self.exes, self.own_pid = exes, own_pid
        self.stubborn, self.refused = set(stubborn), set(refused)
        self.gone, self.exits_before_sigkill = set(gone), set(exits_before_sigkill)
        self.signals = []

    def listdir(self, path):
        return ["self", "cpuinfo", *(str(pid) for pid in self.exes)]

    def readlink(self, path):
        exe = self.exes[int(path.split("/")[2])]
        if exe is None:
            raise PermissionError(path)
        return exe

    def getpid(self):
        return self.own_pid

    def kill(self, pid, sig):
        if pid in self.refused:
            raise PermissionError(pid)
        if pid in self.gone:
            raise ProcessLookupError(pid)
        if sig == signal.SIGKILL and pid in self.exits_before_sigkill:
            raise ProcessLookupError(pid)
        if sig == 0:
            return
        self.signals.append((pid, sig))
        if sig == signal.SIGKILL or pid not in self.stubborn:
            self.gone.add(pid)


class FakeClock:
    """time.time and time.sleep without the wait: sleep moves the clock on."""

    def __init__(self):
        self.now = 0.0

    def time(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


def test_a_stale_proxy_found_by_name_or_by_port_is_stopped(monkeypatch, capsys):
    fake = FakeOs(own_pid=999, stubborn={200}, refused={500}, exes={
        100: "/usr/local/bin/atlas-proxy-v2",
        200: "/usr/local/bin/atlas-proxy-v2 (deleted)",  # its binary was rebuilt
        300: "/usr/bin/python3",
        400: None,  # its exe cannot be read
        999: "/usr/local/bin/atlas-proxy-v2",  # this process
    })
    listening = ('LISTEN 0 4096 *:8090 *:* users:(("atlas-proxy-v2",pid=100,fd=3),'
                 '("proxy",pid=500,fd=7),("me",pid=999,fd=4))\n')
    asked = []
    monkeypatch.setattr(runtime, "os", fake)
    monkeypatch.setattr(runtime, "time", FakeClock())
    monkeypatch.setattr(runtime.subprocess, "run",
                        lambda cmd, **kwargs: asked.append((cmd, kwargs)) or SimpleNamespace(stdout=listening))

    runtime._kill_stale_proxy()

    assert asked == [(["ss", "-tlnpH", f"sport = :{runtime.PROXY_PORT}"],
                      {"capture_output": True, "text": True, "timeout": 3})]
    assert fake.signals == [(100, signal.SIGTERM), (200, signal.SIGTERM), (200, signal.SIGKILL)]
    assert runtime.time.now == pytest.approx(2.0, abs=0.11)  # SIGKILL only after the 2 s grace
    assert capsys.readouterr().out.splitlines() == [
        "  Reaping stale atlas-proxy-v2 (pid 100)",
        "  Reaping stale atlas-proxy-v2 (pid 200)",
        "  Reaping stale atlas-proxy-v2 (pid 500)",
        "  WARN: can't kill pid 500 — not owned by us",
    ]


def test_a_proxy_that_stops_at_sigterm_is_not_waited_for(monkeypatch):
    fake = FakeOs(own_pid=999, exes={100: "/usr/local/bin/atlas-proxy-v2"})
    monkeypatch.setattr(runtime, "os", fake)
    monkeypatch.setattr(runtime, "time", FakeClock())
    monkeypatch.setattr(runtime.subprocess, "run", lambda cmd, **kwargs: SimpleNamespace(stdout=""))

    runtime._kill_stale_proxy()

    assert fake.signals == [(100, signal.SIGTERM)]
    assert runtime.time.now == 0.0


def test_a_proxy_that_exits_on_its_own_before_a_signal_is_passed_over(monkeypatch):
    fake = FakeOs(own_pid=999, gone={600}, stubborn={700}, exits_before_sigkill={700}, exes={
        600: "/usr/local/bin/atlas-proxy-v2",  # exits before SIGTERM
        700: "/usr/local/bin/atlas-proxy-v2",  # exits before SIGKILL
    })
    monkeypatch.setattr(runtime, "os", fake)
    monkeypatch.setattr(runtime, "time", FakeClock())
    monkeypatch.setattr(runtime.subprocess, "run", lambda cmd, **kwargs: SimpleNamespace(stdout=""))

    runtime._kill_stale_proxy()

    assert fake.signals == [(700, signal.SIGTERM)]


def test_without_proc_or_ss_the_reaper_stops_nothing(monkeypatch):
    def missing(*args, **kwargs):
        raise FileNotFoundError(args[0])

    fake = FakeOs(own_pid=999, exes={})
    fake.listdir = missing
    monkeypatch.setattr(runtime, "os", fake)
    monkeypatch.setattr(runtime.subprocess, "run", missing)

    runtime._kill_stale_proxy()

    assert fake.signals == []
