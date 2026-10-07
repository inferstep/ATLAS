#!/usr/bin/env python3
"""The nightly run on real hardware: a smoke run of the head of `dev`, and the tests that the plain jobs leave out.

One run, started by a timer on the development server, in a folder of its own
(--dir). It:

  1. takes the head of `dev` into <dir>/tree, and pulls the `dev` images;
  2. compares the commit each image was built from with that head: when one
     differs, the result is "stale" and nothing more is run;
  3. starts a stack under a name of its own, on loopback ports of its own;
  4. runs the driver of the repository (scripts/e2e-reliability.py) with three
     fixed tasks, once each, and keeps the seconds of each;
  5. runs the test files that the plain jobs leave out (the `integration`
     mark), and counts how many it collected against the number there are;
  6. writes one report file, <dir>/reports/<start time>/report.json;
  7. stops its stack. Always: also when a step failed or the time ran out.

It takes a lock for the graphics card first. When another run holds the lock,
the report says "not run: the card was in use" and the run ends. A run of
another kind that uses the card takes the same lock:
`flock <lock file> <command>`.

The whole run has one time limit. The run writes only inside its own folder,
and removes nothing: each night has a folder of its own under reports/. It
stops and removes only the containers of its own compose project.

With ATLAS_NIGHTLY_TOKEN and --issue it also puts the report into the text of
that one issue (the text is replaced, no comment is added). The token is read
from the environment only; it is not written to a file, a log or a message.

The script that runs is the one of the night before: the tree is moved to the
head after Python has read this file.

Exit status: 0 when the run passed, and when it did not run because the card
was in use; 1 when it failed or the images were stale; 2 when its settings
cannot be used.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
import re
import secrets
import signal
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

PROJECT = "atlas-nightly"
# The service of the compose file -> the image it runs.
IMAGES = {"llama-server": "atlas-llama", "geometric-lens": "atlas-lens", "v3-service": "atlas-v3",
          "sandbox": "atlas-sandbox", "atlas-proxy": "atlas-proxy"}
# Three small tasks of the driver, one of each kind of work: a function is added, a fault is repaired, a question
# is answered with no change to a file.
TASKS = ("add_function", "offbyone", "ask_explain")
# The tests of the seven files that carry the `integration` mark (docs/quality/gates.md).
EXPECTED_TESTS = 125
# Loopback ports of this stack's own, so that it stands beside a stack on the usual ports.
PORTS = {"ATLAS_LLAMA_PORT": 18080, "ATLAS_LENS_PORT": 18099, "ATLAS_V3_PORT": 18070,
         "ATLAS_SANDBOX_PORT": 18020, "ATLAS_PROXY_PORT": 18090}
REVISION = "org.opencontainers.image.revision"
NOT_RUN = "not run: the card was in use"


class StepFailed(Exception):
    """A step of the run ended in a way that the run cannot go on from."""


class Halted(Exception):
    """The run has to end now: its time limit was reached, or it was told to stop."""


def command(argv: list, limit: float, cwd: Path | None = None, env: dict | None = None) -> subprocess.CompletedProcess:
    """Run a command with a time limit of its own. Its whole process group ends with it, also when this run is stopped."""
    process = subprocess.Popen(argv, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
                               start_new_session=True)
    try:
        out, _ = process.communicate(timeout=limit)
    except BaseException:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        process.wait()
        raise
    return subprocess.CompletedProcess(argv, process.returncode, out, "")


def must(what: str, argv: list, limit: float, **where) -> str:
    """The output of a command that has to pass. A failure names the step, the status and the end of the output."""
    try:
        done = command(argv, limit, **where)
    except subprocess.TimeoutExpired:
        raise StepFailed(f"{what}: no end after {limit:g} s") from None
    except OSError as error:
        raise StepFailed(f"{what}: {argv[0]} could not be started ({error})") from None
    if done.returncode != 0:
        raise StepFailed(f"{what}: status {done.returncode}: {done.stdout.strip()[-400:]}")
    return done.stdout


@contextlib.contextmanager
def card_lock(path: Path):
    """Hold the lock of the graphics card for the run. Gives False when another run holds it."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "a", encoding="utf-8") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            yield False
            return
        try:
            yield True
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


@contextlib.contextmanager
def time_limit(minutes: float):
    """Raise Halted when the block has run this long, and when the run is told to stop."""
    def halt(signum, _frame):
        raise Halted(f"the time limit of {minutes:g} minutes was reached" if signum == signal.SIGALRM
                     else "the run was told to stop")
    old = {name: signal.signal(name, halt) for name in (signal.SIGALRM, signal.SIGTERM)}
    signal.setitimer(signal.ITIMER_REAL, minutes * 60)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        for name, handler in old.items():
            signal.signal(name, handler)


