"""atlas doctor — comprehensive install diagnostic (PC-053).

Verifies an ATLAS install is healthy end-to-end. Runs ~20 checks across
the host environment, the docker stack, and a live request through the
proxy: individual checks (docker, compose, nvidia, model_file,
lens_weights, workspace_mounts, image_skew, tier_match
(PC-055), tier_constraints (PC-055.1), asa_steering (BiasBusters #4),
e2e_smoke), five per-container state checks (one per service in
`EXPECTED_SERVICES`), and five per-endpoint health checks. Designed to
be the answer to "is it really working?" — both for humans (pretty
terminal output) and for scripts (--json).

Invoke:
    atlas doctor                 # full check
    atlas doctor --quick         # skip e2e smoke test
    atlas doctor --json          # machine output (for bootstrap, CI)
    atlas doctor -v              # show detail for each check
"""

import argparse
import json
import os
import subprocess
import sys
import urllib.request
from dataclasses import dataclass, asdict
from typing import Optional, List, Dict, Tuple

from atlas import compose as compose_config
from atlas.commands import tier

# Shared ANSI colors + unicode-safe output primitives.
from atlas.display import (
    RESET, BOLD, DIM, RED, GREEN, YELLOW as YELL,
    UNICODE_OK, DASH, safe_print as _safe_print,
)

# Env-derived defaults live in atlas.env (shared with fit, lens,
# publish); re-imported here so doctor.MODEL_FILE-style access keeps
# working. Monkeypatching doctor.<NAME> steers doctor's own checks only —
# fit/lens/publish read atlas.env directly, so patch that module to
# steer them.
from atlas.env import (
    _ENV, PROXY_URL, LLAMA_URL, LENS_URL, SANDBOX_URL, V3_URL,
    MODEL_DIR, MODEL_FILE, MODEL_NAME, LLAMA_PORT, LENS_MODELS_DIR,
    atlas_root as _find_atlas_root,
)

EXPECTED_SERVICES = [
    "llama-server", "geometric-lens",
    "v3-service", "sandbox", "atlas-proxy",
]


@dataclass
class CheckResult:
    name: str
    status: str  # pass | warn | fail | skip
    message: str
    detail: Optional[str] = None


# ---------------------------------------------------------------------------
# Subprocess + HTTP helpers
# ---------------------------------------------------------------------------

def _run(cmd: List[str], timeout: int = 30,
         cwd: Optional[str] = None) -> Tuple[int, str, str]:
    try:
        p = subprocess.run(cmd, capture_output=True, text=True,
                           timeout=timeout, cwd=cwd)
        return p.returncode, p.stdout, p.stderr
    except (subprocess.TimeoutExpired, FileNotFoundError) as e:
        return 1, "", str(e)


def _http_get(url: str, timeout: int = 5) -> Tuple[bool, str]:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return True, resp.read().decode()
    except Exception as e:
        return False, str(e)


# ---------------------------------------------------------------------------
# Individual checks
# ---------------------------------------------------------------------------

def check_docker() -> CheckResult:
    rc, out, err = _run(["docker", "info", "--format", "{{.ServerVersion}}"])
    if rc != 0:
        return CheckResult("docker",  "fail",
            "daemon not reachable",
            (err or out).strip()[:200])
    return CheckResult("docker", "pass", f"daemon reachable (v{out.strip()})")


def check_compose() -> CheckResult:
    rc, out, err = _run(["docker", "compose", "version", "--short"])
    if rc != 0:
        return CheckResult("compose", "fail",
            "docker compose v2 not installed",
            (err or out).strip()[:200])
    return CheckResult("compose", "pass", f"v{out.strip()}")


def _port_listening(host: str, port: int, timeout: float = 2.0) -> bool:
    """True if a TCP server is accepting connections at host:port.
    Portable replacement for `nc -z` which has different flag conventions
    across GNU netcat / BSD nc / nmap-ncat / busybox nc — and may not be
    on PATH at all (notably alpine/socat doesn't ship it).
    """
    import socket
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except (OSError, socket.timeout):
        return False


def _resolve_backend(atlas_root: Optional[str] = None) -> Optional[str]:
    """Resolve which ATLAS_BACKEND the user has configured. Reads the
    shell env first (canonical), then falls back to .env in atlas_root.

    The shell-env-only check in main() missed the macOS hybrid case
    (#32): atlas init writes ATLAS_BACKEND=metal into .env but the user
    rarely sources .env before running atlas doctor, so the env-var
    check returns None and check_metal_native is skipped — leaving Mac
    users wondering why the metal-native diagnostic never appears.

    Returns the backend id string ('cuda' | 'rocm' | 'vulkan' | 'metal')
    or None if no backend is configured anywhere.
    """
    if not atlas_root:
        val = os.environ.get("ATLAS_BACKEND")
        return val.strip().lower() if val else None
    return compose_config.resolve_backend(atlas_root)


def check_arch() -> CheckResult:
    """Surface the host CPU architecture so users see why a given backend
    is or isn't available. Pass-status on x86_64 (default ATLAS target);
    warn on aarch64 Linux with a hint about the arm64 backend matrix;
    pass on Apple Silicon (the macOS hybrid Metal path is shipped, #32).

    See #115 for the multi-arch Docker build status.
    """
    arch = tier.arch_detect()
    if arch == "x86_64":
        return CheckResult("arch", "pass", "x86_64")
    if arch == "aarch64":
        # Apple Silicon gets a different message from arm64 Linux. On
        # macOS the path is native Metal (#32 hybrid), not vulkan-or-
        # cuda-sbsa-l4t. The Linux arm64 matrix doesn't apply here.
        if sys.platform == "darwin":
            return CheckResult("arch", "pass",
                "aarch64 (Apple Silicon) — Metal hybrid path supported (#32)")
        return CheckResult("arch", "warn",
            "aarch64 — vulkan + cuda (sbsa/l4t) only, no rocm",
            "AMD ROCm has no arm64 release. Use ATLAS_BACKEND=vulkan for "
            "AMD GPUs on arm64. NVIDIA CUDA needs the sbsa (DGX Spark) or "
            "l4t (Jetson) base image swap, see docs/SETUP.md#arm64.")
    return CheckResult("arch", "warn",
        f"unsupported arch '{arch}'",
        "ATLAS officially supports x86_64 and aarch64. Other arches may "
        "work via vulkan + lavapipe but are untested. See #115.")


