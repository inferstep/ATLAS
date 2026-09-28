#!/bin/bash
# ATLAS llama-server entrypoint — generation + self-embeddings.
#
# Model-agnostic: the model comes from MODEL_PATH and every memory-bound
# runtime knob (context, KV cache types, batch sizes, parallel slots) is
# env-driven, sized per model + GPU by `atlas tier fit --write` (PC-208).
# Self-embeddings are always on — the Geometric Lens C(x)/G(x) scores
# against the loaded model's own hidden-state dimension.
#
# Runs with --fit off: the model, KV cache, and compute buffers must fit
# entirely in VRAM, or llama-server refuses to start. This is deliberate —
# the silent alternative (llama.cpp demoting layers to CPU) generates at a
# fraction of GPU speed while burning host cores.

SLOT_SAVE_PATH="${SLOT_SAVE_PATH:-/tmp/slots}"
mkdir -p "$SLOT_SAVE_PATH"

CTX_LENGTH="${CONTEXT_LENGTH:-163840}"
KV_CACHE_K="${KV_CACHE_TYPE_K:-f16}"
KV_CACHE_V="${KV_CACHE_TYPE_V:-f16}"
KV_FLAGS=(-ctk "$KV_CACHE_K" -ctv "$KV_CACHE_V")
PARALLEL="${PARALLEL_SLOTS:-4}"
# Batch sizes (PC-208): ubatch drives the compute-buffer size
# (~ubatch × n_embd × 280 bytes), which is what OOMs first on tight
# VRAM. `atlas tier fit --write` sizes these per model + GPU.
UBATCH_SIZE="${UBATCH_SIZE:-1024}"
BATCH_SIZE="${BATCH_SIZE:-1024}"
MODEL_FILE="${MODEL_PATH:?MODEL_PATH must point to the selected GGUF}"
PORT="${PORT:-8080}"

# Self-embeddings require n_batch <= n_ubatch in llama.cpp. A larger logical
# batch is silently clamped upstream, so normalize it here and report the
# value ATLAS actually runs.
case "$BATCH_SIZE:$UBATCH_SIZE" in
  *[!0-9:]*|:*|*:)
    echo "ERROR: BATCH_SIZE and UBATCH_SIZE must be positive integers" >&2
    exit 1
    ;;
esac
if [ "$BATCH_SIZE" -eq 0 ] || [ "$UBATCH_SIZE" -eq 0 ]; then
  echo "ERROR: BATCH_SIZE and UBATCH_SIZE must be positive integers" >&2
  exit 1
fi
if [ "$BATCH_SIZE" -gt "$UBATCH_SIZE" ]; then
  BATCH_SIZE="$UBATCH_SIZE"
fi

# Backend-specific runtime tuning (V3.1.1 multi-backend support).
# ATLAS_BACKEND is written into .env by `atlas init`; unset defaults to
# cuda so existing deployments don't break.
ATLAS_BACKEND="${ATLAS_BACKEND:-cuda}"