def take_head(args) -> str:
    """Move <dir>/tree to the head of the branch and give its commit."""
    tree = args.dir / "tree"
    if not (tree / ".git").exists():
        must("the first copy of the repository", ["git", "clone", "--quiet", "--branch", args.branch, args.repo_url, str(tree)], 600)
    must("the newest commit of the branch", ["git", "-C", str(tree), "fetch", "--quiet", "origin", args.branch], 300)
    must("the move to that commit", ["git", "-C", str(tree), "checkout", "--quiet", "--detach", "FETCH_HEAD"], 120)
    return must("the commit of the tree", ["git", "-C", str(tree), "rev-parse", "HEAD"], 30).strip()


def pull_images(args) -> dict:
    """Pull the image of each service and give, for each service, its digest and the commit it was built from."""
    found = {}
    for service, name in IMAGES.items():
        ref = f"{args.registry}/{name}:{args.tag}"
        must(f"the pull of {ref}", ["docker", "pull", "--quiet", ref], 1200)
        about = json.loads(must(f"the reading of {ref}", ["docker", "image", "inspect", ref], 60))[0]
        digests = about.get("RepoDigests") or [""]
        found[service] = {"image": ref, "digest": digests[0].split("@")[-1],
                          "commit": ((about.get("Config") or {}).get("Labels") or {}).get(REVISION, "")}
    return found


def stale_images(images: dict, head: str) -> list:
    """The images that were not built from the head, each with the commit it names."""
    return [f"{about['image']} was built from {about['commit'][:12] or 'no named commit'}"
            for about in images.values() if about["commit"] != head]


def compose(args) -> list:
    """The start of every compose command of this run: its own project, its own settings file, the tree's file."""
    tree = args.dir / "tree"
    files = [part for name in ["docker-compose.yml", *args.compose_file] for part in ("-f", str(tree / name))]
    return ["docker", "compose", "-p", PROJECT, "--project-directory", str(tree), "--env-file", str(args.dir / "run.env"), *files]


def write_settings(args) -> dict:
    """Write <dir>/run.env: the server's own settings for the model, then this run's ports, folders and image tag."""
    base = args.dir / "nightly.env"
    if not base.is_file():
        raise StepFailed(f"{base} is not there. It holds the settings of the model for this server (ATLAS_MODELS_DIR, "
                         "ATLAS_MODEL_FILE, ATLAS_MODEL_NAME and the sizes). Fix: write it once, from the .env of the "
                         "stack that runs on this server.")
    for name in ("workspace/_reliability", "secrets", "no-deploy-record"):
        (args.dir / name).mkdir(parents=True, exist_ok=True)
    token = args.dir / "secrets" / "service-token"
    if not token.exists():
        token.touch(mode=0o600)
        token.write_text(secrets.token_hex(32), encoding="utf-8")
    own = {**PORTS, "ATLAS_IMAGE_TAG": args.tag, "ATLAS_PROJECT_DIR": str(args.dir / "workspace"),
           "ATLAS_SECRETS_DIR": str(args.dir / "secrets"), "ATLAS_PROXY_UID": os.getuid(), "ATLAS_PROXY_GID": os.getgid()}
    lines = [base.read_text(encoding="utf-8").rstrip("\n"), "# The settings of the nightly run. They come last, so they hold."]
    (args.dir / "run.env").write_text("\n".join(lines + [f"{key}={value}" for key, value in own.items()]) + "\n", encoding="utf-8")
    return own


def start_stack(args) -> None:
    must("the start of the stack", [*compose(args), "up", "-d", "--wait", "--wait-timeout", str(args.start_seconds)],
         args.start_seconds + 120)


def stop_stack(args, report: dict) -> None:
    """Stop this run's own stack and remove its containers. It is called at the end of every run that held the lock."""
    if not (args.dir / "run.env").is_file() or not (args.dir / "tree" / "docker-compose.yml").is_file():
        report["stack_stopped"] = "there was none: the run ended before its settings were written"
        return
    try:
        done = command([*compose(args), "down", "--volumes", "--remove-orphans"], 300)
        report["stack_stopped"] = "yes" if done.returncode == 0 else f"no: status {done.returncode}: {done.stdout.strip()[-300:]}"
    except (subprocess.TimeoutExpired, OSError) as error:
        report["stack_stopped"] = f"no: {error}"
    if report["stack_stopped"] != "yes":
        # A stack that still runs holds the card, so this is the first thing the result says.
        report["result"] = f"failed: the stack was not stopped ({report['stack_stopped'][4:]}); before that: {report['result']}"


