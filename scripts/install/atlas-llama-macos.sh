#!/usr/bin/env bash
# ATLAS macOS native llama-server launcher (#32 hybrid path).
#
# Starts the Metal-accelerated llama-server built by
# scripts/install/atlas-setup-macos.sh, using the same flags as the docker
# entrypoint (inference/entrypoint-v3.1.sh) so behavior is
# identical to the linux + cuda/rocm path. Reads config from .env in
# the ATLAS root.
#
# Run this in its own terminal (it stays in the foreground). Stop with
# Ctrl-C; on stop, the docker stack's proxy will start serving 502s
# until you re-launch.
#
# Usage:
#   ./scripts/install/atlas-llama-macos.sh
#   ./scripts/install/atlas-llama-macos.sh --port 8081       # override port
#   ./scripts/install/atlas-llama-macos.sh --prefix DIR      # custom native install
#   ./scripts/install/atlas-llama-macos.sh --rebuild         # re-run setup first

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ATLAS_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DEFAULT_PREFIX="$HOME/.atlas/macos"

# Flag parsing — just the user-facing ones, everything else comes from .env
OVERRIDE_PORT=""
PREFIX_OVERRIDE=""
REBUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --port)
      [[ $# -ge 2 ]] || { echo "--port requires a value" >&2; exit 2; }
      OVERRIDE_PORT="$2"; shift 2;;
    --prefix)
      [[ $# -ge 2 ]] || { echo "--prefix requires a value" >&2; exit 2; }
      PREFIX_OVERRIDE="$2"; shift 2;;
    --rebuild) REBUILD=1; shift;;
    -h|--help)
      sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

# ---------------------------------------------------------------------------
# Sanity checks — fail fast with clear messages rather than letting
# llama-server crash with a confusing error.
# ---------------------------------------------------------------------------

# Load .env as defaults if present, without executing it as shell code.
# Existing process environment wins, matching Compose and the Python CLI
# configuration policy, so a fully env-configured launch (no .env file) is
# valid. The ATLAS_MODEL_FILE :? guard below still fails with a clear message
# if required config is missing from both the environment and .env.
if [[ -f "$ATLAS_ROOT/.env" ]]; then
while IFS= read -r line || [[ -n "$line" ]]; do
  line="${line#"${line%%[![:space:]]*}"}"
  [[ -z "$line" || "$line" == \#* ]] && continue
  [[ "$line" == export\ * ]] && line="${line#export }"
  [[ "$line" == *=* ]] || continue
  key="${line%%=*}"
  value="${line#*=}"
  key="${key%"${key##*[![:space:]]}"}"
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
  if printenv "$key" >/dev/null 2>&1; then
    continue
  fi
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  if [[ ( "$value" == \"*\" && "$value" == *\" ) ||
        ( "$value" == \'*\' && "$value" == *\' ) ]]; then
    value="${value:1:${#value}-2}"
  fi
  export "$key=$value"
done < "$ATLAS_ROOT/.env"
fi

PREFIX="${PREFIX_OVERRIDE:-${ATLAS_MACOS_PREFIX:-$DEFAULT_PREFIX}}"
TILDE_PREFIX="$(printf '\176/')"
if [[ "$PREFIX" == "$TILDE_PREFIX"* ]]; then
  PREFIX="$HOME/${PREFIX:2}"
fi
LLAMA_SERVER="$PREFIX/bin/llama-server-metal"

if [[ $REBUILD -eq 1 ]]; then
  bash "$SCRIPT_DIR/atlas-setup-macos.sh" --rebuild --prefix "$PREFIX"
fi

if [[ ! -x "$LLAMA_SERVER" ]]; then
  echo "native llama-server not found at $LLAMA_SERVER" >&2
  echo "  Run ./scripts/install/atlas-setup-macos.sh --prefix '$PREFIX' first." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Resolve the runtime knobs. These fallback defaults are deliberately
# SMALLER than the Linux/Docker path (ctx 32768 vs 131072, quantized
# q8_0/q4_0 KV cache vs f16/f16, 1 parallel slot vs 4) to stay within the
# unified-memory budget of a typical Mac — the f16/131072/4 Docker defaults
# roughly 10x the KV-cache footprint and can fail to allocate on smaller
# machines. A real install never hits these fallbacks: `atlas init` / `atlas
# tier fit --write` write the model- and hardware-sized values into .env,
# which the loader above imports and which take precedence here.
# ---------------------------------------------------------------------------

CTX_LENGTH="${ATLAS_CTX_SIZE:-${CONTEXT_LENGTH:-32768}}"
KV_CACHE_K="${ATLAS_KV_TYPE_K:-${KV_CACHE_TYPE_K:-q8_0}}"
KV_CACHE_V="${ATLAS_KV_TYPE_V:-${KV_CACHE_TYPE_V:-q4_0}}"
PARALLEL="${ATLAS_PARALLEL_SLOTS:-${PARALLEL_SLOTS:-1}}"
BATCH_SIZE="${ATLAS_BATCH:-${BATCH_SIZE:-1024}}"
UBATCH_SIZE="${ATLAS_UBATCH:-${UBATCH_SIZE:-1024}}"
PORT="${OVERRIDE_PORT:-${ATLAS_LLAMA_PORT:-8080}}"
HOST="${ATLAS_LLAMA_HOST:-127.0.0.1}"

case "$BATCH_SIZE:$UBATCH_SIZE" in
  *[!0-9:]*|:*|*:)
    echo "ATLAS_BATCH and ATLAS_UBATCH must be positive integers" >&2
    exit 1
    ;;
  *) ;;
esac
if [[ "$BATCH_SIZE" -eq 0 || "$UBATCH_SIZE" -eq 0 ]]; then
  echo "ATLAS_BATCH and ATLAS_UBATCH must be positive integers" >&2
  exit 1
fi
if [[ "$BATCH_SIZE" -gt "$UBATCH_SIZE" ]]; then
  BATCH_SIZE="$UBATCH_SIZE"
fi

# Resolve model path. ATLAS_MODELS_DIR is "./models" (relative to atlas root)
# or an absolute path. ATLAS_MODEL_FILE is the .gguf filename.
MODELS_DIR="${ATLAS_MODELS_DIR:-./models}"
if [[ "$MODELS_DIR" != /* ]]; then
  MODELS_DIR="$ATLAS_ROOT/$MODELS_DIR"
fi
MODEL_FILE="$MODELS_DIR/${ATLAS_MODEL_FILE:?ATLAS_MODEL_FILE not set in .env}"

if [[ ! -f "$MODEL_FILE" ]]; then
  echo "model file not found: $MODEL_FILE" >&2
  echo "  Run 'atlas model install ${ATLAS_MODEL_NAME:-<name>}' to download." >&2
  exit 1
fi

# ASA steering vector (#4 BiasBusters). Match the Docker entrypoint's path and
# model-marker gate so a stale vector cannot steer another model.
CVECTOR_FLAGS=()
CVECTOR_PATH="${ATLAS_CONTROL_VECTOR:-$MODELS_DIR/ast_edit_steering.gguf}"
if [[ "$CVECTOR_PATH" == /models/* ]]; then
  CVECTOR_PATH="$MODELS_DIR/${CVECTOR_PATH#/models/}"
fi
MODEL_BASENAME="$(basename "$MODEL_FILE")"
MODEL_STEM="${MODEL_BASENAME%.gguf}"
MODEL_ID="${ATLAS_MODEL_NAME:-$MODEL_STEM}"
CVECTOR_STATUS="disabled"
if [[ -f "$CVECTOR_PATH" ]]; then
  CVECTOR_MARKER="$CVECTOR_PATH.model"
  CVECTOR_MODEL=""
  if [[ -f "$CVECTOR_MARKER" ]]; then
    CVECTOR_MODEL="$(tr -d '\r\n' < "$CVECTOR_MARKER")"
  fi
  CVECTOR_MODEL_BASE="$(basename "$CVECTOR_MODEL")"
  CVECTOR_MODEL_STEM="${CVECTOR_MODEL_BASE%.gguf}"
  # Case-insensitive marker match, mirroring entrypoint-v3.1.sh and
  # `atlas asa check`'s casefolded canonicalization.
  shopt -s nocasematch
  if [[ "$CVECTOR_MODEL" == "$MODEL_ID" ||
        "$CVECTOR_MODEL_BASE" == "$MODEL_BASENAME" ||
        "$CVECTOR_MODEL_STEM" == "$MODEL_STEM" ||
        "${ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED:-0}" == "1" ]]; then
    shopt -u nocasematch
    CVECTOR_SCALE="${ATLAS_CONTROL_VECTOR_SCALE:-0.5}"
    CVECTOR_FLAGS=(--control-vector-scaled "$CVECTOR_PATH:$CVECTOR_SCALE")
    if [[ -n "${ATLAS_CONTROL_VECTOR_LAYER_RANGE:-}" ]]; then
      read -r LAYER_START LAYER_END LAYER_EXTRA \
        <<< "$ATLAS_CONTROL_VECTOR_LAYER_RANGE"
      if [[ "$LAYER_START" =~ ^[0-9]+$ && "$LAYER_END" =~ ^[0-9]+$ &&
            -z "${LAYER_EXTRA:-}" ]]; then
        CVECTOR_FLAGS+=(--control-vector-layer-range "$LAYER_START" "$LAYER_END")
      else
        echo "invalid ATLAS_CONTROL_VECTOR_LAYER_RANGE; expected 'START END'" >&2
        exit 1
      fi
    fi
    CVECTOR_STATUS="$CVECTOR_PATH (model=$CVECTOR_MODEL, scale=$CVECTOR_SCALE)"
  else
    shopt -u nocasematch
    CVECTOR_STATUS="disabled: marked for ${CVECTOR_MODEL:-unknown}, selected $MODEL_ID"
  fi
fi

# ---------------------------------------------------------------------------
# Banner — same shape as the docker entrypoint for diff-friendly logs
# ---------------------------------------------------------------------------

cat <<EOF
ATLAS llama-server (native macOS Metal) — #32 hybrid path
  Model:                $MODEL_FILE
  Context length:       $CTX_LENGTH
  Parallel slots:       $PARALLEL
  KV cache K / V:       $KV_CACHE_K / $KV_CACHE_V
  Port:                 $PORT
  Host:                 $HOST
  Batch / micro-batch:  $BATCH_SIZE / $UBATCH_SIZE
  ASA steering:         $CVECTOR_STATUS
  Binary:               $LLAMA_SERVER

EOF

# ---------------------------------------------------------------------------
# Launch. Same flags as the docker entrypoint with two differences:
#   --host 127.0.0.1 bind only on loopback by default. Docker Desktop's
#                    host.docker.internal gateway can still reach host
#                    loopback services. Set ATLAS_LLAMA_HOST explicitly
#                    only when a different interface is required.
#   no --mlock       optional on Mac (unified memory makes it less
#                    impactful; can be added back if perf testing
#                    shows it helps)
# Slot save path: tmp dir so we don't pollute the repo.
# ---------------------------------------------------------------------------

SLOT_SAVE_PATH="${TMPDIR:-/tmp}/atlas-slots"
mkdir -p "$SLOT_SAVE_PATH"

SERVER_ARGS=(
  -m "$MODEL_FILE"
  -c "$CTX_LENGTH"
  -ctk "$KV_CACHE_K" -ctv "$KV_CACHE_V"
  --parallel "$PARALLEL"
  --cont-batching
  -ngl 99
  --host "$HOST"
  --port "$PORT"
  --flash-attn on
  --fit off
  -b "$BATCH_SIZE"
  -ub "$UBATCH_SIZE"
  --slot-save-path "$SLOT_SAVE_PATH"
  --ctx-checkpoints 0
  --embeddings
  --jinja
)
# Bash 3.2 (the version shipped by macOS) errors under `set -u` when an empty
# array is expanded directly, so append the optional ASA flags conditionally.
if (( ${#CVECTOR_FLAGS[@]} )); then
  SERVER_ARGS+=("${CVECTOR_FLAGS[@]}")
fi

# Internal service auth: native path reads the checkout's token file
# directly (no container mount). llama-server exempts /health.
TOKEN_FILE="${ATLAS_SERVICE_TOKEN_FILE:-$ATLAS_ROOT/secrets/service-token}"
if [[ -s "$TOKEN_FILE" ]]; then
  SERVER_ARGS+=(--api-key-file "$TOKEN_FILE")
  echo "Internal auth: enabled (api-key-file)"
fi

exec "$LLAMA_SERVER" "${SERVER_ARGS[@]}"