def check_gpu() -> CheckResult:
    """Dispatcher: pick the right vendor-specific GPU check or warn if no
    GPU is detected. NVIDIA + AMD supported; Metal/SYCL not yet packaged.

    #115 addition: AMD GPU on aarch64 -> warn rather than dispatch to the
    rocm container check, since rocm has no arm64 release. The check would
    just fail with a confusing image-pull error otherwise.
    """
    gpus = tier.detect_gpu()
    if not gpus:
        return CheckResult("gpu", "warn",
            "no GPU detected (CPU-only mode — inference will be very slow)",
            "")
    primary = tier.primary_gpu(gpus)
    if primary is None:
        return CheckResult("gpu", "warn",
            "GPUs detected but none selectable",
            "tier.primary_gpu returned None")
    arch = tier.arch_detect()
    if primary.vendor == "nvidia":
        return _check_nvidia_via_docker()
    if primary.vendor == "amd":
        if arch != "x86_64":
            return CheckResult("gpu", "warn",
                f"AMD GPU on {arch}: ROCm has no {arch} release, use "
                f"ATLAS_BACKEND=vulkan instead (#115)",
                f"primary GPU: {primary.name}")
        return _check_amd_via_docker()
    # #32: Apple Silicon ships the Metal hybrid path. Defer the deeper
    # validation to check_metal_native (which fires when ATLAS_BACKEND
    # is metal) — at the gpu-dispatcher level just acknowledge that
    # Apple GPUs ARE supported now, the install just happens to live
    # outside Docker. Don't emit the old "not yet supported" warning.
    if primary.vendor == "apple":
        return CheckResult("gpu", "pass",
            f"[apple] {primary.name} ({primary.vram_gb:.1f} GB unified) "
            f"— Metal hybrid path supported (#32). "
            f"See metal-native check for native llama-server status.")
    return CheckResult("gpu", "warn",
        f"vendor '{primary.vendor}' detected but Docker integration not yet supported "
        f"(SYCL -> roadmap)",
        f"primary GPU: {primary.name}")


def _check_nvidia_via_docker() -> CheckResult:
    """Verify nvidia-container-toolkit by running nvidia-smi inside Docker."""
    # Use the smallest CUDA base image available to keep the check fast.
    rc, out, err = _run([
        "docker", "run", "--rm", "--gpus", "all",
        "nvidia/cuda:12.0.0-base-ubuntu22.04",
        "nvidia-smi", "--query-gpu=name", "--format=csv,noheader",
    ], timeout=120)
    if rc != 0:
        # Distinguish "no GPU" from "toolkit broken"
        joined = (err + out).lower()
        if "could not select device driver" in joined or "nvidia-container" in joined:
            return CheckResult("gpu", "fail",
                "nvidia-container-toolkit not configured",
                (err or out).strip()[:300])
        if "no nvidia gpu" in joined or "no devices" in joined:
            return CheckResult("gpu", "warn",
                "no NVIDIA GPU visible to Docker (CPU-only mode)",
                (err or out).strip()[:300])
        return CheckResult("gpu", "fail",
            "nvidia-smi failed inside Docker",
            (err or out).strip()[:300])
    gpus = [g.strip() for g in out.strip().split("\n") if g.strip()]
    return CheckResult("gpu", "pass",
        f"[nvidia] {len(gpus)} GPU(s): {', '.join(gpus)}")


def _check_amd_via_docker() -> CheckResult:
    """Verify ROCm Docker passthrough by running rocm-smi inside a ROCm
    container. Unlike NVIDIA, ROCm doesn't need a separate container
    runtime — just /dev/kfd + /dev/dri device passthrough with the
    video + render groups. This check validates that whole chain.
    """
    rc, out, err = _run([
        "docker", "run", "--rm",
        "--device=/dev/kfd", "--device=/dev/dri",
        "--group-add", "video", "--group-add", "render",
        "rocm/rocm-terminal:latest",
        "rocm-smi", "--showproductname",
    ], timeout=180)  # +60s headroom for the first-time image pull (~2 GB)
    if rc != 0:
        joined = (err + out).lower()
        if "permission denied" in joined or "no such device" in joined:
            return CheckResult("gpu", "fail",
                "AMD GPU detected but Docker can't reach /dev/kfd — "
                "check amdgpu kernel driver + render/video group membership",
                (err or out).strip()[:300])
        if "no gpus found" in joined or "no rocm devices" in joined:
            return CheckResult("gpu", "warn",
                "no AMD GPU visible to Docker (CPU-only mode)",
                (err or out).strip()[:300])
        return CheckResult("gpu", "fail",
            "rocm-smi failed inside Docker",
            (err or out).strip()[:300])
    # rocm-smi product output is wide; count lines starting with "GPU[" or
    # "card" as a GPU entry (output format varies across ROCm versions).
    gpus = [ln.strip() for ln in out.strip().splitlines()
            if ln.strip() and not ln.startswith(("=", "-"))]
    summary = "; ".join(g[:80] for g in gpus[:3]) if gpus else "rocm-smi succeeded"
    return CheckResult("gpu", "pass", f"[amd] {summary}")


def _check_vulkan_via_docker() -> CheckResult:
    """Verify Vulkan device passthrough by running vulkaninfo inside a
    minimal Mesa-Vulkan container (PC-114, #114).

    Unlike CUDA / ROCm this doesn't require a vendor-specific runtime
    or kernel driver — just /dev/dri passthrough so the Mesa ICDs
    (RADV/ANV/lavapipe) inside the image can find a device. NVIDIA
    users on Vulkan still need the toolkit (same as CUDA path) but
    that's caught by `check_gpu` separately.

    We use the same ubuntu+mesa stack the production Dockerfile.vulkan
    builds on so this validates the exact compat surface the runtime
    image will see. The throwaway container is ~150 MB after first
    pull — bigger than the ROCm check's terminal image but still
    bounded.
    """
    # /dev/dri may not exist on hosts with no GPU at all (or macOS Docker
    # Desktop). Short-circuit before touching docker — produces a clean
    # "no GPU passthrough" message instead of a confusing docker error.
    if not os.path.exists("/dev/dri"):
        return CheckResult("vulkan", "warn",
            "no /dev/dri on host — Vulkan container would only see "
            "the CPU lavapipe ICD (very slow)",
            "On Linux: install kernel modules for your GPU + ensure "
            "the render-node devices exist. On macOS: Vulkan-in-Docker "
            "uses MoltenVK via qemu; native install (#32) is the fast path.")
    rc, out, err = _run([
        "docker", "run", "--rm",
        "--device=/dev/dri",
        "--group-add", "video", "--group-add", "render",
        "ubuntu:22.04",
        "bash", "-c",
        # apt-install Mesa Vulkan stack + run vulkaninfo summary. Cap
        # output so a verbose ICD enum doesn't blow our 300-char detail
        # budget.
        ("apt-get update -qq >/dev/null && "
         + "apt-get install -y -qq libvulkan1 mesa-vulkan-drivers vulkan-tools "
         + ">/dev/null 2>&1 && "
         + "vulkaninfo --summary 2>&1 | head -40"),
    ], timeout=300)  # apt + image pull on cold cache
    if rc != 0:
        joined = (err + out).lower()
        if "permission denied" in joined or "no such device" in joined:
            return CheckResult("vulkan", "fail",
                "Vulkan device passthrough failed — check render/video "
                "group membership on the host",
                (err or out).strip()[:300])
        if "could not find any vulkan" in joined:
            return CheckResult("vulkan", "warn",
                "Vulkan loader found no ICDs (no GPU drivers visible to "
                "the container; lavapipe CPU fallback would still work)",
                (err or out).strip()[:300])
        return CheckResult("vulkan", "fail",
            "vulkaninfo failed inside the test container",
            (err or out).strip()[:300])
    # Pull a one-line summary out of vulkaninfo's `deviceName = ...` rows.
    devices = [ln.split("=")[-1].strip() for ln in out.splitlines()
               if "deviceName" in ln]
    if not devices:
        return CheckResult("vulkan", "warn",
            "vulkaninfo ran but no deviceName lines — Vulkan stack is "
            "responsive but couldn't enumerate physical devices",
            out.strip()[:200])
    summary = "; ".join(d[:60] for d in devices[:3])
    return CheckResult("vulkan", "pass", f"[vulkan] {len(devices)} ICD(s): {summary}")