def service_urls() -> dict:
    """Where the driver and the tests find this stack."""
    return {"ATLAS_PROXY_URL": f"http://127.0.0.1:{PORTS['ATLAS_PROXY_PORT']}",
            "SANDBOX_URL": f"http://127.0.0.1:{PORTS['ATLAS_SANDBOX_PORT']}",
            "LLAMA_URL": f"http://127.0.0.1:{PORTS['ATLAS_LLAMA_PORT']}",
            "LENS_URL": f"http://127.0.0.1:{PORTS['ATLAS_LENS_PORT']}"}


def run_environment(args) -> dict:
    return {**os.environ, **service_urls(), "ATLAS_SERVICE_TOKEN_FILE": str(args.dir / "secrets" / "service-token"),
            "PYTHONDONTWRITEBYTECODE": "1"}


def smoke(args, head: str, folder: Path) -> list:
    """Run the driver with the fixed tasks, once each. Gives a row for each task: its seconds, and how it ended."""
    result = folder / "driver.json"
    urls = service_urls()
    done = command([args.python, "scripts/e2e-reliability.py", "--url", urls["ATLAS_PROXY_URL"],
                    "--workspace", str(args.dir / "workspace" / "_reliability"), "--subdir", "_reliability",
                    "--sandbox-container", f"{PROJECT}-sandbox-1", "--compose-project", PROJECT,
                    "--deploy-dir", str(args.dir / "no-deploy-record"), "--commit", head,
                    "--tasks", ",".join(TASKS), "--reps", "1", "--timeout", str(args.task_seconds),
                    "--json", str(result)],
                   args.task_seconds * len(TASKS) + 300, cwd=args.dir / "tree", env=run_environment(args))
    (folder / "driver.log").write_text(done.stdout, encoding="utf-8")
    try:
        rows = json.loads(result.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        raise StepFailed(f"the driver left no result (status {done.returncode}): {done.stdout.strip()[-400:]}") from None
    tasks = [{"task": row["task"], "seconds": row["wall_s"], "passed": bool(row["task_passed"]),
              "defects": len(row.get("defects") or [])} for row in rows]
    missing = [name for name in TASKS if name not in {row["task"] for row in tasks}]
    if missing:
        raise StepFailed(f"the driver gave no result for {', '.join(missing)} (status {done.returncode})")
    return tasks


def left_out_tests(args, folder: Path) -> dict:
    """Run the tests that the plain jobs leave out. Gives the numbers: collected, passed, failed, skipped."""
    result = folder / "tests.xml"
    done = command([args.python, "-m", "pytest", "-m", "integration", "tests/infrastructure", "-q", "-p", "no:cacheprovider",
                    f"--junitxml={result}"], args.test_seconds, cwd=args.dir / "tree", env=run_environment(args))
    (folder / "tests.log").write_text(done.stdout, encoding="utf-8")
    # Only the numbers in the opening line of each suite are read; the file is not given to an XML parser.
    try:
        suites = re.findall(r"<testsuite\b[^>]*>", result.read_text(encoding="utf-8"))
    except OSError:
        suites = []
    if not suites:
        raise StepFailed(f"the tests left no result (status {done.returncode}): {done.stdout.strip()[-400:]}")
    count = {key: sum(int(number) for suite in suites for number in re.findall(rf'\b{key}="(\d+)"', suite))
             for key in ("tests", "failures", "errors", "skipped")}
    failed = count["failures"] + count["errors"]
    return {"expected": EXPECTED_TESTS, "collected": count["tests"], "passed": count["tests"] - failed - count["skipped"],
            "failed": failed, "skipped": count["skipped"]}


def night(args, report: dict, folder: Path) -> None:
    """The steps of one night, in order. Each fills its part of the report."""
    report["step"] = "take the head of the branch"
    head = report["commit"] = take_head(args)
    report["step"] = "pull the images"
    images = report["images"] = pull_images(args)
    report["stale"] = stale_images(images, head)
    if report["stale"]:
        return
    report["step"] = "start the stack"
    write_settings(args)
    start_stack(args)
    report["step"] = "the smoke run"
    report["tasks"] = smoke(args, head, folder)
    report["step"] = "the tests that the plain jobs leave out"
    report["tests"] = left_out_tests(args, folder)
    report["step"] = "done"


def judge(report: dict) -> str:
    """The one line of the result, from what the report holds."""
    if report.get("stale"):
        return "stale: " + "; ".join(report["stale"])
    faults = []
    for task in report.get("tasks") or []:
        if task["defects"]:
            faults.append(f"{task['task']}: {task['defects']} defect(s) of the harness")
    tests = report.get("tests")
    if tests:
        if tests["collected"] != tests["expected"]:
            faults.append(f"{tests['collected']} tests were collected, and there are {tests['expected']}")
        if tests["failed"]:
            faults.append(f"{tests['failed']} test(s) failed")
    return "failed: " + "; ".join(faults) if faults else "passed"


def as_text(report: dict) -> str:
    """The report as the text of an issue."""
    lines = [f"**Nightly run of `dev`: {report['result']}**", "",
             f"- Started: {report['started']}, ended: {report.get('ended', '')}",
             f"- Commit: `{report.get('commit', 'not read')}`",
             f"- The stack was stopped: {report.get('stack_stopped', 'not started')}"]
    if report.get("images"):
        lines += ["", "| Service | Image digest | Built from |", "|---|---|---|"]
        lines += [f"| {service} | `{about['digest']}` | `{about['commit'][:12]}` |" for service, about in report["images"].items()]
    if report.get("tasks"):
        lines += ["", "| Task | Seconds | The change landed | Harness defects |", "|---|---|---|---|"]
        lines += [f"| {t['task']} | {t['seconds']} | {'yes' if t['passed'] else 'no'} | {t['defects']} |" for t in report["tasks"]]
    if report.get("tests"):
        t = report["tests"]
        counts = (f"- Tests that the plain jobs leave out: {t['collected']} collected of {t['expected']}; "
                  f"{t['passed']} passed, {t['failed']} failed, {t['skipped']} skipped")
        lines += ["", counts]
    return "\n".join(lines) + "\n"


def publish(args, text: str) -> str:
    """Put the report into the text of the one issue, when a token is given. Gives what happened, in words."""
    token = os.environ.get("ATLAS_NIGHTLY_TOKEN", "")
    if not token or not args.issue:
        return "not sent: no token or no issue was given"
    request = urllib.request.Request(
        f"{args.api}/repos/{args.repository}/issues/{args.issue}", data=json.dumps({"body": text}).encode(), method="PATCH",
        headers={"Accept": "application/vnd.github+json", "Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return f"sent to issue {args.issue} (status {response.status})"
    except urllib.error.HTTPError as error:
        return f"not sent: GitHub answered {error.code}"
    except (urllib.error.URLError, TimeoutError, OSError) as error:
        return f"not sent: {type(error).__name__}"


def parse(argv: list | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--dir", type=Path, required=True, help="the folder of the nightly run; it writes nowhere else")
    parser.add_argument("--lock", type=Path, default=Path.home() / ".atlas-card.lock", help="the lock file of the graphics card")
    parser.add_argument("--python", default=sys.executable, help="a Python that has pytest and httpx")
    parser.add_argument("--limit-minutes", type=float, default=30, help="the time limit of the whole run")
    parser.add_argument("--start-seconds", type=int, default=600, help="how long the stack may take to be healthy")
    parser.add_argument("--task-seconds", type=int, default=300, help="the time limit of one task of the smoke run")
    parser.add_argument("--test-seconds", type=int, default=600, help="the time limit of the tests")
    parser.add_argument("--repo-url", default="https://github.com/inferstep/ATLAS.git")
    parser.add_argument("--branch", default="dev")
    parser.add_argument("--registry", default="ghcr.io/inferstep")
    parser.add_argument("--tag", default="dev")
    parser.add_argument("--compose-file", action="append", default=[], help="one more compose file of the tree, for another backend")
    parser.add_argument("--repository", default="inferstep/ATLAS", help="the repository of the issue")
    parser.add_argument("--issue", type=int, default=0, help="the issue whose text holds the newest report")
    parser.add_argument("--api", default="https://api.github.com")
    return parser.parse_args(argv)


def main(argv: list | None = None) -> int:
    args = parse(argv)
    if not args.dir.is_absolute() or not args.dir.is_dir():
        print(f"nightly run: --dir {args.dir} is not a full path of a folder that is there. Fix: make the folder and "
              "give its full path.", file=sys.stderr)
        return 2
    started = dt.datetime.now(dt.timezone.utc)
    folder = args.dir / "reports" / started.strftime("%Y%m%dT%H%M%SZ")
    folder.mkdir(parents=True)
    report = {"started": started.strftime("%Y-%m-%dT%H:%M:%SZ"), "step": "wait for the card"}
    with card_lock(args.lock) as held:
        if not held:
            report["result"] = NOT_RUN
        else:
            try:
                with time_limit(args.limit_minutes):
                    night(args, report, folder)
                report["result"] = judge(report)
            except Halted as halted:
                report["result"] = f"failed: {halted}, in the step: {report['step']}"
            except StepFailed as error:
                report["result"] = f"failed: {error}"
            except Exception as error:  # the run still has to stop its stack and write its report
                report["result"] = f"failed: an error of the run itself, in the step: {report['step']}: {error!r}"
            finally:
                stop_stack(args, report)
    report["ended"] = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    report["sent"] = publish(args, as_text(report))
    (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"nightly run: {report['result']}\nreport: {folder / 'report.json'}\nGitHub: {report['sent']}")
    return 0 if report["result"] in ("passed", NOT_RUN) else 1


if __name__ == "__main__":
    sys.exit(main())
