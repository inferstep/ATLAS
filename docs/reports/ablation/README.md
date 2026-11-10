# V3 Ablation Study — Raw Results

This documents the per-task pass/fail data from the ATLAS V3.0 ablation study on
LiveCodeBench (599 tasks). They are the raw traces behind the 74.6% result,
which is **withdrawn**: the runner graded only the examples printed in each
problem, counted any of three candidates passing as a pass, and let repair see
the examples' expected output (see the withdrawal notice in the
[CHANGELOG](../../../CHANGELOG.md#withdrawn-the-v30-livecodebench-result-746)). The traces are kept so the
withdrawal can be checked. Every pass rate on this page comes from that run.

## Where the data lives

Conditions A–D are published on HuggingFace under
[`benchmarks/v3_ablation/`](https://huggingface.co/datasets/itigges22/ATLAS/tree/main/benchmarks/v3_ablation),
in the same `<condition>/v3_lcb/per_task/*.json` layout described below. Condition E
is in this directory, at `condition_e_full/`.

The [`ablation_traces/`](https://huggingface.co/datasets/itigges22/ATLAS/tree/main/ablation_traces)
folder on HuggingFace does not hold full traces of these conditions. Each
`ablation_traces/ablation_*_final/embeddings.emb` file holds only 3 PASS
records, for the tasks LCB/1873_A, LCB/1873_B and LCB/1873_D. The files in
`ablation_B_final/` to `ablation_F_final/` are identical. You cannot use these
files to check a condition.

## Ablation Conditions

| Condition | HuggingFace path | Phases Active | Pass@1 | Tasks | Description |
|-----------|------------------|---------------|--------|-------|-------------|
| **A** | `benchmarks/v3_ablation/condition_a_baseline/` | None | **54.9%** (329/599) | 599 | Baseline: frozen Qwen3-14B, no V3 pipeline |
| **B** | `benchmarks/v3_ablation/condition_b_phase1/` | Phase 1 | **67.3%** (403/599) | 599 | +PlanSearch, DivSampling, Budget Forcing |
| **C** | `benchmarks/v3_ablation/condition_c_phase1_2/` | Phase 1+2 | **67.3%** (403/599) | 599 | +Blend-ASC, ReASC, S* tiebreaking |
| **D** | `benchmarks/v3_ablation/condition_d_phase1_3/` | Phase 1+3 | **74.6%** (447/599) | 599 | +PR-CoT Repair, Refinement Loop, Derivation Chains |
| **E** | `condition_e_full/` (this directory) | All | Partial | 94 | Full pipeline (discontinued — OOM at scale) |

## Key Findings

- **Phase 1 (Constraint-Driven Generation)**: +12.4pp (54.9% → 67.3%)
- **Phase 2 (Intelligent Compute)**: +0.0pp (67.3% → 67.3%) — no measurable improvement
- **Phase 3 (Verified Iterative Refinement)**: +7.3pp (67.3% → 74.6%)
- **Total V3 improvement**: +19.7pp over baseline

## Data Format

Each condition directory on HuggingFace contains:

```
condition_X/
├── summary.json              # Aggregate results: pass_rate, total_tasks, timing
├── telemetry/
│   ├── v3_events.jsonl       # Per-task V3 pipeline events
│   └── <stage>_events.jsonl  # Events of each stage that ran, for example plan_search_events.jsonl
└── v3_lcb/
    ├── results.json          # Pass rate and phase breakdown
    └── per_task/
        ├── LCB_1873_A.json   # Per-task result: passed, code, phase_solved, candidates
        ├── LCB_1873_B.json
        └── ...               # 599 files per condition
```

`condition_e_full/` in this directory holds only `v3_lcb/per_task/`, with 94 files.

### Per-Task JSON Format

This is `LCB_1873_A.json` from condition D, with the code shortened:

```json
{
  "task_id": "LCB/1873_A",
  "passed": true,
  "code": "# YOUR CODE HERE\nt = int(input())\n...",
  "phase_solved": "phase1",       // "phase1", "pr_cot", "refinement", "derivation", "none"
  "candidates_generated": 3,
  "total_tokens": 1159,
  "total_time_ms": 18086.100578308105,
  "telemetry": {
    "adaptive_k": 3,
    "budget_tier": "standard",
    "plansearch_tokens": 1004
  }
}
```

The `telemetry` keys are different in each condition. Every file has
`adaptive_k` and `budget_tier`. In conditions A and B, every failed task has
empty `code`.

## Reproduction

All conditions used:
- **Model**: Qwen3-14B-Q4_K_M + Qwen3-0.6B-Q8_0 draft (speculative decoding)
- **Dataset**: LiveCodeBench v5 (599 tasks, bzantium mirror)
- **Seeds**: Fixed seed 42 for all conditions
- **1 seed per condition** (k=3 candidates per task in Phase 1)
- **Hardware**: RTX 5060 Ti 16GB, single GPU

Condition D cannot be reproduced at this commit, and reproducing it would
not be useful: the runner no longer has a `--no-phase2` flag, the Qwen3-14B
configuration and several V3.0 stages no longer exist, and the grading flaws
described above are still present in the current runner.

To reproduce condition A (baseline):

```bash
python -m atlas.bench.v3_runner \
  --baseline \
  --run-id condition_a_reproduction
```

## Computing Pass@1 from Raw Data

Fetch a condition, then count passes:

```bash
huggingface-cli download itigges22/ATLAS \
  --repo-type dataset \
  --include "benchmarks/v3_ablation/condition_d_phase1_3/*" \
  --local-dir ./ablation_data
```

```python
import json, glob

condition = "./ablation_data/benchmarks/v3_ablation/condition_d_phase1_3"
tasks = glob.glob(f"{condition}/v3_lcb/per_task/*.json")
passed = sum(1 for t in tasks if json.load(open(t)).get("passed", False))
total = len(tasks)
print(f"Pass@1: {passed}/{total} = {passed/total:.1%}")
# Prints 447/599: the published count of tasks that passed the printed
# examples. This is not LiveCodeBench pass@1 (see the withdrawal notice).
```
