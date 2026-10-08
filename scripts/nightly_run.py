#!/usr/bin/env python3
"""The nightly run on real hardware: a smoke run of the head of `dev`, and the tests that the plain jobs leave out.

One run, started by a timer on the development server, in a folder of its own
(--dir). Only one run of a folder goes at a time. It:

  1. takes the head of `dev` into <dir>/tree. When that changes this very
     file (the run was started from the tree), the new copy does the run;
  2. stops a stack of its own name that an earlier run left;
  3. pulls the `dev` images and compares the commit each was built from with
     the head. The images of a push come some minutes after it, so the run
     looks again for a while. When one still differs, the result is "stale"
     and nothing more is run;
  4. takes the lock of the graphics card, and reads what the card holds;
  5. starts a stack under a name of its own, on loopback ports of its own,
     and asks the services whether they are whole;
  6. runs the driver of the repository (scripts/e2e-reliability.py) with three
     fixed tasks, once each, and keeps the seconds of each;
  7. runs the test files that the plain jobs leave out (the `integration`
     mark), and counts how many it collected against the number there are;
  8. asks the services again, and writes one report file,
     <dir>/reports/<start time>/report.json;
  9. stops its stack. Always: also when a step failed or the time ran out.

Steps 1 to 3 need no card. They come before the lock, and the time limit of
the run does not count them: each of their commands has a limit of its own.

When another run holds the lock of the card, or a process holds the card
itself, the report says "not run: the card was in use" and the run ends. A
run of another kind that uses the card takes the same lock:
`flock <lock file> <command>`.

The run writes only inside its own folder, and removes nothing: each night
has a folder of its own under reports/. It stops and removes only the
containers of its own compose project.

The stack of the run has no service token: the driver and the tests send
none. The run does not start beside a token file in its folder.

With ATLAS_NIGHTLY_TOKEN and --issue it also puts the report into the text of
that one issue (the text is replaced, no comment is added). The token is read
at that one call. No command of the run has it in its environment, it is sent
to GitHub's own address only, and it is not written to a file, a log or a
message.

Exit status: 0 when the run passed, and when it did not run because the card
was in use or another run of the folder was going; 1 when it failed or the
images were stale; 2 when its settings cannot be used.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime as dt
import fcntl
import json
import os
import re
import signal
import subprocess
import sys
import time
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
REPO_URL, BRANCH = "https://github.com/inferstep/ATLAS.git", "dev"
REGISTRY, TAG = "ghcr.io/inferstep", "dev"
REPOSITORY, GITHUB_API = "inferstep/ATLAS", "https://api.github.com"
TOKEN_NAME = "ATLAS_NIGHTLY_TOKEN"
# What a run gives to the copy of this script that it starts in its place: the start time of the run, and the number
# of the open file that holds the lock of the folder.
STARTED_AGAIN = "ATLAS_NIGHTLY_STARTED_AGAIN"
STAMP = "%Y%m%dT%H%M%SZ"
NEW_COPY_LEFT_NO_REPORT = ("failed: taking the head changed the script of the run, the run started the new copy in its "
                           "place, and that copy left no report")
# This file as Python read it when the run started.
AS_STARTED = Path(__file__).read_bytes()
# What the run asks the services about themselves. Each is asked inside its own container, with the tool and at the
# port of its health check. The health check of the lens asks /health, which says that the process serves; /ready
# says whether the lens can score. The proxy's /ready asks the model server, the lens, the sandbox and v3-service.
QUESTIONS = {"the proxy's /ready": ("atlas-proxy", "http://localhost:8090/ready"),
             "the lens's /ready": ("geometric-lens", "http://localhost:8099/ready"),
             "the lens's /health": ("geometric-lens", "http://localhost:8099/health")}
# The parts of the proxy's answer, and what each is called in a result.
PROXY_PARTS = {"inference": "the model server", "lens_ready": "the lens", "sandbox": "the sandbox", "v3": "v3-service"}


class StepFailed(Exception):
    """A step of the run ended in a way that the run cannot go on from."""


class Halted(Exception):
    """The run has to end now: its time limit was reached, or it was told to stop."""


class NewScript(Exception):
    """Taking the head changed the file of this script. The new copy has to do the run."""


def command(argv: list, limit: float, cwd: Path | None = None, env: dict | None = None) -> subprocess.CompletedProcess:
    """Run a command with a time limit of its own. Its whole process group ends with it, also when this run is stopped.

    No command gets the token of the report: it is taken out of the environment that the command starts with.
    """
    given = os.environ if env is None else env
    process = subprocess.Popen(argv, cwd=cwd, env={name: value for name, value in given.items() if name != TOKEN_NAME},
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, start_new_session=True)
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


def kept_open(path: Path, number: int | None):
    """The open file of this number, when it is the lock file: a run keeps it open for the copy that it starts."""
    try:
        if number is not None and os.path.samestat(os.fstat(number), os.stat(path)):
            return os.fdopen(number, "a", encoding="utf-8")
    except OSError:
        pass
    return None


@contextlib.contextmanager
def lock(path: Path, kept: int | None = None):
    """Hold a lock file for the block. Gives the open file, or None when another process holds the lock.

    The lock belongs to the open file. A copy of the script that a run starts in its place gets the number of that
    file (`kept`) and goes on holding the same lock, so no other run can take it in between.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    with kept_open(path, kept) or open(path, "a", encoding="utf-8") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            yield None
            return
        try:
            yield handle
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


