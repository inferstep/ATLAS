"""Proxy runtime — locate, build, launch, align, and stop atlas-proxy.

ensure_proxy() strategy:
1. atlas-proxy already running on PROXY_PORT → use it (and align a Docker
   proxy's /workspace bind to the caller's CWD — PC-038/PC-189)
2. A docker-compose stack owns atlas-proxy (#118) → wait for it or tell
   the user to bring the stack up; never launch a competing local proxy
3. Go available + no docker stack → build (if needed) and launch locally
4. Nothing available → False

stop_local_proxy() reaps a proxy this process launched; it also runs at
exit via atexit.
"""

import os
import shutil
import subprocess
import time
import signal
import atexit
import json
from typing import Optional, List

from atlas import compose as compose_config
from atlas import env as cli_env
from atlas.runtime_artifacts import go_binary_is_current

# Shell env wins; otherwise the Docker .env's port keys drive the URLs.
_ENV_VALUES = compose_config.read_env_file(cli_env.atlas_root())
PROXY_PORT = compose_config.service_port("proxy", values=_ENV_VALUES)
PROXY_URL = os.environ.get("ATLAS_PROXY_URL", f"http://localhost:{PROXY_PORT}")
INFERENCE_URL = compose_config.service_url("llama", values=_ENV_VALUES)
LENS_URL = compose_config.service_url("lens", values=_ENV_VALUES)
SANDBOX_URL = compose_config.service_url("sandbox", values=_ENV_VALUES)
V3_URL = compose_config.service_url("v3", values=_ENV_VALUES)
MODEL_NAME = (os.environ.get("ATLAS_MODEL_NAME")
              or _ENV_VALUES.get("ATLAS_MODEL_NAME", "local-model"))
DEMO_RAW_CAPABILITY = "demo_raw_completion_v1"

_proxy_process = None


def _check_url(url: str, timeout: int = 3) -> bool:
    """Check if a URL is reachable."""
    import urllib.request
    try:
        req = urllib.request.Request(f"{url}/health")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status == 200
    except Exception:
        return False


def _proxy_capabilities(url: Optional[str] = None, timeout: int = 3) -> set[str]:
    """Read the active proxy capability contract from its health response."""
    import urllib.request

    base_url = (url or PROXY_URL).rstrip("/")
    try:
        req = urllib.request.Request(f"{base_url}/health")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if resp.status != 200:
                return set()
            payload = json.loads(resp.read(1 << 20))
    except (OSError, ValueError, TypeError):
        return set()
    capabilities = payload.get("capabilities", []) if isinstance(payload, dict) else []
    if not isinstance(capabilities, list):
        return set()
    return {item for item in capabilities if isinstance(item, str)}


def _proxy_supports_capability(capability: str, url: Optional[str] = None) -> bool:
    return capability in _proxy_capabilities(url)


def _find_go() -> Optional[str]:
    """Find go binary on PATH."""
    return shutil.which("go")


def _find_atlas_dir() -> str:
    """Find the ATLAS repo root (where proxy/ source lives)."""
    d = os.path.dirname(os.path.abspath(__file__))
    for _ in range(4):
        if os.path.exists(os.path.join(d, "proxy", "main.go")):
            return d
        d = os.path.dirname(d)
    # Check CWD
    if os.path.exists(os.path.join(os.getcwd(), "proxy", "main.go")):
        return os.getcwd()
    return ""


def _find_proxy_binary(atlas_dir: str) -> Optional[str]:
    """Find or build the atlas-proxy-v2 binary."""
    # Check PATH first
    on_path = shutil.which("atlas-proxy-v2")
    if on_path:
        return on_path

    # Check common locations
    for candidate in [
        os.path.expanduser("~/.local/bin/atlas-proxy-v2"),
        os.path.join(atlas_dir, "proxy", "atlas-proxy-v2") if atlas_dir else None,
    ]:
        if candidate and os.path.isfile(candidate) and os.access(candidate, os.X_OK):
            return candidate

    return None


