#!/usr/bin/env python3
"""Build the structural_edit-vs-edit_file ASA steering vector from contrast pairs.

Algorithm (matches the Feb 2026 ASA paper, arxiv 2602.04935):
  1. For each (positive, negative) pair, extract per-token residual stream
     activations at a chosen layer via the PC-202-patched /embedding
     endpoint of atlas-llama-server (GPU-accelerated).
  2. Mean across tokens to get one vector per prompt.
  3. Mean across all positive prompts → v_pos. Same for negatives → v_neg.
  4. v_global = v_pos − v_neg. This is the direction in residual space
     that distinguishes "about to emit structural_edit" from "about to emit
     edit_file" on the same task.
  5. Write as a GGUF control vector that llama-server's
     --control-vector-scaled flag consumes.

Layer choice: ~75% of the selected model's depth (BiasBusters practitioner
guidance). `atlas asa build` derives it from the loaded model; direct callers
must pass `--layer`.

Run inside the atlas-geometric-lens container so it can reach the
PC-202 hidden-states endpoint via the same network the lens uses:
    docker exec -i atlas-geometric-lens-1 python3 \\
        /workspace_calib/build_steering_vector.py \\
        --pairs /workspace_calib/contrast_pairs.jsonl \\
        --out /workspace_calib/ast_edit_steering.gguf \\
        --layer <75%-of-model-depth>
"""

from __future__ import annotations

import argparse
import functools
import json
import os
import sys
import time
from pathlib import Path

# Imported at module load so a missing gguf package fails in seconds, not
# after the ~25-minute extraction pass right before the write step.
import gguf
import numpy as np

sys.path.insert(0, "/app")
from geometric_lens.embedding_extractor import extract_per_layer_per_token
from geometric_lens.model_transport import model_request


_SYSTEM_PROMPT = (
    "You are ATLAS, a coding assistant. Choose the right tool for the job."
)


def _llama_url() -> str:
    return os.environ.get(
        "LLAMA_EMBED_URL",
        os.environ.get("LLAMA_URL", "http://llama-server:8080"),
    ).rstrip("/")


@functools.lru_cache(maxsize=2048)
def _render_user_context(user: str) -> str:
    """Render system+user messages with the loaded model's own template."""
    payload = {
        "messages": [
            {"role": "system", "content": _SYSTEM_PROMPT},
            {"role": "user", "content": user},
        ],
    }
    try:
        result = model_request(f"{_llama_url()}/apply-template", payload, timeout=30)
    except Exception as exc:
        raise RuntimeError(
            "llama-server /apply-template is required for model-agnostic "
            f"ASA calibration: {exc}"
        ) from exc
    prompt = result.get("prompt") if isinstance(result, dict) else None
    if not isinstance(prompt, str) or not prompt:
        raise RuntimeError("unexpected llama-server /apply-template response")
    return prompt


def render_prompt(pair: dict) -> str:
    return _render_user_context(pair["user"]) + pair["assistant_prefix"]


def extract_mean_residual(prompt: str, layer: int) -> np.ndarray:
    """Returns the mean residual stream vector at the chosen layer,
    averaged across tokens. Shape: (hidden_dim,)."""
    per_layer, n_tokens, hidden_dim = extract_per_layer_per_token(prompt, [layer])
    rows = per_layer[layer]
    arr = np.asarray(rows, dtype=np.float32)
    if arr.shape != (n_tokens, hidden_dim):
        raise RuntimeError(
            f"layer {layer} shape mismatch: got {arr.shape}, "
            f"expected ({n_tokens}, {hidden_dim})"
        )
    return arr.mean(axis=0)


