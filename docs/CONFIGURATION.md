# ATLAS Configuration Reference

Reference for the environment variables, command-line flags, and configuration files across every ATLAS service. Hardware/runtime settings have safe defaults; model selection is explicit. Some variable groups are documented where they are used and only pointed to from here: bootstrap-only knobs (`ATLAS_BOOTSTRAP_*`, `ATLAS_REPO_URL`, `ATLAS_INSTALL_DIR`, `ATLAS_GO_VERSION`) in [SETUP.md](SETUP.md), TUI-only variables (`ATLAS_TUI_LOG`, `ATLAS_TUI_MOUSE`, `ATLAS_TUI_STARTUP_NOTE`) in [CLI.md](CLI.md)'s environment table.

---

## Quick Start

```bash
atlas init                 # selects a registry model and writes .env
docker compose up -d
```

For a manual/BYO install, copy `.env.example` and set `ATLAS_MODEL_FILE` and
`ATLAS_MODEL_NAME`. Compose intentionally fails when either is missing instead
of silently choosing a model family.

---

## 1. Docker Compose (.env)

These variables are read by `docker-compose.yml` and control host-side port mappings and model paths. Copy `.env.example` to `.env` to configure:

**Precedence** (highest first): process environment → `.env` file →
in-code default. An empty value in a higher layer is treated as unset
and falls through. `atlas.config_schema.resolve_typed()` is the
single resolver that implements this.

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_MODELS_DIR` | `./models` | Host path to directory containing GGUF model weights |
| `ATLAS_MODEL_FILE` | **required** | Selected model filename (must exist in ATLAS_MODELS_DIR) |
| `ATLAS_MODEL_NAME` | **required** | Selected model identifier; normally the filename without `.gguf`. `.env` must set it; individual services fall back to `local-model` if unset. |
| `ATLAS_CTX_SIZE` | `131072` | Context window size in tokens, TOTAL across all parallel slots (mapped to `CONTEXT_LENGTH` inside the llama container). Sized per model + GPU by `atlas tier fit --write`. |
| `ATLAS_PARALLEL_SLOTS` | `4` | Concurrent request slots. llama-server divides `ATLAS_CTX_SIZE` by this for per-slot context. |
| `ATLAS_MAX_TOKENS` | `8192` | Per-turn generation ceiling (`max_tokens`). An agent turn is a tool call or a whole-file `write_file` (a few thousand tokens); 8192 covers a ~600-line generation and bounds a content runaway to a couple minutes. Raise only for genuinely large single-file writes. |
| `ATLAS_MAX_COMPLETION_TOKENS` | `8192` | Ceiling the proxy forces onto **passthrough** generation requests (`/v1/chat/completions`, `/v1/completions`, `/completion`, `/completions`, `/infill`) that carry no completion bound, a non-positive one (`-1` means "unlimited" to llama-server), or one above the ceiling. Without it, a client that disconnects mid-stream leaves an unbounded zombie generation holding a llama slot until the context fills. The agent loop's own calls are governed by `ATLAS_MAX_TOKENS`, which they always send. |
| `ATLAS_AGENT_HISTORY_BUDGET` | (unset) | Optional hard ceiling (tokens) on the kept conversation window. **Unset by default**: the window is sized to the slot — `per-slot context − ATLAS_MAX_TOKENS − 2048 − slot/8` (the `slot/8` term is tokenizer slack: the chars/4 estimate under-counts dense code and JSON-escaped tool results) — so it uses the whole slot rather than an artificial cap. Set this only to bound per-turn re-encode cost below slot capacity on SWA models (trades retained context for faster turns). The active file under edit is pinned in the trim regardless, so it never falls out of the window — and its size is counted against the budget. If llama-server still rejects a prompt as over-context, the loop force-trims to the minimum window and retries once instead of failing the session. |
| `ATLAS_DEDUP_READS` | `1` | When `1`, a whole-file re-read of an unchanged file returns a compact pointer **only if the content is still in the live context**; if the content was trimmed out, the full file is re-served (so the model never edits blind). Set `0` to always serve the full re-read. |
| `ATLAS_KV_TYPE_K` | `f16` | KV-cache K quantization (`f16`, `q8_0`, `q4_0`). Set by `atlas tier fit --write`. |
| `ATLAS_KV_TYPE_V` | `f16` | KV-cache V quantization. Set by `atlas tier fit --write`. |
| `ATLAS_UBATCH` | `1024` | llama-server micro-batch size (`-ub`). Drives the compute-buffer VRAM cost (~ubatch × n_embd × 280 bytes) — the term that OOMs first on tight cards. Set by `atlas tier fit --write`. |
| `ATLAS_BATCH` | `1024` | llama-server logical batch size (`-b`). Must be no larger than `ATLAS_UBATCH` because self-embeddings are always enabled. Set by `atlas tier fit --write`. |
| `ATLAS_EMBED_POOLING` | `none` | Pooling mode for the self-embedding `/embedding` endpoint (`--pooling`). `--pooling` is server-global in llama.cpp, and two consumers read the endpoint: whole-text `C(x)` and the per-step PRM path, which needs per-token vectors. Only `none` serves both — per-token vectors pool down to the whole-text vector, while a pooled response cannot be unpooled. The lens pools client-side and requests `embd_normalize: -1`, so it reads the same vector under either mode. Do not let the server normalize: `C(x)` is fitted on unnormalized vectors (‖v‖≈137) and reads a flat ~0.8 for every input once they are scaled to unit norm. |
| `ATLAS_PROJECT_DIR` | (cwd at `compose up`) | Host directory bind-mounted to `/workspace` inside the atlas-proxy container. Switch projects by re-creating the proxy container with this var set. |
| `ATLAS_DIAGNOSTIC_HOST_DIR` | `./diagnostics` | Host path bind-mounted into atlas-proxy at `/data/diagnostics`, the containment root for private diagnostic captures (`ATLAS_SHADOW_CAPTURE`, default off — see [OPERATIONS.md](OPERATIONS.md#private-diagnostics-task-contract-shadow-capture)). Bind-mounted rather than a named volume so the directory carries the invoking user's ownership, which is the identity the proxy runs as. Deliberately separate from the V3 telemetry volume. Nothing is written here unless a capture is named. |
| `ATLAS_GHCR_OWNER` | `inferstep` | GHCR namespace to pull images from. Set to your own GitHub username if you've published forked images. |
| `ATLAS_IMAGE_TAG` | `latest` | Image tag to pull (`latest` for main, `dev` for the dev branch, `3.1.3` or `sha-...` for pinned releases). Registry semver tags carry no leading `v`; `atlas upgrade`/`atlas rollback` strip one if given. |
| `ATLAS_LLAMA_PORT` | `8080` | llama-server host port |
| `ATLAS_LENS_PORT` | `8099` | Geometric Lens host port |
| `ATLAS_V3_PORT` | `8070` | V3 Pipeline service host port |
| `ATLAS_SANDBOX_PORT` | `30820` | Sandbox host port (container listens on 8020) |
| `ATLAS_SANDBOX_MEM` | `1536m` | Memory cap on the sandbox container (`docker` `mem_limit`, with `memswap_limit` tracking it so the sandbox cannot swap). It is one term of the **memory envelope** and not an independent knob: a value sized as a fraction of host RAM without subtracting the resident model is what let a staged verification command trigger a host-global OOM that killed the inference server. On a 16 GiB host running a 12B Q4 model the sandbox's share is about 1.5 GiB, not 11. Accepts `docker` size strings (`8g`, `512m`) or bytes; `0` removes the cap (unlimited — this is the configuration that caused the kill). See "The memory envelope" in OPERATIONS.md. |
| `ATLAS_LLAMA_MEM` | (unset — no limit) | Memory cap on the inference container. It had none, which is why the kernel's victim selection chose it: unbounded and largest. The obvious fix is a cap, and the first draft of this table proposed 9.5 GiB — measurement then showed that is **below what the deployed server already uses**: process peak RSS 10.31 GiB, an anonymous working set of 8.81 GiB once its 2.11 GiB of swapped pages are counted back, and 16 MiB of reclaimable page cache to give back under pressure. A limit under the peak does not produce reclaim, it produces a certain kill of the one process everything depends on. So it ships **unset** until a real-model canary establishes a value with measured headroom. Set it only with that evidence, and never below the observed peak. |
| `ATLAS_LLAMA_BUDGET_BYTES` | `11811160064` (11 GiB) | The inference service's measured **expectation**, used for the envelope arithmetic only — it enforces nothing. Above the observed 10.31 GiB peak RSS. Because the service is unbounded it contributes no maximum to the fit check; instead the proxy reports when this expectation exceeds what the enforced budgets leave over. |
| `ATLAS_LENS_MEM` | `1792m` | Memory cap on the geometric-lens container (measured peak 1.56 GiB). |
| `ATLAS_V3_MEM` | `512m` | Memory cap on the v3-service container (measured peak 52 MiB; it orchestrates, the executor runs). |
| `ATLAS_PROXY_MEM` | `256m` | Memory cap on the proxy container (measured peak 33 MiB). |
| `ATLAS_HOST_MEMORY_BYTES` | (unset) | Your machine's RAM, in bytes. Setting it turns the envelope from a comment into a startup check: the proxy adds every container maximum plus `ATLAS_HOST_RESERVE_BYTES` and, if the sum exceeds this, **refuses to run commands** and says by how much it is over. Reading files and answering questions still work — the diagnosis has to be readable. Unset means nothing is declared and nothing is checked; the per-command contract still applies. |
| `ATLAS_HOST_RESERVE_BYTES` | `1610612736` (1.5 GiB) | What must remain for the kernel, the container daemon, logging, monitoring and an orderly shutdown. A budget like any other, never the leftover. |
| `ATLAS_EXEC_MEMORY_BYTES` | `1073741824` (1 GiB) | Resident-memory ceiling for one untrusted command **and every process it spawns**, however it spawns them. Below `ATLAS_SANDBOX_MEM` on purpose: the executor process and the output it buffers share that container with the command. Enforced by a sampler that reads `VmHWM` across the tree and stops it deliberately, so the reason is reportable rather than an exit code that reads like a failing test. |
| `ATLAS_EXEC_ADDRESS_SPACE_MULTIPLE` | `0` (off) | Optional kernel `RLIMIT_AS` backstop at `ATLAS_EXEC_MEMORY_BYTES` × this value. Off by default because address space is not memory: a JVM reserves tens of gigabytes of virtual space before touching any of it, so a limit sized as memory refused the mapping and `javac` and `kotlinc` failed on every run while `ruby` and `php` passed. Set it only for a deployment that wants a second wall and runs no managed runtime. `0` or at least `1.0`; anything between is refused at startup. |
| `ATLAS_EXEC_MAX_PROCESSES` | `256` | How many processes one command's tree may contain before it is stopped. |
| `ATLAS_EXEC_OUTPUT_BYTES` | `33554432` (32 MiB) | How much stdout + stderr the executor will buffer for one command. Must be below `ATLAS_EXEC_MEMORY_BYTES`: the executor holds this buffer, so a cap at or above the per-command ceiling means the server dies before the command does. Validated at startup. |
| `ATLAS_EXEC_CONCURRENCY` | `1` | How many untrusted commands may run at once. Raising it is checked against the envelope: N commands at the per-command ceiling must still fit inside `ATLAS_SANDBOX_MEM`. |
| `ATLAS_SANDBOX_PIDS` | `1024` | PID cap on the sandbox container (`docker` `pids_limit`) — a kernel-level fork-bomb stop, far above any normal build and far below a bomb. Constant across hosts, so it defaults inline in compose; override only if a legitimate build needs more concurrent processes. |
| `ATLAS_SANDBOX_MAX_EXECUTION_TIME` | `300` | Per-call execution ceiling (seconds) inside the sandbox executor. Compose maps this onto the executor's `MAX_EXECUTION_TIME`; the 300s default matches the proxy's `run_command` cap so long builds/tests aren't cut off by the executor's internal 60s default. |
| `ATLAS_SHELL_OBSERVE_MAX_FILES` | `5000` | Ceiling on how many files `/shell` will hash when a caller asks for an observation. Above it the executor reports `digest_truncated: true` rather than passing off a partial digest as a complete one. Only paid by callers that send `observe_paths`. |
| `ATLAS_SANDBOX_TMP_SIZE` | `2G` | Size ceiling for the sandbox's `/tmp` tmpfs (pip download/build dir, mktemp targets). tmpfs sizes are ceilings, not reservations — raising them costs nothing until bytes are written, and written bytes count against `ATLAS_SANDBOX_MEM`, which stays the real backstop. |
| `ATLAS_SANDBOX_PIP_SIZE` | `1G` | Size ceiling for the sandbox's `~/.local` tmpfs (`pip install --user` target). Heavy scientific installs need room: pandas+pyarrow+numpy alone unpack ~400 MB, which overflowed the previous fixed 256 MB with `No space left on device`. |
| `ATLAS_SANDBOX_CACHE_SIZE` | `512M` | Size ceiling for the sandbox's `~/.cache` tmpfs (pip wheel cache, ruff, mypy). |
| `ATLAS_PROXY_PORT` | `8090` | atlas-proxy host port (TUI and OpenAI-compat clients connect here) |
| `ATLAS_BACKEND` | `cuda` | Inference backend. `cuda` (NVIDIA, V3.1.0+), `rocm` (AMD, V3.1.1, x86_64 only), `vulkan` (universal fallback), `metal` (Apple Silicon hybrid: native llama-server + Docker for the rest — see [SETUP_MACOS.md](SETUP_MACOS.md)), `sycl` (Intel Arc, roadmap). Set by `atlas init`; the entrypoint scripts read this to pick per-vendor env vars. ROCm + Vulkan + Metal also require bringing up the stack with `-f docker-compose.rocm.yml`, `-f docker-compose.vulkan.yml`, or `-f docker-compose.macos.yml` respectively (the wizard prints the right command). On aarch64 hosts (DGX Spark, Snapdragon X Elite, Jetson, Pi 5) `atlas init` filters out `rocm` since AMD has no arm64 release — see [SETUP.md § arm64](SETUP.md#arm64). |
| `ATLAS_MACOS_PREFIX` | `~/.atlas/macos` | macOS Metal only. Native llama.cpp install root shared by setup, launcher, and doctor. Set this when setup used `--prefix`. |
| `ATLAS_LLAMA_HOST` | `127.0.0.1` | macOS Metal only. Bind address for the native llama-server launched by `scripts/atlas-llama-macos.sh`. Loopback by default; override only when access from another host is intentionally required. |
| `ATLAS_GPU_INDEX` | (unset — all GPUs visible; the ROCm/Vulkan overlay files default it to `0`) | Vendor-local index of the GPU ATLAS should use on multi-GPU hosts. Compose passes it into the llama-server container; the entrypoint maps it to `CUDA_VISIBLE_DEVICES` (NVIDIA) or the HIP/Vulkan equivalent, and skips the export when empty. `docker-compose.rocm.yml` and `docker-compose.vulkan.yml` pin it to `0` when unset. |
| `ATLAS_GFX_TARGET` | `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a` | **ROCm only.** AMD compute target(s), semicolon-separated. Forwarded to `Dockerfile.rocm` as `AMDGPU_TARGETS` at build time. Trim to your GPU for a smaller image — see [SETUP.md § AMD GPU Targets](SETUP.md#amd-gpu-targets-dockerfilerocm). |
| `ATLAS_ROCM_TAG` | `6.2-complete` | **ROCm only.** Base image tag for `rocm/dev-ubuntu-22.04`. Bump when you want to test a newer ROCm release. |
| `ATLAS_UBUNTU_TAG` | `24.04` | **Vulkan only.** Base image tag for the `ubuntu` build stage of `Dockerfile.vulkan` (compose build arg). |
| `ATLAS_HSA_OVERRIDE_GFX_VERSION` | (unset) | **ROCm only.** Force a specific HSA gfx version at runtime — workaround for "officially unsupported" GPUs (e.g., older Vega) that still work with a compatible target. Example: `10.3.0` makes RDNA1 cards masquerade as RDNA2 for HIP kernel selection. |
| `ATLAS_CONFIG_SCHEMA_VERSION` | (stamped by `atlas config migrate`) | Schema-version stamp that `atlas config migrate` writes into `.env` — leave it in place. |

Docker Compose also sets inter-service URLs using Docker networking (e.g., `http://llama-server:8080`). These are fixed inside the Docker network and usually do not need to be configured by users. On macOS Metal, `docker-compose.macos.yml` keeps the container-side URL at `llama-server:8080` but forwards it to the native host-side `${ATLAS_LLAMA_PORT:-8080}`, so the port can move when 8080 is already occupied.