def _build_proxy(atlas_dir: str) -> Optional[str]:
    """Build atlas-proxy-v2 from source using Go."""
    go_bin = _find_go()
    if not go_bin or not atlas_dir:
        return None

    proxy_src = os.path.join(atlas_dir, "proxy")
    if not os.path.isfile(os.path.join(proxy_src, "main.go")):
        return None

    output = os.path.expanduser("~/.local/bin/atlas-proxy-v2")
    os.makedirs(os.path.dirname(output), exist_ok=True)

    print("  Building atlas-proxy from source...")
    try:
        result = subprocess.run(
            [go_bin, "build", "-o", output, "."],
            cwd=proxy_src,
            capture_output=True,
            text=True,
            timeout=120,
        )
        if result.returncode == 0:
            print(f"  Built: {output}")
            return output
        else:
            print(f"  Build failed: {result.stderr[:200]}")
            return None
    except Exception as e:
        print(f"  Build failed: {e}")
        return None


def _select_proxy_binary(atlas_dir: str) -> Optional[str]:
    """Select or rebuild the local proxy for the current checkout."""
    binary = _find_proxy_binary(atlas_dir)
    source_dir = os.path.join(atlas_dir, "proxy") if atlas_dir else ""
    if binary and go_binary_is_current(binary, source_dir):
        return binary
    if binary:
        print("  atlas-proxy source changed; rebuilding the installed binary...")
    return _build_proxy(atlas_dir) if _find_go() else None


def _kill_stale_proxy() -> None:  # noqa: C901
    """Reap any pre-existing atlas-proxy-v2 process before launching a new
    one. Without this, an orphaned proxy from a previous `atlas` session
    (whose parent died ungracefully — terminal closed, SIGKILL, etc.)
    keeps running with the OLD binary in memory even after a rebuild has
    replaced the binary on disk. Users then see "I just fixed that bug"
    confusion: the on-disk fix is real, but the running process never
    picked it up. We re-find candidates two ways for robustness:

      1. /proc walk for processes whose exe basename is atlas-proxy-v2
         (catches orphans whose ppid=1).
      2. ss/lsof on PROXY_PORT (catches the case where the binary was
         renamed or replaced — /proc/<pid>/exe shows "(deleted)").

    Anything found gets SIGTERM, then SIGKILL after 2s if still alive.
    """
    pids: List[int] = []
    # /proc walk — works on Linux, where this proxy actually runs.
    try:
        for entry in os.listdir("/proc"):
            if not entry.isdigit():
                continue
            pid = int(entry)
            if pid == os.getpid():
                continue
            try:
                exe = os.readlink(f"/proc/{pid}/exe")
            except (OSError, PermissionError):
                continue
            base = os.path.basename(exe.split(" ")[0])
            # Match exact name or "<name> (deleted)" form.
            if base == "atlas-proxy-v2" or base.startswith("atlas-proxy-v2"):
                pids.append(pid)
    except FileNotFoundError:
        # best-effort: swallow on failure (caller continues)
        pass

    # Fallback: anything listening on PROXY_PORT.
    try:
        result = subprocess.run(
            ["ss", "-tlnpH", f"sport = :{PROXY_PORT}"],
            capture_output=True, text=True, timeout=3,
        )
        for line in result.stdout.splitlines():
            # Format: ... users:(("name",pid=N,fd=M))
            if "pid=" in line:
                for tok in line.split("pid="):
                    digits = ""
                    for c in tok:
                        if c.isdigit():
                            digits += c
                        else:
                            break
                    if digits:
                        pid = int(digits)
                        if pid != os.getpid() and pid not in pids:
                            pids.append(pid)
    except (FileNotFoundError, subprocess.TimeoutExpired):
        # best-effort: swallow on failure (caller continues)
        pass

    for pid in pids:
        try:
            print(f"  Reaping stale atlas-proxy-v2 (pid {pid})")
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            continue
        except PermissionError:
            print(f"  WARN: can't kill pid {pid} — not owned by us")
            continue
    if pids:
        # Give SIGTERM ~2s to take, then escalate to SIGKILL.
        deadline = time.time() + 2.0
        while time.time() < deadline:
            alive = [p for p in pids if _pid_alive(p)]
            if not alive:
                break
            time.sleep(0.1)
        for pid in pids:
            if _pid_alive(pid):
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    # best-effort: swallow on failure (caller continues)
                    pass


def _pid_alive(pid: int) -> bool:
    """True if the given pid currently exists."""
    try:
        os.kill(pid, 0)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def _proxy_log_path() -> str:
    """Path the proxy's stdout+stderr is redirected to. We want this in
    one well-known place so panics aren't silently lost (the previous
    /dev/null redirect ate every "[agent] error: ..." line and made
    debugging impossible)."""
    cache = os.path.expanduser("~/.cache/atlas")
    os.makedirs(cache, exist_ok=True)
    return os.path.join(cache, "proxy.log")


