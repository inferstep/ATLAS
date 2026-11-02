#!/bin/bash
set -euo pipefail

# ATLAS Model Downloader — the shell entry point the bootstrap installer
# calls before the `atlas` console script is guaranteed to be on PATH.
#
# Model URLs, SHA-256 pins, resume, the HF_TOKEN path, and the Lens/ASA
# artifact bundles all live in `atlas model install` (registry:
# atlas/commands/model_registry.py). This script resolves configuration,
# delegates, and maintains the `default.gguf` symlink — nothing else. It
# used to carry its own four-entry URL table, which had drifted to the
# point that Qwen3.5-9B-Q6_K was the only registry model still fetchable
# through it.
#
# Config resolution (first hit wins):
#   1. Existing env vars (set by caller — e.g. atlas-bootstrap.sh)
#   2. .env in repo root (Docker Compose convention)
#   3. .env.example (the shipped defaults)
#
# Usage:
#   ./scripts/install/download-models.sh          # model + compatible artifacts
#   ./scripts/install/download-models.sh --lens   # artifacts only
#
# We deliberately do NOT source scripts/lib/config.sh here. That library
# is K3s-oriented (requires atlas.conf, validates NodePorts) and explodes
# on a Docker Compose install when the repo lives at /opt/atlas owned by
# root. PC-051.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1" >&2; }

# Run the CLI out of this checkout even when it isn't pip-installed yet.
atlas_cli() {
    PYTHONPATH="$REPO_ROOT${PYTHONPATH:+:$PYTHONPATH}" \
        python3 -m atlas "$@"
}

# Pull the keys we need through the CLI's own .env parser, so this script
# and `atlas model install` can never disagree about what is configured.
load_config() {
    local assignments
    if ! assignments="$(PYTHONPATH="$REPO_ROOT${PYTHONPATH:+:$PYTHONPATH}" \
            python3 - <<'PY'
import os
import shlex

from atlas import compose, env

root = env.atlas_root()
values = compose.read_env_file(root)
if not values:
    values = compose.read_env_path(os.path.join(root, ".env.example"))
for key in ("ATLAS_MODEL_NAME", "ATLAS_MODEL_FILE", "ATLAS_MODEL_URL"):
    print(f"{key}={shlex.quote(os.environ.get(key) or values.get(key, ''))}")
PY
    )"; then
        log_error "Could not read the ATLAS configuration (is python3 present?)"
        exit 1
    fi
    eval "$assignments"
}

models_dir() {
    # The installer's own resolution (ATLAS_MODELS_DIR env > .env >
    # <repo>/models), asked for rather than reimplemented.
    atlas_cli model list --json --no-color \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["models_dir"])'
}

registry_model_file() {
    # On-disk filename for a registry name, or empty if unknown.
    local registry_name=$1
    atlas_cli model list --json --no-color | python3 -c '
import json, sys
name = sys.argv[1]
for m in json.load(sys.stdin)["models"]:
    if m["name"] == name:
        print(m["model_file"])
        break
' "$registry_name"
}

download_lens_weights() {
    # Artifacts are coupled to the selected model. Delegate to the registry
    # installer rather than downloading one architecture's bundle globally.
    local selected="${ATLAS_MODEL_NAME:-${ATLAS_MODEL_FILE%.gguf}}"
    if [[ -z "$selected" ]]; then
        log_error "No model selected. Set ATLAS_MODEL_NAME or run atlas init."
        return 1
    fi
    log_info "Installing compatible Lens/ASA artifacts for $selected"
    atlas_cli model install-artifacts "$selected" --no-color
}

main() {
    load_config

    echo "=========================================="
    echo "  ATLAS Model Downloader"
    echo "=========================================="
    echo ""

    # Subcommand: --lens fetches lens weights only and exits.
    if [[ "${1:-}" == "--lens" ]]; then
        download_lens_weights
        exit 0
    fi

    if [[ -z "${ATLAS_MODEL_FILE:-}" && -z "${ATLAS_MODEL_NAME:-}" ]]; then
        log_error "No model selected."
        log_error "Set ATLAS_MODEL_NAME (registry name) or ATLAS_MODEL_FILE in"
        log_error ".env, or run 'atlas init' to pick one for this hardware."
        exit 1
    fi

    local models_path model_file
    models_path="$(models_dir)"
    model_file="${ATLAS_MODEL_FILE:-}"
    echo "Models directory: $models_path"
    echo "Model:            ${ATLAS_MODEL_NAME:-$model_file}"
    echo ""

    if [[ -n "$model_file" && -f "$models_path/$model_file" ]]; then
        # Idempotent re-runs: bootstrap calls this on every install.
        log_info "$model_file already present, skipping download"
    elif [[ -n "${ATLAS_MODEL_URL:-}" ]]; then
        # Explicit URL wins — an unregistered / BYO model.
        log_info "Installing from ATLAS_MODEL_URL (unregistered model)"
        local url_args=(--url "$ATLAS_MODEL_URL")
        [[ -n "$model_file" ]] && url_args+=(--file "$model_file")
        atlas_cli model install "${url_args[@]}" --yes --no-color || return 1
    else
        local selected="${ATLAS_MODEL_NAME:-${model_file%.gguf}}"
        log_info "Installing registry model $selected"
        # --no-lens: the artifact bundle is fetched by the --lens pass the
        # installer runs next, so a model without one must not block the
        # gguf download here.
        if ! atlas_cli model install "$selected" --no-lens --yes --no-color; then
            log_error "Could not install '$selected'."
            log_error ""
            log_error "Options:"
            log_error "  1. Pick a registered model:  atlas model list"
            log_error "  2. Set ATLAS_MODEL_URL=<url> for your own GGUF and re-run"
            log_error "  3. Place the file manually in $models_path"
            return 1
        fi
    fi

    # Resolve the on-disk filename when only a registry name was configured.
    if [[ -z "$model_file" ]]; then
        model_file="$(registry_model_file "$ATLAS_MODEL_NAME")"
    fi

    if [[ -z "$model_file" || ! -f "$models_path/$model_file" ]]; then
        log_error "Model file not present after install: $models_path/${model_file:-?}"
        exit 1
    fi
    log_info "Model verified: $model_file"

    # Default-model symlink. Relative target (both live in models_path) so the
    # link survives the directory being reached via a different path — e.g. a
    # container mount at /models, or a relative ATLAS_MODELS_DIR resolved from
    # another CWD.
    ln -sf "$model_file" "$models_path/default.gguf"

    echo ""
    echo "=========================================="
    echo "  Model Download Complete!"
    echo "=========================================="
    echo ""
    echo "Models available:"
    ls -lh "$models_path"/*.gguf 2>/dev/null || echo "  No .gguf files found"
    echo ""
}

main "$@"