def _check_metal_native(atlas_root: Optional[str] = None) -> CheckResult:
    """Verify the macOS hybrid path (#32) is wired correctly: the native
    llama-server binary exists where the setup script puts it, and the
    docker stack is configured to forward to it via host.docker.internal.

    Three failure modes we surface here:
      1. Setup script never ran   -> binary missing at $HOME/.atlas/macos/bin/
      2. Setup ran but binary won't execute (corrupt download, wrong arch)
      3. The macos compose overlay isn't applied (so docker is trying to
         pull/build a normal llama-server image that won't work on Mac)

    Only fires when ATLAS_BACKEND=metal. On Linux + Windows this would
    just be noise. NOT a Docker check (the binary lives on the host),
    so it's cheap (~1ms) and runs unconditionally for Mac users.
    """
    if sys.platform != "darwin":
        return CheckResult("metal-native", "skip",
            "not on macOS — metal hybrid path doesn't apply", "")

    # Expected setup-script output location. Keep aligned with
    # scripts/atlas-setup-macos.sh DEFAULT_PREFIX.
    values = compose_config.read_env_file(atlas_root) if atlas_root else _ENV
    prefix = (os.environ.get("ATLAS_MACOS_PREFIX")
              or values.get("ATLAS_MACOS_PREFIX")
              or "~/.atlas/macos")
    prefix = os.path.expanduser(prefix)
    binary = os.path.join(prefix, "bin", "llama-server-metal")

    if not os.path.isfile(binary):
        return CheckResult("metal-native", "fail",
            "native llama-server not found — run scripts/atlas-setup-macos.sh",
            f"expected at {binary}. See docs/SETUP_MACOS.md.")

    if not os.access(binary, os.X_OK):
        return CheckResult("metal-native", "fail",
            "native llama-server is not executable — re-run "
            "scripts/atlas-setup-macos.sh --rebuild",
            f"{binary} exists but lacks +x. Likely a botched copy or "
            f"transferred over a filesystem that strips perms (smb/nfs).")

    # Sanity-check the binary at least loads. Exit code alone isn't
    # reliable: llama-server treats `--help` as a parse failure (prints
    # usage, exits 1) by convention. Instead look for usage markers in
    # the combined output — anything matching means the binary's main
    # ran and printed the help text. No usage markers + nonzero exit =
    # the binary never reached main (dyld failure, missing dylib,
    # corrupt build from an interrupted cmake).
    rc, out, err = _run([binary, "--help"], timeout=5)
    combined = (out + err).lower()
    usage_markers = ("usage", "options", "--ctx-size", "llama-server")
    looks_like_usage = any(m in combined for m in usage_markers)
    if rc != 0 and not looks_like_usage:
        return CheckResult("metal-native", "fail",
            "native llama-server exists but won't run "
            f"(exit {rc}, no usage output) — try --rebuild",
            (err or out).strip()[:300] or "binary produced no output")

    # Confirm the host port is listening. If the user ran setup but
    # hasn't started atlas-llama-macos.sh yet, surface that as a warn
    # (not fail) — they may just not be ready yet. Use a small Python
    # socket probe instead of `nc` since macOS / BSD nc has different
    # flags than GNU nc and may not be on PATH at all.
    if not _port_listening("127.0.0.1", LLAMA_PORT, timeout=2):
        return CheckResult("metal-native", "warn",
            f"native llama-server installed at {binary} but nothing "
            f"listening on :{LLAMA_PORT} — start it with scripts/atlas-llama-macos.sh",
            "Open a separate terminal and run the launcher; this check "
            "will turn green once the server is up and serving.")

    return CheckResult("metal-native", "pass",
        f"native llama-server up at {binary}, listening on :{LLAMA_PORT}")


def _compose_ps(project_dir: str) -> List[Dict]:
    """Run `docker compose ps --format json` and parse (handles both NDJSON and array forms).

    Must run from `project_dir` — that's where docker-compose.yml lives.
    Without this, `atlas doctor` invoked from outside the repo sees
    "no containers" even when the stack is fully healthy.
    """
    try:
        cmd = compose_config.command(
            project_dir, ["ps", "--all", "--format", "json"])
    except FileNotFoundError:
        return []
    rc, out, err = _run(cmd, cwd=project_dir)
    if rc != 0 or not out.strip():
        return []
    services: List[Dict] = []
    # Newer compose: NDJSON (one object per line)
    for line in out.strip().split("\n"):
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
            if isinstance(obj, list):
                services.extend(obj)
            else:
                services.append(obj)
        except json.JSONDecodeError:
            continue
    return services


def check_containers(services: List[Dict], start_hint: str = "docker compose up -d"
                     ) -> List[CheckResult]:
    if not services:
        return [CheckResult("containers", "fail",
            f"no containers found — run `{start_hint}` first",
            "compose ps returned empty")]

    found = {s.get("Service", s.get("Name", "")): s for s in services}
    results: List[CheckResult] = []
    for name in EXPECTED_SERVICES:
        svc = found.get(name)
        if svc is None:
            results.append(CheckResult(f"container/{name}", "fail",
                "not running",
                "service not in `docker compose ps` output"))
            continue
        state = svc.get("State", "?")
        health = svc.get("Health", "")
        status_str = svc.get("Status", "")
        if state == "running" and health in ("healthy", ""):
            results.append(CheckResult(f"container/{name}", "pass", state))
        elif state == "running" and health == "starting":
            results.append(CheckResult(f"container/{name}", "warn",
                f"{state}/starting", "still warming up — re-run doctor in 30s"))
        else:
            results.append(CheckResult(f"container/{name}", "fail",
                f"{state}/{health or '-'}", status_str))
    return results


def check_health_endpoints() -> List[CheckResult]:
    endpoints = [
        ("llama",   f"{LLAMA_URL}/health"),
        ("lens",    f"{LENS_URL}/health"),
        ("v3",      f"{V3_URL}/health"),
        ("sandbox", f"{SANDBOX_URL}/health"),
        ("proxy",   f"{PROXY_URL}/health"),
    ]
    results = []
    for name, url in endpoints:
        ok, body = _http_get(url)
        if not ok:
            detail = body[:200]
            if name == "llama":
                # The most common cause: the model + KV + compute budget
                # doesn't fit in VRAM, so llama-server (--fit off) refuses
                # to start and the container crash-loops.
                detail += ("\nIf `docker compose logs llama-server` shows a "
                           "CUDA out-of-memory allocation error, the runtime "
                           "budget doesn't fit this GPU — run `atlas tier "
                           "fit --write`, then recreate the container.")
            results.append(CheckResult(f"health/{name}", "fail",
                "endpoint unreachable", detail))
            continue
        try:
            data = json.loads(body)
            status = data.get("status", "ok")
        except json.JSONDecodeError:
            status = "ok (non-json)"
        normalized = str(status).strip().lower()
        result_status = (
            "pass" if normalized in {"ok", "healthy", "ready",
                                      "ok (non-json)"}
            else "warn"
        )
        results.append(CheckResult(f"health/{name}", result_status,
                                   str(status), body[:200]))
    return results