def _launch_local_proxy(proxy_bin: str) -> bool:
    """Launch atlas-proxy-v2 as a local background process."""
    global _proxy_process

    # Defensive: kill any leftover proxy from a previous session before
    # we try to bind PROXY_PORT. Also handles the "rebuilt binary on
    # disk but old binary still in memory" case — the running orphan
    # gets reaped so the next launch picks up the fixed code.
    _kill_stale_proxy()

    env = os.environ.copy()
    env["ATLAS_PROXY_PORT"] = PROXY_PORT
    env["ATLAS_INFERENCE_URL"] = INFERENCE_URL
    env["ATLAS_LLAMA_URL"] = INFERENCE_URL
    env["ATLAS_LENS_URL"] = LENS_URL
    env["ATLAS_SANDBOX_URL"] = SANDBOX_URL
    env["ATLAS_V3_URL"] = V3_URL
    env["ATLAS_MODEL_NAME"] = MODEL_NAME
    # Pin the proxy's "where to write files" target to the directory the user
    # invoked `atlas` from, overriding the proxy's stale-history heuristics.
    env["ATLAS_WORKSPACE_DIR"] = os.getcwd()

    log_path = _proxy_log_path()
    try:
        # log_fd is intentionally held open for the lifetime of the proxy
        # child process — Popen below pipes its stdout/stderr into this
        # descriptor, and closing it here would yank the proxy's output.
        # The OS reclaims the fd when stop_local_proxy() reaps the child
        # at atexit. CodeQL's file-not-closed alert is a false positive
        # for this pattern.
        log_fd = open(log_path, "ab", buffering=0)
    except OSError as e:
        print(f"  WARN: can't open {log_path}: {e}; proxy logs disabled")
        log_fd = subprocess.DEVNULL

    try:
        _proxy_process = subprocess.Popen(
            [proxy_bin],
            env=env,
            cwd=os.getcwd(),
            stdout=log_fd,
            stderr=subprocess.STDOUT,
        )

        # Register cleanup
        atexit.register(stop_local_proxy)

        # Wait for health
        for _ in range(30):
            time.sleep(0.5)
            if _check_url(PROXY_URL, timeout=1):
                print(f"  Proxy logs → {log_path}")
                return True

        print("  Proxy started but not responding on health check")
        print(f"  Last 20 lines of {log_path}:")
        try:
            with open(log_path, "rb") as f:
                tail = f.read()[-4096:].decode("utf-8", errors="replace")
            for line in tail.splitlines()[-20:]:
                print(f"    {line}")
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
        return False

    except Exception as e:
        print(f"  Failed to start proxy: {e}")
        return False


def stop_local_proxy():
    """Stop the locally-launched proxy on exit."""
    if _proxy_process and _proxy_process.poll() is None:
        _proxy_process.terminate()
        try:
            _proxy_process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _proxy_process.kill()