**Runtime-tuning passthrough.** `.env.example` carries a commented "Runtime tuning" section, and compose passes each key through to the owning container as an empty-default env var — so setting any of `ATLAS_V3_TIMEOUT`, `ATLAS_MAX_TOKENS`, `ATLAS_MAX_COMPLETION_TOKENS`, `ATLAS_AGENT_HISTORY_BUDGET`, `ATLAS_KEEP_LLAMA_WARM`, `ATLAS_FRESH_SLOT_PER_SESSION`, the repetition-sampling keys (`ATLAS_DRY_MULTIPLIER`, `ATLAS_DRY_BASE`, `ATLAS_DRY_ALLOWED_LENGTH`, `ATLAS_DRY_PENALTY_LAST_N`, `ATLAS_REPEAT_PENALTY`, `ATLAS_REPEAT_LAST_N`, `ATLAS_TRANSCRIPTION_SAMPLER`, `ATLAS_RESTATE_LAST_READ`) (proxy, § 2), `ATLAS_SANDBOX_MAX_EXECUTION_TIME` (sandbox, § 5), or `ATLAS_GPU_INDEX` / `ATLAS_GRAMMAR_MODE` in `.env` reaches the container without a compose edit. (`ATLAS_BACKEND` is not in the passthrough: it drives the host-side overlay choice, and the ROCm/Vulkan overlays set their own container-side value.) An empty/unset key means the in-code default applies.

**Restart policy.** Every service in `docker-compose.yml` runs with `restart: unless-stopped`, so the stack comes back up after a host reboot or a container crash without a manual `docker compose up`.