def check_internal_auth(atlas_root: str) -> List[CheckResult]:
    """Internal service auth: token file status + live enforcement.

    Three outcomes per the auth design (proxy/auth.go, ADR 0001):
      - no token file       -> warn (auth disabled; localhost-only model)
      - token, loose perms  -> fail (the credential is readable by others)
      - token configured    -> probe one authenticated surface both ways:
        an unauthenticated POST must 401 and an authenticated GET must
        not 401 — this catches the two silent failure modes (a service
        that never loaded the token, and a client/server token mismatch
        after rotation without restart). Token values never appear in
        output.
    """
    from atlas import token as token_mod
    results: List[CheckResult] = []
    ok, detail = token_mod.check_file_permissions(atlas_root)
    tok = token_mod.read_token(atlas_root)
    if not tok:
        results.append(CheckResult(
            "internal_auth", "warn",
            "internal service auth disabled (no secrets/service-token)",
            "run `atlas init` (or `atlas init --rotate-token`) to enable; "
            "services stay open-localhost until then"))
        return results
    if not ok:
        results.append(CheckResult("internal_auth", "fail", detail))
        return results

    # Live enforcement probe against the proxy (representative surface;
    # /health stays open by design so probe /v1/models).
    probe_url = f"{PROXY_URL}/v1/models"
    try:
        req = urllib.request.Request(probe_url, method="GET")
        req.add_header("Authorization", "Bearer atlas-doctor-wrong-token")
        try:
            urllib.request.urlopen(req, timeout=5)
            enforced = False
        except urllib.error.HTTPError as e:
            enforced = (e.code == 401)
        if not enforced:
            results.append(CheckResult(
                "internal_auth", "warn",
                "token file present but the proxy accepts wrong tokens",
                "the proxy container predates the token or lacks the "
                "secrets mount — docker compose up -d atlas-proxy"))
            return results
        req2 = urllib.request.Request(probe_url, method="GET")
        req2.add_header("Authorization", f"Bearer {tok}")
        try:
            urllib.request.urlopen(req2, timeout=5)
            results.append(CheckResult(
                "internal_auth", "pass",
                "enabled and enforced (401 without token, 200 with)"))
        except urllib.error.HTTPError as e:
            if e.code == 401:
                results.append(CheckResult(
                    "internal_auth", "fail",
                    "host token rejected by the proxy (rotation without "
                    "restart?)",
                    "docker compose restart  # services reload the token"))
            else:
                results.append(CheckResult(
                    "internal_auth", "pass",
                    f"enforced (wrong token 401; valid token {e.code})"))
    except Exception as e:
        results.append(CheckResult(
            "internal_auth", "skip",
            f"token present; enforcement probe skipped ({type(e).__name__})"))
    return results


def check_status_dimensions() -> List[CheckResult]:
    """Render the canonical seven-dimension lens/ASA status from the
    proxy's /v1/calibration/status endpoint — the SAME source the TUI
    badge reads, so doctor and the TUI cannot disagree. Emits one result
    carrying all seven rows. It fails when the agent is blocked: the lens
    is required (docs/adr/0011), and the proxy refuses every request while
    it cannot score, sending the user here.
    """
    ok, body = _http_get(f"{PROXY_URL}/v1/calibration/status", timeout=5)
    if not ok:
        return [CheckResult("status_dimensions", "skip",
                            "proxy /v1/calibration/status unreachable "
                            "(is the stack up?)")]
    try:
        import json as _json
        dims = _json.loads(body).get("dimensions", [])
    except Exception:
        return [CheckResult("status_dimensions", "skip",
                            "calibration status returned non-JSON")]
    if not dims:
        return [CheckResult("status_dimensions", "skip",
                            "no dimensions in calibration status "
                            "(older proxy image?)")]
    # An uncalibrated lens still scores, so it is informational (pass),
    # with the per-dimension status in the detail. A blocked agent is not:
    # no request runs until the lens can score.
    lines = [f"{d.get('name')}: {d.get('status')} — {d.get('detail')}"
             for d in dims]
    blocked = [d for d in dims if d.get("status") == "blocked"]
    if blocked:
        return [CheckResult("status_dimensions", "fail",
                            "requests are refused: " + str(blocked[0].get("detail")),
                            detail="\n".join(lines))]
    return [CheckResult("status_dimensions", "pass",
                        "lens/ASA status by dimension",
                        detail="\n".join(lines))]


def check_model_file(atlas_root: str) -> CheckResult:
    if not MODEL_FILE:
        return CheckResult("model_file", "fail",
            "ATLAS_MODEL_FILE is not configured",
            "Run `atlas init` to select a registry model, or set "
            "ATLAS_MODEL_FILE + ATLAS_MODEL_NAME in .env for a BYO GGUF.")
    # MODEL_DIR is typically `./models` (relative to the compose cwd).
    # Resolve relative paths against atlas_root, not the doctor's cwd.
    base = MODEL_DIR if os.path.isabs(MODEL_DIR) else os.path.join(atlas_root, MODEL_DIR)
    path = os.path.normpath(os.path.join(base, MODEL_FILE))
    if not os.path.exists(path):
        return CheckResult("model_file", "fail",
            f"missing: {path}",
            "Registry model? run `atlas model install <name>`. "
            "BYO model? place the .gguf in ATLAS_MODELS_DIR "
            f"({MODEL_DIR}) and set ATLAS_MODEL_FILE + ATLAS_MODEL_NAME in "
            ".env — see docs/CONFIGURATION.md \"Adding your own model\", "
            "or run `atlas model install --url <hf-url>` / `atlas onboard`.")
    size = os.path.getsize(path)
    if size < 100 * 1024 * 1024:  # < 100 MB
        return CheckResult("model_file", "warn",
            f"{path} exists but only {size} bytes — likely truncated",
            "expected > 1 GB for a typical GGUF; re-download the selected "
            f"registry model or re-fetch your own .gguf into {MODEL_DIR}.")
    gb = size / (1024 * 1024 * 1024)
    return CheckResult("model_file", "pass", f"{MODEL_FILE} ({gb:.1f} GB)")