case "$ATLAS_BACKEND" in
  cuda)
    # NVIDIA CUDA runtime knobs (unchanged from V3.1.0).
    #   GGML_CUDA_NO_PINNED=0     — keep pinned host memory for fast H2D
    #   CUDA_DEVICE_MAX_CONNECTIONS=1 — single-stream batching is fine,
    #                                   higher values seen no benefit
    #   CUDA_MODULE_LOADING=LAZY  — defer kernel loading until first use
    export GGML_CUDA_NO_PINNED="${GGML_CUDA_NO_PINNED:-0}"
    export CUDA_DEVICE_MAX_CONNECTIONS="${CUDA_DEVICE_MAX_CONNECTIONS:-1}"
    export CUDA_MODULE_LOADING="${CUDA_MODULE_LOADING:-LAZY}"
    if [ -n "$ATLAS_GPU_INDEX" ] && [ -z "$CUDA_VISIBLE_DEVICES" ]; then
      export CUDA_VISIBLE_DEVICES="$ATLAS_GPU_INDEX"
    fi
    ;;
  rocm)
    # AMD ROCm/HIP runtime knobs. llama.cpp's HIP backend shares the
    # GGML_CUDA_* names internally (it mirrors the CUDA backend at the
    # GGML layer) so GGML_CUDA_NO_PINNED still applies. The vendor-side
    # CUDA_DEVICE_MAX_CONNECTIONS / CUDA_MODULE_LOADING vars are inert
    # under HIP and don't need to be set.
    export GGML_CUDA_NO_PINNED="${GGML_CUDA_NO_PINNED:-0}"
    if [ -n "$ATLAS_GPU_INDEX" ] && [ -z "$HIP_VISIBLE_DEVICES" ]; then
      export HIP_VISIBLE_DEVICES="$ATLAS_GPU_INDEX"
      # Newer ROCm (5.7+) prefers ROCR_VISIBLE_DEVICES; set both for
      # cross-version compatibility.
      export ROCR_VISIBLE_DEVICES="${ROCR_VISIBLE_DEVICES:-$ATLAS_GPU_INDEX}"
    fi
    # HSA_OVERRIDE_GFX_VERSION: force a specific gfx target. Useful when
    # rocm-smi reports an "unsupported" GPU (e.g., a consumer Vega/RDNA1
    # variant) that should still work with a near-compatible target.
    # Example: ATLAS_HSA_OVERRIDE_GFX_VERSION=10.3.0 makes RDNA1 cards
    # masquerade as RDNA2 for HIP kernel selection.
    if [ -n "$ATLAS_HSA_OVERRIDE_GFX_VERSION" ]; then
      export HSA_OVERRIDE_GFX_VERSION="$ATLAS_HSA_OVERRIDE_GFX_VERSION"
    fi
    ;;
  vulkan)
    # Vulkan universal backend (#114). The same llama-server binary runs
    # on any Vulkan-capable ICD: Mesa RADV (AMD), Mesa ANV (Intel),
    # nvidia-container-toolkit's libGLX_nvidia (NVIDIA), MoltenVK (Apple
    # via QEMU), Adreno (Snapdragon), or lavapipe (CPU software). The
    # compose overlay decides which ICD by setting device passthrough +
    # NVIDIA_DRIVER_CAPABILITIES; the entrypoint itself stays neutral.
    #
    # GGML_VK_VISIBLE_DEVICES: equivalent to CUDA_VISIBLE_DEVICES /
    # HIP_VISIBLE_DEVICES — pins to a specific Vulkan physical device
    # index when the host has multiple ICDs (e.g. iGPU + dGPU).
    if [ -n "$ATLAS_GPU_INDEX" ] && [ -z "$GGML_VK_VISIBLE_DEVICES" ]; then
      export GGML_VK_VISIBLE_DEVICES="$ATLAS_GPU_INDEX"
    fi
    # MESA_VK_DEVICE_SELECT: Mesa-specific selector ("vendorID:deviceID"
    # or "DeviceName"). Operator can set ATLAS_VK_DEVICE_SELECT to force
    # a specific physical device when GGML_VK_VISIBLE_DEVICES isn't
    # granular enough (e.g. two Intel Arc cards).
    if [ -n "$ATLAS_VK_DEVICE_SELECT" ]; then
      export MESA_VK_DEVICE_SELECT="$ATLAS_VK_DEVICE_SELECT"
    fi
    ;;
  metal|sycl)
    echo "Warning: ATLAS_BACKEND=$ATLAS_BACKEND but this entrypoint runs in Docker."
    echo "  Metal runs via the macOS hybrid path (docs/SETUP_MACOS.md), not in Docker. SYCL is roadmap."
    echo "  Continuing with default CPU-only behavior; performance will be poor."
    ;;
  *)
    echo "Warning: ATLAS_BACKEND='$ATLAS_BACKEND' unrecognized; treating as cuda."
    export GGML_CUDA_NO_PINNED="${GGML_CUDA_NO_PINNED:-0}"
    export CUDA_DEVICE_MAX_CONNECTIONS="${CUDA_DEVICE_MAX_CONNECTIONS:-1}"
    export CUDA_MODULE_LOADING="${CUDA_MODULE_LOADING:-LAZY}"
    ;;