@contextlib.contextmanager
def halt_on(signum: int, why: str):
    """Raise Halted in the block when the signal comes."""
    def halt(_signum, _frame):
        raise Halted(why)
    old = signal.signal(signum, halt)
    try:
        yield
    finally:
        signal.signal(signum, old)


@contextlib.contextmanager
def time_limit(minutes: float):
    """Raise Halted when the block has run this long."""
    with halt_on(signal.SIGALRM, f"the time limit of {minutes:g} minutes was reached"):
        signal.setitimer(signal.ITIMER_REAL, minutes * 60)
        try:
            yield
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)


def now_in(report: dict, step: str) -> None:
    """Write down the step that the run is in: a run that ends early names it in its result."""
    report["step"] = step


# --- the steps that need no card ---------------------------------------------------------------------------------

def take_head(args) -> str:
    """Move <dir>/tree to the head of the branch and give its commit."""
    tree = args.dir / "tree"
    if not (tree / ".git").exists():
        must("the first copy of the repository", ["git", "clone", "--quiet", "--branch", BRANCH, REPO_URL, str(tree)], 600)
    must("the newest commit of the branch", ["git", "-C", str(tree), "fetch", "--quiet", "origin", BRANCH], 300)
    must("the move to that commit", ["git", "-C", str(tree), "checkout", "--quiet", "--detach", "FETCH_HEAD"], 120)
    return must("the commit of the tree", ["git", "-C", str(tree), "rev-parse", "HEAD"], 30).strip()


def script_changed(args) -> bool:
    """Whether taking the head changed this very file: the run was started from the tree, and the file holds other bytes now."""
    own = Path(__file__).resolve()
    try:
        return own == (args.dir / "tree" / "scripts" / "nightly_run.py").resolve() and own.read_bytes() != AS_STARTED
    except OSError:
        return False