def check_lens_weights(atlas_root: str) -> CheckResult:
    """Report the selected model's real Lens capability.

    Published legacy weights remain useful inputs for rebuilding, but the
    current runtime deliberately disables interventions until model identity,
    C(x) normalization, and G(x) thresholds are present and valid.
    """
    try:
        from atlas.commands import lens as lens_cmd, model_registry
    except ImportError as exc:
        return CheckResult("lens_weights", "skip",
                           "Lens validation unavailable", str(exc))

    selected = model_registry.by_name(MODEL_NAME)
    if selected is None and MODEL_FILE:
        selected = model_registry.by_name(
            os.path.basename(MODEL_FILE).rsplit(".", 1)[0])

    weights_dir = (model_registry.lens_artifact_dir_for(selected, atlas_root)
                   if selected is not None else None)
    if not weights_dir:
        configured = (os.environ.get("ATLAS_LENS_MODELS")
                      or compose_config.read_env_file(atlas_root).get(
                          "ATLAS_LENS_MODELS")
                      or LENS_MODELS_DIR)
        weights_dir = (configured if os.path.isabs(configured)
                       else os.path.normpath(os.path.join(atlas_root,
                                                          configured)))

    expected = (list(selected.lens_artifact_files)
                if selected is not None else ["cost_field.pt"])
    missing = [name for name in expected
               if not os.path.isfile(os.path.join(weights_dir, name))]
    if missing:
        status = ("fail" if selected is not None
                  and selected.lens_status == "supported" else "warn")
        return CheckResult(
            "lens_weights", status, f"missing: {', '.join(missing)}",
            f"expected in {weights_dir}; run `atlas lens build` for the "
            "selected model or point ATLAS_LENS_MODELS at its bundle")

    runtime_missing = lens_cmd._missing_runtime_artifacts(weights_dir)
    if runtime_missing:
        return CheckResult(
            "lens_weights", "warn",
            "legacy Lens weights present; calibrated interventions disabled",
            f"{weights_dir} is missing {', '.join(runtime_missing)}. "
            "Run `atlas lens build` for this model before claiming calibrated "
            "C(x)/G(x) support.")

    invalid = lens_cmd._invalid_runtime_artifacts(
        weights_dir, MODEL_NAME, 0)
    if invalid:
        return CheckResult(
            "lens_weights", "fail", "Lens calibration is invalid",
            "; ".join(invalid))

    return CheckResult("lens_weights", "pass",
                       f"calibrated model bundle in {weights_dir}")


def check_asa_steering(atlas_root: str) -> CheckResult:
    """ASA steering vector (BiasBusters #4) presence.

    Warn-not-fail: ATLAS works without it. When present, llama-server
    auto-applies it on startup via `--control-vector-scaled` (see
    `inference/entrypoint-v3.1.sh`). When absent, the
    `structural_edit`-vs-`edit_file` proposal bias is unsteered and we lean
    entirely on the grammar gate downstream.

    Recovery is documented in `geometric-lens/asa_calibration/README.md`
    — or just re-run `./scripts/atlas-bootstrap.sh` which builds it as
    part of install (with HuggingFace prebuilt fallback).
    """
    try:
        from atlas.commands import asa as _asa
        verdict = _asa._check_asa(atlas_root)
    except Exception as exc:
        return CheckResult("asa_steering", "warn",
                           "ASA validation unavailable", str(exc))

    if verdict.verdict != "compat":
        return CheckResult("asa_steering", "warn",
                           "ASA steering disabled", verdict.reason)
    status = "warn" if verdict.unverified else "pass"
    message = ("ASA vector present but dimension unverified"
               if verdict.unverified else "ASA vector matches selected model")
    return CheckResult("asa_steering", status, message, verdict.reason)


def check_grammar_mode(atlas_root: Optional[str] = None) -> CheckResult:
    """Does ATLAS_GRAMMAR_MODE match the registry's profile for the model?

    The mode is a property of the model: gemma under the strict schema
    grammar emits `done` instead of calling tools. `atlas init` writes the
    registry's value; a .env edited by hand, or written before the registry
    carried it, can disagree, and then the install runs a configuration the
    model was not measured with.
    """
    from atlas.commands import model_registry
    values = compose_config.read_env_file(atlas_root) if atlas_root else _ENV
    model = (values.get("ATLAS_MODEL_FILE") or values.get("ATLAS_MODEL_NAME") or "")
    m = (model_registry.by_model_file(model) or model_registry.by_name(model)) if model else None
    if m is None:
        return CheckResult("grammar_mode", "skip",
                           "model not in the registry; no grammar profile to compare")
    effective = (values.get("ATLAS_GRAMMAR_MODE") or "strict").strip().lower()
    if effective != m.grammar_mode:
        return CheckResult(
            "grammar_mode", "warn",
            f"ATLAS_GRAMMAR_MODE is {effective}; {m.name} is measured with {m.grammar_mode}",
            f"set ATLAS_GRAMMAR_MODE={m.grammar_mode} in .env and restart the proxy")
    return CheckResult("grammar_mode", "pass",
                       f"ATLAS_GRAMMAR_MODE={effective} matches {m.name}")


def check_tier_constraints(atlas_root: Optional[str] = None) -> CheckResult:
    """PC-055.1 cross-check: does the host meet the recommended tier's
    per-axis minimums (RAM, CPU, disk)?

    Distinct from `tier_match`:
      - `tier_match` asks "is the configured model right for this hardware?"
      - `tier_constraints` asks "can this hardware actually run anything at
        the tier we'd recommend, given ATLAS's CPU/RAM/disk needs?"

    Catches the "16 GB GPU but 8 GB RAM" case where llama-server fits on
    the GPU but the host OOMs during V3 pipeline + sandbox compiles.

    Passes `atlas_root` to tier.probe() so the disk-free check measures
    the partition where models will actually live (typically ATLAS_INSTALL_DIR
    or the repo root), not `/`. Without this, a user with `/opt/atlas` on
    a separate `/data` mount would get a misleading disk check.
    """
    try:
        from atlas.commands import tier
    except ImportError as e:
        return CheckResult("tier_constraints", "skip",
            "tier module unavailable", str(e))
    p = tier.probe(install_dir=atlas_root)
    if not p.has_gpu:
        return CheckResult("tier_constraints", "skip",
            "no GPU detected (cpu tier)")
    recommended = tier.classify(p)
    checks = tier.evaluate_constraints(p, recommended)
    overall = tier.overall_status(checks)
    failed = [c for c in checks if c.status == "fail"]
    warned = [c for c in checks if c.status == "warn"]
    if overall == "fail":
        return CheckResult("tier_constraints", "warn",
            f"{len(failed)} hard constraint(s) below {recommended.tier}-tier minimum: "
            f"{', '.join(c.name for c in failed)}",
            "\n".join(c.message for c in failed) +
            "\n\nATLAS may OOM or fail to install at the recommended tier. "
            "Either upgrade host resources or downgrade tier "
            "(`atlas tier list` for alternatives).")
    if overall == "warn":
        return CheckResult("tier_constraints", "warn",
            f"{len(warned)} borderline constraint(s) for {recommended.tier} tier: "
            f"{', '.join(c.name for c in warned)}",
            "\n".join(c.message for c in warned) +
            "\n\nATLAS will run but may struggle under load.")
    return CheckResult("tier_constraints", "pass",
        f"{recommended.tier} tier fits comfortably "
        f"({p.cpu_cores} cores, {p.system_ram_gb:.0f} GB RAM, "
        f"{p.disk_free_gb:.0f} GB disk)")


