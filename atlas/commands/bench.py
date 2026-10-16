"""Bench command — run benchmarks with live progress."""

import contextlib
import json
import os
import sys
import time
from pathlib import Path

from atlas import display
from atlas import env as cli_env


def _atlas_root() -> Path:
    """The repo root, as a Path. Canonical resolution lives in
    atlas.env; kept as a module hook so tests can pin the root."""
    return Path(cli_env.atlas_root())


def bench(dataset: str = "livecodebench", max_tasks: int = 0,  # noqa: C901
          selection_strategy: str = "random", run_id: str = None) -> int:
    """Run benchmark with live progress display. Returns a process exit
    code: 0 on success (including a fully-resumed run), 1 when the runner
    fails or produces nothing.

    Delegates to the V3 runner (atlas.bench.v3_runner) but displays progress
    inline. The runner is launched with the repo root as its working
    directory (results are written under repo-root benchmark/results/), so
    the command behaves the same from any directory.
    """
    display.phase_label(f"Benchmark: {dataset}")

    root = _atlas_root()

    # Build runner command.
    #
    # --baseline turns the V3 phases off, which is the point of this
    # command: the corpus `atlas lens build --from-results` reads is one
    # code/passed pair per task, produced by the model unaided. On that
    # path the runner generates a single candidate per task, so candidate
    # selection has nothing to choose between — selection_strategy is
    # carried through as run metadata only (see main()'s --strategy help).
    run_id = run_id or f"bench_{dataset}_{int(time.time())}"
    cmd = [
        sys.executable, "-m", "atlas.bench.v3_runner",
        "--run-id", run_id,
        "--baseline",
        "--selection-strategy", selection_strategy,
    ]
    if max_tasks > 0:
        cmd.extend(["--max-tasks", str(max_tasks)])

    # Connectivity (llama/RAG URLs, model name) is resolved by benchmark.config
    # from the deployment's .env / atlas.conf — nothing model- or
    # deployment-specific is pinned here. An explicit LLAMA_URL/LENS_URL in
    # the environment still wins. Generation stays serialized (the safe
    # default for any architecture).
    env = os.environ.copy()
    env["ATLAS_PARALLEL_TASKS"] = "1"

    display.info(f"Run ID: {run_id}")
    display.info(f"Strategy: {selection_strategy}")
    if max_tasks > 0:
        display.info(f"Tasks: {max_tasks}")

    import subprocess
    from collections import deque
    tail = deque(maxlen=15)  # last runner lines, shown if the run fails
    proc = subprocess.Popen(
        cmd, env=env, cwd=str(root),
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
        text=True, bufsize=1,
    )

    results_dir = root / "benchmark" / "results" / run_id / "v3_lcb" / "per_task"
    pass_count = 0
    task_count = 0
    dataset_total = 0  # parsed from the runner's "[done/total]" prefix
    saw_complete = False  # runner printed its BENCHMARK COMPLETE summary

    try:
        for line in proc.stdout:
            line = line.strip()
            if line:
                tail.append(line)
            # Parse runner output for progress
            if line.startswith("[") and "/" in line and "LCB" in line:
                task_count += 1
                if "PASS" in line:
                    pass_count += 1
                # The runner emits "[done/total] <task>: ..." — take the
                # dataset size from the line rather than pinning it; a
                # malformed prefix just keeps the previous total.
                with contextlib.suppress(IndexError, ValueError):
                    dataset_total = int(
                        line[1:].split("]", 1)[0].split("/", 1)[1].strip())
                total = max_tasks if max_tasks > 0 else \
                    (dataset_total or task_count)
                display.progress_bar(task_count, total, pass_count, line.split("]")[-1].strip()[:40])
            elif "BENCHMARK COMPLETE" in line:
                saw_complete = True
                display.progress_done()
                print()
            elif line.startswith(("Downloading ", "Downloaded ", "Warning:",
                                  "Cached LiveCodeBench", "Failed:",
                                  "Resuming:", "LIMITED MODE",
                                  "Loading LiveCodeBench")):
                # Dataset fetch + resume status: surface live, or silent
                # fallbacks (partial cache, failed sources) look like
                # nothing happened.
                display.info(line.lstrip())
    except KeyboardInterrupt:
        proc.terminate()
        display.warn("Benchmark interrupted")
        return 1

    proc.wait()

    if proc.returncode != 0:
        display.error(f"benchmark runner exited with code {proc.returncode}")
        for l in tail:
            print(f"    {l}")
        return 1

    if task_count == 0 and not saw_complete:
        # The runner exited without processing a single task (e.g. an aborted
        # pre-flight). Results already on disk are from an earlier run — don't
        # summarize them as if this run produced them.
        display.error("runner exited without processing any tasks")
        for l in tail:
            print(f"    {l}")
        return 1
    if task_count == 0:
        # Resume found every task already complete — the summary below is
        # the prior run's results, which is exactly what the operator wants.
        display.info("no tasks remaining — all requested tasks were already "
                     "complete on disk (resumed run)")

    # Final results
    if results_dir.exists():
        results = list(results_dir.glob("*.json"))
        # Read each result via context manager so handles close promptly
        # even when we hit a large results dir on a long benchmark run.
        def _load(f):
            try:
                with open(f) as fh:
                    d = json.load(fh)
            except (OSError, json.JSONDecodeError):
                return {}
            return d if isinstance(d, dict) else {}
        p = sum(1 for f in results if _load(f).get("passed"))
        total = len(results)
        rate = p / max(total, 1) * 100
        display.separator()
        display.success(f"pass@1: {p}/{total} ({rate:.1f}%)")
        display.info(f"Results: {root / 'benchmark' / 'results' / run_id}/")
        display.info(f"Lens retrain: atlas lens build --force --from-results "
                     f"{results_dir}")
        display.separator()
        return 0
    display.error("No results found")
    for l in tail:
        print(f"    {l}")
    return 1


def main(argv=None) -> int:
    """`atlas bench` subcommand entry point."""
    import argparse
    parser = argparse.ArgumentParser(
        prog="atlas bench",
        description="Generate + self-label candidates for the loaded model "
                    "(baseline benchmark run). Results feed "
                    "`atlas lens build --from-results`.")
    parser.add_argument("--tasks", "--max-tasks", dest="tasks", type=int,
        default=0, help="number of tasks to run (0 = all, default)")
    parser.add_argument("--strategy", "--selection-strategy", dest="strategy",
        default="random", choices=["lens", "random", "logprob", "oracle"],
        help="candidate selection strategy, recorded in the run metadata "
             "(default: random). It picks among several candidates for the "
             "same task, and this command runs the runner's baseline path — "
             "one candidate per task — so all four values give the same "
             "results here. Strategies differ only on multi-candidate runs: "
             "`python -m atlas.bench.v3_runner --selection-strategy ...` "
             "without --baseline")
    parser.add_argument("--run-id", default=None,
        help="name for this run (default: bench_livecodebench_<timestamp>); "
             "results land in benchmark/results/<run-id>/")
    args = parser.parse_args(argv)
    if args.tasks < 0:
        parser.error("--tasks must be >= 0 (0 runs the full dataset)")

    # The runner supports LiveCodeBench only, so no --dataset flag is exposed.
    return bench(max_tasks=args.tasks, selection_strategy=args.strategy,
                 run_id=args.run_id)
