# Geometric Lens Models & Training Data

## Active Models

| File | Size | Purpose |
|------|------|---------|
| `cost_field.pt` | ~8M | C(x) cost field — hidden-dim→512→128→1 MLP, maps embeddings to correctness energy. Dim follows the trained model (bundled: 3840, Gemma 4 12B). |
| `cost_field.safetensors` | ~8M | Pickle-free twin of `cost_field.pt`, written together by `save_cost_field` and shipped by `atlas lens publish`. |
| `model_identity.json` | <1K | Selected model name + embedding width. Current runtimes require it to match `ATLAS_MODEL_NAME`; same-width models are not interchangeable. |
| `cx_normalization.json` | <1K | Per-model C(x) sigmoid midpoint and steepness, derived from PASS/FAIL energy separation. |
| `gx_xgboost.json` | 17K | G(x) XGBoost ensemble — native XGBoost JSON dump (preferred loader path, see PC-031). |
| `gx_weights.json` | ~11M | G(x) PCA projection + training stats (hidden-dim→128). |
| `gx_thresholds.json` | <1K | Per-model `severe`, `off_rails`, and `low` operating thresholds. |
| `provenance.json` | <2K | Build manifest written by `atlas lens build` into every activated bundle: dataset, sample counts, metrics, hyperparameters, per-file SHA-256. Consumed by `atlas artifact verify/snapshot/rollback`. |

`gx_xgboost.pkl` (the legacy pickle fallback) is removed on retrain —
`save_gx` deletes it so a previous model's pickle can't shadow the JSON.

The checked-in reference bundle predates the identity/calibration files above.
It remains useful as frozen provenance, but current runtime interventions stay
disabled until `atlas lens build` produces a complete bundle for the selected
model.

## Training Data

**In-repo sample**: `geometric-lens/data/sample/embeddings.json` — 10 embeddings (5 PASS, 5 FAIL) showing the training-data format (`{"embeddings": [...], "labels": [1|0, ...]}`; 3840-dim, from a Gemma 4 12B bench run).

**Full dataset on HuggingFace**: https://huggingface.co/datasets/itigges22/ATLAS

| Dataset | Embeddings | PASS | FAIL | Dimension | Size |
|---------|-----------|------|------|-----------|------|
| Phase 0 (original) | 597 | 504 | 93 | 4096 | 48MB |
| Full training set | 13,398 | 4,835 | 8,563 | 4096 | 1.1GB |
| Fox 9B variant | 800 | 400 | 400 | 4096 | 65MB |
| 5120-dim variant | 520 | — | — | 5120 | 53MB |

Note: The large training files (>2MB) are stored on HuggingFace, not in the git repo. Only the sample and model weights are committed.

## Training Stats

| File | Contents |
|------|---------|
| `phase0_stats.json` | Phase 0 C(x) training: Val AUC 0.9467, Sep 2.04x, 3-fold CV |
| `retrain_stats.json` | C(x) retrain: Val AUC 0.8245, 800 samples |
| `gx_train_stats.json` | G(x) XGBoost: 13,398 samples, PCA-128 + SupCon + LDA |

## Training

All training is CLI-driven — `atlas lens build` trains both halves
(C(x) + G(x)), calibrates the per-model thresholds, and writes the
`provenance.json` manifest into the activated bundle:

```bash
# From a bench run's per-task results (the onboarding path):
atlas bench --run-id mymodel_lens --tasks 200
atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task

# From a labeled sample file ({"text": ..., "label": 0|1} array/JSONL —
# the canonical set is on the HuggingFace dataset above):
atlas lens build --samples path/to/labeled.json
```

See [docs/CLI.md § atlas lens](../../../docs/CLI.md#atlas-lens) for flags
and minimum sample requirements.