esac

# BiasBusters #4 (ASA steering vectors). Vectors are model-specific, so a
# sidecar `<vector>.model` must identify the selected model before the
# entrypoint activates one. The default path lives next to the
# model file on the persistent /models volume so it survives container
# rebuilds. Operator drops the vector once (workflow:
# ATLAS/geometric-lens/asa_calibration/README.md), and every llama-server
# start picks it up automatically. To override the path or scale, set
# ATLAS_CONTROL_VECTOR / ATLAS_CONTROL_VECTOR_SCALE / _LAYER_RANGE.
# Default scale is conservative (0.5) — bump if behavior change is too
# subtle, drop if non-tool tasks degrade.
ATLAS_CONTROL_VECTOR="${ATLAS_CONTROL_VECTOR:-/models/ast_edit_steering.gguf}"
MODEL_BASENAME="$(basename "$MODEL_FILE")"
MODEL_STEM="${MODEL_BASENAME%.gguf}"
ATLAS_MODEL_ID="${ATLAS_MODEL_NAME:-$MODEL_STEM}"
CVECTOR_FLAGS=()
CVECTOR_STATUS="not present at $ATLAS_CONTROL_VECTOR — build it via geometric-lens/asa_calibration/README.md"
if [ -f "$ATLAS_CONTROL_VECTOR" ]; then
  CVECTOR_MARKER="$ATLAS_CONTROL_VECTOR.model"
  CVECTOR_MODEL=""
  if [ -f "$CVECTOR_MARKER" ]; then
    CVECTOR_MODEL="$(tr -d '\r\n' < "$CVECTOR_MARKER")"
  fi
  # Case-insensitive marker match — `atlas asa check` canonicalizes with
  # casefold, so the boot gate must accept the same markers it does.
  CVECTOR_MODEL_LC="$(printf '%s' "$CVECTOR_MODEL" | tr '[:upper:]' '[:lower:]')"
  if [ "$CVECTOR_MODEL_LC" = "$(printf '%s' "$ATLAS_MODEL_ID" | tr '[:upper:]' '[:lower:]')" ] || \
     [ "$CVECTOR_MODEL_LC" = "$(printf '%s' "$MODEL_BASENAME" | tr '[:upper:]' '[:lower:]')" ] || \
     [ "$CVECTOR_MODEL_LC" = "$(printf '%s' "$MODEL_STEM" | tr '[:upper:]' '[:lower:]')" ] || \
     [ "${ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED:-0}" = "1" ]; then
    CVECTOR_SCALE="${ATLAS_CONTROL_VECTOR_SCALE:-0.5}"
    CVECTOR_FLAGS=(--control-vector-scaled "$ATLAS_CONTROL_VECTOR:$CVECTOR_SCALE")
    if [ -n "$ATLAS_CONTROL_VECTOR_LAYER_RANGE" ]; then
      read -r -a CVECTOR_LAYER_RANGE <<< "$ATLAS_CONTROL_VECTOR_LAYER_RANGE"
      CVECTOR_FLAGS+=(--control-vector-layer-range "${CVECTOR_LAYER_RANGE[@]}")
    fi
    CVECTOR_STATUS="$ATLAS_CONTROL_VECTOR (model=$CVECTOR_MODEL, scale=$CVECTOR_SCALE${ATLAS_CONTROL_VECTOR_LAYER_RANGE:+, layers=$ATLAS_CONTROL_VECTOR_LAYER_RANGE})"
  else
    CVECTOR_STATUS="disabled: $ATLAS_CONTROL_VECTOR is marked for '${CVECTOR_MODEL:-unknown}', selected model is '$ATLAS_MODEL_ID'"
  fi
