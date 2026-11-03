"""Shared CLI environment resolution.

Parses the compose .env (the Docker deployment's source of truth) and
resolves the service URLs and model settings the CLI commands share.
Shell environment wins over .env values. Lives outside the command
modules so doctor, fit, lens, and publish read one source without
importing each other.
"""

import os

from atlas import compose as compose_config


def atlas_root() -> str:
    """The repo root (the directory holding docker-compose.yml). Resolved from
    this file first so commands work from any cwd, then by walking up from the
    cwd; falls back to the cwd.

    The one resolver — command modules that need a patchable module
    attribute import it under a local alias (`_atlas_root`,
    `_find_atlas_root`). Two callers deliberately do NOT use it because
    they ask a different question: `init._find_atlas_root` walks the cwd
    only and returns None outside a checkout (the wizard must refuse to
    write .env/secrets into an arbitrary directory, which this function's
    cwd fallback would defeat), and `fit._cwd_deployment_root` exists to
    contrast the cwd's deployment with this one so `--write` can warn.
    """
    starts = (os.path.dirname(os.path.abspath(__file__)),
              os.path.abspath(os.getcwd()))
    for start in starts:
        cur = start
        while True:
            if os.path.isfile(os.path.join(cur, "docker-compose.yml")):
                return cur
            parent = os.path.dirname(cur)
            if parent == cur:
                break
            cur = parent
    return os.path.abspath(os.getcwd())


# The compose .env at the resolved repo root. It is the Docker deployment's
# source of truth, so the model/dir checks below reflect what is actually
# configured even when the shell doesn't export ATLAS_MODEL_FILE. Parsing
# lives in compose.read_env_file — one .env parser for the whole CLI.
_ENV = compose_config.read_env_file(atlas_root())


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name) or _ENV.get(name)
    if raw in (None, ""):
        return default
    try:
        return int(raw)
    except ValueError:
        return default


# Defaults — shell env first, then the compose .env's port keys. Model
# selection has no vendor fallback: the installer must choose a concrete
# model explicitly.
PROXY_URL    = compose_config.service_url("proxy",   values=_ENV)
LLAMA_URL    = compose_config.service_url("llama",   values=_ENV)
LENS_URL     = compose_config.service_url("lens",    values=_ENV)
SANDBOX_URL  = compose_config.service_url("sandbox", values=_ENV)
V3_URL       = compose_config.service_url("v3",      values=_ENV)
MODEL_DIR    = os.environ.get("ATLAS_MODELS_DIR")  or _ENV.get("ATLAS_MODELS_DIR", "./models")
MODEL_FILE   = os.environ.get("ATLAS_MODEL_FILE")  or _ENV.get("ATLAS_MODEL_FILE", "")
MODEL_NAME   = os.environ.get("ATLAS_MODEL_NAME")  or _ENV.get("ATLAS_MODEL_NAME", "local-model")
LLAMA_PORT   = _env_int("ATLAS_LLAMA_PORT", 8080)
# Match docker-compose.yml's `${ATLAS_LENS_MODELS:-./services/geometric-lens/geometric_lens/models}`
# host-side bind-mount source so checks see the same directory the
# container will actually receive.
LENS_MODELS_DIR = (os.environ.get("ATLAS_LENS_MODELS")
                   or _ENV.get("ATLAS_LENS_MODELS")
                   or "./services/geometric-lens/geometric_lens/models")
