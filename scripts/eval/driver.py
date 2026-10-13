#!/usr/bin/env python3
"""Run a frozen held-out suite through ATLAS or the baseline, and report.

  driver.py check SUITE --image IMAGE
  driver.py run SUITE --arm atlas|baseline --out RESULTS.jsonl --image IMAGE \\
            --compose-project PROJECT --workspace-root DIR [...]
  driver.py report RESULTS.jsonl [--against BASELINE.jsonl]

The suite format and the run rules are on #238; the baseline arm is specified
on #242. Records hold task ids; reports hold aggregates only.
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

from atlas_arm import run_atlas
from baseline_arm import Workspace, run_baseline
from grading import build_workspace, check_controls, grade, resolve_image
from provenance import run_context
from report import arm_summary, compare
from suite import SuiteError, load_suite

REPO = HERE.parents[1]
DEV_PROJECT = "atlas"  # the development stack: never measured with held-out tasks
CAPTURE_ENV = "ATLAS_V3_CAPTURE_POOL"


def main(argv=None) -> int:
    args = parse_args(argv)
    try:
        return args.func(args)
    except SuiteError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2


def cmd_check(args) -> int:
    """Every grader's controls must separate before any run."""
    image_id = resolve_image(args.image)
    if refused([] if image_id else [f"docker cannot resolve the grader image {args.image!r}"]):
        return 2
    print(f"grader image {args.image} = {image_id}")
    problems = [p for t in load_suite(Path(args.suite)) for p in check_controls(t, image_id)]
    for p in problems:
        print(f"control: {p}")
    print("controls separate" if not problems else f"{len(problems)} control problem(s)")
    return 1 if problems else 0


def cmd_run(args) -> int:
    suite_root = Path(args.suite)
    tasks = select(load_suite(suite_root), args.tasks)
    identity, problems = stack_identity(args)
    if refused(problems):
        return 2
    context, problems = run_context(args, suite_root)
    if refused(problems):
        return 2
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    with open(args.out, "a") as out:
        for rep in range(1, args.repeats + 1):
            for task in tasks:
                record = run_one(task, rep, args, identity, stamp, context)
                out.write(json.dumps(record) + "\n")
                out.flush()
                print(f"{task.id} r{rep} {args.arm}: {record['status']} / {record['grade']}", flush=True)
    return 0


def cmd_report(args) -> int:
    runs = read_records(args.results)
    if args.against:
        print(json.dumps(compare(runs, read_records(args.against)), indent=2))
    else:
        print(json.dumps(arm_summary(runs), indent=2))
    return 0


def run_one(task, rep: int, args, identity: dict, stamp: str, context: dict) -> dict:
    """A fresh workspace, one session in the arm, then the grade."""
    subdir = f"eval-{args.arm}-{stamp}-{task.id}-r{rep}"
    host = Path(args.workspace_root) / subdir
    build_workspace(task, host)
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    if args.arm == "atlas":
        # The stack's own session timeout ends the session; this is a backstop.
        result = run_atlas(task, args.proxy_url, subdir, subdir, int(args.budget_s) + 180)
    else:
        ws = Workspace(host, f"{args.workspace_mount}/{subdir}")
        result = run_baseline(task, ws, args.llama_url, args.sandbox_url,
                              args.budget_s, args.context_tokens)
    g = grade(task, host, context["grader_image"]["id"])
    return dict(context, arm=args.arm, task=task.id, kind=task.kind, lang=task.lang,
                task_network=task.network, rep=rep, subdir=subdir, started_utc=started,
                status=result.status, reason=result.reason, turns=result.turns,
                tokens=result.tokens, wall_s=result.wall_s, grade=g.outcome,
                grade_reason=g.reason, grade_output=g.output, budget_s=args.budget_s,
                stack=identity)


def refused(problems: list) -> bool:
    for p in problems:
        print(f"error: {p}", file=sys.stderr)
    return bool(problems)


def stack_identity(args) -> tuple:
    """What the run measures, and why it must not run, if it must not."""
    if args.compose_project == DEV_PROJECT:
        return {}, [f"the {DEV_PROJECT!r} project is the development stack; held-out "
                    f"runs use a stack of their own (#238)"]
    rel = _reliability_runner()
    ident = rel.deployed_identity(args.compose_project, Path(args.deploy_dir), args.commit)
    problems = [] if ident["verified"] else list(ident["problems"])
    if capture_is_on(args.compose_project):
        problems.append(f"{CAPTURE_ENV} is set in the v3-service: pool capture keeps task text")
    proxy = rel.stack_identity(args.proxy_url)
    return dict(proxy, commit=ident["commit"], images=ident["images"]), problems


