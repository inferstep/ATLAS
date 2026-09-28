# Publishing Lens + ASA Artifacts

This guide walks you through contributing trained **Geometric Lens** (`cost_field.pt`) or **ASA control vectors** (`*.gguf`) back to ATLAS so other users running the same base model get them automatically.

It's the long-form walkthrough for the artifact-contribution flow introduced in [CONTRIBUTING.md](../CONTRIBUTING.md#contributing-trained-artifacts-lens--asa). CLI flag reference lives in [CLI.md](CLI.md). If you just want flag syntax, that's the right place — read this one first if you've never published before.

---

## What you'll do, end to end

1. **Train** an artifact locally (`atlas lens build` or `atlas asa build`).
2. **Publish** it (`atlas lens publish` / `atlas asa publish`) — one command uploads the binary to a HuggingFace repo you own and opens a registry PR against `github.com/inferstep/ATLAS`.
3. **Wait for review** — the maintainer verifies the artifact and merges the PR (see [What happens after you submit](#what-happens-after-you-submit)).

Once merged, downstream users see your model under `atlas model list` and get
the artifact automatically on `atlas model install <name>`.

---

## What you need before you start

| Requirement | Where to get it | Required? |
|---|---|---|
| HuggingFace account | https://huggingface.co/join | **Yes** — you own the repo your artifact lives in |
| HuggingFace write token | https://huggingface.co/settings/tokens (scope: write) | **Yes** — set as `HF_TOKEN` env var |
| `huggingface_hub` Python pkg | `pip install huggingface_hub` | **Yes** on the host (already in the lens container) |
| `gh` CLI (GitHub) | https://cli.github.com | **Optional** — auto-opens the registry PR. Without it, you'll get a paste-ready PR body printed to your terminal |
| `gh` authenticated | `gh auth login` | Only if you installed `gh` above |

You do **not** need a GitHub PAT separately — `gh` handles its own auth.
You do **not** need write access to the ATLAS repo — the PR is opened from your fork.

**Set your HF token:**

```bash
export HF_TOKEN=hf_xxxxxxxxxxxxxxxxxxxxxxxx
# add to ~/.bashrc or ~/.zshrc so it sticks
```

The CLI also reads `HUGGINGFACE_HUB_TOKEN` and `HUGGING_FACE_HUB_TOKEN` if you've already set one for `huggingface-cli` use.

---

## What publish does

Every publish command runs the same sequence; only the artifact and the
registry field it sets differ:

1. **Pre-flight** — checks credentials and tooling: `HF_TOKEN` set, `huggingface_hub` installed, `gh` present or not. It then verifies the artifact: the lens flow checks the artifact files exist and the calibration JSONs are complete and valid; the ASA flow validates the `.gguf` and its model marker instead. The torch checkpoint itself is not loaded as a gate — a failed dim probe warns and continues.
2. **Hash** — SHA-256s the artifact so the PR carries a tamper-detectable fingerprint.
3. **Upload to HF** — creates the repo (idempotent), uploads the artifact files, and generates a model card README with license + base-model badge.
4. **Render PR body** — produces a markdown checklist with the HF URL, SHA-256, input dim, license, and a suggested diff for `atlas/commands/model_registry.py`.
5. **Open the PR** — with `gh` installed and authed, the PR is built entirely through the GitHub API (branch created on your fork if you can't push upstream, a complete `Model(...)` registry entry committed) and opened against the `dev` branch — no local git checkout needed. If the model is already registered upstream or `gh` is unavailable, the body is printed for manual paste into https://github.com/inferstep/ATLAS/compare.

`--dry-run` runs pre-flight, hash, and PR-body render but skips the HF upload
and PR. `--skip-pr` uploads to HF and prints the PR body for manual paste.

---

## Publishing everything at once

After onboarding a model you have both lens halves and the ASA vector. Publish
them as one action:

```bash
atlas publish --lens-repo your-username/atlas-lens-<model> \
              --asa-repo  your-username/atlas-asa-<model>
```

This opens a **single registry PR** whose entry carries
`lens_status="supported"` and `asa_status="supported"` together.
`--lens-only` / `--asa-only` delegate to the per-component commands below.

## Publishing a Lens artifact

Assumes you've run `atlas lens build --samples your-data.json` and have the
C(x), G(x), model identity, and per-model calibration files in the artifact
directory. If not, see the `atlas lens build` section in [CLI.md](CLI.md).

```bash
# Preview the PR body without uploading anything
atlas lens publish <model-name> \
    --repo your-username/atlas-lens-model-name \
    --dry-run

# Real upload + open the registry PR
atlas lens publish <model-name> \
    --repo your-username/atlas-lens-model-name
```

`--repo` is the HuggingFace destination (created if it doesn't exist);
`atlas-lens-<model-slug>` is the naming convention. The upload includes C(x)
(`cost_field.pt`, plus `cost_field.safetensors` when it is at least as fresh as
the `.pt`), G(x) (`gx_xgboost.json`, `gx_weights.json`), `model_identity.json`,
`cx_normalization.json`, and `gx_thresholds.json`.

After any re-publish, update the `lens_artifact_sha256` / `asa_artifact_sha256`
entries for the model in `atlas/commands/model_registry.py` — the installer
verifies downloads against those hashes and will refuse files that don't match
the pinned values. For HF LFS objects the hash is the `x-linked-etag` response
header of the `resolve/` URL; for small non-LFS files use `sha256sum` on the
uploaded file.

### Common flags

| Flag | Purpose |
|---|---|
| `--license mit` | License declared in the model card (default `apache-2.0`; `mit` / `bsd-3-clause` also fine) |
| `--dry-run` | Hash + render PR body, skip HF upload and PR creation |
| `--skip-pr` | Upload to HF, print PR body for manual paste (use when `gh` is missing or you want to edit the body) |
| `--artifact-dir DIR` | Override which directory's `cost_field.pt` gets uploaded |

---

## Publishing an ASA control vector

Same shape, different artifact. Assumes you've trained a vector with `atlas asa
build` (see [CLI.md](CLI.md) for the training walkthrough).

```bash
atlas asa publish <model-name> \
    --repo your-username/atlas-asa-model-name \
    --dry-run

atlas asa publish <model-name> \
    --repo your-username/atlas-asa-model-name
```

The ASA delta: publish reads GGUF metadata from the `.gguf` to extract residual
dim, layer count, and the `model_hint` baked in by the calibration script,
uploads the single `.gguf` file, and sets `asa_status="supported"`. If your
`ATLAS_CONTROL_VECTOR` is a container-relative path like `/models/x.gguf`, the
CLI auto-resolves it to the host path via `<atlas_root>/models/` and
`$ATLAS_MODELS_DIR`.

---

## What happens after you submit

1. The maintainer gets a notification on the new PR
2. The artifact is pulled from your HF repo onto a verification VM
3. A private trust-gate test scores the artifact against a held-out pair set — designed to reject artifacts that mis-rank or look adversarial
4. On pass: the registry PR is merged. On fail: a comment lands on the PR explaining what tripped and how to address it

**Why the trust-gate set is private:** if it lived in this repo, anyone could train an artifact specifically tuned to pass it without actually generalizing. Security-by-obfuscation in this case is the right call — it forces submissions to be honestly good, not gate-aware.

Turnaround time is typically a day or two depending on maintainer availability. If your PR has been open for a week without a response, ping `@itigges22` in the PR thread.

---

## Troubleshooting

### `HF_TOKEN env var not set`

You haven't exported a token, or it's only set in a different shell. Run `echo $HF_TOKEN` to verify — if it's empty, `export HF_TOKEN=hf_...` and try again.

### `huggingface_hub not installed`

Run `pip install huggingface_hub`. The lens container has it baked in, but the host Python that runs `atlas` needs it too.

### `gh: command not found`

Either install `gh` from https://cli.github.com, or use `--skip-pr` — the CLI will print the PR body and you paste it into github.com/inferstep/ATLAS/compare manually. Both paths produce the same review outcome. (With `gh` present, no git checkout is required — the PR is created entirely through the GitHub API, including the fork for non-maintainers.)

### `Artifact input dim (unverified)` in the PR body

The dim probe needs `torch` installed on the host (`pip install torch`). Without it, the PR body shows "unverified" and the maintainer will probe the dim on their side. Not a blocker — the upload still happens.

### `Lens artifact is incomplete or uncalibrated` / `Lens calibration is invalid`

Publish refuses to upload when the artifact directory is missing runtime files or its calibration JSONs don't match the model — typically an interrupted training run, or a directory holding a previous model's artifacts. Re-run `atlas lens build` and confirm it ends with `Build complete (C(x) + G(x)).` before retrying.

### Non-permissive licenses

The CLI accepts any `--license` value — there's no runtime license gate. The gate is review: the maintainer PR checklist includes "License is permissive for redistribution", and ATLAS only merges artifacts under permissive licenses (apache-2.0, mit, bsd-3-clause). If your training data was scraped under a more restrictive license, that license can't be loosened by repackaging the trained weights — please don't try.

---

## Workflow expectations for contributors

- **One artifact per PR.** Mixing a lens + ASA upload in the same PR makes it harder to bisect a verification failure.
- **Use real model names** matching the canonical registry naming (check `atlas model list` for examples) so the PR doesn't bounce on a naming mismatch.
- **Don't push artifacts to your HF repo by hand** before running publish — let the CLI manage it so the SHA in the PR body matches what's actually on HF.
- **If you find a bug in the publish flow** (not the artifact itself), open a separate GitHub issue rather than burying it in the PR comments.

---

## See also

- [CLI.md — atlas lens / atlas asa command reference](CLI.md)
- [CONFIGURATION.md — env vars including HF_TOKEN](CONFIGURATION.md)
- [CONTRIBUTING.md — broader contribution guidelines](../CONTRIBUTING.md)