def write_gguf_control_vector(out_path: Path, layer: int, vector: np.ndarray,
                              n_pairs: int, model_hint: str = "unknown",
                              layer_count: int = 0) -> None:
    """Write the steering vector in llama.cpp's control-vector GGUF format.

    Format expected by llama-server --control-vector-scaled:
      - Metadata key "general.architecture" = "controlvector"
      - Metadata key "controlvector.model_hint" = model architecture name
      - Metadata key "controlvector.layer_count" = total layers (informational)
      - One tensor per layer named "direction.<layer>", shape (hidden_dim,),
        dtype f32. Layers without a direction tensor are not steered.
    """
    writer = gguf.GGUFWriter(str(out_path), arch="controlvector")
    writer.add_string("controlvector.model_hint", model_hint)
    writer.add_uint32("controlvector.layer_count",
                      layer_count if layer_count > 0 else layer + 1)
    writer.add_string(
        "general.description",
        f"ATLAS BiasBusters #4 ASA — structural_edit vs edit_file (n={n_pairs} pairs, layer {layer})",
    )
    # llama.cpp expects shape [hidden_dim] f32 named direction.N
    writer.add_tensor(f"direction.{layer}", vector.astype(np.float32))
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--pairs", required=True, type=Path,
                    help="contrast_pairs.jsonl — paired structural_edit/edit_file prompts")
    ap.add_argument("--out", required=True, type=Path,
                    help="output GGUF control vector path")
    ap.add_argument("--layer", type=int, required=True,
                    help="layer to extract residuals from; use ~75%% of the "
                         "selected model's layer count")
    ap.add_argument("--limit", type=int, default=0,
                    help="cap pairs processed (0 = all). Useful for smoke tests.")
    ap.add_argument("--model-hint", default="unknown",
                    help="architecture name stamped into the vector's "
                         "controlvector.model_hint metadata")
    ap.add_argument("--layer-count", type=int, default=0,
                    help="total layer count stamped into the vector "
                         "metadata (0 = use --layer + 1)")
    args = ap.parse_args()

    pairs = []
    with args.pairs.open() as f:
        for line in f:
            line = line.strip()
            if line:
                pairs.append(json.loads(line))
    if args.limit > 0:
        pairs = pairs[: args.limit * 2]  # *2 because each scenario is 2 lines
    print(f"loaded {len(pairs)} prompts ({len(pairs) // 2} contrast pairs)")

    pos_means: list[np.ndarray] = []
    neg_means: list[np.ndarray] = []

    started = time.monotonic()
    for i, pair in enumerate(pairs):
        prompt = render_prompt(pair)
        try:
            mean_vec = extract_mean_residual(prompt, args.layer)
        except Exception as exc:
            print(f"[{i}] extraction failed: {exc}", file=sys.stderr)
            return 2
        if pair["label"] == "structural_edit":
            pos_means.append(mean_vec)
        elif pair["label"] == "edit_file":
            neg_means.append(mean_vec)
        else:
            print(f"[{i}] unknown label {pair['label']!r}", file=sys.stderr)
            return 2

        if (i + 1) % 50 == 0 or (i + 1) == len(pairs):
            elapsed = time.monotonic() - started
            rate = (i + 1) / elapsed if elapsed > 0 else 0
            eta = (len(pairs) - (i + 1)) / rate if rate > 0 else 0
            print(
                f"  [{i+1:>4}/{len(pairs)}] "
                f"pos={len(pos_means)} neg={len(neg_means)} "
                f"elapsed={elapsed:.1f}s rate={rate:.2f}/s eta={eta:.0f}s",
                flush=True,
            )

    if not pos_means or not neg_means:
        print(
            f"need both structural_edit and edit_file pairs; got pos={len(pos_means)} neg={len(neg_means)}",
            file=sys.stderr,
        )
        return 2

    v_pos = np.mean(pos_means, axis=0)
    v_neg = np.mean(neg_means, axis=0)
    v_global = (v_pos - v_neg).astype(np.float32)
    norm = float(np.linalg.norm(v_global))
    print(f"\nv_pos shape={v_pos.shape}")
    print(f"v_neg shape={v_neg.shape}")
    print(f"v_global = v_pos - v_neg, ||v_global||={norm:.4f}")

    args.out.parent.mkdir(parents=True, exist_ok=True)
    write_gguf_control_vector(args.out, args.layer, v_global, len(pos_means),
                              model_hint=args.model_hint,
                              layer_count=args.layer_count)
    size = os.path.getsize(args.out)
    print(f"wrote {args.out} ({size} bytes, layer {args.layer}, hidden_dim {len(v_global)})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
