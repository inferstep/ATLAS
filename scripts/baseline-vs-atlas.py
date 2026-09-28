#!/usr/bin/env python3
"""What does the model score WITHOUT the harness?

Twelve benchmark runs measured ATLAS against a fixed task set and never once
against the alternative, so "68% task success" has had no denominator. This
runs the same AoC tasks straight at llama-server with no gates, no V3, no
lens, no detectors and no repeat breakers, and scores the result with the
same checker, so the delta is attributable to the harness rather than to
task selection or grading.

The AoC tasks carry the comparison because they are self-contained (one
file, an input on disk, a printed answer), objectively scored, and verified
against a HOLDOUT input the solution never saw, which fails a hardcoded
answer. Nothing about the scoring is a judgement call.

Two baseline arms, because "the bare model" is ambiguous and the honest
comparison needs both ends of it:

  oneshot  one generation, no feedback. Raw model capability.
  loop     up to --attempts generations, each shown the previous failure
           (traceback, or the wrong answer). This is what a naive agent
           loop buys with none of ATLAS's machinery, and it is the arm
           ATLAS actually has to beat to justify itself.

Prompts and the checker are IMPORTED from the e2e suite rather than copied,
so the two sides cannot drift apart.
"""

import argparse
import importlib.util
import json
import re
import shutil
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]


def _load_suite():
    """Import the hyphenated e2e module so tasks and checkers stay shared."""
    path = REPO / "scripts" / "e2e-reliability.py"
    spec = importlib.util.spec_from_file_location("e2e_suite", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["e2e_suite"] = mod
    spec.loader.exec_module(mod)
    return mod


def _llama_url() -> str:
    """Resolve llama-server the way the rest of the tooling does: from .env."""
    port = "8080"
    env = REPO / ".env"
    if env.exists():
        for line in env.read_text().splitlines():
            if line.startswith("ATLAS_LLAMA_PORT="):
                port = line.split("=", 1)[1].strip()
    return f"http://localhost:{port}/v1/chat/completions"


# The model emits its answer inside a fence more often than not.
_FENCE = re.compile(r"```(?:python|py)?\s*\n(.*?)```", re.S)


def _extract_code(reply: str) -> str:
    blocks = _FENCE.findall(reply)
    if blocks:
        return max(blocks, key=len).strip() + "\n"
    return reply.strip() + "\n"


def _ask(url: str, messages: list, max_tokens: int, temperature: float) -> str:
    body = json.dumps({
        "messages": messages,
        "temperature": temperature,
        "max_tokens": max_tokens,
        # Gemma routes everything to reasoning_content and leaves content
        # empty unless thinking is off; without this the arm scores 0/N for
        # a reason that has nothing to do with capability.
        "chat_template_kwargs": {"enable_thinking": False},
    }).encode()
    req = urllib.request.Request(
        url, body, {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=900) as fh:
        data = json.load(fh)
    msg = data["choices"][0]["message"]
    return (msg.get("content") or msg.get("reasoning_content") or "").strip()


SYSTEM = ("You are a Python programmer. Reply with the complete contents of "
          "solve.py in a single ```python fenced block and nothing else. The "
          "file must read its input from input.txt in the working directory "
          "and print the answer.")


def run_task(suite, task, url, arm: str, attempts: int,
             temperature: float) -> dict:
    """One task, one arm. Returns the verdict and what it took to get there."""
    work = Path(tempfile.mkdtemp(prefix="baseline-"))
    try:
        for name, content in task.files.items():
            (work / name).write_text(content)

        messages = [{"role": "system", "content": SYSTEM},
                    {"role": "user", "content": task.prompt}]
        started = time.time()
        detail, ok = "no attempt made", False
        tries = 0

        for attempt in range(attempts if arm == "loop" else 1):
            tries = attempt + 1
            reply = _ask(url, messages, max_tokens=2048,
                         temperature=temperature)
            code = _extract_code(reply)
            (work / "solve.py").write_text(code)

            ok, detail = task.check(work)
            if ok:
                break
            if arm == "loop" and attempt < attempts - 1:
                messages += [
                    {"role": "assistant", "content": reply},
                    {"role": "user", "content":
                        f"That was wrong: {detail}\n\nFix solve.py and reply "
                        f"with its complete corrected contents in a single "
                        f"```python block."},
                ]
        return {"task": task.name, "arm": arm, "passed": ok, "detail": detail,
                "attempts": tries, "wall_s": round(time.time() - started, 1)}
    except Exception as exc:  # a crashed arm is a failed arm, not a crashed run
        return {"task": task.name, "arm": arm, "passed": False,
                "detail": f"{type(exc).__name__}: {exc}", "attempts": 0,
                "wall_s": 0.0}
    finally:
        shutil.rmtree(work, ignore_errors=True)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tasks", default="aoc_sonar,aoc_course,aoc_slope,aoc_shoal")
    ap.add_argument("--reps", type=int, default=2)
    ap.add_argument("--attempts", type=int, default=4,
                    help="generations the loop arm gets before giving up")
    ap.add_argument("--arms", default="oneshot,loop")
    ap.add_argument("--temperature", type=float, default=0.6)
    ap.add_argument("--json", dest="json_out", default="")
    args = ap.parse_args()

    suite = _load_suite()
    url = _llama_url()
    names = [t.strip() for t in args.tasks.split(",") if t.strip()]
    arms = [a.strip() for a in args.arms.split(",") if a.strip()]

    results = []
    for arm in arms:
        for name in names:
            task = suite.TASKS[name]
            for rep in range(1, args.reps + 1):
                print(f"[{arm}] {name} rep {rep} ...", flush=True)
                row = run_task(suite, task, url, arm, args.attempts,
                               args.temperature)
                row["rep"] = rep
                results.append(row)
                mark = "PASS" if row["passed"] else "fail"
                print(f"      {mark} attempts={row['attempts']} "
                      f"{row['wall_s']}s — {row['detail'][:90]}", flush=True)

    print("\n" + "=" * 62)
    print(f"{'arm':10} {'task':14} {'passed':>8}")
    for arm in arms:
        for name in names:
            rows = [r for r in results if r["arm"] == arm and r["task"] == name]
            p = sum(1 for r in rows if r["passed"])
            print(f"{arm:10} {name:14} {p:>4}/{len(rows)}")
        rows = [r for r in results if r["arm"] == arm]
        p = sum(1 for r in rows if r["passed"])
        pct = (100.0 * p / len(rows)) if rows else 0.0
        print(f"{arm:10} {'TOTAL':14} {p:>4}/{len(rows)}  ({pct:.0f}%)")
    print("=" * 62)
    print("Compare against ATLAS on the same four tasks, from the e2e runs.")

    if args.json_out:
        Path(args.json_out).write_text(json.dumps(results, indent=1))
        print(f"wrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
