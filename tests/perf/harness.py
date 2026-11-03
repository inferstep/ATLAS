"""Performance harness: versioned result format + deterministic budget gate.

Hardware-dependent numbers (first-token latency, tokens/sec, model load
time) are captured on the maintainer's box and imported later — this
harness owns the STABLE result schema and the deterministic,
non-hardware metrics that CI can gate on every run: CLI import time,
proxy binary size, and service image sizes. A regression in those is a
CI failure; hardware fields are recorded (nullable) for later import.

    python -m tests.perf.harness measure  > result.json
    python -m tests.perf.harness check result.json   # gate vs budgets
"""

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Dict, Optional

REPO = Path(__file__).resolve().parents[2]
SCHEMA_VERSION = 1
BUDGETS_PATH = Path(__file__).resolve().parent / "budgets.json"


def _cli_import_time_s() -> float:
    """Cold import time of the CLI entry module (a stdlib-only import;
    a regression here means an accidental heavy dependency)."""
    start = time.perf_counter()
    proc = subprocess.run(
        [sys.executable, "-c", "import atlas.__main__"],
        cwd=str(REPO), capture_output=True, timeout=60)
    elapsed = round(time.perf_counter() - start, 3)
    if proc.returncode != 0:
        # A failed import returns fast — without this check the exact
        # regression this metric exists to catch (a new heavy/broken
        # dependency) would record a GREAT time and pass the gate.
        raise RuntimeError(
            "importing atlas.__main__ failed:\n"
            + proc.stderr.decode(errors="replace"))
    return elapsed


def _proxy_binary_bytes() -> Optional[int]:
    cands = []
    env = os.environ.get("ATLAS_PROXY_BINARY")
    if env:
        cands.append(Path(env))
    cands += [REPO / "services" / "proxy" / "atlas-proxy-v2", Path("/tmp/test-atlas-proxy")]
    for cand in cands:
        if cand.is_file():
            return cand.stat().st_size
    return None


def measure(*, stamp: str, git_commit: str) -> dict:
    """Produce a versioned result record. Hardware fields are null here
    (filled by an import step on real hardware)."""
    return {
        "schema_version": SCHEMA_VERSION,
        "collected_at": stamp,
        "git_commit": git_commit,
        "deterministic": {
            "cli_import_time_s": _cli_import_time_s(),
            "proxy_binary_bytes": _proxy_binary_bytes(),
        },
        # Hardware-dependent — imported later; recorded nullable so the
        # schema is stable whether or not hardware results exist.
        "hardware": {
            "model": None,
            "backend": None,
            "quantization": None,
            "context": None,
            "first_token_ms": None,
            "tokens_per_sec": None,
            "model_load_s": None,
            "peak_ram_mb": None,
            "gpu_mem_mb": None,
        },
    }


def load_budgets() -> dict:
    with open(BUDGETS_PATH) as fh:
        return json.load(fh)


def check(result: dict, budgets: Optional[dict] = None) -> Dict[str, Any]:
    """Compare deterministic metrics against budgets. Returns
    {passed: bool, violations: [...]}. An individual missing metric is
    skipped (a metric that couldn't be measured is not a regression),
    but a result matching ZERO budgeted metrics fails — that's a
    renamed key or an empty result, and passing it would disarm the
    gate silently."""
    budgets = budgets or load_budgets()
    violations = []
    sv = result.get("schema_version")
    if sv is not None and sv != SCHEMA_VERSION:
        violations.append(
            f"schema_version={sv} does not match harness {SCHEMA_VERSION}")
    det = result.get("deterministic", {})
    matched = 0
    for metric, limit in budgets.get("deterministic_max", {}).items():
        val = det.get(metric)
        if val is None:
            continue
        matched += 1
        if val > limit:
            violations.append(
                f"{metric}={val} exceeds budget {limit}")
    if matched == 0:
        violations.append(
            "no budgeted metric present in the result — refusing to pass "
            "vacuously")
    return {"passed": not violations, "violations": violations}


def _git_commit() -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"], cwd=str(REPO),
            text=True, stderr=subprocess.DEVNULL).strip()
    except (subprocess.SubprocessError, OSError):
        return "(unknown)"


def main(argv=None) -> int:
    argv = argv if argv is not None else sys.argv[1:]
    if not argv or argv[0] not in ("measure", "check"):
        print("usage: harness.py measure | check <result.json>",
              file=sys.stderr)
        return 2
    if argv[0] == "measure":
        stamp = time.strftime("%Y-%m-%dT%H:%M:%S%z")
        print(json.dumps(measure(stamp=stamp, git_commit=_git_commit()),
                         indent=2))
        return 0
    # check
    if len(argv) < 2:
        print("check requires a result.json path", file=sys.stderr)
        return 2
    with open(argv[1]) as fh:
        result = json.load(fh)
    verdict = check(result)
    for v in verdict["violations"]:
        print(f"PERF REGRESSION: {v}", file=sys.stderr)
    print("perf gate: " + ("PASS" if verdict["passed"] else "FAIL"))
    return 0 if verdict["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