def start_the_new_copy(argv: list, stamp: str, report: dict, folder: Path, held) -> None:
    """Replace this run by the tree's copy of the script. It gets the same arguments and the start time of the run.

    The lock of the folder stays held: its file stays open across the start, and the new copy gets its number.
    """
    report["result"] = NEW_COPY_LEFT_NO_REPORT
    (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    sys.stdout.flush()
    os.set_inheritable(held.fileno(), True)
    os.execve(sys.executable, [sys.executable, str(Path(__file__).resolve()), *argv],
              {**os.environ, STARTED_AGAIN: f"{stamp} {held.fileno()}"})


def stop_a_left_stack(args) -> str:
    """Stop a stack of this run's name that an earlier run left. Gives what was found, in words.

    A run that was killed leaves its stack, and Docker starts that stack again. Only one run of the folder goes at a
    time, so a stack of this name that is there now is such a stack.
    """
    left = must("the look for a stack that an earlier run left",
                ["docker", "ps", "--all", "--quiet", "--filter", f"label=com.docker.compose.project={PROJECT}"], 60).split()
    if not left:
        return "none"
    write_settings(args)
    must("the stop of the stack that an earlier run left", [*compose(args), "down", "--volumes", "--remove-orphans"], 300)
    return f"{len(left)} container(s) of an earlier run were stopped first"


def pull_images() -> dict:
    """Pull the image of each service and give, for each service, its digest and the commit it was built from."""
    found = {}
    for service, name in IMAGES.items():
        ref = f"{REGISTRY}/{name}:{TAG}"
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


def images_of_the_head(args, report: dict) -> None:
    """Pull the images until each was built from the head, or the wait is over.

    The images of a push come some minutes after it, and a newer push can end their build. So the run looks ten more
    times, and takes the head again each time.
    """
    looks = 10 if args.image_wait_minutes > 0 else 0
    pause = args.image_wait_minutes * 60 / 10
    for look in range(looks + 1):
        now_in(report, "pull the images")
        report["images"] = pull_images()
        report["stale"] = stale_images(report["images"], report["commit"])
        report["waited_for_images"] = f"{look * pause:g} s"
        if not report["stale"] or look == looks:
            return
        now_in(report, "wait for the images of the head")
        time.sleep(pause)
        now_in(report, "take the head of the branch")
        report["commit"] = take_head(args)


def before_the_card(args, report: dict, first_start: bool) -> None:
    """The steps that need no card: the head, a stack that was left, the images. No lock is held and no limit runs."""
    now_in(report, "take the head of the branch")
    report["commit"] = take_head(args)
    if first_start and script_changed(args):
        raise NewScript
    now_in(report, "stop a stack that an earlier run left")
    report["left_stack"] = stop_a_left_stack(args)
    images_of_the_head(args, report)


# --- the steps that need the card --------------------------------------------------------------------------------

def card_use() -> tuple:
    """What the graphics card holds now: whether a process computes on it, and the amount in words.

    A stack that simply runs holds the card and takes no lock. A card that cannot be read counts as free, and the
    words say why it was not read.
    """
    try:
        done = command(["nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"], 60)
    except (subprocess.TimeoutExpired, OSError) as error:
        return False, f"not read: nvidia-smi gave no answer ({error})"
    if done.returncode != 0:
        return False, f"not read: nvidia-smi ended with status {done.returncode}"
    rows = [row for row in (line.split(",") for line in done.stdout.splitlines()) if len(row) == 2 and row[0].strip().isdigit()]
    if not rows:
        return False, "no process computes on it"
    amount = sum(int(row[1]) for row in rows if row[1].strip().isdigit())
    return True, f"{len(rows)} process(es) hold {amount} MiB"


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
    if token.exists():
        raise StepFailed(f"{token} is there. With a token file the services of the stack ask for the token, and the "
                         "driver and the tests send none, so every session and most tests would be refused. Fix: "
                         "remove that file.")
    # The proxy and the sandbox mount the same workspace, so both run as the user of this run.
    own = {**PORTS, "ATLAS_IMAGE_TAG": TAG, "ATLAS_PROJECT_DIR": str(args.dir / "workspace"),
           "ATLAS_SECRETS_DIR": str(args.dir / "secrets"), "ATLAS_PROXY_UID": os.getuid(), "ATLAS_PROXY_GID": os.getgid(),
           "ATLAS_SANDBOX_UID": os.getuid(), "ATLAS_SANDBOX_GID": os.getgid()}
    lines = [base.read_text(encoding="utf-8").rstrip("\n"), "# The settings of the nightly run. They come last, so they hold."]
    (args.dir / "run.env").write_text("\n".join(lines + [f"{key}={value}" for key, value in own.items()]) + "\n", encoding="utf-8")
    return own


def start_stack(args) -> None:
    must("the start of the stack", [*compose(args), "up", "-d", "--wait", "--wait-timeout", str(args.start_seconds)],
         args.start_seconds + 120)


def stop_stack(args, report: dict) -> None:
    """Stop this run's own stack and remove its containers. It is called at the end of every run that took the card."""
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
        report["result"] = f"failed: the stack was not stopped ({report['stack_stopped'][4:]}); before that: {report.get('result')}"


def read_answer(printed: str) -> dict:
    """The answer of a service from what curl printed: the body, and the status on the last line.

    Lines before the body (a warning of compose) are not part of it.
    """
    lines = [line for line in printed.splitlines() if line.strip()]
    try:
        return {"status": int(lines[-1]), "answer": json.loads(lines[-2])}
    except (IndexError, ValueError):
        return {"status": 0, "answer": f"no answer that can be read: {printed.strip()[-200:]}"}


def ask(args, service: str, url: str) -> dict:
    """Ask a service of this run's stack one question, inside its container. Gives the status and the answer."""
    try:
        done = command([*compose(args), "exec", "-T", service, "curl", "-s", "-m", "60", "-w", "\n%{http_code}", url], 90)
    except (subprocess.TimeoutExpired, OSError) as error:
        return {"status": 0, "answer": f"no answer: {error}"}
    return read_answer(done.stdout)


def not_whole(answers: dict) -> list:
    """What the answers say is wrong with the services, in words. An empty list: each service is whole."""
    faults = []
    proxy = answers["the proxy's /ready"]
    if not isinstance(proxy["answer"], dict):
        faults.append(f"the proxy gave no answer about the services ({proxy['answer']})")
    else:
        for part, name in PROXY_PARTS.items():
            if proxy["answer"].get(part) is not True:
                why = f" ({proxy['answer']['lens_reason']})" if part == "lens_ready" and proxy["answer"].get("lens_reason") else ""
                faults.append(f"the proxy says that {name} is not ready{why}")
    lens = answers["the lens's /ready"]
    if lens["status"] != 200:
        said = lens["answer"].get("detail") if isinstance(lens["answer"], dict) else None
        why = said.get("reason") if isinstance(said, dict) else lens["answer"]
        faults.append(f"the lens says that it cannot score ({why})")
    return faults


def ask_the_services(args, settle_seconds: float) -> tuple:
    """Ask the services until each is whole, or the time to settle is over. Gives the last answers and what is wrong.

    A lens that started while the model server still loaded tries its self test again when it is asked.
    """
    looks = 10 if settle_seconds > 0 else 0
    answers, faults = {}, []
    for look in range(looks + 1):
        answers = {name: ask(args, service, url) for name, (service, url) in QUESTIONS.items()}
        faults = not_whole(answers)
        if not faults or look == looks:
            break
        time.sleep(settle_seconds / 10)
    return answers, faults


def service_urls() -> dict:
    """Where the driver and the tests find this stack."""
    return {"ATLAS_PROXY_URL": f"http://127.0.0.1:{PORTS['ATLAS_PROXY_PORT']}",
            "SANDBOX_URL": f"http://127.0.0.1:{PORTS['ATLAS_SANDBOX_PORT']}",
            "LLAMA_URL": f"http://127.0.0.1:{PORTS['ATLAS_LLAMA_PORT']}",
            "LENS_URL": f"http://127.0.0.1:{PORTS['ATLAS_LENS_PORT']}"}


def run_environment() -> dict:
    return {**os.environ, **service_urls(), "PYTHONDONTWRITEBYTECODE": "1"}


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
                   args.task_seconds * len(TASKS) + 300, cwd=args.dir / "tree", env=run_environment())
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
                    f"--junitxml={result}"], args.test_seconds, cwd=args.dir / "tree", env=run_environment())
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


