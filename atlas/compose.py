"""Backend-aware Docker Compose command construction.

ATLAS has one base Compose file plus backend overlays.  Any CLI command that
mutates or inspects the deployment must select the same files that were used
to start it; otherwise a Metal host can accidentally resolve the CUDA
llama-server service from the base file.
"""

from __future__ import annotations

import os
import shlex
import subprocess
from typing import Dict, Iterable, List, Mapping, Optional


# Backend -> the overlay file(s) layered on top of docker-compose.yml, in
# merge order. Most backends add a single overlay; the GPU-less CPU path
# stacks two (the Vulkan overlay for the lavapipe image, then the CPU
# overlay that strips the /dev/dri passthrough for headless hosts). cuda /
# nvidia are intentionally absent: they keep Compose's default discovery
# (base file alone), so a developer's docker-compose.override.yml still
# applies. These keys match scripts/atlas-bootstrap.sh's compose_files_args.
_OVERLAY_BY_BACKEND = {
    "metal": ["compose/docker-compose.macos.yml"],
    "rocm": ["compose/docker-compose.rocm.yml"],
    "vulkan": ["compose/docker-compose.vulkan.yml"],
    "cpu": ["compose/docker-compose.vulkan.yml", "compose/docker-compose.cpu.yml"],
}

# Service URL resolution: explicit URL env var > port env var / `.env`
# port key > built-in default. The port keys and defaults mirror
# docker-compose.yml's `${ATLAS_*_PORT:-...}` publish stanzas.
_SERVICES = {
    "proxy":   ("ATLAS_PROXY_URL",     "ATLAS_PROXY_PORT",   "8090"),
    "llama":   ("ATLAS_INFERENCE_URL", "ATLAS_LLAMA_PORT",   "8080"),
    "lens":    ("ATLAS_LENS_URL",      "ATLAS_LENS_PORT",    "8099"),
    "sandbox": ("ATLAS_SANDBOX_URL",   "ATLAS_SANDBOX_PORT", "30820"),
    "v3":      ("ATLAS_V3_URL",        "ATLAS_V3_PORT",      "8070"),
}


def service_port(
    service: str,
    atlas_root: Optional[str] = None,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> str:
    """Resolve a service's published host port: shell env > `.env` > default."""
    env = os.environ if environ is None else environ
    _, port_key, default_port = _SERVICES[service]
    port = env.get(port_key)
    if not port:
        if values is None:
            # Function-local: atlas.env imports this module, so the repo-root
            # resolver can only be reached lazily from here.
            from atlas.env import atlas_root as _resolve_root
            values = read_env_file(atlas_root or _resolve_root())
        port = values.get(port_key)
    return port or default_port


def service_url(
    service: str,
    atlas_root: Optional[str] = None,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> str:
    """Resolve a service's base URL: explicit URL env var wins, then the
    port keys via :func:`service_port`, then the built-in default."""
    env = os.environ if environ is None else environ
    url_key, _, _ = _SERVICES[service]
    explicit = env.get(url_key)
    if explicit:
        return explicit.rstrip("/")
    port = service_port(service, atlas_root, values, environ)
    return "http://localhost:{}".format(port)


def container_id(
    atlas_root: str,
    service: str,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
    fallback: Optional[str] = None,
) -> Optional[str]:
    """Resolve a compose service's running container via `docker compose
    ps -q <service>` so non-default project names (COMPOSE_PROJECT_NAME,
    a renamed checkout dir) still work. Returns `fallback` when compose
    resolution fails or the service isn't up."""
    try:
        result = subprocess.run(
            command(atlas_root, ["ps", "-q", service], values=values,
                    environ=environ),
            cwd=atlas_root, capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired, FileNotFoundError):
        return fallback
    if result.returncode != 0:
        return fallback
    ids = [line.strip() for line in result.stdout.splitlines() if line.strip()]
    return ids[0] if ids else fallback


def read_env_file(atlas_root: str) -> Dict[str, str]:
    """Read the checkout's Compose ``.env`` without executing shell code."""
    return read_env_path(os.path.join(atlas_root, ".env"))


def read_env_path(path: str) -> Dict[str, str]:
    """Parse one ``.env``-shaped file. The single .env parser for the CLI:
    ``KEY=VALUE`` lines, an optional ``export`` prefix, ``#`` comments
    (whole-line and whitespace-preceded inline), and surrounding quotes
    stripped. Never executes the file."""
    values: Dict[str, str] = {}
    try:
        with open(path, encoding="utf-8-sig") as fh:
            for raw in fh:
                line = raw.strip()
                if line.startswith("export "):
                    line = line[len("export "):].lstrip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                key, value = line.split("=", 1)
                stripped = value.lstrip()
                if stripped.startswith("#") and stripped != value:
                    # Empty value followed by an inline comment
                    # ("KEY= # note") parses as empty under ATLAS's .env
                    # parsing (this is not a byte-for-byte reimplementation
                    # of docker compose's parser).
                    value = ""
                else:
                    value = stripped
                    head, marker, _ = value.partition("#")
                    if marker and head and head[-1] in " \t":
                        value = head
                values[key.strip()] = value.strip().strip("'\"")
    except OSError:
        # An unreadable optional .env is treated as absent; callers still
        # honor explicit process-environment values and built-in defaults.
        pass
    return values


def resolve_backend(
    atlas_root: str,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> Optional[str]:
    """Resolve backend with shell environment taking precedence over `.env`."""
    env = os.environ if environ is None else environ
    backend = env.get("ATLAS_BACKEND")
    if not backend and values is not None:
        backend = values.get("ATLAS_BACKEND")
    if not backend:
        backend = read_env_file(atlas_root).get("ATLAS_BACKEND")
    return backend.strip().lower() if backend else None


def file_args(
    atlas_root: str,
    backend: Optional[str] = None,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> List[str]:
    """Return Compose ``-f`` arguments for the resolved backend.

    CUDA keeps Compose's default discovery so a developer's conventional
    ``docker-compose.override.yml`` still applies. Backends with explicit
    ATLAS overlays name the base file plus each overlay in merge order (the
    ``cpu`` backend stacks the Vulkan + CPU overlays).
    """
    selected = backend or resolve_backend(atlas_root, values, environ)
    overlays = _OVERLAY_BY_BACKEND.get(selected or "")
    if not overlays:
        return []
    args = ["-f", "docker-compose.yml"]
    for overlay in overlays:
        if not os.path.isfile(os.path.join(atlas_root, overlay)):
            raise FileNotFoundError(
                "ATLAS backend {!r} requires missing {}".format(
                    selected, overlay)
            )
        args += ["-f", overlay]
    return args


def command(
    atlas_root: str,
    args: Iterable[str],
    backend: Optional[str] = None,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> List[str]:
    """Build a complete ``docker compose`` argv list."""
    return [
        "docker", "compose",
        *file_args(atlas_root, backend, values, environ),
        *list(args),
    ]


def format_command(
    atlas_root: str,
    args: Iterable[str],
    backend: Optional[str] = None,
    values: Optional[Mapping[str, str]] = None,
    environ: Optional[Mapping[str, str]] = None,
) -> str:
    """Return a shell-display form of :func:`command`."""
    return shlex.join(command(atlas_root, args, backend, values, environ))