def check_tier_match() -> CheckResult:
    """PC-055 cross-check: warn if .env settings overshoot the host's tier.

    Example: a user on tier-small selecting a model above that tier's
    memory budget may OOM. Doctor flags this as a
    warning so the user knows to either downgrade the model or upgrade
    the GPU. We never hard-fail on tier mismatch — sometimes the user
    knows better than the heuristic (e.g., they pre-allocated VRAM
    elsewhere and want a smaller-than-recommended model).
    """
    try:
        from atlas.commands import tier, model_registry
    except ImportError as e:
        return CheckResult("tier_match", "skip",
            "tier module unavailable", str(e))
    p = tier.probe()
    if not p.has_gpu:
        return CheckResult("tier_match", "skip",
            "no GPU detected (cpu tier)")
    recommended = tier.classify(p)
    rec_model = model_registry.for_tier(recommended.tier)
    actual_model = MODEL_FILE
    if rec_model is not None and actual_model == rec_model.model_file:
        # PC-056.1: even on exact tier match, cross-check that the
        # claimed Lens artifacts actually exist on disk. Registry can
        # say "supported" while the .pt files are missing — config
        # drift that leaves the lens unable to score, which stops agent
        # work at run time.
        try:
            from atlas.commands import model_registry
            atlas_root = _find_atlas_root()
            artifact_state = model_registry.lens_artifacts_present(
                rec_model, atlas_root)
            if not artifact_state["ok"]:
                return CheckResult("tier_match", "warn",
                    f"`{actual_model}` registered as Lens-supported but "
                    f"{len(artifact_state['missing_files'])} artifact "
                    f"file(s) missing: "
                    f"{', '.join(artifact_state['missing_files'])}",
                    f"Expected in {artifact_state['expected_dir']}. "
                    f"Without these files the lens cannot score, and ATLAS "
                    f"stops agent work, although the registry says it "
                    f"should work. Either "
                    f"download the artifacts (see "
                    f"geometric-lens/geometric_lens/models/README.md) "
                    f"or set ATLAS_LENS_MODELS to point at a dir that "
                    f"has them.")
        except (ImportError, AttributeError):
            # best-effort: swallow on failure (caller continues)
            pass
        return CheckResult("tier_match", "pass",
            f"{recommended.tier} tier matches configured model "
            f"({rec_model.model_display})")
    # Mismatch — figure out direction. Reverse-lookup which tier owns
    # the configured model, then compare.
    actual_tier_name = model_registry.tier_for_model(actual_model)
    if actual_tier_name is None:
        return CheckResult("tier_match", "warn",
            f"configured model `{actual_model}` is not in any tier preset",
            f"host classified as {recommended.tier}; consider one of the "
            f"presets: `atlas tier list`")
    # Warn only when actual > recommended (overshoot risks OOM).
    # Undershoot (smaller model than tier supports) is fine — just
    # leaves performance on the table.
    tiers_order = ["cpu", "small", "medium", "large", "xlarge"]
    rec_idx = tiers_order.index(recommended.tier)
    act_idx = tiers_order.index(actual_tier_name)
    if act_idx > rec_idx:
        rec_display = (rec_model.model_display if rec_model is not None
                       else f"the {recommended.tier}-tier preset")
        return CheckResult("tier_match", "warn",
            f"running {actual_tier_name}-tier model on {recommended.tier}-tier "
            f"hardware ({p.vram_gb:.1f} GB VRAM)",
            f"OOM risk. Recommended for your VRAM: "
            f"{rec_display}. Run `atlas tier` for detail.")
    # Undershoot path: smaller model than the tier supports. Normally
    # safe (just leaves perf on the table). PC-056: also warn if the
    # actual model has no Lens artifacts — that means G(x) silently
    # no-ops at runtime, regardless of tier-fit. PC-056.1: also warn
    # if the model claims `supported` but the artifact files are
    # actually missing on disk — config drift between registry claim
    # and reality.
    try:
        from atlas.commands import model_registry
        actual_model_record = model_registry.by_name(
            actual_model.rsplit(".", 1)[0])
        if actual_model_record is not None and \
                actual_model_record.lens_status != "supported":
            return CheckResult("tier_match", "warn",
                f"configured model `{actual_model}` has Lens status "
                f"`{actual_model_record.lens_status}`: the lens cannot score "
                f"it, so ATLAS stops agent work on it",
                "Build a Lens bundle for it (`atlas bench`, then "
                "`atlas lens build --from-results`), or run "
                "`atlas model recommend` for a model that has one.")
        # PC-056.1: model claims supported — verify artifact files actually
        # exist where the registry says they should.
        if actual_model_record is not None and \
                actual_model_record.lens_status == "supported":
            atlas_root = _find_atlas_root()
            artifact_state = model_registry.lens_artifacts_present(
                actual_model_record, atlas_root)
            if not artifact_state["ok"]:
                return CheckResult("tier_match", "warn",
                    f"`{actual_model}` registered as Lens-supported but "
                    f"{len(artifact_state['missing_files'])} artifact "
                    f"file(s) missing: "
                    f"{', '.join(artifact_state['missing_files'])}",
                    f"Expected in {artifact_state['expected_dir']}. "
                    f"Without these files the lens cannot score, and ATLAS "
                    f"stops agent work, although the registry says it "
                    f"should work. Either "
                    f"download the artifacts (see "
                    f"geometric-lens/geometric_lens/models/README.md) "
                    f"or set ATLAS_LENS_MODELS to point at a dir that "
                    f"has them.")
    except (ImportError, AttributeError):
        # best-effort: swallow on failure (caller continues)
        pass
    return CheckResult("tier_match", "pass",
        f"running {actual_tier_name}-tier model on {recommended.tier}-tier "
        f"hardware (under-utilized but safe)")


def check_workspace_mounts(services: List[Dict]) -> CheckResult:
    """Proxy and sandbox must bind the SAME host directory as /workspace.

    The proxy serves read_file/write_file against ITS /workspace while
    run_command executes in the SANDBOX's /workspace. When the two
    containers are recreated at different times with different
    ATLAS_PROJECT_DIR values (or one from a different cwd), they silently
    bind different host directories — file tools then operate on a
    different filesystem than shell commands, the agent is told files
    that exist "don't exist", and nothing else detects it (every /health
    passes). Observed live 2026-07-18: proxy bound to the repo, sandbox
    to the project dir; every agent session gave up believing its files
    were missing.
    """
    names: Dict[str, str] = {}
    for s in services:
        svc = s.get("Service", "")
        if svc in ("atlas-proxy", "sandbox") and s.get("State") == "running":
            names[svc] = s.get("Name") or ""
    if len(names) < 2:
        return CheckResult("workspace_mounts", "skip",
            "proxy or sandbox not running (see container checks)")
    fmt = '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
    sources: Dict[str, str] = {}
    for svc, cname in names.items():
        rc, out, err = _run(["docker", "inspect", cname, "--format", fmt])
        if rc != 0:
            return CheckResult("workspace_mounts", "skip",
                f"docker inspect {cname} failed", (err or out)[:200])
        sources[svc] = out.strip()
    proxy_src = sources.get("atlas-proxy", "")
    sandbox_src = sources.get("sandbox", "")
    if not proxy_src or not sandbox_src:
        return CheckResult("workspace_mounts", "warn",
            "no /workspace bind mount found on proxy or sandbox",
            f"proxy={proxy_src or '(none)'} sandbox={sandbox_src or '(none)'}")
    if os.path.realpath(proxy_src) == os.path.realpath(sandbox_src):
        return CheckResult("workspace_mounts", "pass",
            f"proxy and sandbox share {proxy_src}")
    return CheckResult("workspace_mounts", "fail",
        "proxy and sandbox bind DIFFERENT host dirs as /workspace "
        f"{DASH} file tools and run_command are operating on different "
        "filesystems (agent sessions will see missing files)",
        f"proxy={proxy_src} sandbox={sandbox_src} {DASH} set "
        "ATLAS_PROJECT_DIR to your project directory in .env, then "
        "`docker compose up -d --force-recreate atlas-proxy sandbox`")