def with_the_card(args, report: dict, folder: Path) -> None:
    """The steps that need the card, in order: the stack, the sessions, the tests. Each fills its part of the report."""
    now_in(report, "start the stack")
    write_settings(args)
    start_stack(args)
    now_in(report, "ask the services whether they are whole")
    answers, faults = ask_the_services(args, args.settle_seconds)
    report["services"] = {"at the start": answers}
    if faults:
        raise StepFailed("a service was not whole when the stack had started: " + "; ".join(faults))
    now_in(report, "the smoke run")
    report["tasks"] = smoke(args, report["commit"], folder)
    now_in(report, "the tests that the plain jobs leave out")
    report["tests"] = left_out_tests(args, folder)
    now_in(report, "ask the services again")
    report["services"]["at the end"], report["not_whole_at_the_end"] = ask_the_services(args, 0)
    now_in(report, "done")


# --- the result --------------------------------------------------------------------------------------------------

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
    faults += [f"at the end of the run {fault}" for fault in report.get("not_whole_at_the_end") or []]
    return "failed: " + "; ".join(faults) if faults else "passed"


@contextlib.contextmanager
def failures_into(report: dict):
    """A step that fails ends the block, and the result of the report says why."""
    try:
        yield
    except NewScript:
        raise
    except Halted as halted:
        report["result"] = f"failed: {halted}, in the step: {report['step']}"
    except StepFailed as error:
        report["result"] = f"failed: {error}"
    except Exception as error:  # the run still has to stop its stack and write its report
        report["result"] = f"failed: an error of the run itself, in the step: {report['step']}: {error!r}"


