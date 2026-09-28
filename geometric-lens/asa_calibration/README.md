# ASA steering vectors — structural_edit vs edit_file bias

May 2026 BiasBusters #4. Complements the grammar-level enforcement
(items #2/#3 in `proxy/agent.go`) by shifting the proposal distribution
upstream: even before grammar can ban `edit_file`, the residual stream
is already biased toward `structural_edit` for whole-function/class/element
swaps.

## Published vectors predate the tool rename

The tool this vector biases toward was named `ast_edit` when the currently
published vectors were built, and the contrast prompts in
`generate_pairs.py` embedded that literal string. The prompt templates now
say `structural_edit`; the published `.gguf` artifacts do not.

The artifacts keep the filename `ast_edit_steering.gguf`, because
`atlas/cli/commands/model_registry.py` pins them by name and SHA256 against
the HuggingFace dataset — renaming the file would 404 every download.

A published vector still encodes the underlying decision (replace a whole
named node vs. patch a line), which is what the contrast was built to
capture. Whether it still helps, or harms tool calling, under the new tool
name has not been measured. To rebuild against the current prompts, per
model:

```
# regenerates contrast_pairs.jsonl from the current templates, then
# extracts residuals in the lens container
atlas asa build <registry-model-name>
atlas asa check
atlas asa publish <registry-model-name>
```

`asa build` regenerates `contrast_pairs.jsonl` on demand when `--pairs` is
omitted, so it picks up the renamed templates in `generate_pairs.py` with no
extra step. After publishing, update the pinned SHA256 in
`model_registry.py`.

This is independent of the Geometric Lens retrain, which is the one that
consumes `atlas bench` results.

## Pipeline

```
contrast_pairs.jsonl  →  build_cvector_prompts.py  →  positive.txt + negative.txt
                                                           │
                                                           ▼
                                              llama-cvector-generator
                                                           │
                                                           ▼
                                              ast_edit_steering.gguf
                                                           │
                                                           ▼
                              llama-server --control-vector-scaled FILE:SCALE
```

## Steps

1. **Curate contrast pairs** in `contrast_pairs.jsonl`. Generate 1000 pairs
   with `generate_pairs.py` from
   richly-templated base scenarios (Python functions / decorated
   functions / async / generators / classes / dataclasses; HTML body /
   head / header / nav / main / footer / form / aside / section /
   article; post-write_file-rejection variants). Each pair is one
   `structural_edit` correct example and one `edit_file` incorrect example
   for the **same** user task; positional order matters
   (cvector-generator contrasts line N of positive vs line N of
   negative). To regenerate with a different seed, n, or extended
   pools:
   ```
   python generate_pairs.py --out contrast_pairs.jsonl --n 1000 --seed 42
   ```
   Add new templates or variation pools at the top of `generate_pairs.py`
   to expand coverage; rerun and the cvector vector picks up the new
   diversity on the next extraction.

2. **Build prompt files**:
   ```bash
   python build_cvector_prompts.py \
     --pairs contrast_pairs.jsonl \
     --positive structural_edit_positive.txt \
     --negative structural_edit_negative.txt
   ```
   The script calls the loaded llama-server's `/apply-template` endpoint, so
   the prompts use the selected model's own chat template rather than tokens
   from a hard-coded model family.

3. **Generate the control vector** (requires `llama-cvector-generator`
   binary built from `/tmp/llama.cpp/tools/cvector-generator/`):
   ```bash
   llama-cvector-generator \
     -m /models/your-model.gguf \
     --positive-file structural_edit_positive.txt \
     --negative-file structural_edit_negative.txt \
     --method mean \
     -o ast_edit_steering.gguf \
     -ngl 99
   ```
   `--method mean` is the simple mean-difference variant (matches the
   ASA paper's v_global = μ(pos) − μ(neg)). `--method pca` is the
   principal-component variant; better signal but needs more pairs to
   be stable.

4. **Drop the vector and its model marker at the standard path** (`atlas asa
   build` writes both automatically):
   ```
   /models/ast_edit_steering.gguf
   /models/ast_edit_steering.gguf.model
   ```
   `inference/entrypoint-v3.1.sh` checks for this file at every
   start and appends `--control-vector-scaled` only when the marker identifies
   the selected model. A vector left behind after a model switch is disabled
   rather than silently applied in the wrong residual space.

   To override the path, scale, or layer range:
   - `ATLAS_CONTROL_VECTOR=/some/other/path.gguf` (default `/models/ast_edit_steering.gguf`)
   - `ATLAS_CONTROL_VECTOR_SCALE=1.0` (default `0.5` — conservative; bump to 1.0–1.5 if behavior change is too subtle, drop toward 0.2 if other tasks degrade)
   - `ATLAS_CONTROL_VECTOR_LAYER_RANGE="24 30"` (default: all layers)

5. **Validate** by re-running the May 7 flask-app test that surfaced
   the bias. Expected outcome: model proposes `structural_edit` on first
   attempt for whole-function rewrites, without needing the grammar
   trigger to fire.

## Layer scope (optional)

By default `--control-vector-scaled` applies the steering to all
layers. The ASA practitioner's guide recommends ~75% of the selected model's
depth. `atlas asa build` derives that layer from llama-server metadata. If you
want to scope the runtime application:

```
--control-vector-scaled FILE:SCALE \
--control-vector-layer-range 24 30
```

Narrower ranges are safer (less collateral damage to non-tool tasks)
but the signal is weaker.

## Why not always-on?

ASA composes with the grammar gate (items #2/#3) — the grammar is a
hard ban that fires after a write_file rejection on .py/.html. ASA
covers the cases the grammar gate doesn't: **first-attempt** decisions
where no rejection has happened yet. Both can run together; they don't
conflict.

The gating decision lives at the entrypoint (env var present ⇒ on,
absent ⇒ off). No proxy changes are needed beyond the startup log
that surfaces the configured state.
