# ATLAS Benchmark Harness (`atlas.bench`)

The benchmark harness ships inside the `atlas` package so a pip-installed
CLI can run the whole model-onboarding loop without extra checkout state.
The user-facing entry point is `atlas bench` (see
[docs/CLI.md § atlas bench](../../docs/CLI.md#atlas-bench)); this package is
what it drives.

## What it's for

Candidate generation for lens training first, scoreboard second:

1. `atlas bench` runs `atlas.bench.v3_runner` in **baseline mode** (V3
   pipeline phases off) against whatever model llama-server has loaded.
2. Each task's generated candidate is executed locally and written as
   `benchmark/results/<run-id>/v3_lcb/per_task/*.json` with `code` +
   `passed` labels.
3. `atlas lens build --from-results <that dir>` trains the model's own
   C(x)/G(x) bundle from those labels (and merges any banked embeddings
   from `telemetry/embeddings.emb`).

The pass@1 summary printed at the end is the secondary product.

## Layout

| Module | Purpose |
|---|---|
| `v3_runner.py` | Runner entry point (`python -m atlas.bench.v3_runner`). Baseline and V3-mode runs, resume-on-rerun, telemetry banking, ablation knobs (`ATLAS_V3_*`, docs/CONFIGURATION.md § 8.9). |
| `runner.py` | Code execution: isolated subprocesses with resource limits, assertion (`execute_code`) and stdin/stdout (`execute_code_stdio`) modes. |
| `config.py` | Connectivity + paths: resolves llama/lens URLs and the model name from the deployment's `.env` (Docker) or `atlas.conf` (K3s); results land under repo-root `benchmark/`. |
| `models.py` | Data models (`BenchmarkTask`, results). |
| `best_of_k.py` | Lens candidate scoring (`score_candidate`, used by `v3_runner`). |
| `geo_learning.py` | Embedding banking: extracts and stores per-candidate embeddings + labels as the bench runs, so lens training gets several labeled samples per task. |
| `datasets/` | Dataset loaders — LiveCodeBench v5 (`livecodebench.py`) over the `base.py` download/cache contract. Caches under repo-root `benchmark/datasets/.cache/`. |

The V3 pipeline **stages** the runner exercises are not in this package:
they live in `v3-service/stages/` (shared with the deployed service — one
implementation, two callers). The runner puts the checkout's `v3-service/`
on `sys.path` at startup.

Repo-root `benchmark/` holds only data: dataset caches and
`benchmark/results/<run-id>/` output.

## Relationship to the product path

The stage engines, candidate selection, and the lens backend are one
shared implementation with the deployed v3-service. The orchestrators
differ deliberately; read bench numbers with that in mind:

- **Lens usage.** The bench scores C(x) via `/internal/lens/score-text`;
  the product additionally runs per-step G(x) scoring and threshold
  interventions on every write.
- **Verification oracle.** The bench verifies against the tests its
  dataset loader returns, which today are only the examples printed in each
  problem (see "Published results" below); the product has no oracle at runtime and uses
  self-tests + build/syntax gates instead.
- **Candidate budget.** `atlas bench` is baseline-mode (one candidate per
  task); the product allocates candidates adaptively when V3 fires.

## Published results

None current. The V3 (14B) ablation study's 74.6% LiveCodeBench figure is
withdrawn: this runner's LiveCodeBench loader cannot decode the dataset's
hidden tests and falls back to the 1-5 examples printed in each problem, so
every `pass` it reports means "passed the printed examples". In V3 mode it
also counts a task as passed when any candidate passes them, and repair sees
their expected output. See the notice at the top of
[docs/reports/V3_ABLATION_STUDY.md](../../docs/reports/V3_ABLATION_STUDY.md);
the raw traces stay indexed in
[docs/reports/ablation/README.md](../../docs/reports/ablation/README.md) so
the withdrawal can be checked. Numbers from this runner are not LiveCodeBench
pass@1 until those are fixed. Per-registry-model numbers will be tracked in
[#28](https://github.com/inferstep/ATLAS/issues/28).