def one_night(args, report: dict, folder: Path, first_start: bool) -> None:
    """The whole run. It leaves the one line of its result in the report."""
    with failures_into(report):
        before_the_card(args, report, first_start)
        if report["stale"]:
            report["result"] = judge(report)
    if "result" in report:
        return
    now_in(report, "take the lock of the card")
    with lock(args.lock) as held:
        if not held:
            report["result"] = NOT_RUN
            return
        now_in(report, "read what the card holds")
        in_use, report["card"] = card_use()
        if in_use:
            report["result"] = f"{NOT_RUN} ({report['card']})"
            return
        try:
            with failures_into(report), time_limit(args.limit_minutes):
                with_the_card(args, report, folder)
                report["result"] = judge(report)
        finally:
            stop_stack(args, report)


def lens_account(report: dict) -> str:
    """What the lens said about itself at the start, in a few words. A lens with no calibration scores, and no threshold acts."""
    health = ((report.get("services") or {}).get("at the start") or {}).get("the lens's /health") or {}
    answer = health.get("answer")
    lens = ((answer.get("subsystems") or {}).get("lens") if isinstance(answer, dict) else None)
    if not isinstance(lens, dict):
        return "not read"
    words = {True: "yes", False: "no"}
    return "; ".join(f"{said}: {words.get(lens.get(key), 'not said')}" for said, key in (
        ("self test passed", "self_test_pass"), ("C(x) calibrated", "cx_calibrated"), ("G(x) calibrated", "gx_calibrated")))