def check_image_skew(services: List[Dict]) -> CheckResult:
    """PC-052 follow-up: warn if the 5 atlas-* images aren't on the same tag."""
    atlas_imgs = [s.get("Image", "") for s in services
                  if "atlas-" in s.get("Image", "")]
    if not atlas_imgs:
        return CheckResult("image_skew", "skip",
            "no atlas-* images found in compose ps")
    tags = set()
    for img in atlas_imgs:
        if ":" in img:
            tags.add(img.rsplit(":", 1)[1])
        else:
            tags.add("<no-tag>")
    if len(tags) > 1:
        return CheckResult("image_skew", "warn",
            f"mixed tags across atlas-* services: {', '.join(sorted(tags))}",
            "Pin ATLAS_IMAGE_TAG in .env to align all 5 services. "
            "Mixing major versions can break inter-service contracts.")
    return CheckResult("image_skew", "pass",
        f"all atlas-* images on tag :{next(iter(tags))}")


def check_inference_contract() -> CheckResult:
    """Does the llama-server behind ATLAS serve what the lens needs?

    Generation working says nothing about the two extras the Geometric Lens
    depends on: pooled self-embeddings on /embedding, and the PC-202 per-layer
    hidden-states extension. A bring-your-own llama-server that generates
    perfectly can be missing both, and the symptom is not an error — it is
    lens scores that look plausible and are numerically meaningless.

    GH #146 reported exactly this ("issues with embeddings and the PC-202
    hidden-states patch") after pointing ATLAS at a host-run server. Nothing
    in `atlas doctor` looked, so the run said 23 passed and the problem
    surfaced later as bad scores.

    Reuses atlas.client.probe_llama, the same probe `atlas lens check` runs.
    """
    try:
        from atlas.client import probe_llama
    except Exception as e:  # pragma: no cover - import guard
        return CheckResult("inference_contract", "skip",
                           f"probe unavailable: {type(e).__name__}")

    probe = probe_llama()
    if not probe.reachable:
        return CheckResult("inference_contract", "skip",
                           f"llama-server unreachable ({probe.error or 'no detail'})")

    missing = []
    if probe.embedding_dim <= 0:
        missing.append("/embedding returns no vector (start with --embeddings)")
    if not probe.has_hidden_states_patch:
        missing.append("no per-layer hidden states (PC-202 extension absent)")

    if not missing:
        return CheckResult("inference_contract", "pass",
                           f"embeddings {probe.embedding_dim}-dim + per-layer hidden states")

    # Not a hard failure: ATLAS runs without the lens, it just degrades to
    # sandbox-only verification. Saying so beats a red check the user cannot
    # act on, and beats silence they cannot diagnose.
    return CheckResult(
        "inference_contract", "warn",
        "llama-server does not serve everything the lens needs: "
        + "; ".join(missing)
        + ". C(x)/G(x) scoring degrades to neutral and the V3 pipeline falls "
          "back to sandbox-only verification. The bundled image provides both; "
          "a bring-your-own server needs --embeddings --pooling mean and the "
          "PC-202 build. See docs/SETUP.md 'Bringing your own llama-server'.")


def check_e2e_smoke() -> CheckResult:
    """Generate through the public proxy passthrough.

    `/v1/chat/completions` is intentionally a raw passthrough, so this checks
    the complete client -> proxy -> inference path without paying the cost of
    the orchestration loop on `/v1/agent`. This distinction matters on the
    macOS hybrid deployment, where the request also crosses the Docker-to-host
    bridge before reaching native Metal inference.
    """
    # Router-mode llama.cpp (`--models-preset`) rejects a request with no
    # model: "model name is missing from the request". Single-model servers
    # ignore the field, so sending it always is strictly more compatible than
    # omitting it (GH #146, reported against a BYO router-mode backend).
    model = (os.environ.get("ATLAS_MODEL_NAME")
             or _ENV.get("ATLAS_MODEL_NAME")
             or _ENV.get("ATLAS_MODEL_FILE", ""))
    body = {
        "messages": [{"role": "user", "content": "Reply with the single word: ATLAS"}],
        # Reasoning-capable templates may emit internal reasoning before the
        # visible answer. Leave enough budget that the smoke test does not
        # mistake a truncated reasoning preamble for failed inference.
        "max_tokens": 300,
        "temperature": 0,
        "stream": False,
        # llama.cpp ignores unsupported template kwargs. Reasoning-aware
        # templates use this to keep a smoke test short and deterministic.
        "chat_template_kwargs": {"enable_thinking": False},
    }
    if model:
        body["model"] = model
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        f"{PROXY_URL}/v1/chat/completions",
        data=data,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            payload = json.loads(resp.read().decode())
    except Exception as e:
        return CheckResult("e2e_smoke", "fail",
            f"proxy completion POST failed: {type(e).__name__}", str(e)[:300])
    choices = payload.get("choices", [])
    if not choices:
        return CheckResult("e2e_smoke", "fail",
            "proxy completion returned no choices",
            json.dumps(payload)[:300])
    msg = choices[0].get("message", {})
    content = (msg.get("content", "") or "").strip()
    finish = choices[0].get("finish_reason", "")
    if not content:
        return CheckResult("e2e_smoke", "fail",
            f"proxy completion returned an empty completion (finish={finish})",
            json.dumps(payload)[:400])
    return CheckResult("e2e_smoke", "pass",
        f"model produced {len(content)} chars (finish={finish})",
        content[:300])


# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

def _icon(status: str, color: bool) -> str:
    # Without color OR without unicode support, fall back to ASCII brackets.
    # This covers --no-color, non-TTY stdout, AND TTYs with ASCII-only encoding.
    if not color or not UNICODE_OK:
        return {"pass": "[OK]  ", "warn": "[WARN]",
                "fail": "[FAIL]", "skip": "[SKIP]"}[status]
    return {"pass": f"{GREEN}✓{RESET}", "warn": f"{YELL}⚠{RESET}",
            "fail": f"{RED}✗{RESET}", "skip": f"{DIM}-{RESET}"}[status]


def _print_result(r: CheckResult, verbose: bool, color: bool) -> None:
    name = f"{BOLD}{r.name}{RESET}" if color else r.name
    pad = " " * max(0, 32 - len(r.name))
    _safe_print(f"  {_icon(r.status, color)} {name}{pad}  {r.message}")
    # Show the remediation/detail by default for actionable problems (fail/warn)
    # — a failure with no visible fix-it hint is a dead end. `pass` details stay
    # behind --verbose to keep a healthy run terse.
    if (verbose or r.status in ("fail", "warn")) and r.detail:
        for line in r.detail.splitlines():
            _safe_print(f"      {DIM if color else ''}{line}{RESET if color else ''}")


