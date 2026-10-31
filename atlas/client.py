"""HTTP client for llama-server and the sandbox executor. Pure urllib, no dependencies."""

import json
import os
import urllib.request
import urllib.error
from dataclasses import dataclass
from typing import Optional

from atlas import compose as compose_config

# Shell env wins; otherwise the Docker .env's port keys drive the URLs.
INFERENCE_URL = compose_config.service_url("llama")
# geometric-lens. ATLAS_LENS_URL is the current name and wins; ATLAS_RAG_URL
# is the pre-rename spelling, kept so an existing .env keeps working
# (CONFIGURATION.md documents the alias and pins this module as its reader).
# It used to be read first, which gave the deprecated name precedence over
# its own replacement. service_url("lens") supplies the port-derived default
# (and reads ATLAS_LENS_URL itself, so the first term is belt-and-braces).
LENS_URL = (os.environ.get("ATLAS_LENS_URL")
            or os.environ.get("ATLAS_RAG_URL")
            or compose_config.service_url("lens"))


# --- Health checks ---


# --- llama-server model probe ---
#
# These helpers return None on any failure: the probe degrades field-by-field
# instead of aborting on the first unreachable endpoint.

def llama_url() -> str:
    """Resolve where llama-server is listening.

    Mirrors geometric-lens/embedding_extractor.py's resolution order so
    `atlas lens check` agrees with what the lens service itself sees,
    then falls back to the Docker .env's port keys via compose config.
    """
    for key in ("ATLAS_LLAMA_URL", "LLAMA_EMBED_URL", "LLAMA_URL"):
        value = os.environ.get(key)
        if value:
            return value
    return compose_config.service_url("llama")


@dataclass
class LlamaProbe:
    """Snapshot of what the running llama-server can tell us about the model."""
    reachable: bool
    url: str
    embedding_dim: int = 0          # 0 when /embedding failed or didn't return
    n_layers: int = 0               # 0 when /props didn't carry n_layer
    model_name: str = ""            # whatever /props reports (often a path)
    has_hidden_states_patch: bool = False  # PC-202: layers extension present
    error: str = ""                 # short human description when reachable=False


def get_json_or_none(url: str, timeout: float = 5.0) -> Optional[dict]:
    """GET a JSON endpoint. Returns parsed dict or None on any failure."""
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return json.loads(resp.read())
    except (urllib.error.URLError, urllib.error.HTTPError,
            json.JSONDecodeError, OSError, ValueError):
        return None


def post_json_or_none(url: str, body: dict, timeout: float = 30.0) -> Optional[dict]:
    """POST JSON, parse response. Returns parsed obj or None on failure."""
    payload = json.dumps(body).encode()
    req = urllib.request.Request(
        url, data=payload, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())
    except (urllib.error.URLError, urllib.error.HTTPError,
            json.JSONDecodeError, OSError, ValueError):
        return None


def probe_llama(url: Optional[str] = None,
                sample_text: str = "def hello():\n    return 42") -> LlamaProbe:
    """Discover what the running llama-server knows about its loaded model.

    Three probes:
      1. /health  -> is the server reachable at all?
      2. /props   -> model metadata (n_layer, model_name)
      3. /embedding (POST) -> the authoritative embedding dim, plus a
         PC-202 hidden-states ping (layers=[0]) to detect the patch.

    Failures degrade gracefully: a probe step that times out or returns
    a non-JSON body sets that field to its zero value rather than
    raising. Caller inspects reachable + embedding_dim to decide verdict.
    """
    url = url or llama_url()
    probe = LlamaProbe(reachable=False, url=url)

    # 1. /health — fast existence check
    health = get_json_or_none(f"{url}/health", timeout=3.0)
    if health is None:
        probe.error = (f"llama-server not reachable at {url}. "
                       f"Bring the stack up with `docker compose up -d` "
                       f"(or `docker compose -f docker-compose.yml "
                       f"-f compose/docker-compose.rocm.yml up -d` on AMD), "
                       f"then re-run.")
        return probe
    probe.reachable = True

    # 2. /props — n_layer + model name. Field names changed across
    # llama-server versions; tolerate both `n_layer` and `default_generation_settings`.
    props = get_json_or_none(f"{url}/props", timeout=5.0) or {}
    probe.n_layers = (
        int(props.get("n_layer", 0))
        or int((props.get("default_generation_settings") or {}).get("n_layer", 0))
    )
    probe.model_name = (props.get("model_path")
                        or props.get("model_name")
                        or props.get("model")
                        or "")

    # 3. /embedding — authoritative dim. Send a small sample; pooled or
    # per-token both yield the dim. The PC-202 patch is signalled by the
    # presence of `hidden_states_dim` in the response when `layers` is
    # requested; on an unpatched server the field is silently absent.
    emb = post_json_or_none(f"{url}/embedding",
                            {"content": sample_text, "layers": [0]},
                            timeout=30.0)
    if isinstance(emb, list) and emb:
        first = emb[0]
        if isinstance(first, dict):
            raw = first.get("embedding")
            if isinstance(raw, list) and raw:
                if isinstance(raw[0], list):
                    probe.embedding_dim = len(raw[0])  # per-token
                else:
                    probe.embedding_dim = len(raw)     # pooled
            if "hidden_states_dim" in first:
                probe.has_hidden_states_patch = True
    if probe.embedding_dim == 0:
        probe.error = (f"llama-server at {url} is up but /embedding "
                       f"didn't return an embedding. Likely cause: model "
                       f"was started without `--embeddings`. Check "
                       f"inference/entrypoint-v3.1.sh.")
    return probe


# --- Generation ---


# --- Sandbox ---