**Removed variables (`.env`).** These keys from older installs are ignored on read; `atlas config validate` flags them and `atlas config migrate` drops them: `ATLAS_REGISTRY` (the model registry is in-package), `ATLAS_REDIS_MAXMEMORY` and `ATLAS_REDIS_MEM` (lens state moved from Redis to SQLite, [ADR 0007](adr/0007-sqlite-state-store.md)), `SQLITE_DB_PATH` and `ATLAS_LENS_ONLINE_LEARNING` (the pattern cache and its SQLite store were removed; the lens keeps no state), `ATLAS_LENS_HOST_DIR`, `ATLAS_LENS_DATA_DIR` and `ATLAS_LENS_RETRAIN_MIN` (the proxy no longer collects a lens training corpus from agent use), `ATLAS_ENABLE_TRAINING` (training is always available), `ATLAS_RPG_PLANNING` (RPG planning was removed — [issue #148](https://github.com/inferstep/ATLAS/issues/148) is the record), `ATLAS_CANDIDATE_POLICY` (one rule decides which V3 candidate lands, and nothing selects another — [CANDIDATE_POLICY.md](CANDIDATE_POLICY.md)), and `ATLAS_CALL_GRAPH` (the call graph always runs, for Python files). K3s-side removals are listed in § 8.10.

`PARALLEL_SLOTS` and `KV_CACHE_TYPE_K/V` are accepted as fallbacks, but the
canonical `ATLAS_*` names take precedence and are what `atlas init` and
`atlas tier fit --write` write.

#### Backend-vs-Compose-override matrix

| `ATLAS_BACKEND` | Required compose invocation |
|---|---|
| `cuda` (default) | `docker compose up -d` |
| `rocm` | `docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d` |
| `vulkan` | `docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d` |
| `metal` (hybrid) | `./scripts/atlas-llama-macos.sh` + `docker compose -f docker-compose.yml -f docker-compose.macos.yml up -d` |
| `sycl` | Not yet packaged — Intel Arc users should use `vulkan` for now |

`atlas init` prints the right invocation as part of its "Next steps" summary. CLI-managed Compose operations also resolve the overlay from `ATLAS_BACKEND`; `atlas-bootstrap.sh` picks the Linux override automatically from its hardware probe.

### Adding your own model (drop-in / unregistered)

`atlas init` and `atlas model install <name>` only know models in the built-in
registry. To run a model that *isn't* registered (a brand-new release, a custom
quant), wire it up by hand. `atlas onboard` automates the safe parts of this and
stops at the one step only you can do (the rebuild); the manual flow is:

1. **Place the GGUF in `ATLAS_MODELS_DIR`** (default `./models`). Either drop the
   file in yourself, or fetch it with `atlas model install --url <hf-url>`
   (downloads into the models dir; no SHA pin since it's unregistered).

2. **Point `.env` at it** — set both keys:
   ```dotenv
   ATLAS_MODEL_FILE=your-model-Q4_K_M.gguf
   ATLAS_MODEL_NAME=your-model-Q4_K_M
   ```

3. **Size the runtime for this model + your GPU**:
   ```bash
   atlas tier fit          # preview: ctx / KV type / ubatch + the VRAM budget
   atlas tier fit --write  # apply to .env
   ```
   This reads the GGUF header (layer count, KV-head geometry, sliding-window
   layout) and your GPU's VRAM, and solves for the largest context that keeps
   inference **fully on-GPU**. Different models have wildly different KV
   footprints — a budget tuned for one model can OOM or silently spill to CPU
   (5× slower) on another. The server runs with `--fit off`, so an oversized
   config refuses to start rather than spilling. If it reports the model
   doesn't fit, it names the largest quant file size that would — see
   [TROUBLESHOOTING.md § What fits on my GPU?](TROUBLESHOOTING.md#what-fits-on-my-gpu)
   for pre-download sizing guidance.

4. **Restart inference only** (don't tear the stack down — that triggers a long
   CUDA rebuild): `docker compose up -d llama-server --no-deps --force-recreate`.

5. **Confirm the engine recognizes the architecture.**
   `docker compose logs -f llama-server` — a healthy load ends in
   `server is listening`. If you instead see `error loading model: unknown
   (model) architecture '<arch>'`, your `atlas-llama` image's bundled llama.cpp
   predates that architecture and **you must rebuild the inference image**:
   ```bash
   # The image pins llama.cpp (LLAMA_CPP_REV in inference/Dockerfile.v31) so
   # the hidden-states patch applies cleanly — a plain rebuild reuses that same
   # pinned revision and will NOT pick up newer architectures. Override the
   # pin with a llama.cpp commit that knows your model's architecture:
   docker compose build --build-arg LLAMA_CPP_REV=<sha> llama-server   # ~70 min on CUDA
   ```
   > ⚠️ **Do not strip ATLAS's custom llama.cpp features when rebuilding.** The
   > build re-applies `inference/patches/expose-hidden-states.patch` (the
   > per-layer `hidden_states` extension the Geometric Lens relies on) to the
   > freshly-cloned source. If upstream has drifted, the `git apply` step can fail
   > and the build aborts — **rebase the patch, don't delete it or remove the
   > `git apply` line from `inference/Dockerfile.v31`**, or you'll silently lose
   > the lens plumbing. See [TROUBLESHOOTING.md](TROUBLESHOOTING.md) "Rebuilding
   > llama.cpp for a new model architecture".

6. **Retrain the Geometric Lens for the new model.** The lens `C(x)` is
   dimension-coupled to the model's hidden size, so artifacts trained for one
   model won't load against another. `atlas lens check` reports the mismatch.
   Build the new model's *own* candidates and retrain — all through the CLI.
   Connectivity (ports, model file) resolves automatically from your
   deployment's config (`.env` on Docker, `atlas.conf` on K3s):
   ```bash
   # 1. Generate + self-label this model's solutions (hours on a large model).
   #    Results land in benchmark/results/<run-id>/v3_lcb/per_task/ (code + passed).
   atlas bench --run-id mymodel_lens --tasks 200

   # 2. Retrain C(x) on those candidates. --force overwrites existing
   #    artifacts (writes geometric-lens/geometric_lens/models/cost_field.pt)
   atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task

   # 3. ASA control vector; defaults to 75% of the loaded model's layer count
   atlas asa build
   ```
   (The candidate sandbox executes locally via a `python3` subprocess — only
   llama-server + geometric-lens are network dependencies. `atlas lens build`
   also writes a `provenance.json` manifest — dataset, sample counts, metrics,
   hyperparameters, per-file hashes — into every activated bundle.)

   **Per-model calibration.** The learned C(x) energy scale and G(x) score
   distribution are model-specific. `atlas lens build` therefore writes
   `model_identity.json` (the loaded model name and embedding dimension),
   `cx_normalization.json` (sigmoid midpoint/steepness derived from this
   model's PASS/FAIL energies) and `gx_thresholds.json` beside the weights.
   One model's grounded G(x) writes may cluster at 0.05, another's
   at 0.45, so a single hardcoded off-rails/regression cutoff fires for one
   model and never for another. A threshold file looks like:
   ```json
   { "off_rails": 0.15, "low": 0.30, "severe": 0.05 }
   ```
   The lens service loads it per-model and returns the values in every score
   response; the proxy uses them for its run-of-N / severe regression checks.
   `off_rails` is the per-token "stop generating" cutoff; `low` is the
   aggregate `gx_min` that counts as a low-quality write (run-of-2 → corrective);
   `severe` is the single-write cutoff that intervenes immediately. **If the
   file is absent or invalid, scores remain visible as uncalibrated telemetry
   but threshold-based intervention is disabled.** ATLAS never borrows another
   model's cutoffs. Calibrate them from the same labeled candidates used in
   step 2. The build uses the 5th, 10th, and 20th percentiles of this model's
   passing scores for `severe`, `off_rails`, and `low`, then writes
   `gx_thresholds.json`
   into `geometric-lens/geometric_lens/models/`. It publishes/downloads with the
   rest of the lens artifacts. (`atlas lens build` emits this file
   automatically, calibrated from the run's `pass` percentiles.)

   The Lens service also requires `model_identity.json` to match
   `ATLAS_MODEL_NAME`. Matching embedding dimensions alone are not sufficient:
   two different models can share a hidden width while having unrelated
   representation geometry. Missing or mismatched identity keeps Lens scoring
   unavailable and is surfaced by readiness, `atlas lens check`, and the TUI;
   the proxy refuses requests until it matches, because the lens is required.

   Do **not** reuse another model's solution set — both lens halves are
   dimension-coupled to the model: `C(x)` must learn *this* model's cost
   geometry, and `G(x)`'s PCA projection is shaped to the embedding width.
   `atlas lens build` trains both from the same samples in one run.
   After the build, restart the lens service so it loads the new artifacts:
   `docker compose restart geometric-lens`.

7. **Verify:** `atlas doctor` should come back green (lens dim now matches, model
   loads, e2e smoke passes).

> **Templating is not a per-model chore.** The chat template ships *inside* the
> GGUF and is rendered via llama-server's `--jinja` — you never hand-write one.
> For reasoning models, ATLAS sends `enable_thinking: false` and falls back to
> `reasoning_content` if a model emits its answer there, so most models work
> without any template work.

---

## 2. atlas-proxy

The Go proxy that runs the agent loop, routes tool calls, and orchestrates the ATLAS pipeline (llama-server + Lens + V3 + sandbox).

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_PROXY_PORT` | `8090` | Port to listen on |
| `ATLAS_INFERENCE_URL` | `http://localhost:8080` | llama-server endpoint for generation |
| `ATLAS_LLAMA_URL` | (falls back to ATLAS_INFERENCE_URL) | llama-server endpoint for grammar-constrained calls |
| `ATLAS_LENS_URL` | `http://localhost:8099` | Geometric Lens scoring endpoint |
| `ATLAS_SANDBOX_URL` | `http://localhost:30820` | Sandbox code execution endpoint |
| `ATLAS_V3_URL` | `http://localhost:8070` | V3 Pipeline service endpoint |
| `ATLAS_V3_TIMEOUT` | `300` | Interactive wall-clock cap (seconds) on a single V3 pipeline call from the agent path (`write_file` / `edit_file`). A call also never takes more than half of the session's remaining time. On timeout the proxy falls back to the model's own content (still syntax- and structural-gated) instead of hanging the session, and the run's final summary names each such file — bounds the long-tail Phase-3 repair stall (observed ~11 min on a 103-line write). The proxy sends the cap it applies to each call as `budget_ms`, and the v3-service plans every phase against it: when the remaining budget cannot afford one refinement iteration it skips straight to the fallback (`refinement_skip` event) instead of starting work the bridge would abandon. A caller that sends no cap gets the service's own reading of this value. Set `0` to disable the cap (uncapped behavior for offline bench runs). Measured on a 12B Q4 at 4 slots (2026-08-03, 28-session run): 11 of 29 write-class calls reached the cap, 33 of the 41 minutes spent in those calls. On that tier the cap is the common outcome rather than a long-tail guard, and each one discards a completed plan, its candidates and their sandbox verification. Raise it (or set `0`) when measuring what the pipeline produces; the interactive default trades that output for a bounded wait. |
| `ATLAS_MAX_READ_BYTES` | (derived) | Byte cap on a single `read_file` result (`readFileByteCap` in `proxy/tools.go`). Unset, the cap is half the per-slot context budget treated as a worst-case 1-token/char (dense content: G-code, minified JS, base64), clamped to [2 KB, 200 KB] — one read can never overflow the slot. Any positive integer overrides the derived value. A capped read reports the real `EndLine` of the shown range and records only the shown bytes for dedup. |
| `ATLAS_MODEL_NAME` | `local-model` | Neutral fallback request identifier; `/v1/models` reports llama-server's loaded model when available |
| `ATLAS_KEEP_LLAMA_WARM` | `1` | Set to `0` to disable the keep-warm goroutine that pings llama-server every 45s with a 1-token completion. Keeping warm avoids the cold-start path that fires after 1-2 min idle. Disable for CPU-only or tightly power-budgeted setups. |
| `ATLAS_FRESH_SLOT_PER_SESSION` | `1` | Set to `0` to disable per-session llama.cpp KV-slot erase. With it enabled (default), the proxy POSTs `/slots/<n>?action=erase` for every slot at the start of each agent loop invocation, giving each turn a clean cache. Retries are decided by what the status means: 408, 409, 425, 429 and 5xx other than 501 name a passing state (busy, loading, rate-limited, timed out) and are retried with a bounded pause; 400, 401, 403, 404, 501 and any other 4xx are the server's answer (no such slot or action, no slots endpoint or `--slot-save-path`, a refused key) and are not retried. Adds ~1-2s to the first turn but prevents cross-session token-state leakage (e.g. filenames hallucinated from prior sessions). |
| `ATLAS_CANDIDATE_CAPTURE_ONLY` | `off` | Research tool for an outcome-blind experiment on a **private** proxy process, not a product setting. When on, the delivery rule, the declared verification and the hard vetoes all run and are recorded, and the candidate grant is suppressed: nothing is minted, nothing is consumed, and no candidate byte reaches the task workspace. The recorded decision keeps its own answer (`candidate_authorized_strict` is not rewritten). Off by default; only `1`, `true`, `yes` or `on` enable it, and any other value fails closed to ordinary behaviour. Never set this on a deployed service, and never use it to measure the product. See [CANDIDATE_POLICY.md](CANDIDATE_POLICY.md). |
| `ATLAS_MAX_TURNS` | (unset) | Operator override for the agent-loop turn cap. Any positive int caps all tiers; unset / `0` / invalid falls through to tier defaults (T0=5, T1/T2/T3=uncapped). |
| `ATLAS_REASONING_BUDGET` | `6144` | Per-turn reasoning-token budget (estimated at 4 chars/token). When a generation accumulates this much `reasoning_content` without emitting any content tokens, the proxy cuts the stream and re-prompts. Bounds reasoning spirals. `0` disables. Forwarded to the proxy container by `docker-compose.yml`, so a `.env` setting takes effect on the Docker deployment. |
| `ATLAS_DRY_MULTIPLIER` | `0` (off) | DRY sampling strength. Off because every edit tool asks the model to *copy* text it can see in context, and DRY penalizes exactly that: the penalty is `multiplier × base^(matched − allowed_length)`, so at the default `0.8`/`1.75`/`6` a 12-token verbatim run carries a −23.0 logit penalty on the correct next token. `ATLAS_DRY_PENALTY_LAST_N` does not protect the copy, because llama-server's `init_sampler()` seeds the DRY ring buffer with the *prompt* tokens — the file the model is copying from is itself in the repetition window. Setting a non-zero value re-enables it; do that only for a model that degenerates into repetition loops and does not need to copy. |
| `ATLAS_DRY_BASE` | `1.75` | DRY penalty base (llama.cpp default). Only used when `ATLAS_DRY_MULTIPLIER` is non-zero. |
| `ATLAS_DRY_ALLOWED_LENGTH` | `6` | Longest sequence that may repeat unpenalized. Raised above llama.cpp's default of `2` because 3-token runs are ordinary in source. Only used when `ATLAS_DRY_MULTIPLIER` is non-zero. |
| `ATLAS_DRY_PENALTY_LAST_N` | `2048` | DRY lookback window (tokens). Note this does *not* exclude the prompt — see `ATLAS_DRY_MULTIPLIER`. Only used when `ATLAS_DRY_MULTIPLIER` is non-zero. |
| `ATLAS_TRANSCRIPTION_SAMPLER` | `1` | Greedy decoding for the agent loop: sends `samplers: ["top_k"]`, `top_k: 1`, which drops every other sampler from the chain. Agent turns are transcription, not prose — the tool call names a path and repeats a span of the file verbatim, and there is exactly one right answer for each token. Sampling from a distribution over near-identical tokens is what produces `scoreElement` for `scorerElement` and `hood.y` for `food.y`. This is sent instead of `temperature: 0` because temperature sits *last* in llama.cpp's sampler chain, so it would pick greedily from an already penalty-distorted distribution rather than from the raw one. Set `0` to restore the server's configured sampler chain. |
| `ATLAS_RESTATE_LAST_READ` | `1` | Appends the most recently read file, line-numbered, as a final user message immediately before the generation point. The `read_file` result the model copies from otherwise sits thousands of tokens back, behind the system prompt and every tool description, while the model's own emitted copy sits adjacent to the cursor — so its own drifting output is the closest match for what it is writing. Restating puts the source at the same distance. Skipped when the content is already the last message (right after `read_file`), when nothing has been read yet, and for files over 24 KB. Set `0` to disable. |
| `ATLAS_REPEAT_PENALTY` | `1.0` (off) | Classic repetition penalty. Off by default: it scores individual tokens and degrades code generation. Available for the pure repeated-whitespace degeneration that DRY's newline sequence-breaker cannot see. Setting any value other than `1.0` also sends `ATLAS_REPEAT_LAST_N`. |
| `ATLAS_REPEAT_LAST_N` | `64` | Lookback for `ATLAS_REPEAT_PENALTY`. Only sent when that penalty is enabled. |
| `ATLAS_PERMISSION_TIMEOUT_SEC` | `600` | Fail-safe timeout (seconds) on interactive permission requests (`proxy/permissions.go`). If no decision arrives at `POST /v1/permission` and the client neither disconnects nor cancels, the tool call is denied rather than hanging the turn. Compose forwards it from `.env` to the proxy container. |
| `ATLAS_AGENT_SESSION_TIMEOUT_SEC` | `600` | Total wall-clock budget for one agent request, in seconds, of which the last 30s are reserved for finalisation (`proxy/agent.go`, `sessionBudget`). Accepted between 120 and 3600; anything malformed, zero, negative or out of range falls back to the default and logs that it did. Raising it also raises the shutdown budget below, which is validated at startup. |
| `ATLAS_SHUTDOWN_GRACE_SEC` | `650` | How much grace the operator declares the environment will give the proxy after SIGTERM, in seconds (`proxy/main.go`, `shutdownBudget`). The proxy cannot read an orchestrator's real termination budget, so this is a declaration that is checked against the session configuration instead. It must be **strictly greater** than `ATLAS_AGENT_SESSION_TIMEOUT_SEC` + 10s of close-hook margin — at the defaults, 600 + 10 = 610 < 650. A configuration that does not fit refuses to serve at startup rather than running with a shutdown that cannot complete, and rather than quietly shortening the session the operator asked for. This value and the container's `stop_grace_period` (and any orchestrator termination grace) must be raised **together**: raising only this one leaves the process still draining when the runtime kills it, and raising only the runtime's grace leaves the proxy refusing to start. |
| `ATLAS_FENCED_FIRST_CONTENT_SEC` | `60` | Fenced file-content sub-call: seconds allowed before the first content token arrives (`proxy/agent.go`, `fencedFirstContentTimeout`). This budget also covers prompt evaluation, so a large context or a reasoning preamble on a slow or contended GPU may need more. When it expires the sub-call is cancelled. Accepted between 1 and 600; any other value falls back to the default and is logged. |
| `ATLAS_FENCED_IDLE_SEC` | `30` | Fenced file-content sub-call: seconds of complete silence allowed after content has started (`fencedIdleTimeout`). Same bounds and fallback as above. |
| `ATLAS_FENCED_STALL_SEC` | `25` | Fenced file-content sub-call: seconds allowed between the server opening the stream and the first content frame (`fencedStalledTimeout`). Same bounds and fallback as above. |
| `ATLAS_ALLOW_CREDENTIAL_READS` | unset | By default the agent's file tools keep known sensitive configuration files out of model context: `read_file` and `outline_file` refuse them, `search_files` skips them, `move_file` refuses to move them. Shell commands are not covered (they run in the sandbox, which mounts the workspace) and ask for approval instead. The files are (`.env`, `.env.*` except `.env.example`, `.netrc`, `.npmrc`, `.pypirc`, `*.pem`, `*.key`, SSH private keys, `.aws/credentials`, `.kube/config`, `.docker/config.json`, `secrets/service-token`, `secrets/api-keys.json`) — their contents would otherwise flow into prompts, logs, and session files. Set `1` to explicitly include them when you know a file is non-sensitive; the refusal message names this override. Compose forwards it from `.env` to the proxy container. |
| `ATLAS_GRAMMAR_MODE` | `strict` | Schema-constrained JSON sampling. Default `strict` ships the full tool-call schema in `response_format` so llama-server's C-side sampler converts it to internal GBNF and the token decoder can ONLY emit our `tool_call/text/done` union. Set to `loose` to send a `{"type":"json_object"}` payload instead (valid JSON, no shape enforcement). **`loose` is REQUIRED for Gemma-family models** — strict schema-GBNF makes them spam `done` instead of calling tools. The mode is a property of the model: the registry records it (`grammar_mode`; gemma is `loose`, the Qwen entries `strict`), `atlas init` writes it, and `atlas doctor` warns when `.env` disagrees with the registry for the configured model. |
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | Path to the ASA control-vector GGUF (the filename is intentionally stable — the registry SHA-pins it and the `.model` marker sits beside it — even though the tool it steers is now called `structural_edit`). The proxy reads this only for the `/v1/calibration/status` presence/marker probe; the vector itself is loaded by the llama-server entrypoint (see § 6 for `ATLAS_CONTROL_VECTOR_SCALE`, `_LAYER_RANGE`, `_ALLOW_UNVERIFIED` — those are entrypoint-consumed, not proxy). Compose forwards all four `ATLAS_CONTROL_VECTOR*` keys from `.env` to the llama-server container, which is what loads the vector; the proxy container keeps its in-code default path for the probe. |
| `ATLAS_WORKSPACE_DIR` | (proxy's container `/workspace`) | Working-dir override that the proxy substitutes for the TUI-supplied `working_dir` field. Set inside the container so file tools always resolve under `/workspace` regardless of what the client sends. |
| `ATLAS_VERIFY_IN` | `sandbox` | Where `run_command` and the V3 verify path execute: `sandbox` (default) routes through the sandbox container; `host` runs commands directly on the proxy host (only safe when the proxy itself is local, not containerized). Per-project override: `[execution] target = "host"` in `.atlas/config.toml`. |

### Internal Settings (not configurable via env)

| Setting | Value | Description |
|---------|-------|-------------|
| Max turns (T0 Conversational) | 5 | Text-only chat — shape constraint, not runaway protection |
| Max turns (T1 / T2 / T3) | `0` (uncapped) | The 8 stuck-pattern detectors (parse-error, tool-repeat, reasoning-repeat, lens-regression, exploration-budget, path-aware error-loop, action-gate, verification-gate) are the safety net. Operator can re-cap any tier with `ATLAS_MAX_TURNS=<n>`. |
| Exploration budget warning | 4 consecutive reads | Injects "write your changes now" |
| Exploration budget skip | 5+ consecutive reads | Skips the read, returns warning |
| Error loop breaker | 3 consecutive failures on the **same path** | Path-aware — same `(tool, path)` 3× breaks the loop; rotating failure paths do not trip it |
| T2 trigger (V3 activation) | `lines ≥ 10` AND (`hasLogicIndicators` ≥ 2 family matches OR known code/markup extension) | `classifyFileTier` in `proxy/tools.go`. Config files / data exts / styles / prose / shell scripts always T1; under 10 lines always T1; recognized code/markup extensions auto-T2 even without logic-indicator matches. |
| write_file rejection | Existing files > 5 lines | Forces `structural_edit` (whole node, .py/.html/.htm) or `edit_file` (surgical). Skipped when the existing file looks corrupted on disk (self-heal). |
| Session file manifest | Fires on the write that takes the session's created-file set to ≥ 2, once per new file | `[system note]` listing every file the session created, so later files reference earlier ones (`render_template`, `<script src>`) instead of re-creating or inlining them. The same set is merged into V3's `project_context`, which otherwise only carries files the model has *read*. `proxy/context.go`. |
| Asset-graph lint | After each successful write/edit/move/delete; advisory, deduped, skipped for projects > 400 files | Cross-file coherence findings as `[system note]`s + an `asset_lint` SSE event: templates no `render_template` names, orphaned `.js`/`.css` in `static/` OR flat layouts, `src`/`href`/`url_for` targets that don't exist, `render_template`/`{% extends %}` names with no template file, and `fetch()`/form-action URLs no Flask route matches. Never blocks a write or a `done`. `proxy/gates.go`. |
| Fallback syntax gate | `edit_file`; the `write_file` iteration fast-path, V3-error fallback, and V3-winner baseline check (NOT the T0/T1 direct path — see the structural gate row) | Routes the would-be content through the sandbox `/syntax-check`; confirmed-unparsable content is rejected instead of landing on disk with `success=true` (truncated tool calls). Fail-open when the sandbox is unreachable or the type unsupported. `edit_file` and the iteration fast-path block only healthy→broken transitions (repairs of already-broken files, and content the strict checker rejects both before and after such as multi-doc YAML or JSONC, proceed). `proxy/gates.go`. |
| Structural gate (#147) | Every `.py` write path: `edit_file`, `structural_edit`, and all `write_file` branches (V3 winner, V3-error fallback, iteration fast-path, T0/T1 direct) | Resolves the composed post-edit file through v3-service `POST /internal/structural_check` and refuses a write that INTRODUCES an unresolved direct-identifier call — a call the file neither imports, defines, binds, nor gets from builtins would raise `NameError` at runtime while parsing fine. Same healthy→broken rule as the syntax gate (a pre-existing unresolved name mid-repair is allowed; an uncheckable original — transient service failure, unreadable file — fails open). A vetoed V3 winner falls back to the model's own gate-passing baseline instead of rejecting (the offending call is V3-authored). Python-only; fail-open when v3-service is unreachable or tree-sitter unavailable. `project_context` is the session's read and written `.py` files (truncated to 4 KB each) minus the edited file's own pre-edit body. It runs on every request. `proxy/gates.go`. |
| Honesty gates & permission modes | verification / action / claim-check gates run in **every** mode including `yolo` | Yolo skips permission prompts, not completion checks; bounces stay capped by `maxGateBounces` so unattended runs can't loop. |
| Suspicious-shrinkage guard | `oldSize ≥ 100B` AND `newSize < 64B` | Rejects writes that replace a non-trivial file with a stub (doctype-only / mid-output cut). See `validateNotSuspiciouslyShrunk`. |
| Per-step grammar gate | Trigger: write_file rejection on existing .py/.html/.htm > 5 lines | Bans `edit_file` and `write_file` from GBNF tool-name production for next decision |
| ASA control vector | Model-gated `/models/ast_edit_steering.gguf` | Activates `--control-vector-scaled` only when the adjacent `.model` marker matches `ATLAS_MODEL_NAME`; a stale vector from another model stays disabled. |
| Conversation trim | Trigger: conversation exceeds the token budget | Keeps `system + most-recent user message (pinned) + a token-budgeted tail` (`budgetedKeepLast` / `trimMessages` in `proxy/agent.go`). The tail is sized to the per-slot budget (see `ATLAS_AGENT_HISTORY_BUDGET`, § 1), floored at 8 kept messages. The pin is the most-recent `role=="user"` message so long tool-call chains don't push the user's task off the tail. |
| Command stdout limit | 8,000 chars | Prevents context flooding |
| Command stderr limit | 4,000 chars | Prevents context flooding |
| Search results limit | 200 matches | Prevents context flooding |
| File search skip | Files > 1 MB | Performance |
| max_tokens | 8,192 (override via `ATLAS_MAX_TOKENS`) | Per-turn generation ceiling sent to llama-server |
| temperature (agent loop) | 0.3 default; 0.7 on retry after a stuck-loop nudge | Sent to llama-server. Per-service — V3 (§ 3) uses its own default. |

### Stuck-pattern detectors

These 8 safety detectors run in place of a per-tier turn cap. Each fires independently — first match breaks the loop or injects a corrective.

| Detector | Threshold | Source |
|----------|-----------|--------|
| Tool-call repetition | Same `(tool, args)` signature `3×` within the last `8` calls. `write_file` signatures key on the target path alone — a whole-file rewrite of the same path counts as repetition even when each attempt's content differs; `edit_file`/`structural_edit` keep full-args signatures so distinct surgical edits don't trip it. | `proxy/detectors.go` (`toolRepeatThreshold=3`, `toolRepeatWindow=8`) |
| Reasoning repetition | Same reasoning snippet `2×` consecutive turns | `proxy/detectors.go` (`reasoningRepeatThreshold=2`) |
| Lens regression | `gx_score_min` runs `2` consecutive turns below the selected artifact's `low` threshold, OR one turn below its `severe` threshold; disabled if uncalibrated | `proxy/lens.go`, `gx_thresholds.json` |
| Exploration-budget | 4 consecutive read-only calls → nudge; 5+ → skip | `proxy/agent.go` |
| Path-aware error-loop | 3 consecutive failures on the **same** path (rotating paths don't trip) | `proxy/agent.go` (`consecutiveErrors >= 3` + path match) |
| Action gate | Turn emits `done` but the user prompt has action-intent and no successful write/edit/structural_edit fired this loop | `proxy/agent.go` (action_gate) |
| Verification gate | Turn emits `done` after a fix-intent prompt, or after a failed run, with no passing verification run since; or a work request's code has no current run showing it works. A parse, lint, build or a run whose exit status the command hides does not count | `proxy/agent.go` (verification_gate), `proxy/command_evidence.go` |
| Claim-check gate | `done` summary makes universal claims (`works perfectly`, `tested all routes`) without backing evidence, OR the prompt asks for multi-issue work | `proxy/agent.go` + `proxy/gates.go` |

### Plan-mode auto-revision (plan_adherence)

| Setting | Value | Description |
|---------|-------|-------------|
| `planAutoReviseThreshold` | `5` | Consecutive off-plan tool calls before the proxy regenerates the plan |
| `planMaxRevisions` | `2` | Hard cap on auto-revisions per loop (prevents revision oscillation) |

### Hard-blocked patterns (`proxy/permissions.go`)

These checks (`denyCommandReason` / `denyWritePathReason`, dispatched from `shouldDenyToolCall` in `proxy/permissions.go`) run BEFORE the per-tool permission gate, so `yolo` mode does not bypass them.

| Tool | Pattern | Behavior |
|------|---------|----------|
| `run_command` | `rm -rf /`, `rm -rf /*`, `mkfs*`, `dd … of=/dev/*` | Reject — host-destroying commands |
| `write_file` / `edit_file` / `structural_edit` / `move_file` (destination) | `.env`, `*.pem`, `*.key`, `*credentials*` | Reject — model can't accidentally clobber secret files |

Command patterns are anchored to the command position of each shell segment (prefixes like `sudo`/`env` are skipped), so only the destructive form is blocked — `grep mkfs notes.txt` or `dd of=out.bin` pass. Write-path patterns match on the base name: `.env` requires the file to be named exactly `.env` (`app/.env` is caught; `.env.local` and `.env.example` are not), `*credentials*` matches any base name containing `credentials`.

### Shell-mutation gate

`run_command` runs a real shell inside the sandbox container, which is the actual safety boundary: read-only rootfs, `no-new-privileges`, and only the project dir bind-mounted writable at `/workspace` (cwd is forced under `/workspace`), so the model cannot touch the host — the blast radius of any command is the project folder. Given that, `validateShellCommand` blocks **only catastrophic commands**: whole-project/root wipes (`rm -rf /`, `rm -rf .`, `rm -rf *`, `rm -rf /workspace`), `find -delete` / `-exec rm` (recursive from the search root), fork bombs, and device/filesystem destruction (`dd of=/dev/…`, `mkfs`, `wipefs`, redirect onto a block device). `bash -c "…"` / `eval "…"` are unwrapped one layer so a wrapped catastrophic command can't slip through. Ordinary file management — `mv`, `cp`, `mkdir`, `rm <file>`, `rm -rf <named-subdir>`, `chmod`, `sed -i`, `>` redirects — runs freely. Content edits are still *nudged* toward `write_file`/`edit_file`/`structural_edit` (where V3 + the lens add value) via the system prompt, but are no longer hard-refused at the shell. `move_file` remains available as a structured move/rename with clobber-protection.

| Pattern | Reaction |
|---------|----------|
| Recursive `rm` of a root / glob-everything target (`/`, `.`, `*`, `~`, `..`, `$HOME`, `/workspace`, or their `/*` variants) | Reject — targeted recursive deletes (`rm -rf build`, `rm -rf node_modules`) run freely |
| `find … -delete` or `find … -exec rm` | Reject — deletes recursively from the search root |
| Fork bomb (function whose body pipes to itself and backgrounds, then self-invokes) | Reject |
| `mkfs*` / `wipefs`, `dd … of=/dev/…`, or a `>` redirect onto a block device (`/dev/sd*`, `/dev/nvme*`, …) | Reject — device/filesystem destruction |
| `bash -c "…"` / `sh -c "…"` / `zsh -c "…"` / `dash -c "…"` / `ksh -c "…"` / `eval …` | Unwrapped one layer; the inner command is re-checked against the rules above |

These checks are enforced regardless of permission mode — `yolo` does NOT bypass them.

### Symbol indexing (per-session startup)

`proxy/context.go` scans the project once per `/v1/agent` session to seed a symbol → file map so the planner can resolve names like `dashboard` to `app/dashboard.py` without re-reading the tree on every turn.

| Setting | Value | Description |
|---------|-------|-------------|
| `projectScanMaxFiles` | `50` | Max `.py` files read during the scan |
| `projectScanMaxBytes` | `500 KB` | Total source-byte budget across all scanned files |
| `projectScanTimeout` | `5 s` | Round-trip cap on the v3-service call; falls back to "no injection" on timeout |
| `symbolMaxCandidates` | `10` | Max symbols extracted from a user message before regex-order truncation |

### Event broker (`/events` SSE)

| Setting | Value | Description |
|---------|-------|-------------|
| `subscriberBuffer` | `256` | Per-subscriber channel buffer. Slow consumers start dropping events once the buffer fills. |

---

## 3. V3 Pipeline Service

Python HTTP service that orchestrates the V3 code generation pipeline (PlanSearch, DivSampling, Budget Forcing, PR-CoT, etc.).

### Environment Variables

Inter-service URLs (`ATLAS_INFERENCE_URL`, `ATLAS_LENS_URL`,
`ATLAS_SANDBOX_URL`) resolve as in § 2. This table lists only vars unique to
the V3 service.

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_V3_PORT` | `8070` | Port to listen on |
| `ATLAS_MODEL_NAME` | `local-model` | Neutral fallback request identifier; deployments pass the selected model explicitly |
| `ATLAS_PLAN_THINKING` | `0` | Enable template-level reasoning during V3 plan generation for models that support it. `0` keeps planner `max_tokens=2048`; `1` raises it to `8192` so reasoning does not consume the plan JSON budget. |
| `ATLAS_V3_TELEMETRY_DIR` | `/data/telemetry` | Container-side directory for live-pipeline stage telemetry: the same per-stage `*.jsonl` files the bench runner writes (`plan_search_events.jsonl`, `pr_cot_events.jsonl`, …) plus one `pipeline_summary.jsonl` line per task (task id, phases run with outcome and duration, veto events, final `phase_solved`). Compose mounts the `v3-telemetry` named volume at the default path so lines survive container restarts. Set `0`/`off`/`none` to disable. Fail-soft: an unwritable directory disables telemetry and never affects generation. |

### Internal Constants

| Setting | Value | Description |
|---------|-------|-------------|
| BASE_TEMPERATURE | 0.6 | Default V3 generation temperature (per-service; the agent loop in § 2 uses 0.3) |
| DIVERSITY_TEMPERATURE | 0.8 | Temperature for diverse candidate sampling |
| MAX_TOKENS | 8,192 | Max output tokens per generation call |
| PlanSearch plans | 3 (max 7) | Number of structural plans generated |
| DivSampling perturbations | 12 | 4 roles + 4 instructions + 4 styles |
| Budget Forcing tiers | 5 | nothink (0), light (1024), standard (2048), hard (4096), extreme (8192) |
| PR-CoT perspectives | 4 | logical_consistency, information_completeness, biases, alternative_solutions |
| PR-CoT max rounds | 3 | Maximum repair attempts |
| Refinement max iterations | 2 | Maximum refinement cycles |
| Refinement time budget | 120s | Maximum time for refinement loop |
| Constraint min cosine distance | 0.15 | Prevents hypothesis repetition |

---

## 4. Geometric Lens

Python FastAPI service for C(x)/G(x) scoring (`/internal/lens/*`). Every route it serves is internal to the stack; the proxy and v3-service are its only callers.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GEOMETRIC_LENS_ENABLED` | — | Removed. The lens has no off switch: ATLAS requires it ([ADR 0011](adr/0011-the-lens-is-required.md)). The service ignores the variable. |
| `LLAMA_URL` | `http://llama-server:8080` | llama-server endpoint. Read by `config.py:LlamaConfig` and also by `embedding_extractor.py` as the embedding source. |
| `LLAMA_EMBED_URL` | (falls back to `LLAMA_URL`) | Dedicated embedding endpoint. Use this if you have a separate embedding server; otherwise embeddings reuse the LLAMA_URL host. |
| `LLAMA_EMBED_CAPACITY_TOKENS` | (unset) | The embedding server's physical batch (`-ub`): the longest input one Lens score can be computed from. Docker Compose sets it from `ATLAS_UBATCH`, the value llama-server runs with, so `/health` reports the capacity before any request is refused; a refusal's own count replaces it. Information only. An input past the capacity is reported `unscored` (typed, with the server's counts), never truncated or split; see [ADR 0010](adr/0010-lens-capacity-boundary-is-typed.md). Values that are not positive integers are ignored. |
| `ATLAS_ALLOW_PICKLE_GX` | unset | Opt-in for loading the legacy `gx_xgboost.pkl` G(x) format. Unpickling executes arbitrary code, so the service refuses `.pkl` by default and asks for the JSON export (`gx_xgboost.json`). Set to `1` once to load an old bundle, then re-export. |

### Scoring Model Parameters

| Parameter | Value | Description |
|-----------|-------|-------------|
| C(x) sigmoid midpoint | Per model | `cx_normalization.json`; energy value mapping to 0.5 |
| C(x) sigmoid steepness | Per model | `cx_normalization.json`; derived from PASS/FAIL separation |
| G(x) `low` threshold | Per model | `gx_thresholds.json`; separates likely-correct from uncertain |
| G(x) `severe` threshold | Per model | `gx_thresholds.json`; below this is likely incorrect |

Missing or invalid calibration never falls back to another model's values.
C(x) reports a neutral normalized score and G(x) reports `uncalibrated`;
threshold-based intervention remains disabled while raw telemetry stays visible.

---

## 5. Sandbox

Python FastAPI service for isolated code execution with compilation, linting, and testing.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MAX_EXECUTION_TIME` | `60` (in-code); `300` in the Compose stack | Maximum execution time in seconds. Compose sets it from `ATLAS_SANDBOX_MAX_EXECUTION_TIME` (default 300) so per-call limits match the proxy's `run_command` cap. |
| `WORKSPACE_BASE` | `/tmp/sandbox` | Base directory for execution workspaces |
| `ATLAS_SANDBOX_WORKSPACE_ROOT` | `/workspace` | The bind-mounted project root the executor confines `cwd` to. Container deployments keep the default; the E2E acceptance test overrides it to run the executor on the host under a temp dir. |
| `ATLAS_SANDBOX_UID` / `ATLAS_SANDBOX_GID` | `1000` | Runtime uid/gid of the (non-root) sandbox container. `atlas init` writes the invoking user's ids so `/workspace` writes keep the right owner — the container drops all capabilities, so ownership must match. |
| `ATLAS_PROXY_UID` / `ATLAS_PROXY_GID` | `1000` | Runtime uid/gid of the atlas-proxy container — same rationale as the sandbox ids: the proxy writes the `/workspace` host bind mount for file tools, and the image's baked-in non-root user (uid 1001) can't write operator-owned dirs. `atlas init` writes the invoking user's ids. |
| `ATLAS_SANDBOX_CPUS` | `2` (compose fallback); `atlas init` writes ~75% of host cores | CPU quota for the sandbox container so a runaway build can't starve llama-server. The compose fallback is a conservative `2` so a raw `docker compose up` is never unlimited; `atlas init` writes ~75% of host cores (floor 1) — the recommended setting. `0` removes the cap (unlimited — not recommended). |
| `ATLAS_SERVICE_TOKEN_FILE` | `/run/atlas-secrets/service-token` (containers) / `<root>/secrets/service-token` (host) | Path to the per-installation internal-auth token. Generated by `atlas init` (0600); mounted read-only into every Docker-Compose service. Present => all services require `Authorization: Bearer <token>` on non-health routes and clients inject it automatically. Absent => auth disabled (pre-token behavior; doctor warns). The K3s templates do not mount the token, so internal auth is disabled on K3s (doctor warns there too). Rotate with `atlas init --rotate-token` + `docker compose restart`. The token value itself never goes in `.env`, argv, or logs. |
| `ATLAS_SECRETS_DIR` | `./secrets` | Host dir mounted read-only at `/run/atlas-secrets` in every service (holds `service-token`; the lens api-keys mount is separate). |
| `ATLAS_LOG_FORMAT` | `line` | `json` emits one structured JSON object per log line (ts, level, service, request_id, invocation_id, msg) across proxy/v3/lens/sandbox. On v3-service `invocation_id` names the single `/v3/generate` or `/v3/plan` call the record belongs to: one request id can cover several invocations (the planner and the generation pipeline share it, and a plan revision adds another), so joining a log line to a specific pipeline run needs both. PlanSearch's worker threads carry both identities explicitly, since a thread inherits neither; `line` is the human-readable default. Both pass through the private-value filter. Compose forwards it from `.env` to all four service containers; `atlas config validate` recognizes the key. |
| `ATLAS_TRUST_MODE` | `trusted` | `untrusted` refuses command execution (`run_command` and `run_background`); `trusted` = sandbox execution; `fully-trusted` permits host execution. Unrecognized → `trusted`. Read by the proxy; compose forwards it from `.env` to the proxy container. |
| `ATLAS_SANDBOX_NET_INTERNAL` | `false` | Set `true` to make the sandbox's network internal: executed code loses all egress while proxy/v3/lens still reach the executor. Caveats: the `127.0.0.1:30820` host publish goes dead (doctor's direct probe fails; use `docker compose exec sandbox curl localhost:8020/health`), and dependency-installing verification (`pip install`, `npm install`) fails by design. |
| `ATLAS_SHELL_SNAPSHOT_MAX_FILES` | `20000` | File-count cap when `/shell` copies a bounded workspace snapshot into tmpfs for overlay-file runs (V3 candidate testing without touching real files). Exceeding the cap fails the snapshot with a structured error. |
| `ATLAS_SHELL_SNAPSHOT_MAX_BYTES` | `268435456` (256 MB) | Total-bytes cap on the same workspace snapshot. |
| `ATLAS_SHELL_SNAPSHOT_MAX_FILE_BYTES` | `16777216` (16 MB) | Per-file size cap — larger files are skipped (logged), not fatal. |

### Internal Limits

| Setting | Value | Description |
|---------|-------|-------------|
| Default timeout per request | 30s | Can be overridden per request up to `MAX_EXECUTION_TIME` |
| `/shell` stdout truncation | 4,000 chars | Last N chars kept |
| `/shell` stderr truncation | 2,000 chars | Last N chars kept |
| `error_message` truncation | 500 chars | First N chars kept on `/execute` failures |
| Timeout response | `Execution timed out after Ns` | `/execute` timeout returns `success: false`, `returncode: -1`, this message as `stderr`, and empty `stdout` |
| Supported languages | 12 | python, javascript, typescript, go, rust, c, cpp, bash, html/htm, xml, json, yaml/yml |

### Background process tools

`run_background` / `tail_background` / `stop_background` are sandbox-only — they require `ATLAS_VERIFY_IN=sandbox` (the default).

| Setting | Value | Description |
|---------|-------|-------------|
| `BG_MAX_LINES` | `500` | Ring-buffer size per stream (stdout / stderr) per job |
| `BG_MAX_JOBS` | `32` | Hard cap on concurrent background jobs |
| `BG_RETENTION_SEC` | `600` | How long finished jobs stay queryable via `tail_background` before reaping (10 min) |
| `BG_MAX_AGE_SEC` | `7200` | Still-running jobs abandoned this long (2 h) are killed (whole process group) so leaked servers can't exhaust the container's PID limit |

### Workspace paths

| Mount | Path | Source | Purpose |
|-------|------|--------|---------|
| Workspace bind-mount | `/workspace` | Host `${ATLAS_PROJECT_DIR}` (or `${ATLAS_PROJECTS_DIR}` under K3s) | Persistent, user-visible. `run_background`, `/shell`, and project-context file lookups all see this. |
| Execute tmpfs | `WORKSPACE_BASE` (default `/tmp/sandbox`) | Per-request `tempfile.mkdtemp` | Ephemeral, per-`/execute` call. The universal tmpfs sandboxes language toolchains run here. |

---

## 6. llama-server

C++ inference server (llama.cpp) with CUDA GPU acceleration and grammar-constrained JSON output.

Both Docker Compose and K3s use the same image with the same entrypoint (`inference/entrypoint-v3.1.sh`), so the flag set is identical. The only differences are which env vars feed the entrypoint and what default value each deployment passes for them.

### Entrypoint env vars (read by `entrypoint-v3.1.sh`)

| Env var | Docker default | K3s default | Description |
|---------|----------------|-------------|-------------|
| `MODEL_PATH` | `/models/${ATLAS_MODEL_FILE}` | `/models/${ATLAS_MAIN_MODEL}` | GGUF path inside the container |
| `PORT` | `8080` | `${ATLAS_LLAMA_PORT}` (defaults to `8080`) | Listen port |
| `CONTEXT_LENGTH` | `${ATLAS_CTX_SIZE:-131072}` | `${ATLAS_CONTEXT_LENGTH}` (atlas.conf default `16384`) | Context window in tokens, TOTAL across all slots. Size per model + GPU with `atlas tier fit --write`. When the env var is entirely absent (running the entrypoint outside compose/K3s), the script's own fallback is `163840`. |
| `PARALLEL_SLOTS` | `${ATLAS_PARALLEL_SLOTS:-${PARALLEL_SLOTS:-4}}` (compose default `4`) | `${ATLAS_PARALLEL_SLOTS}` (atlas.conf default `1`) | Concurrent request slots. Compose defaults to `4` because the `/demo` split-pane runs V3 (which fans out into 3 parallel PlanSearch candidates) alongside a base-agent session — 4 total concurrent inferences. The nested name is a `.env` fallback. |
| `KV_CACHE_TYPE_K` | `${ATLAS_KV_TYPE_K:-${KV_CACHE_TYPE_K:-f16}}` | `f16` (entrypoint default) | KV cache key quantization (`f16`, `q8_0`, `q4_0`). Set by `atlas tier fit --write`; the nested name is a fallback. |
| `KV_CACHE_TYPE_V` | `${ATLAS_KV_TYPE_V:-${KV_CACHE_TYPE_V:-f16}}` | `f16` (entrypoint default) | KV cache value quantization. Set by `atlas tier fit --write`; the nested name is a fallback. |
| `UBATCH_SIZE` | `${ATLAS_UBATCH:-1024}` | `1024` (entrypoint default) | Micro-batch size (`-ub`). Drives the compute-buffer VRAM cost (~ubatch × n_embd × 280 bytes). Set by `atlas tier fit --write`. |
| `BATCH_SIZE` | `${ATLAS_BATCH:-1024}` | `1024` (entrypoint default) | Logical batch size (`-b`). Normalized to `UBATCH_SIZE` when larger because self-embeddings require `n_batch <= n_ubatch`. Set by `atlas tier fit --write`. |
| `SLOT_SAVE_PATH` | `/tmp/slots` | `/tmp/slots` | Slot-save directory used by `/slots/0?action=save` |
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | same | ASA control-vector path (requires matching `.model` marker). Not forwarded from `.env` by compose — the entrypoint default applies on Docker; the three knobs below *are* forwarded. |
| `ATLAS_CONTROL_VECTOR_SCALE` | `0.5` | same | Scale applied to the control vector |
| `ATLAS_CONTROL_VECTOR_LAYER_RANGE` | (unset → all layers) | same | Optional layer restriction — two space-separated integers, e.g. `"24 30"`, passed to `--control-vector-layer-range` |
| `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED` | `0` | same | Emergency opt-out of the `.model` marker gate. Setting `1` can apply an incompatible vector and is not recommended. |
| `ATLAS_GPU_INDEX` | (unset → all GPUs visible) | — | GPU selection on multi-GPU hosts. Mapped to `CUDA_VISIBLE_DEVICES` (or the HIP/Vulkan equivalent); the export is skipped when empty. |

### Effective llama-server flags

The entrypoint always launches with this flag set (regardless of deployment mode):

| Flag | Value | Description |
|------|-------|-------------|
| `-m` | `$MODEL_PATH` | Model path |
| `-c` | `$CONTEXT_LENGTH` | Context window |
| `-ctk` / `-ctv` | `$KV_CACHE_TYPE_K` / `_V` | KV cache quantization (default `f16` / `f16`) |
| `--parallel` | `$PARALLEL_SLOTS` | Concurrent request slots |
| `--cont-batching` | — | Continuous batching |
| `-ngl` | `99` | Offload all GPU layers |
| `--fit off` | — | Refuse to start if the model + KV + compute buffers don't fit in VRAM (instead of silently demoting layers to CPU at 5× slower generation). Size the budget with `atlas tier fit --write`. |
| `--host` | `0.0.0.0` | Listen on all interfaces |
| `--port` | `$PORT` | Listen port |
| `--flash-attn` | `on` | Flash attention |
| `--mlock` | — | Lock model in RAM (prevents swapping) |
| `-b` / `-ub` | `$BATCH_SIZE` / `$UBATCH_SIZE` | Batch / micro-batch size (defaults `1024` / `1024`; batch cannot exceed micro-batch while embeddings are enabled) |
| `--slot-save-path` | `$SLOT_SAVE_PATH` | Where llama-server persists slot state |
| `--ctx-checkpoints` | `0` | Disable context checkpoints |
| `--embeddings` | — | Enable self-embedding endpoint (lens C(x)/G(x) needs this) |
| `--jinja` | — | Jinja chat-template support |
| `--control-vector-scaled` | `$ATLAS_CONTROL_VECTOR:$ATLAS_CONTROL_VECTOR_SCALE` | Added only when the vector exists and its `.model` marker matches the selected model |

Prompt caching stays enabled so each agent-loop turn reuses its encoded prefix; cross-session isolation comes from the proxy erasing the KV slot at session start (`ATLAS_FRESH_SLOT_PER_SESSION`, § 2), not from disabling the cache.

> **Note:** The Docker entrypoint and the K3s entrypoint are the same script. The only practical knobs that diverge are `CONTEXT_LENGTH` (Docker defaults `131072` via `ATLAS_CTX_SIZE`; K3s defaults `16384` via `ATLAS_CONTEXT_LENGTH`) and `PARALLEL_SLOTS` (Docker compose defaults `4` via `ATLAS_PARALLEL_SLOTS` to support `/demo` split-pane plus V3 plan-search fanout; K3s defaults `1` via `atlas.conf`). The runtime-sizing keys (`ATLAS_KV_TYPE_K/V`, `ATLAS_UBATCH`, `ATLAS_BATCH`) are compose-only; on K3s the entrypoint defaults apply unless set in the deployment manifest.

---

## 7. Python CLI

The standalone Python CLI (`pip install -e . && atlas`) reads these variables. Service URLs resolve in order: explicit URL env var → the corresponding `ATLAS_*_PORT` key (shell env, then the checkout's Docker `.env`) → built-in default — so on a Docker install with non-default ports, bare `atlas` commands work without any `ATLAS_*_URL` exports.

The inter-service URLs (`ATLAS_INFERENCE_URL`, `ATLAS_LENS_URL`,
`ATLAS_SANDBOX_URL`, `ATLAS_V3_URL`) resolve as in § 2, with the
port-fallback chain described above. This table lists only vars unique to the
CLI. TUI-only variables (`ATLAS_TUI_LOG`, `ATLAS_TUI_MOUSE`,
`ATLAS_TUI_STARTUP_NOTE`) are documented in [CLI.md](CLI.md)'s environment
table; bootstrap-only knobs (`ATLAS_BOOTSTRAP_*` and friends) in
[SETUP.md](SETUP.md).

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_PROXY_URL` | `http://localhost:${ATLAS_PROXY_PORT:-8090}` | atlas-proxy endpoint (used by `atlas doctor` and honored by the TUI as its `--proxy` default) |
| `ATLAS_RAG_URL` | (unset) | Deprecated spelling of `ATLAS_LENS_URL`, still honoured by `atlas/client.py` so an existing `.env` keeps working. `ATLAS_LENS_URL` takes precedence. Prefer `ATLAS_LENS_URL`. |
| `ATLAS_AUTO_WORKSPACE` | `1` | On TUI launch, the CLI checks whether the Docker proxy/sandbox `/workspace` binds cover your cwd and recreates those containers pointed at your project when they don't. Set `0` to keep whatever bind the containers already have. |
| `ATLAS_MODELS_DIR` | `./models` | Host directory holding GGUF model files (used by `atlas doctor` and `atlas model`). |
| `ATLAS_MODEL_FILE` | **required** | Selected model filename inside `ATLAS_MODELS_DIR`. |
| `ATLAS_LENS_MODELS` | `./geometric-lens/geometric_lens/models` | Host path that maps to the lens's weight directory. Used by `atlas doctor` so it checks the same directory Docker bind-mounts into the lens container. |
| `ATLAS_MODEL_NAME` | `local-model` | Neutral fallback request identifier; normal installs set the selected model |
| `HF_TOKEN` | (unset) | HuggingFace write token used by `atlas lens publish` / `atlas asa publish` for artifact upload. Get one at https://huggingface.co/settings/tokens (scope: write). `HUGGINGFACE_HUB_TOKEN` and `HUGGING_FACE_HUB_TOKEN` are also honored. Full walkthrough: [PUBLISHING.md](PUBLISHING.md). |
| `ATLAS_BACKEND` | `cuda` (default) / `rocm` / `vulkan` / `metal` | Which llama-server build dispatch path is active. Written by `atlas init` based on GPU vendor (or `--backend vulkan` override). The entrypoint reads this to pick vendor-specific runtime flags. `vulkan` is the universal fallback — ~20–40% slower than tuned native backends but covers AMD/Intel/Snapdragon/Apple-via-MoltenVK/CPU with one image. `metal` is the macOS hybrid (native llama-server + Docker for the rest, [SETUP_MACOS.md](SETUP_MACOS.md)). See [SETUP.md § Vulkan](SETUP.md). |
| `ATLAS_VK_DEVICE_SELECT` | (unset → first Vulkan ICD enumerated) | Vulkan-only: forwarded to `MESA_VK_DEVICE_SELECT` to pin a specific physical device when multiple ICDs are visible (e.g., dGPU + iGPU, two Intel Arc cards). Format: `"vendorID:deviceID"` (hex) or a device-name substring. Use `GGML_VK_VISIBLE_DEVICES` (numeric index) instead when the Mesa selector isn't granular enough. |
| `ATLAS_GPU_VENDOR` | (auto-detected) | Process-env override read by `atlas init` / `atlas tier` on multi-vendor hosts: `nvidia`, `amd`, `apple`, `intel`. Auto-detect picks the largest-VRAM GPU. Not read from `.env`. |
| `ATLAS_UPSTREAM_REPO` | `inferstep/ATLAS` | GitHub repo that `atlas lens publish` / `atlas publish` open the registry PR against. Override to target a fork. |
| `ATLAS_PARALLEL_TASKS` | `4` (worker count) | Benchmark runner: number of tasks `atlas.bench.v3_runner` processes concurrently (default `4`; the runner's per-call timeout scaler reads the same var with default `1`). `atlas bench` pins it to `1` — the safe setting for any model; export a higher value only when driving `atlas.bench.v3_runner` directly on hardware that can take it. |
| `ATLAS_LLM_PARALLEL` | `0` | Benchmark runner: set `1` to allow concurrent llama-server calls instead of serialized generation. |
| `ATLAS_API_KEYS_PATH` | `./secrets/api-keys.json` | Read by the TUI to locate the bearer-token file written by `atlas init`; the token is attached to `/v1/agent` requests for forward compatibility (the proxy does not currently enforce it). |
| `ATLAS_UPGRADE_PULL_TIMEOUT` | `3600` | Timeout (seconds) `atlas upgrade` allows for pulling the target images before the staged upgrade aborts and restores the previous release. |
| `ATLAS_UPGRADE_SKIP_VERIFY` | (unset) | Set `1` to skip the keyless cosign signature verification `atlas upgrade` runs on each target image before applying. |

The CLI sends no generation parameters of its own beyond `atlas.client.chat_stream`'s defaults (`max_tokens=8192`, `temperature=0.6`); interactive generation runs through the proxy's agent loop (§ 2) and the TUI. No stop sequences are sent anywhere — llama-server applies the GGUF's own chat template (`--jinja`).

---

## 8. K3s Configuration (atlas.conf)

For K3s deployment only. Copy `atlas.conf.example` to `atlas.conf` and edit. The install pipeline reads this file, renders `templates/*.yaml.tmpl` via `envsubst`, and applies the resulting manifests in `manifests/*.yaml`.

> **Note:** `atlas.conf` is only used by K3s deployment scripts. Docker Compose uses `.env` instead. The two files configure different deployment targets and should not be mixed.

> Every variable below is consumed by at least one of: the install/uninstall scripts, the K3s manifest templates, or the benchmark ablation runner (`atlas.bench.v3_runner`). Vars an older `atlas.conf` sets that aren't listed here are ignored (see § 8.10).

### 8.1 Cluster & Network

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_NAMESPACE` | `atlas` | Kubernetes namespace for every ATLAS pod / service / PVC |
| `ATLAS_NODE_IP` | `auto` | Node IP for NodePort URL output. `auto` runs `ip` then `hostname -I` then `hostname -i`. |
| `ATLAS_KUBECONFIG` | `/etc/rancher/k3s/k3s.yaml` | Path to kubeconfig the install scripts use. Leave `auto` to inherit from environment. |
| `ATLAS_PROXY_NODEPORT` | `30080` | atlas-proxy external port |
| `ATLAS_LENS_NODEPORT` | `31144` | geometric-lens external port |
| `ATLAS_LLAMA_NODEPORT` | `32735` | llama-server external port |
| `ATLAS_SANDBOX_NODEPORT` | `30820` | sandbox external port |
| `ATLAS_V3_NODEPORT` | `30070` | v3-service external port. The template exposes it on NodePort `30070` by default (debug access to `/v3/*`); delete the `type: NodePort` / `nodePort:` lines in `templates/v3-service-deployment.yaml.tmpl` to make the service cluster-internal. |
| `ATLAS_LLAMA_PORT` | `8080` | llama-server internal port (matches Dockerfile EXPOSE) |
| `ATLAS_LENS_PORT` | `8099` | geometric-lens internal port |
| `ATLAS_PROXY_PORT` | `8090` | atlas-proxy internal port |
| `ATLAS_V3_PORT` | `8070` | v3-service internal port |
| `ATLAS_SANDBOX_PORT` | `8020` | sandbox internal port |

### 8.2 Storage paths

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_MODELS_DIR` | `/opt/atlas/models` | GGUF model files. Mounted into llama-server at `/models` (read-only) via `hostPath` in `templates/llama-deployment.yaml.tmpl`. |
| `ATLAS_PROJECTS_DIR` | `/opt/atlas/data/projects` | User project workspace. Bind-mounted at `/workspace` in BOTH atlas-proxy and sandbox pods (`hostPath` with `DirectoryOrCreate`) so the agent sees the same files in both. `uninstall.sh --data` removes this folder too, also when it is not inside `ATLAS_DATA_DIR`; the script lists it with its path before it asks. |
| `ATLAS_DATA_DIR` | `/opt/atlas/data` | Parent of `ATLAS_PROJECTS_DIR`. Printed at install time; `uninstall.sh` removes it when `--data` is set, and refuses to run with `--data` when this setting or `ATLAS_PROJECTS_DIR` is empty, is not a full path, or names the root folder, your home folder or a folder that holds this repository. Not mounted as a volume itself. |

### 8.3 Model & Inference

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_MAIN_MODEL` | **required** | Main GGUF filename. Becomes `MODEL_PATH=/models/<name>` inside the container. |
| `ATLAS_DRAFT_MODEL` | (unset) | Draft model for speculative decoding. **Deliberately inert** — see `ATLAS_ENABLE_SPECULATIVE`. |
| `ATLAS_CONTEXT_LENGTH` | `16384` | Context window in tokens, TOTAL across all slots (llama-server divides it by `ATLAS_PARALLEL_SLOTS`; at the default `1` slot, total = per-slot). V3's `--parallel 1` budget is sized around 16K; raise if you have GPU headroom and want longer turns. |
| `ATLAS_PARALLEL_SLOTS` | `1` | Concurrent KV slots. V3 self-embeddings push VRAM tight on 16 GB cards, so `1` is the safe default. |

### 8.4 Resource limits (Kubernetes pod spec)

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_LLAMA_CPU_REQUEST` | `2` | CPU request for llama-server |
| `ATLAS_LLAMA_CPU_LIMIT` | `4` | CPU limit for llama-server |
| `ATLAS_LLAMA_MEMORY_REQUEST` | `8Gi` | Memory request for llama-server |
| `ATLAS_LLAMA_MEMORY_LIMIT` | `16Gi` | Memory limit for llama-server |
| `ATLAS_SERVICE_CPU_REQUEST` | `0.5` | CPU request for non-llama services (proxy, lens, v3-service, sandbox) |
| `ATLAS_SERVICE_CPU_LIMIT` | `2` | CPU limit for non-llama services |
| `ATLAS_SERVICE_MEMORY_REQUEST` | `512Mi` | Memory request for non-llama services |
| `ATLAS_SERVICE_MEMORY_LIMIT` | `2Gi` | Memory limit for non-llama services |

> GPU is requested as a count (`nvidia.com/gpu: 1`), not a memory budget — there is no `ATLAS_LLAMA_GPU_MEMORY` knob.

### 8.5 Feature flags

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_ENABLE_SPECULATIVE` | `false` | **Speculative decoding is deliberately off, and the knob is deliberately not wired.** No entrypoint passes llama.cpp's draft flags (`-md` / `--model-draft`): on the reference serving host, speculative decoding measured *slower* than plain decoding for ATLAS's workload, whose generations are long and low-entropy enough that draft rejection dominates. The variable is kept so the decision stays visible rather than getting rediscovered and "fixed". If you want to revisit it, A/B it on your own hardware first — do not wire it up on principle. |

### 8.6 Timeouts (seconds)

| Variable | Default | Used by |
|----------|---------|---------|
| `ATLAS_LLM_TIMEOUT` | `120` | `scripts/verify-install.sh` for the smoke-test `curl` against llama-server |
| `ATLAS_HEALTH_CHECK_TIMEOUT` | `10` | `scripts/verify-install.sh` `--max-time` for `curl` against each `/health` endpoint during post-install verification. (The healthchecks defined inside the K3s templates use hardcoded timeouts, not this var.) |

### 8.7 V3 ablation knobs (benchmark-only)

Consumed by `atlas/bench/v3_runner.py:_load_v3_config` for ablation studies. The production `v3-service` reads its own constants from the `v3-service/stages/*.py` config dataclasses and does NOT pick these up at runtime.

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_V3_BUDGET_FORCING_DEFAULT_TIER` | `"standard"` | Default Budget Forcing tier when difficulty estimation is unavailable |
| `ATLAS_V3_PLAN_SEARCH_NUM_PLANS` | `3` | Plans generated per problem (overrides `PlanSearchConfig.num_plans`) |

### 8.8 Advanced

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_GHCR_OWNER` | `inferstep` | GHCR namespace for service images. Read by the K3s manifests **and** `scripts/build-containers.sh`, which tags local builds exactly as the manifests reference them (`ghcr.io/${ATLAS_GHCR_OWNER}/atlas-*`) so side-loaded images are picked up without editing manifests. |
| `ATLAS_IMAGE_TAG` | `latest` | Image tag for both the local-build path and the GHCR pull path |

The install scripts also honor three runtime-only env vars (not in `atlas.conf` itself):

| Variable | Default | Description |
|----------|---------|-------------|
| `ATLAS_CONFIG_FILE` | (auto) | Path override for `atlas.conf` itself. `scripts/lib/config.sh` looks at this before falling back to `$K8S_DIR/atlas.conf`. |
| `ATLAS_AUTO_CONFIRM` | `false` | Set to `true` in the environment to skip the interactive install prompts in `scripts/install.sh` |
| `ATLAS_MODEL_URL` | (unset) | Direct GGUF URL for an unregistered model. `scripts/download-models.sh` passes it to `atlas model install --url`, bypassing the registry lookup. |

### 8.9 Removed variables

Vars removed in earlier trims are ignored if left in an `atlas.conf`; see CHANGELOG. Most recently removed: `ATLAS_PVC_PROJECTS_SIZE` (sized the `lens-projects` PVC, which the lens mounted and nothing read after the project indexer was removed), `ATLAS_V3_EWC_LAMBDA`, `ATLAS_V3_REPLAY_BUFFER_MAX_SIZE`, `ATLAS_V3_REPLAY_BUFFER_REPLAY_RATIO`, `ATLAS_V3_LENS_FEEDBACK_ENABLED` and `ATLAS_V3_LENS_FEEDBACK_RETRAIN_INTERVAL` (bench-runner knobs for the online lens retrain, removed with the lens retrain endpoint; the runner applied only the retrain interval), `ATLAS_LENS_TRAINING_DIR` (the hostPath of the lens training corpus the proxy no longer collects), `ATLAS_PVC_LENS_STATE_SIZE` (sized the `lens-state` PVC, removed with the pattern cache and its SQLite store), `ATLAS_JWT_SECRET` (generated a secret into `.jwt_secret` and a Kubernetes Secret that no pod ever mounted), `ATLAS_LORA_DIR` and `ATLAS_TRAINING_DIR` (directories `install.sh` created and `uninstall.sh` deleted, that nothing wrote to and no template mounted), and `ATLAS_ENABLE_TRAINING` (was reserved with no reader — the nightly-retrain CronJob it anticipated was never built; lens retraining is host-side `atlas lens build`).