def _emit(results: List[CheckResult], args: argparse.Namespace, color: bool,
          already_printed: bool = False) -> int:
    n_pass = sum(1 for r in results if r.status == "pass")
    n_warn = sum(1 for r in results if r.status == "warn")
    n_fail = sum(1 for r in results if r.status == "fail")
    n_skip = sum(1 for r in results if r.status == "skip")

    if args.json:
        out = {
            "summary": {"pass": n_pass, "warn": n_warn,
                        "fail": n_fail, "skip": n_skip},
            "checks": [asdict(r) for r in results],
        }
        # ensure_ascii=False keeps unicode in detail fields readable; if
        # stdout truly can't encode it, write bytes directly with
        # backslash-escape so we don't crash on the way out.
        body = json.dumps(out, indent=2, ensure_ascii=not UNICODE_OK)
        try:
            print(body)
        except UnicodeEncodeError:
            sys.stdout.buffer.write(body.encode("ascii", errors="backslashreplace"))
            sys.stdout.buffer.write(b"\n")
        return 1 if n_fail else 0

    if not already_printed:
        for r in results:
            _print_result(r, args.verbose, color)
    _safe_print()
    parts = [f"{n_pass} passed"]
    if n_warn:
        parts.append(f"{YELL if color else ''}{n_warn} warnings{RESET if color else ''}")
    if n_fail:
        parts.append(f"{RED if color else ''}{n_fail} failed{RESET if color else ''}")
    if n_skip:
        parts.append(f"{n_skip} skipped")
    _safe_print("  " + ", ".join(parts))
    if n_fail == 0 and n_warn == 0:
        _safe_print(f"  {GREEN if color else ''}ATLAS install is healthy.{RESET if color else ''}")
    elif n_fail == 0:
        _safe_print(f"  {YELL if color else ''}ATLAS install is functional with warnings.{RESET if color else ''}")
    else:
        _safe_print(f"  {RED if color else ''}ATLAS install has failures {DASH} re-run with -v for detail.{RESET if color else ''}")
    return 1 if n_fail else 0


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas doctor",
        description="Diagnose ATLAS install health (PC-053)")
    parser.add_argument("--quick", action="store_true",
        help="skip the e2e smoke test (saves ~10s)")
    parser.add_argument("--json", action="store_true",
        help="emit JSON output (for bootstrap, CI, scripts)")
    parser.add_argument("--verbose", "-v", action="store_true",
        help="show detail for each check")
    parser.add_argument("--no-color", action="store_true",
        help="disable ANSI color in human output")
    args = parser.parse_args(argv)

    color = sys.stdout.isatty() and not args.no_color and not args.json
    atlas_root = _find_atlas_root()

    if not args.json:
        hdr = f"{BOLD}ATLAS doctor{RESET}" if color else "ATLAS doctor"
        _safe_print(f"{hdr} {DASH} checking install health (root: {atlas_root})")
        _safe_print()

    results: List[CheckResult] = []

    def _add(r: CheckResult) -> None:
        """Record a result and, in human mode, print it immediately so a
        long run (GPU image pulls, e2e smoke) shows progress as it goes.
        JSON mode keeps buffering for one machine-readable document."""
        results.append(r)
        if not args.json:
            _print_result(r, args.verbose, color)

    # 1. Docker
    docker = check_docker()
    _add(docker)
    if docker.status == "fail":
        # Without docker, every subsequent check is meaningless.
        _add(CheckResult("compose", "skip",
            "skipped (docker unreachable)"))
        return _emit(results, args, color, already_printed=not args.json)

    # 2. Docker compose v2
    _add(check_compose())

    # 2.5. CPU architecture (#115) — surface aarch64 + the backend
    # availability matrix for arm64 hosts before the GPU check, so
    # users see why rocm gets steered to vulkan on DGX Spark / Snapdragon
    # X Elite / Apple Silicon / Jetson / Pi 5.
    _add(check_arch())

    # 3. GPU runtime — vendor-aware (NVIDIA: nvidia-container-toolkit;
    # AMD: /dev/kfd passthrough). Slow on first run since each vendor
    # branch pulls a small base image (~500 MB CUDA, ~2 GB ROCm).
    _add(check_gpu())

    # Resolve which backend the user has configured. Reads shell env
    # first, then .env in atlas_root. Without the .env fallback, the
    # macOS hybrid case (#32) misses the metal-native check because
    # atlas init writes ATLAS_BACKEND into .env and users rarely
    # source it before running doctor.
    backend = _resolve_backend(atlas_root)

    # 3.5. Vulkan ICD passthrough (PC-114) — only fires when the user
    # has explicitly opted into the Vulkan backend. Skipping by default
    # keeps doctor cheap on CUDA/ROCm hosts where the apt-install-
    # vulkan-tools step inside the check container would add ~30s for
    # no signal.
    if backend == "vulkan":
        _add(_check_vulkan_via_docker())

    # 3.6. macOS hybrid path (#32) — verify native llama-server binary
    # exists at the setup-script's install prefix, is executable, and
    # is listening on ATLAS_LLAMA_PORT (so the socat compose forward
    # will succeed).
    # Only fires when backend == metal so it's noise-free on
    # cuda/rocm/vulkan hosts.
    if backend == "metal":
        _add(_check_metal_native(atlas_root))

    # 4. Compose stack — pass atlas_root as cwd so compose finds
    # docker-compose.yml even when doctor is invoked from elsewhere
    # on the filesystem.
    services = _compose_ps(atlas_root)

    # 5. Per-container state
    try:
        start_hint = compose_config.format_command(atlas_root, ["up", "-d"])
    except FileNotFoundError as exc:
        _add(CheckResult("compose/backend", "fail", str(exc)))
        start_hint = "docker compose up -d"
    container_results = check_containers(services, start_hint=start_hint)
    for item in container_results:
        _add(item)

    # 6. Endpoint health (only if at least one container is running)
    if any(r.status == "pass" for r in container_results):
        for item in check_health_endpoints():
            _add(item)
        for item in check_internal_auth(atlas_root):
            _add(item)
        for item in check_status_dimensions():
            _add(item)

    # 7. Model file (host-side)
    _add(check_model_file(atlas_root))

    # 8. Lens weights (host-side)
    _add(check_lens_weights(atlas_root))

    # 8.5. ASA steering vector (BiasBusters #4 — warn-not-fail). Optional
    # but on by default when present; sits next to lens_weights since both
    # are host-side artifact checks.
    _add(check_asa_steering(atlas_root))
    _add(check_grammar_mode(atlas_root))

    # 9.5. Workspace mount alignment — proxy file tools and sandbox
    # run_command must see the same host directory as /workspace, or the
    # agent operates split-brained (reads/writes one dir, runs commands
    # in another) with every /health still green.
    _add(check_workspace_mounts(services))

    # 10. Image-tag skew (PC-052)
    _add(check_image_skew(services))

    # 10.5. Tier match (PC-055) — soft cross-check that .env model
    # matches host hardware. Warn on overshoot (OOM risk), pass on
    # match or undershoot.
    _add(check_tier_match())

    # 10.6. Tier constraints (PC-055.1) — does the host meet the
    # recommended tier's CPU/RAM/disk minimums? Catches "16 GB GPU
    # but 8 GB RAM" cases where llama fits but host OOMs under V3.
    # Pass atlas_root so disk check measures the right partition.
    _add(check_tier_constraints(atlas_root))

    # 11. End-to-end smoke
    if args.quick:
        _add(CheckResult("e2e_smoke", "skip",
            "skipped (--quick)"))
    else:
        _add(check_e2e_smoke())
        # After the smoke test: generation working is the precondition for
        # asking whether the extras are there.
        _add(check_inference_contract())

    return _emit(results, args, color, already_printed=not args.json)


if __name__ == "__main__":
    sys.exit(main())