def _docker_workspace_for(container: str) -> Optional[str]:
    """Return the host path bind-mounted to /workspace inside the named
    Docker container, or None if the container isn't running (or docker
    isn't on PATH). See ISSUES.md PC-038, PC-189.
    """
    if not shutil.which("docker"):
        return None
    try:
        result = subprocess.run(
            [
                "docker", "inspect", container,
                "--format",
                "{{range .Mounts}}{{if eq .Destination \"/workspace\"}}{{.Source}}{{end}}{{end}}",
            ],
            capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return None
        src = result.stdout.strip()
        return src if src else None
    except Exception:
        return None


def _compose_container(service: str, fallback: str) -> str:
    """Resolve a compose service's container via `docker compose ps -q`
    so non-default project names (COMPOSE_PROJECT_NAME, renamed checkout
    dirs) still work; fall back to the conventional name."""
    root = cli_env.atlas_root()
    return compose_config.container_id(root, service, fallback=fallback) or fallback


def _docker_proxy_workspace() -> Optional[str]:
    return _docker_workspace_for(
        _compose_container("atlas-proxy", "atlas-atlas-proxy-1"))


def _docker_sandbox_workspace() -> Optional[str]:
    return _docker_workspace_for(
        _compose_container("sandbox", "atlas-sandbox-1"))


def _recreate_docker_proxy(atlas_dir: str, project_dir: str) -> bool:
    """Recreate the Docker atlas-proxy AND sandbox containers with a new
    ATLAS_PROJECT_DIR bind. Both services mount ${ATLAS_PROJECT_DIR}:/workspace
    and MUST share the same host path — the agent loop reads files via the
    proxy and runs commands via the sandbox; if their /workspace binds drift,
    the model can read app.py through the proxy but `python app.py` in the
    sandbox 404s. Returns True when both are back up and healthy.
    See PC-038, PC-189.
    """
    if not atlas_dir:
        return False
    print(f"  Aligning proxy + sandbox workspace → {project_dir}")
    env = os.environ.copy()
    env["ATLAS_PROJECT_DIR"] = project_dir
    try:
        result = subprocess.run(
            compose_config.command(atlas_dir, [
                "up", "-d", "atlas-proxy", "sandbox",
                "--no-deps", "--no-build", "--force-recreate",
            ]),
            cwd=atlas_dir, env=env,
            capture_output=True, text=True, timeout=60,
        )
        if result.returncode != 0:
            tail = (result.stderr or result.stdout)[:240]
            print(f"  Workspace recreate failed: {tail}")
            return False
        for _ in range(40):
            time.sleep(0.5)
            if _check_url(PROXY_URL, timeout=1):
                return True
        print("  Recreated but proxy not responding on health check")
        return False
    except Exception as e:
        print(f"  Recreate failed: {e}")
        return False


def _rebuild_docker_proxy_for_capability(
    atlas_dir: str,
    project_dir: str,
    capability: str,
) -> bool:
    """Build and recreate atlas-proxy from checkout source, then verify it."""
    if not atlas_dir:
        return False
    print("  Active atlas-proxy predates the raw demo contract; rebuilding it...")
    env = os.environ.copy()
    env["ATLAS_PROJECT_DIR"] = project_dir
    try:
        result = subprocess.run(
            compose_config.command(atlas_dir, [
                "up", "-d", "--build", "--no-deps", "--force-recreate",
                "atlas-proxy",
            ]),
            cwd=atlas_dir,
            env=env,
            capture_output=True,
            text=True,
            timeout=300,
        )
        if result.returncode != 0:
            tail = (result.stderr or result.stdout)[-500:]
            print(f"  Proxy rebuild failed: {tail}")
            return False
        deadline = time.time() + 60.0
        while time.time() < deadline:
            if _proxy_supports_capability(capability):
                print("  Proxy rebuilt with the current demo contract")
                return True
            time.sleep(1.0)
        print("  Rebuilt proxy never advertised the required demo capability")
        return False
    except (OSError, subprocess.TimeoutExpired) as e:
        print(f"  Proxy rebuild failed: {e}")
        return False


def _repair_proxy_capability(atlas_dir: str, capability: str) -> bool:
    """Replace a reachable but stale proxy with one from this checkout."""
    if _proxy_supports_capability(capability):
        return True
    if _docker_compose_owns_proxy(atlas_dir):
        return _rebuild_docker_proxy_for_capability(atlas_dir, os.getcwd(), capability)
    if not _find_go():
        print(
            "  Active atlas-proxy is too old for /demo and Go is unavailable "
            "to rebuild it."
        )
        return False
    proxy_bin = _build_proxy(atlas_dir)
    if not proxy_bin or not _launch_local_proxy(proxy_bin):
        return False
    return _proxy_supports_capability(capability)


def _align_workspace(atlas_dir: str) -> None:
    """If the proxy is running in Docker and its /workspace bind doesn't
    cover the current working directory, recreate it so it does. See PC-038.

    Disable with ATLAS_AUTO_WORKSPACE=0 — the user keeps whatever bind the
    proxy was started with.
    """
    if os.environ.get("ATLAS_AUTO_WORKSPACE", "1") == "0":
        return
    cwd = os.path.realpath(os.getcwd())
    proxy_bound = _docker_proxy_workspace()
    if proxy_bound is None:
        # Local proxy (not in Docker) — _launch_local_proxy already pinned
        # ATLAS_WORKSPACE_DIR to os.getcwd(), so nothing to do.
        return
    sandbox_bound = _docker_sandbox_workspace()

    # No-op if BOTH binds cover cwd. Either being out of range triggers a
    # recreate of both — they must share a path (PC-189).
    def _covers_cwd(bound: Optional[str]) -> bool:
        if not bound:
            return False
        try:
            rel = os.path.relpath(cwd, os.path.realpath(bound))
        except ValueError:
            return False
        return rel == "." or not rel.startswith("..")

    if _covers_cwd(proxy_bound) and _covers_cwd(sandbox_bound):
        return
    if not _recreate_docker_proxy(atlas_dir, cwd):
        print("  Continuing with the existing workspace — file operations may not see your project.")


def _docker_compose_owns_proxy(atlas_dir: Optional[str]) -> bool:
    """True if a docker-compose stack rooted at atlas_dir defines an
    atlas-proxy service. Used by ensure_proxy() to avoid auto-launching
    a competing local proxy when the user's expected deployment is the
    docker stack (Linux + CUDA/ROCm, macOS hybrid #32). The check is
    cheap (~10ms) and silently false when docker isn't installed.

    Note: this only tells us "the user has docker-compose configured
    for atlas-proxy" — it doesn't tell us whether the stack is up
    YET. That distinction matters: if the stack is configured but
    down, the user might be mid-bringup → we should refuse to launch
    a local proxy (which would collide with the container when it
    binds :8090) rather than help them shoot their foot.
    """
    if not atlas_dir or not shutil.which("docker"):
        return False
    compose_file = os.path.join(atlas_dir, "docker-compose.yml")
    if not os.path.exists(compose_file):
        return False
    try:
        result = subprocess.run(
            compose_config.command(atlas_dir, ["config", "--services"]),
            cwd=atlas_dir, capture_output=True, text=True, timeout=5,
        )
        if result.returncode != 0:
            return False
        return "atlas-proxy" in result.stdout.split()
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
        return False


def _wait_for_proxy(timeout: float = 60.0) -> bool:
    """Poll PROXY_URL until it responds or timeout elapses. Used when
    the docker stack owns atlas-proxy and we're waiting for it to come
    up (e.g. user just ran `docker compose up -d` and the container
    is mid-startup)."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if _check_url(PROXY_URL, timeout=1):
            return True
        time.sleep(1.0)
    return False


def ensure_proxy(required_capability: Optional[str] = None) -> bool:
    """Ensure atlas-proxy is running, launching it locally if needed.

    Strategy:
    1. Already running on PROXY_PORT → use it (and align its workspace
       to the user's CWD if it's a Docker proxy — PC-038)
    2. Docker-compose stack OWNS atlas-proxy (#118) → don't compete.
       Either wait for it to come up (it's starting) or tell the user
       to bring the stack up. Launching a local proxy here would
       collide with the container on :8090.
    3. Go available + no docker stack → build (if needed) and launch
       locally from CWD
    4. Nothing available → return False
    """
    atlas_dir = _find_atlas_dir()

    # Already running?
    if _check_url(PROXY_URL):
        if required_capability and not _repair_proxy_capability(
            atlas_dir, required_capability
        ):
            return False
        _align_workspace(atlas_dir)
        return True

    # #118: macOS hybrid + Linux + CUDA/ROCm all run atlas-proxy in
    # docker. If the user's compose stack defines it, the stack owns
    # :8090 — don't auto-launch a competing local proxy. Two sub-cases:
    if _docker_compose_owns_proxy(atlas_dir):
        # The stack is configured. Wait for the proxy to bind — it
        # may be mid-startup (we polled too early).
        print("  Docker compose stack owns atlas-proxy. Waiting for it to come up...")
        if _wait_for_proxy(timeout=60):
            print(f"  Proxy responded on port {PROXY_PORT}")
            if required_capability and not _repair_proxy_capability(
                atlas_dir, required_capability
            ):
                return False
            _align_workspace(atlas_dir)
            return True
        # Still nothing after 60s — the stack probably isn't running.
        # Tell the user how to start it instead of silently launching a
        # local proxy that would collide later.
        print("  Proxy never responded. The docker stack is probably not running.")
        print("  Start it with:")
        print(f"    {compose_config.format_command(atlas_dir, ['up', '-d'])}")
        print("  Then re-run atlas.")
        return False

    # Try to find or build and launch locally (dev workflow, no docker).
    proxy_bin = _select_proxy_binary(atlas_dir)

    if proxy_bin:
        print(f"  Starting local proxy ({os.path.basename(proxy_bin)})...")
        if _launch_local_proxy(proxy_bin):
            if required_capability and not _proxy_supports_capability(
                required_capability
            ):
                print("  Local proxy started without the required demo capability")
                return False
            print(f"  Proxy ready on port {PROXY_PORT}")
            return True

    return False