def capture_is_on(project: str, run=subprocess.run) -> bool:
    """Whether the project's v3-service writes V3 pool captures."""
    ps = run(["docker", "ps", "-q", "--filter", f"label=com.docker.compose.project={project}",
              "--filter", "label=com.docker.compose.service=v3-service"],
             capture_output=True, text=True, timeout=30)
    ids = (ps.stdout or "").split()
    if not ids:
        return False
    env = run(["docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", ids[0]],
              capture_output=True, text=True, timeout=30)
    return any(line.startswith(CAPTURE_ENV + "=") and line.split("=", 1)[1].strip()
               for line in (env.stdout or "").splitlines())


def select(tasks: list, wanted: str) -> list:
    if not wanted:
        return tasks
    ids = [w.strip() for w in wanted.split(",") if w.strip()]
    known = {t.id: t for t in tasks}
    missing = [i for i in ids if i not in known]
    if missing:
        raise SuiteError(f"not in the suite: {', '.join(missing)}")
    return [known[i] for i in ids]


def read_records(path: str) -> list:
    with open(path) as f:
        return [json.loads(line) for line in f if line.strip()]


def _reliability_runner():
    """scripts/e2e-reliability.py, for the stack checks it already owns (#241)."""
    spec = importlib.util.spec_from_file_location("atlas_reliability",
                                                  REPO / "scripts" / "e2e-reliability.py")
    mod = importlib.util.module_from_spec(spec)
    # Registered first: its dataclasses resolve their (postponed) annotations
    # through sys.modules, and without the entry the import fails.
    sys.modules[spec.name] = mod
    spec.loader.exec_module(mod)
    return mod


def _head() -> str:
    p = subprocess.run(["git", "-C", str(REPO), "rev-parse", "--short", "HEAD"],
                       capture_output=True, text=True)
    return p.stdout.strip() if p.returncode == 0 else ""


def parse_args(argv):
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(required=True)
    check = sub.add_parser("check", help="every grader's controls separate")
    check.add_argument("suite")
    check.add_argument("--image", required=True, help="the sandbox image graders run in")
    check.set_defaults(func=cmd_check)
    run = sub.add_parser("run", help="run the suite through one arm")
    run.add_argument("suite")
    run.add_argument("--arm", choices=("atlas", "baseline"), required=True)
    run.add_argument("--out", required=True, help="session records are appended here (JSON lines)")
    run.add_argument("--image", required=True, help="the sandbox image graders run in")
    run.add_argument("--compose-project", required=True, help="the eval stack's compose project")
    run.add_argument("--workspace-root", required=True, help="host path the stack mounts as its workspace")
    run.add_argument("--workspace-mount", default="/workspace")
    run.add_argument("--repeats", type=int, default=3)
    run.add_argument("--tasks", default="", help="comma-separated task ids (one block); default all")
    run.add_argument("--budget-s", type=float, default=600.0,
                     help="the stack's session timeout; the baseline gets the same")
    run.add_argument("--context-tokens", type=int, default=0,
                     help="the model's context window; default: the model server's /props")
    run.add_argument("--proxy-url", default="http://127.0.0.1:8090")
    run.add_argument("--llama-url", default="http://127.0.0.1:8080")
    run.add_argument("--sandbox-url", default="http://127.0.0.1:30820")
    run.add_argument("--deploy-dir", default=str(Path.home() / "atlas-ralph"))
    run.add_argument("--commit", default="", help="default: this checkout's HEAD")
    run.set_defaults(func=cmd_run)
    rep = sub.add_parser("report", help="aggregates only, with no task text")
    rep.add_argument("results")
    rep.add_argument("--against", default="", help="the baseline's records, to compare")
    rep.set_defaults(func=cmd_report)
    args = ap.parse_args(argv)
    if getattr(args, "func", None) is cmd_run and not args.commit:
        args.commit = _head()
    return args


if __name__ == "__main__":
    sys.exit(main())
