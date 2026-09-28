#!/usr/bin/env python3
"""Score the bare model on the freshly authored tasks.

The denominator for any claim about the harness. Same statements the ATLAS arm
gets, same reference answers, same holdout check, no tools and no pipeline.

Two arms, because "the bare model" is ambiguous:
  oneshot  one generation, no feedback
  loop     up to --attempts generations, each shown the previous failure
"""

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "scripts"))

from novel_tasks import build_tasks  # noqa: E402

SYSTEM = ("You are a Python programmer. Reply with the complete contents of "
          "solve.py in a single ```python fenced block and nothing else. The "
          "program must read input.txt from the working directory and print "
          "the answer.")
FENCE = re.compile(r"```(?:python|py)?\s*\n(.*?)```", re.S)


def _llama_url() -> str:
    port = "8080"
    env = REPO / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            if line.startswith("ATLAS_LLAMA_PORT="):
                port = line.split("=", 1)[1].strip()
    return f"http://localhost:{port}/v1/chat/completions"


def _ask(url, messages, max_tokens=2048, temperature=0.6) -> str:
    body = json.dumps({
        "messages": messages, "temperature": temperature,
        "max_tokens": max_tokens,
        # Without this the model routes everything to reasoning_content and
        # returns an empty content, which scores 0 for a reason that has
        # nothing to do with capability.
        "chat_template_kwargs": {"enable_thinking": False},
    }).encode()
    req = urllib.request.Request(url, body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=900) as fh:
        data = json.load(fh)
    msg = data["choices"][0]["message"]
    return (msg.get("content") or msg.get("reasoning_content") or "").strip()


def _extract(reply: str) -> str:
    blocks = FENCE.findall(reply)
    return (max(blocks, key=len).strip() + "\n") if blocks else reply.strip() + "\n"


# Scoring is EXACT MATCH by decision. The output-separator ambiguity that
# once made "42, 242" a defensible reading is fixed at the source: every
# prompt now states "fields separated by single spaces". A benchmark pass
# must be the answer a user would actually see, byte for byte.


def _run_against(work: Path, data: str, timeout=60):
    """Run solve.py with `data` as input.txt. Returns (ok, output_or_reason)."""
    (work / "input.txt").write_text(data)
    try:
        proc = subprocess.run([sys.executable, "solve.py"], cwd=str(work),
                              capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return False, f"did not finish within {timeout}s"
    if proc.returncode != 0:
        return False, f"failed: {proc.stderr.strip()[:120]}"
    out = proc.stdout.strip()
    return (True, out) if out else (False, "printed nothing")


def _check(work: Path, task) -> tuple:
    ok, got = _run_against(work, task.input_text)
    if not ok:
        return False, got
    if got != task.expected:
        return False, f"got {got!r}, want {task.expected!r}"
    # Same program, an input it never saw. A hardcoded answer dies here.
    ok2, got2 = _run_against(work, task.holdout_text)
    if not ok2:
        return False, f"correct on its own input but broke on the holdout: {got2}"
    if got2 != task.holdout_expected:
        return False, (f"holdout mismatch: got {got2!r}, want "
                       f"{task.holdout_expected!r} — answer looks hardcoded")
    return True, f"{got} correct, and correct on the holdout"


def solve_one(url, task, arm, attempts) -> dict:
    work = Path(tempfile.mkdtemp(prefix="novel-"))
    try:
        messages = [{"role": "system", "content": SYSTEM},
                    {"role": "user", "content": task.prompt}]
        started, ok, detail, tries = time.time(), False, "no attempt", 0
        for attempt in range(attempts if arm == "loop" else 1):
            tries = attempt + 1
            reply = _ask(url, messages)
            (work / "solve.py").write_text(_extract(reply))
            ok, detail = _check(work, task)
            if ok:
                break
            if arm == "loop" and attempt < attempts - 1:
                messages += [
                    {"role": "assistant", "content": reply},
                    {"role": "user", "content":
                        f"That was wrong: {detail}\n\nFix solve.py and reply "
                        f"with its complete corrected contents in one "
                        f"```python block."},
                ]
        return {"task": task.name, "family": task.family, "arm": arm,
                "passed": ok, "detail": detail, "attempts": tries,
                "wall_s": round(time.time() - started, 1)}
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--count", type=int, default=50)
    ap.add_argument("--seed", type=int, default=20260806)
    ap.add_argument("--arms", default="oneshot,loop")
    ap.add_argument("--attempts", type=int, default=4)
    ap.add_argument("--json", dest="json_out", default="")
    args = ap.parse_args()

    url = _llama_url()
    tasks = build_tasks(args.count, args.seed)
    arms = [a.strip() for a in args.arms.split(",") if a.strip()]

    results = []
    for arm in arms:
        for task in tasks:
            row = solve_one(url, task, arm, args.attempts)
            results.append(row)
            print(f"[{arm}] {row['task']:12} "
                  f"{'PASS' if row['passed'] else 'fail':4} "
                  f"n={row['attempts']} {row['wall_s']:6}s — {row['detail'][:70]}",
                  flush=True)

    print("\n" + "=" * 66)
    for arm in arms:
        rows = [r for r in results if r["arm"] == arm]
        p = sum(1 for r in rows if r["passed"])
        print(f"{arm:9} TOTAL {p:3}/{len(rows)}  ({100.0 * p / len(rows):.0f}%)")
        byfam = {}
        for r in rows:
            got, tot = byfam.get(r["family"], (0, 0))
            byfam[r["family"]] = (got + (1 if r["passed"] else 0), tot + 1)
        for fam in sorted(byfam):
            got, tot = byfam[fam]
            print(f"          {fam:10} {got}/{tot}")
    print("=" * 66)

    if args.json_out:
        Path(args.json_out).write_text(json.dumps(results, indent=1))
        print(f"wrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