fi

# Embedding convention for the Geometric Lens. `--pooling` is
# server-global in llama.cpp and two consumers read /embedding: the
# whole-text C(x) path and the per-step PRM path (extract_per_token,
# score-per-step with no explicit layer). Only `none` serves both —
# per-token vectors pool down to the whole-text vector, while a pooled
# response cannot be unpooled.
#
# Pooling and L2 normalization are therefore applied client-side, in
# embedding_extractor.extract_embedding and the bench's
# extract_embedding_urllib, which both request `embd_normalize: -1` and
# normalize after pooling. Normalizing first does not commute: the mean
# of per-token unit vectors points elsewhere than the normalized mean of
# the raw vectors. That is what makes the calibrated artifacts reproduce
# here (the 2026-07-15 bench incident: per-token output pooled without
# normalizing served C(x) ~600 against a calibrated ~20-30, with every
# health check green).
ATLAS_EMBED_POOLING="${ATLAS_EMBED_POOLING:-none}"

echo "=== ATLAS llama-server — $MODEL_BASENAME ==="
echo "  Backend: $ATLAS_BACKEND${ATLAS_GPU_INDEX:+ (GPU index=$ATLAS_GPU_INDEX)}"
echo "  Model: $MODEL_FILE"
echo "  Context: $CTX_LENGTH | KV: K=$KV_CACHE_K V=$KV_CACHE_V | Parallel: $PARALLEL | Batch: $BATCH_SIZE/$UBATCH_SIZE"
echo "  Embeddings: ENABLED (model self-embeddings for the Geometric Lens)"
echo "  Embed convention: pooling=$ATLAS_EMBED_POOLING (normalization is per-request via embd_normalize)"
echo "  Slot save path: $SLOT_SAVE_PATH"
echo "  ASA steering: $CVECTOR_STATUS"
echo "  GPU fit: --fit off — the model, KV cache, and compute buffers must"
echo "  fit entirely in VRAM. If startup fails below with a CUDA out-of-memory"
echo "  allocation error, the budget above is too large for this GPU: run"
echo "  'atlas tier fit --write' on the host to size it, then recreate this"
echo "  container. See docs/TROUBLESHOOTING.md 'Model + KV cache don't fit'."

# Prompt caching stays on so each agent-loop turn can reuse its encoded
# prefix. Cross-session isolation comes from the proxy erasing the KV slot at
# session start (PC-045), not from disabling the cache.
# Internal service auth: enable llama-server's API key when the
# per-installation token is mounted (atlas init writes it; compose
# mounts it read-only). llama-server exempts /health, so the compose
# healthcheck stays headerless. The token never appears in argv —
# --api-key-file reads it in-process.
API_KEY_FLAGS=()
TOKEN_FILE="${ATLAS_SERVICE_TOKEN_FILE:-/run/atlas-secrets/service-token}"
if [ -s "$TOKEN_FILE" ]; then
  API_KEY_FLAGS=(--api-key-file "$TOKEN_FILE")
  echo "Internal auth: enabled (api-key-file)"
fi


exec /usr/local/bin/llama-server \
  -m "$MODEL_FILE" \
  -c "$CTX_LENGTH" \
  "${KV_FLAGS[@]}" \
  --parallel "$PARALLEL" \
  --cont-batching \
  -ngl 99 \
  --fit off \
  --host 0.0.0.0 \
  --port "$PORT" \
  --flash-attn on \
  --mlock \
  -b "$BATCH_SIZE" \
  -ub "$UBATCH_SIZE" \
  --slot-save-path "$SLOT_SAVE_PATH" \
  --ctx-checkpoints 0 \
  --embeddings \
  --pooling "$ATLAS_EMBED_POOLING" \
  --jinja \
  "${CVECTOR_FLAGS[@]}" \
  "${API_KEY_FLAGS[@]}"