def as_text(report: dict) -> str:
    """The report as the text of an issue."""
    at_the_end = "; ".join(report.get("not_whole_at_the_end") or []) or "whole"
    lines = [f"**Nightly run of `dev`: {report['result']}**", "",
             f"- Started: {report['started']}, ended: {report.get('ended', '')}",
             f"- Commit: `{report.get('commit', 'not read')}`",
             f"- Waited for the images of that commit: {report.get('waited_for_images', 'not reached')}",
             f"- A stack that an earlier run left: {report.get('left_stack', 'not looked for')}",
             f"- The card before the start: {report.get('card', 'not read')}",
             f"- The services at the end of the run: {at_the_end if 'not_whole_at_the_end' in report else 'not asked'}",
             f"- The lens about itself: {lens_account(report)}",
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
    token = os.environ.get(TOKEN_NAME, "")
    if not token or not args.issue:
        return "not sent: no token or no issue was given"
    # The token goes to GitHub's own address, or to a port of this machine (the stand-in of a test). Nowhere else.
    if args.api != GITHUB_API and not re.fullmatch(r"http://127\.0\.0\.1:\d{1,5}", args.api):
        return f"not sent: the token goes only to {GITHUB_API}, and --api names another address"
    request = urllib.request.Request(
        f"{args.api}/repos/{REPOSITORY}/issues/{args.issue}", data=json.dumps({"body": text}).encode(), method="PATCH",
        headers={"Accept": "application/vnd.github+json", "Authorization": f"Bearer {token}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return f"sent to issue {args.issue} (status {response.status})"
    except urllib.error.HTTPError as error:
        return f"not sent: GitHub answered {error.code}"
    except (urllib.error.URLError, TimeoutError, OSError) as error:
        return f"not sent: {type(error).__name__}"


def parse(argv: list) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--dir", type=Path, required=True, help="the folder of the nightly run; it writes nowhere else")
    parser.add_argument("--lock", type=Path, default=Path.home() / ".atlas-card.lock", help="the lock file of the graphics card")
    parser.add_argument("--python", default=sys.executable, help="a Python that has pytest and httpx")
    parser.add_argument("--limit-minutes", type=float, default=30, help="the time limit of the stack, the sessions and the tests")
    parser.add_argument("--image-wait-minutes", type=float, default=20, help="how long the run looks again for the images of the head")
    parser.add_argument("--start-seconds", type=int, default=600, help="how long the stack may take to be healthy")
    parser.add_argument("--settle-seconds", type=float, default=60, help="how long the services may take to be whole after that")
    parser.add_argument("--task-seconds", type=int, default=300, help="the time limit of one task of the smoke run")
    parser.add_argument("--test-seconds", type=int, default=600, help="the time limit of the tests")
    parser.add_argument("--compose-file", action="append", default=[], help="one more compose file of the tree, for another backend")
    parser.add_argument("--issue", type=int, default=0, help="the issue whose text holds the newest report")
    parser.add_argument("--api", default=GITHUB_API, help="the address of GitHub's API; another address gets no token")
    return parser.parse_args(argv)


def start_time() -> tuple:
    """The start time of the run, whether this is its first start, and the open file of the folder's lock.

    A copy that a run started in its place gets that run's time and the number of the file that holds the lock.
    """
    stamp, _, number = os.environ.pop(STARTED_AGAIN, "").partition(" ")
    try:
        return dt.datetime.strptime(stamp, STAMP).replace(tzinfo=dt.timezone.utc), False, int(number)
    except ValueError:
        return dt.datetime.now(dt.timezone.utc), True, None


def main(argv: list | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    args = parse(argv)
    started, first_start, kept = start_time()
    if not args.dir.is_absolute() or not args.dir.is_dir():
        print(f"nightly run: --dir {args.dir} is not a full path of a folder that is there. Fix: make the folder and "
              "give its full path.", file=sys.stderr)
        return 2
    folder = args.dir / "reports" / started.strftime(STAMP)
    report = {"started": started.strftime("%Y-%m-%dT%H:%M:%SZ"), "step": "start",
              "script": "as it was started" if first_start else "the new copy: taking the head changed the script"}
    # The lock of the folder comes first: a second run that looked for "a stack that an earlier run left" before it
    # held this lock would stop the stack of a run that is in its middle.
    with lock(args.dir / "run.lock", kept) as alone:
        if not alone:
            print("nightly run: another run of this folder is going. This one did nothing.")
            return 0
        folder.mkdir(parents=True, exist_ok=not first_start)
        try:
            with halt_on(signal.SIGTERM, "the run was told to stop"), failures_into(report):
                one_night(args, report, folder, first_start)
        except NewScript:
            start_the_new_copy(argv, started.strftime(STAMP), report, folder, alone)
        report["ended"] = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        report["sent"] = publish(args, as_text(report))
        (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"nightly run: {report['result']}\nreport: {folder / 'report.json'}\nGitHub: {report['sent']}")
    return 0 if report["result"] == "passed" or report["result"].startswith(NOT_RUN) else 1


if __name__ == "__main__":
    sys.exit(main())
