#!/usr/bin/env python3
"""The nightly run on real hardware: a smoke run of the head of `dev`, and the tests that need a real model.

One run, started by a timer on the development server, in a folder of its own
(--dir). Only one run of a folder goes at a time. It:

  1. takes the head of `dev` into <dir>/tree. When that changes this very
     file or the judge beside it (the run was started from the tree), the
     new copy does the run;
  2. stops a stack of its own name that an earlier run left;
  3. pulls the `dev` images and compares the commit each was built from with
     the head. The images of a push come some minutes after it, so the run
     looks again for a while. When one still differs, the night is "not
     run" and nothing more is done;
  4. takes the lock of the graphics card, and reads what the card holds;
  5. starts a stack under a name of its own, on loopback ports of its own,
     and asks the services whether they are whole;
  6. runs the driver of the repository (scripts/e2e-reliability.py) with three
     fixed tasks, once each, and keeps the seconds of each;
  7. runs the tests that need a real model, and judges the result file with
     the judge of such runs (scripts/tests_counted.py): every one of them is
     to be collected, to pass, and not to be skipped;
  8. asks the services again, and writes one report file,
     <dir>/reports/<start time>/report.json;
  9. stops its stack. Always: also when a step failed or the time ran out.

Steps 1 to 3 need no card. They come before the lock, and the time limit of
the run does not count them: each of their commands has a limit of its own.

When another run holds the lock of the card, or a process holds the card
itself, the report says "not run: the card was in use" and the run ends. A
run of another kind that uses the card takes the same lock:
`flock <lock file> <command>`.

The run is best effort. A night with no run (the card was in use, the images
were not of the head yet, or the server was off) is not a failure: no check
of the repository waits for it.

The run writes only inside its own folder, and removes nothing: each night
has a folder of its own under reports/. It stops and removes only the
containers of its own compose project.

The stack of the run has no service token: the driver and the tests send
none. The run does not start beside a token file in its folder.

The result of a run goes to GitHub as a status on the commit that ran:
`server/nightly` for a night, `server/smoke` for the smoke run of one commit.
Green for passed, red for failed, none for a run that did not take place.
The status is written with the key of an app that can only write statuses.
The key is a file of the server (--status-key), outside the folder of the
run. openssl signs with it and gets it as the path of that file: this
script never reads the key. A status that cannot be sent waits in
<dir>/to-send. Sending changes neither the result of a run nor its exit
status.

With --tick the run is one look by the server's timer: it sends what
waits; then, when the card is free, it runs the night in its hour, once a
day, or else the tip of one branch `smoke/<name>`; else it does nothing and
leaves no report. With --tick --look it says what is due and starts nothing
(status 0: a run is due; 3: none). The rules of a look, the files of the
timer (scripts/server/) and the steps on the server are on the gates page,
docs/quality/gates.md, sections "The result on GitHub" and "The timer". The
server only calls out: nothing of this listens.

With --commit <id> it is the smoke run of one commit of a branch, for a pull
request that changes text every request carries (scripts/smoke_result.py
asks for its result). A look of the timer starts it, or a maintainer by
hand. Then it:

  - takes only what a maintainer has pushed under the name `smoke/`: the
    commit has to be the tip of a branch `smoke/<name>` of the repository.
    The run fetches those branches and no other name. A fetch of a commit
    by its id shows nothing: it also gives the head of a pull request from
    outside. And a branch of another name can be one that a program
    pushes. This run builds and starts what the commit holds, with Docker,
    on the server. It deletes nothing on GitHub: its output gives the
    command that removes the branch;
  - takes the head of `dev` as in a night. The commit has to stand on it;
  - never runs a file of the commit on the server itself. This script, the
    driver and its modules, the compose files, the script that makes the
    mark, the model server's folder and the files that the stack mounts stay
    those of `dev`. A commit that changes one of them is not run, and the
    result names the file;
  - builds the image of each service whose build folder the commit changes,
    under a name of this run's own, each build with a time limit, and pulls
    the other images as built from the head of `dev`. When no build folder
    differs, it builds nothing and says so;
  - runs the three sessions, and no tests;
  - removes the images that it built, by their own names, and nothing else.
    The copy of the commit's build folders stays in the folder of the run;
  - prints one result line for the person who started it, with the mark
    from the copy of scripts/smoke_result.py that `dev` has. When the
    commit's mark is not the one of `dev` and the proxy is not built from
    the commit, the run does not take place: no session would carry the
    text that the line names.

Exit status: 0 when the run passed, and when it did not run (the card was in
use, the images were not of the head, another run of the folder was going,
the commit is not one that the run takes, a look found nothing due); 1 when
it failed; 2 when its settings cannot be used; 3 when a look with --look
found nothing due.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import datetime as dt
import fcntl
import http.client
import importlib.util
import json
import os
import re
import signal
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

PROJECT = "atlas-nightly"
# The service of the compose file -> the image it runs.
IMAGES = {"llama-server": "atlas-llama", "geometric-lens": "atlas-lens", "v3-service": "atlas-v3",
          "sandbox": "atlas-sandbox", "atlas-proxy": "atlas-proxy"}
# What a run for one commit can build, as the compose file builds it: for each service the folder that is the
# context of its build, its Dockerfile, and the paths of the repository that the build reads. The model server is
# not here: its image is never built on the server.
BUILDS = {"geometric-lens": ("geometric-lens", "geometric-lens/Dockerfile", ("geometric-lens/",)),
          "v3-service": (".", "v3-service/Dockerfile", ("v3-service/", ".dockerignore")),
          "sandbox": ("sandbox", "sandbox/Dockerfile", ("sandbox/",)),
          "atlas-proxy": ("proxy", "proxy/Dockerfile", ("proxy/",))}
# What a run takes from the tree of `dev` and runs or mounts on the server itself, each with what it is. A name
# that ends with a slash is a folder. The compose files of the run come on top (taken_from_dev). A run for one
# commit never takes one of these from the commit, so a commit that changes one is not run.
FROM_DEV = {"scripts/nightly_run.py": "the script of this run",
            "scripts/server/": "the files of the timer, which run on the server itself",
            "scripts/tests_counted.py": "the judge of the tests, which the script loads on the server itself",
            "scripts/e2e-reliability.py": "the driver of the sessions, which runs on the server itself",
            "scripts/code_quality.py": "a module of the driver, which runs on the server itself",
            "scripts/reliability_report.py": "a module of the driver, which runs on the server itself",
            "scripts/reliability_stream.py": "a module of the driver, which runs on the server itself",
            "scripts/smoke_result.py": "the script that makes the mark of the result line",
            "inference/": "the model server, whose image is never built on the server and whose start script the "
                          "stack mounts from the tree",
            "geometric-lens/geometric_lens/models/": "the model files of the lens, which the stack mounts"}
# A run for one commit takes only the tip of a branch of this name: what a maintainer has pushed for a smoke run.
# No workflow starts on a push to such a branch, and no program pushes one.
SMOKE = "smoke/"
# The names of this run's own for the smoke branches that it fetches, and for the images that it builds. An image
# name that starts with `localhost/` is in no registry, so Docker cannot pull another image under it.
BRANCHES, OWN_IMAGES = "refs/nightly/smoke/", "localhost/atlas-nightly"
# The service that sends the text which the mark is made from.
SENDS_THE_TEXT = "atlas-proxy"
RESULT_LINE = ("Smoke run on {commit}: {good} of {tasks} sessions with no harness defect, {seconds} s; the changed "
               "text is part of every request (text {mark}).")
# Three small tasks of the driver, one of each kind of work: a function is added, a fault is repaired, a question
# is answered with no change to a file.
TASKS = ("add_function", "offbyone", "ask_explain")
# The tests of the seven files that carry the `integration` mark (docs/quality/gates.md).
# Loopback ports of this stack's own, so that it stands beside a stack on the usual ports.
PORTS = {"ATLAS_LLAMA_PORT": 18080, "ATLAS_LENS_PORT": 18099, "ATLAS_V3_PORT": 18070,
         "ATLAS_SANDBOX_PORT": 18020, "ATLAS_PROXY_PORT": 18090}
REVISION = "org.opencontainers.image.revision"
NOT_RUN = "not run: the card was in use"
STALE = "not run: the images are not of the head of the branch yet"
REPO_URL, BRANCH = "https://github.com/inferstep/ATLAS.git", "dev"
REGISTRY, TAG = "ghcr.io/inferstep", "dev"
REPOSITORY, GITHUB_API = "inferstep/ATLAS", "https://api.github.com"
# The status that a run writes on the commit that ran, by the kind of run. Each name starts with `server/`: no
# required check has such a name.
STATUS = {"night": "server/nightly", "one commit": "server/smoke"}
# Where a status waits until it is sent, where it lies when it is sent, and where one goes that was tried for too
# long. All three are folders of the run's own folder.
WAITING, SENT, GIVEN_UP, KEEP_TRYING_DAYS = "to-send", "sent", "not-sent", 14
# The file of the run's folder that holds the time when a look found a process on the card with no lock, and for how
# many minutes after it the looks start nothing.
CARD_HELD, QUIET_MINUTES = "card-held-with-no-lock", 60
# The name of a smoke branch that the timer runs: one part after `smoke/`, of letters, digits, `.`, `_` and `-`.
SMOKE_BRANCH = re.compile(r"smoke/[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
# The steps of a run, as the report names the one that a run ended in. A status names a step only from this list.
STEPS = ("start", "take the head of the branch", "stop a stack that an earlier run left", "pull the images",
         "wait for the images of the head", "take the lock of the card", "read what the card holds", "start the stack",
         "ask the services whether they are whole", "the smoke run", "the tests that need a real model",
         "ask the services again", "done", "look for the commit at the tip of a smoke branch",
         "read what the commit changes", "make the mark of the commit's text", "build the images of the commit")
# What a run gives to the copy of this script that it starts in its place: the start time of the run, and the number
# of the open file that holds the lock of the folder.
STARTED_AGAIN = "ATLAS_NIGHTLY_STARTED_AGAIN"
# The exit status of a look that starts nothing (--look) and found nothing due.
NOTHING_DUE = 3
STAMP = "%Y%m%dT%H%M%SZ"
NEW_COPY_LEFT_NO_REPORT = ("failed: taking the head changed the script of the run, the run started the new copy in its "
                           "place, and that copy left no report")
# The judge of a run of tests that must all run, and the list of such tests: the file beside this one.
JUDGE = Path(__file__).resolve().with_name("tests_counted.py")


def load_the_judge():
    spec = importlib.util.spec_from_file_location("atlas_tests_counted", JUDGE)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


counted = load_the_judge()
# The tests of the run: the group that needs a real model.
TESTS = counted.GROUPS["model"]
# This file and the judge as Python read them when the run started.
AS_STARTED = {path: path.read_bytes() for path in (Path(__file__).resolve(), JUDGE)}
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


class NotRun(Exception):
    """The run does not take place, and that is no failure. The words are the reason and the way."""


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


def ended(what: str, argv: list, limit: float, **where) -> subprocess.CompletedProcess:
    """A command that has to end in its time. Its status is the caller's to read."""
    try:
        return command(argv, limit, **where)
    except subprocess.TimeoutExpired:
        raise StepFailed(f"{what}: no end after {limit:g} s") from None
    except OSError as error:
        raise StepFailed(f"{what}: {argv[0]} could not be started ({error})") from None


def must(what: str, argv: list, limit: float, **where) -> str:
    """The output of a command that has to pass. A failure names the step, the status and the end of the output."""
    done = ended(what, argv, limit, **where)
    if done.returncode != 0:
        raise StepFailed(f"{what}: status {done.returncode}: {done.stdout.strip()[-400:]}")
    return done.stdout


def kept_open(path: Path, number: int | None):
    """The open file of this number, when it is the lock file: a run keeps it open for the copy that it starts."""
    if number is None:
        return None
    try:
        is_the_lock_file = os.path.samestat(os.fstat(number), os.stat(path))
    except OSError:
        # The number is no open file of this process, or the lock file is not there: the caller opens the file afresh.
        return None
    return os.fdopen(number, "a", encoding="utf-8") if is_the_lock_file else None


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
    """Whether taking the head changed this very file or the judge beside it: the run was started from the tree, and one
    of the two holds other bytes now."""
    own = Path(__file__).resolve()
    try:
        return (own == (args.dir / "tree" / "scripts" / "nightly_run.py").resolve()
                and any(path.read_bytes() != was for path, was in AS_STARTED.items()))
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
    # A look of the timer has chosen what to run. The new copy runs that, and does not choose again.
    chosen = f" {report['commit']}" if report.get("mode") == "one commit" else ""
    os.execve(sys.executable, [sys.executable, str(Path(__file__).resolve()), *argv],
              {**os.environ, STARTED_AGAIN: f"{stamp} {held.fileno()}{chosen}"})


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


def pull_images(services=tuple(IMAGES)) -> dict:
    """Pull the image of each service and give, for each service, its digest and the commit it was built from."""
    found = {}
    for service, name in ((service, IMAGES[service]) for service in services):
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
    # The images that a run for one commit built come last, so they hold. That file is written by the run itself.
    files += ["-f", str(args.own_images)] if args.own_images else []
    return ["docker", "compose", "-p", PROJECT, "--project-directory", str(tree), "--env-file", str(args.dir / "run.env"), *files]


def as_compose_reads(value: str) -> str:
    """The value of a line of a settings file as compose reads it: without its quotes, and without a comment after it."""
    value = value.strip()
    if value[:1] in ("'", '"'):
        end = value.find(value[0], 1)
        return value[1:end] if end > 0 else value[1:]
    return re.split(r"\s#", value, maxsplit=1)[0].strip()


def model_name(args) -> str:
    """The name of the model of this server, from <dir>/nightly.env. One of the tests compares the served model with
    it, and skips without it."""
    base = args.dir / "nightly.env"
    if not base.is_file():
        raise StepFailed(f"{base} is not there. It holds the settings of the model for this server (ATLAS_MODELS_DIR, "
                         "ATLAS_MODEL_FILE, ATLAS_MODEL_NAME and the sizes). Fix: write it once, from the .env of the "
                         "stack that runs on this server.")
    given = re.findall(r"^[ \t]*(?:export[ \t]+)?ATLAS_MODEL_NAME[ \t]*=(.*)$", base.read_text(encoding="utf-8"), re.M)
    name = as_compose_reads(given[-1]) if given else ""
    if not name:
        raise StepFailed(f"{base} has no line `ATLAS_MODEL_NAME=<name>`. The model server does not start without the name "
                         "of its model, and a test compares the served model with it. Fix: add the line, from the .env "
                         "of the stack that runs on this server.")
    return name


def write_settings(args) -> dict:
    """Write <dir>/run.env: the server's own settings for the model, then this run's ports, folders and image tag."""
    base = args.dir / "nightly.env"
    model_name(args)
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


def run_environment(args) -> dict:
    """What the driver and the tests get: where the stack is, and the name of the model that it serves."""
    return {**os.environ, **service_urls(), "ATLAS_MODEL_NAME": model_name(args), "PYTHONDONTWRITEBYTECODE": "1"}


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


def model_tests(args, folder: Path) -> dict:
    """Run the tests that need a real model. Gives the numbers of the result file, as the judge of such runs reads them."""
    result = folder / "tests.xml"
    done = command([args.python, "-m", "pytest", "-m", "integration", *TESTS.files, "-q", "-p", "no:cacheprovider",
                    f"--junitxml={result}"], args.test_seconds, cwd=args.dir / "tree", env=run_environment(args))
    (folder / "tests.log").write_text(done.stdout, encoding="utf-8")
    try:
        numbers = counted.read(result.read_text(encoding="utf-8"))
    except (OSError, counted.Unreadable):
        raise StepFailed(f"the tests left no result (status {done.returncode}): {done.stdout.strip()[-400:]}") from None
    return {"expected": TESTS.expected, **numbers}


def with_the_card(args, report: dict, folder: Path) -> None:
    """The steps that need the card, in order: the stack, the sessions, the tests. Each fills its part of the report.

    A run for one commit runs no tests.
    """
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
    if not args.commit:
        now_in(report, "the tests that need a real model")
        report["tests"] = model_tests(args, folder)
    now_in(report, "ask the services again")
    report["services"]["at the end"], report["not_whole_at_the_end"] = ask_the_services(args, 0)
    now_in(report, "done")


# --- one commit of a branch ---------------------------------------------------------------------------------------

def under(name: str, path: str) -> bool:
    """Whether a file of the repository is this path, or lies in it when the path is a folder (it ends with a slash)."""
    return name.startswith(path) if path.endswith("/") else name == path


def pushed_for_a_smoke_run(args, commit: str) -> list:
    """Hold that the commit is the tip of a branch `smoke/<name>` of the repository. Gives the names of those branches.

    Only those branches are fetched, under names of this run's own, and the names are pruned: a branch that is gone
    no longer counts. The tip, and not a commit further down: one push stands for one commit. A fetch of the
    commit by its id would show nothing, because the address of the repository also gives the head of a pull
    request from outside. A branch of another name can be one that a program pushes.
    """
    tree = str(args.dir / "tree")
    must("the smoke branches of the repository", ["git", "-C", tree, "fetch", "--quiet", "--prune", "--no-tags", "origin",
                                                    f"+refs/heads/{SMOKE}*:{BRANCHES}*"], 300)
    tips = must("the tips of the smoke branches", ["git", "-C", tree, "for-each-ref", "--format=%(objectname) %(refname)", BRANCHES], 120)
    names = [SMOKE + name[len(BRANCHES):] for tip, name in (line.split(" ", 1) for line in tips.splitlines()) if tip == commit]
    if not names:
        raise NotRun(f"the commit {commit} is not the tip of a branch `{SMOKE}<name>` of the repository. The run builds "
                     "and starts what a commit holds, with Docker, on the server, so it takes only what a maintainer "
                     f"has pushed under the name `{SMOKE}`. Fix: read the whole change; then push exactly this commit, "
                     f"`git push origin {commit}:refs/heads/{SMOKE}<name>`, and start the run again")
    return names


def stands_on(args, head: str, commit: str) -> None:
    """Hold that the head of `dev` is part of the commit's history: then what differs from `dev` is what the commit changes."""
    done = ended("the look whether the commit stands on the head", ["git", "-C", str(args.dir / "tree"), "merge-base",
                                                                    "--is-ancestor", head, commit], 120)
    if done.returncode == 1:
        raise NotRun(f"the commit {commit[:12]} does not stand on the head of `{BRANCH}` ({head[:12]}). The run takes "
                     "every service that the commit does not change as it was built from that head. Fix: update the "
                     f"branch onto `{BRANCH}`, push, and start the run for the new commit")
    if done.returncode != 0:
        raise StepFailed(f"the look whether the commit stands on the head: status {done.returncode}: {done.stdout.strip()[-400:]}")


def changed_by(args, head: str, commit: str) -> list:
    """The files that differ between the head of `dev` and the commit. A file that was moved is there under both names."""
    printed = must("the files that the commit changes", ["git", "-C", str(args.dir / "tree"), "diff", "--name-only", "--no-renames",
                                                         "-z", head, commit], 120)
    return [name for name in printed.split("\0") if name]


def taken_from_dev(args, files: list) -> None:
    """Refuse a commit that changes a file which the run takes from `dev`, and name the file."""
    own = {**FROM_DEV, **dict.fromkeys(["docker-compose.yml", *args.compose_file], "a compose file of the stack")}
    found = [(name, what) for name in files for path, what in own.items() if under(name, path)]
    if found:
        name, what = found[0]
        more = f" It changes {len(found) - 1} more such file(s)." if len(found) > 1 else ""
        raise NotRun(f"the commit changes `{name}`: {what}. The run takes that from `{BRANCH}`, so it would run the "
                     f"copy of `{BRANCH}` under the name of the commit.{more} Fix: bring that change to `{BRANCH}` in "
                     "a pull request of its own; then update the branch and start the run for the new commit")


def differing_services(files: list) -> list:
    """The services whose image the commit changes: one of the paths that its build reads differs."""
    return [service for service, (_context, _dockerfile, paths) in BUILDS.items()
            if any(under(name, path) for name in files for path in paths)]


def carried_by_the_sessions(report: dict) -> None:
    """Hold that the sessions of this run carry the text that the commit's mark names.

    The mark is made from the commit's recordings. The text itself is sent by the proxy. With another mark than
    `dev` has and a proxy that is the one of `dev`, the line of the result would name a text that no session carried.
    """
    if report["mark"] != report["mark_of_dev"] and SENDS_THE_TEXT not in report["changed_services"]:
        raise NotRun(f"the recordings of the commit show another text than `{BRANCH}` sends (text {report['mark']}; "
                     f"`{BRANCH}` has {report['mark_of_dev']}), and the commit does not change the proxy, which sends "
                     "that text. So no session of this run would carry the text of the commit. Fix: put the change "
                     "of the text into the same commit as its recordings; then start the run for that commit")


def mark_of_the_text(args, commit: str) -> str:
    """The mark of the text that every request carries at the commit, from the copy of the script that `dev` has."""
    printed = must("the mark of the commit's text", [args.python, "scripts/smoke_result.py", "--mark", commit], 120,
                   cwd=args.dir / "tree").strip()
    if not re.fullmatch(r"[0-9a-f]{12}", printed):
        raise StepFailed(f"the mark of the commit's text: scripts/smoke_result.py printed no mark: {printed[-200:]}")
    return printed


def own_image(service: str, stamp: str, commit: str) -> str:
    """The name of an image that this run builds: of this machine only, with the start time of the run and the commit."""
    return f"{OWN_IMAGES}/{IMAGES[service]}:{stamp}-{commit[:12]}"


def build_images(args, report: dict, folder: Path) -> None:
    """Build the image of each service that the commit changes, from a copy of the commit's build folders.

    The copy is made from the commit's files as git holds them; the tree of the run stays at the head of `dev`. Each
    name goes into the report before its build starts, so the end of the run also removes what a stopped build left.
    """
    services, tree, commit = report["changed_services"], str(args.dir / "tree"), report["commit"]
    if not services:
        report["build"] = (f"nothing was built: no build folder of a service differs between the commit and the head of "
                           f"`{BRANCH}`, so each image is the one that was built from that head")
        return
    paths = sorted({path.rstrip("/") for service in services for path in BUILDS[service][2]})
    paths = [path for path in paths if ended("the look for a build folder", ["git", "-C", tree, "cat-file", "-e", f"{commit}:{path}"], 60).returncode == 0]
    must("the copy of the commit's build folders", ["git", "-C", tree, "archive", "--format=tar", f"--output={folder / 'commit.tar'}",
                                                    commit, "--", *paths], 300)
    (folder / "commit").mkdir()
    must("the unpacking of that copy", ["tar", "-x", "-f", str(folder / "commit.tar"), "-C", str(folder / "commit")], 300)
    for service in services:
        context, dockerfile, _paths = BUILDS[service]
        name = report["built"][service] = own_image(service, folder.name, commit)
        must(f"the build of {service} from the commit", ["docker", "build", "--tag", name, "--label", f"{REVISION}={commit}",
                                                         "--file", str(folder / "commit" / dockerfile), str(folder / "commit" / context)],
             args.build_seconds)
    report["build"] = f"built from the commit: {', '.join(services)}"
    args.own_images = folder / "own-images.yml"
    # JSON is YAML. An image of this run is never looked for in a registry.
    args.own_images.write_text(json.dumps({"services": {service: {"image": name, "pull_policy": "never"}
                                                        for service, name in report["built"].items()}}, indent=2) + "\n", encoding="utf-8")


def remove_built(report: dict) -> None:
    """Remove the images that this run built, by their own names, and nothing else. It is called at the end of every run for a commit."""
    removed = {}
    for name in report["built"].values():
        try:
            if command(["docker", "image", "inspect", "--format", "{{.Id}}", name], 60).returncode != 0:
                removed[name] = "there was none: its build did not end"
                continue
            done = command(["docker", "image", "rm", name], 300)
            removed[name] = "yes" if done.returncode == 0 else f"no: status {done.returncode}: {done.stdout.strip()[-300:]}"
        except (subprocess.TimeoutExpired, OSError) as error:
            removed[name] = f"no: {error}"
    report["built_removed"] = removed


def before_the_card_for_a_commit(args, report: dict, folder: Path, first_start: bool) -> None:
    """The steps of a run for one commit that need no card: the head, the commit, the other images, the builds."""
    now_in(report, "take the head of the branch")
    report["head_of_dev"] = head = take_head(args)
    if first_start and script_changed(args):
        # The tree only ever holds `dev`, so the copy that takes over is the one of `dev`, never the commit's.
        raise NewScript
    now_in(report, "look for the commit at the tip of a smoke branch")
    report["pushed_as"] = pushed_for_a_smoke_run(args, report["commit"])
    stands_on(args, head, report["commit"])
    now_in(report, "read what the commit changes")
    files = changed_by(args, head, report["commit"])
    taken_from_dev(args, files)
    report["changed_services"] = differing_services(files)
    now_in(report, "make the mark of the commit's text")
    report["mark"], report["mark_of_dev"] = mark_of_the_text(args, report["commit"]), mark_of_the_text(args, head)
    carried_by_the_sessions(report)
    now_in(report, "stop a stack that an earlier run left")
    report["left_stack"] = stop_a_left_stack(args)
    now_in(report, "pull the images")
    report["images"] = pull_images([service for service in IMAGES if service not in report["changed_services"]])
    report["stale"] = stale_images(report["images"], head)
    if report["stale"]:
        return
    now_in(report, "build the images of the commit")
    build_images(args, report, folder)


def result_line(report: dict) -> str:
    """The one line for the text of the pull request, or nothing when the run has no such line to give.

    A run that failed for another reason than a harness defect gives none: the line would say 3 of 3 for a run
    that failed.
    """
    tasks = report.get("tasks") or []
    good = sum(1 for task in tasks if not task["defects"])
    if len(tasks) != len(TASKS) or (report["result"] != "passed" and good == len(TASKS)):
        return ""
    return RESULT_LINE.format(commit=report["commit"], good=good, tasks=len(TASKS), mark=report["mark"],
                              seconds=round(sum(task["seconds"] for task in tasks)))


def one_commit(args, report: dict, folder: Path, first_start: bool) -> None:
    """The whole run for one commit of a branch. It leaves the one line of its result in the report."""
    report.update({"commit": args.commit, "built": {}})
    try:
        with failures_into(report):
            before_the_card_for_a_commit(args, report, folder, first_start)
            if report["stale"]:
                report["result"] = judge(report)
        if "result" not in report:
            on_the_card(args, report, folder)
    finally:
        remove_built(report)
    report["line"] = result_line(report)


# --- the result --------------------------------------------------------------------------------------------------

def judge(report: dict) -> str:
    """The one line of the result, from what the report holds."""
    if report.get("stale"):
        return f"{STALE} ({'; '.join(report['stale'])})"
    faults = []
    for task in report.get("tasks") or []:
        if task["defects"]:
            faults.append(f"{task['task']}: {task['defects']} defect(s) of the harness")
    tests = report.get("tests")
    if tests:
        faults += counted.faults(tests, tests["expected"])
    faults += [f"at the end of the run {fault}" for fault in report.get("not_whole_at_the_end") or []]
    return "failed: " + "; ".join(faults) if faults else "passed"


@contextlib.contextmanager
def failures_into(report: dict):
    """A step that fails ends the block, and the result of the report says why."""
    try:
        yield
    except NewScript:
        raise
    except NotRun as reason:
        report["result"] = f"not run: {reason}"
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
    if "result" not in report:
        on_the_card(args, report, folder)


def on_the_card(args, report: dict, folder: Path) -> None:
    """Take the lock of the card, do the steps that need it under the time limit, and stop the stack."""
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


# --- the result on GitHub ----------------------------------------------------------------------------------------

class NotSent(Exception):
    """GitHub could not be asked, or a status could not be sent now. The words say why, and they are fixed words: no
    path, no name of a machine and nothing that a tool printed."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """An answer that points to another address is not followed: the token would go there too."""

    def redirect_request(self, *args, **kwargs):
        return None


# The paths of the calls that this script makes. A value from a file, from an argument or from an answer goes into a
# path only as letters, digits and the few signs of a commit id, a number or an encoded name of a branch.
PATHS = re.compile(rf"/repos/{re.escape(REPOSITORY)}/(?:installation|statuses/[0-9a-f]{{40}}|commits/[0-9a-f]{{40}}/(?:statuses|pulls)\?per_page=100"
                   r"|activity\?ref=[A-Za-z0-9._%-]+&per_page=5)|/app/installations/[0-9]+/access_tokens")


def github(method: str, path: str, bearer: str = "", body: dict | None = None):
    """One call to GitHub's own address, and the decoded answer. A call that reads sends no key: the repository is public."""
    if not PATHS.fullmatch(path):
        raise NotSent("the call was not made: its path is not one that this script makes")
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "atlas-nightly",
               **({"Authorization": f"Bearer {bearer}"} if bearer else {})}
    request = urllib.request.Request(f"{GITHUB_API}{path}", data=json.dumps(body).encode() if body is not None else None,
                                     method=method, headers=headers)
    try:
        # No proxy of the environment either: no variable changes where the call goes.
        with urllib.request.build_opener(NoRedirect, urllib.request.ProxyHandler({})).open(request, timeout=30) as response:
            answer = response.read()
    except urllib.error.HTTPError as error:
        raise NotSent(f"GitHub answered {error.code}") from None
    except (http.client.HTTPException, OSError):
        # A URLError and a timeout are an OSError too.
        raise NotSent("GitHub could not be reached") from None
    try:
        return json.loads(answer)
    except ValueError:
        return None


def counts_hold(report: dict) -> bool:
    """Whether the report holds the numbers of a run that passed: each session there with no harness defect and, for
    a night, every expected test collected and passed, none failed and none skipped."""
    tasks = report.get("tasks") or []
    sessions = [task.get("task") for task in tasks] == list(TASKS) and not any(task.get("defects") for task in tasks)
    if report.get("mode") != "night":
        return sessions
    tests = report.get("tests") or {}
    whole = bool(tests.get("expected")) and tests.get("expected") == tests.get("collected") == tests.get("passed")
    return sessions and whole and tests.get("failed") == 0 and tests.get("skipped") == 0


def status_of(report: dict) -> tuple:
    """The status for the commit that ran, or None with the reason why the run gets none.

    Green only for a run that passed and whose report holds the numbers of a passed run. Red for a run that failed.
    A run that did not take place gets no status. The text has fixed words, numbers, the mark of the text and the
    start time of the run in UTC: nothing that a tool, a test or the model wrote. It has no link.
    """
    result, commit, name = str(report.get("result", "")), str(report.get("commit", "")), STATUS.get(report.get("mode"))
    if not name or not re.fullmatch(r"[0-9a-f]{40}", commit):
        return None, "no status: the report does not say which commit ran"
    if report["mode"] == "one commit" and not report.get("pushed_as"):
        return None, "no status: the run ended before the commit was found at the tip of a smoke branch"
    if result != "passed" and not result.startswith("failed"):
        return None, "no status: the run did not take place"
    if result == "passed" and not counts_hold(report):
        return None, "no status: the report says passed, and its numbers are not those of a run that passed"
    said = ["passed" if result == "passed" else "failed", f"started {report['started']}", *numbers_of(report)]
    if result != "passed" and counts_hold(report):
        said.append(what_failed(report))
    return {"context": name, "sha": commit, "state": "success" if result == "passed" else "failure",
            "description": "; ".join(said)}, ""


def what_failed(report: dict) -> str:
    """For a failed run whose numbers are those of a run that passed: what failed, in fixed words. The numbers do
    not say it."""
    if str(report.get("stack_stopped", "")).startswith("no"):
        return "the stack was not stopped"
    return "a service was not whole at the end" if report.get("not_whole_at_the_end") else "see the report on the server"


def numbers_of(report: dict) -> list:
    """What a status says about the sessions and the tests of a run, in fixed words and numbers."""
    tasks, tests = report.get("tasks") or [], report.get("tests") or {}
    if len(tasks) != len(TASKS):
        return [f"ended in the step: {report.get('step') if report.get('step') in STEPS else 'not named'}"]
    said = [f"{sum(1 for task in tasks if not task.get('defects'))} of {len(TASKS)} sessions with no harness defect"]
    if report["mode"] == "one commit":
        return said + [f"text {report['mark']}" if re.fullmatch(r"[0-9a-f]{12}", str(report.get("mark"))) else "text not marked"]
    if not tests:
        return said + ["tests: not run"]
    passed, failed, skipped, expected = (int(tests.get(key, 0)) for key in ("passed", "failed", "skipped", "expected"))
    whole = passed == expected == tests.get("collected") and not failed and not skipped
    return said + [f"{passed} of {expected} tests passed" if whole else f"tests: {passed} passed, {failed} failed, {skipped} skipped of {expected}"]


def key_fault(args) -> str:
    """Why the key cannot be used, or nothing. It is a file of the user of this run that nobody else can read, and
    it lies outside the folder of the run, so no container of the stack can have it."""
    if args.status_key is None or not args.status_app:
        return "no key: the run was started without --status-key or without --status-app"
    try:
        about = args.status_key.stat()
    except OSError:
        return "no key: the file that --status-key names is not there"
    if not stat.S_ISREG(about.st_mode) or about.st_uid != os.getuid():
        return "the key is not used: it is not a file of the user of this run"
    if about.st_mode & 0o077:
        return "the key is not used: its file is open to the group or to others. Fix: chmod 600 on that file"
    if args.dir.resolve() in args.status_key.resolve().parents:
        return ("the key is not used: its file lies in the folder of the run, which the containers of the stack can "
                "read. Fix: move it out of that folder")
    return ""


def base64url(raw: bytes) -> bytes:
    return base64.urlsafe_b64encode(raw).rstrip(b"=")


def token_for_statuses(args) -> str:
    """A token of the app, made for one send: for this one repository and for statuses only. It is kept nowhere.

    openssl signs with the key and gets the key as the path of its file. This script never reads the key.
    """
    now = int(time.time())
    unsigned = b".".join(base64url(json.dumps(part).encode()) for part in (
        {"alg": "RS256", "typ": "JWT"}, {"iat": now - 60, "exp": now + 300, "iss": str(args.status_app)}))
    try:
        done = subprocess.run(["openssl", "dgst", "-sha256", "-sign", str(args.status_key)], input=unsigned,
                              capture_output=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise NotSent("openssl could not be started, or did not end") from None
    if done.returncode != 0 or not done.stdout:
        raise NotSent(f"openssl could not sign with the key (status {done.returncode})")
    proof = (unsigned + b"." + base64url(done.stdout)).decode()
    try:
        installation = github("GET", f"/repos/{REPOSITORY}/installation", proof)
        token = github("POST", f"/app/installations/{int(installation['id'])}/access_tokens", proof,
                       {"repositories": [REPOSITORY.split("/")[1]], "permissions": {"statuses": "write"}})["token"]
    except (KeyError, TypeError, ValueError):
        token = None
    if not isinstance(token, str) or not token:
        raise NotSent("GitHub gave an answer of another shape")
    return token


def is_ours(status) -> bool:
    """Whether a waiting file holds a status as this script writes it. Another file is not sent."""
    return (isinstance(status, dict) and status.get("context") in STATUS.values() and status.get("state") in ("success", "failure")
            and bool(re.fullmatch(r"[0-9a-f]{40}", str(status.get("sha")))) and isinstance(status.get("description"), str)
            and len(status["description"]) <= 140)


def not_the_runs_own(about: os.stat_result) -> bool:
    """Whether a file or a folder is not the run's own: another user owns it, or the group or others can write it."""
    return about.st_uid != os.getuid() or bool(about.st_mode & 0o022)


def folder_fault(args) -> str:
    """Why the folder of the waiting statuses cannot be used, or nothing. As for the key: it is a folder of the user
    of this run, and nobody else can write into it. What lies in another folder is not sent."""
    try:
        about = (args.dir / WAITING).lstat()
    except OSError:
        return ""
    if not stat.S_ISDIR(about.st_mode) or not_the_runs_own(about):
        return ("the folder of the waiting statuses is not a folder that only the user of this run can write. Fix: "
                "look at what lies in it; then chmod go-w on that folder")
    return ""


def keep_for_sending(args, status: dict, stamp: str) -> None:
    (args.dir / WAITING).mkdir(mode=0o700, exist_ok=True)
    if folder_fault(args):
        return
    place = args.dir / WAITING / f"{stamp}-{status['context'].replace('/', '-')}.json"
    # The file is made for the user of the run alone, whatever the settings of the server give a new file.
    with os.fdopen(os.open(place, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w", encoding="utf-8") as out:
        out.write(json.dumps(status, indent=2) + "\n")


def set_aside(args, path: Path, folder: str) -> None:
    (args.dir / folder).mkdir(mode=0o700, exist_ok=True)
    path.replace(args.dir / folder / path.name)


def waiting_status(path: Path, now: dt.datetime) -> dict | None:
    """The status that a waiting file holds, or None when the file is not one to send: this script did not write it,
    it is not a file of the run's own, or it has waited for longer than KEEP_TRYING_DAYS."""
    try:
        about = path.lstat()
        status = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not stat.S_ISREG(about.st_mode) or not_the_runs_own(about):
        return None
    too_old = now.timestamp() - about.st_mtime > KEEP_TRYING_DAYS * 86400
    return status if is_ours(status) and not too_old else None


def send_one(args, status: dict) -> str:
    """Send one status, with a token made for it. Gives a note when the account that wrote it is not the one that
    the settings name, and else nothing. Only the name, the state and the text of the status are sent."""
    made = github("POST", f"/repos/{REPOSITORY}/statuses/{urllib.parse.quote(str(status['sha']), safe='')}", token_for_statuses(args),
                  {key: status[key] for key in ("state", "context", "description")})
    creator = made.get("creator") if isinstance(made, dict) else None
    writer = creator.get("id") if isinstance(creator, dict) else None
    if not isinstance(writer, int) or writer == args.status_writer:
        return ""
    return (f"; the statuses of this key are written by the account {writer}, and --status-writer names "
            f"{args.status_writer or 'none'}. Fix: give --status-writer {writer}")


def send_waiting(args, now: dt.datetime) -> str:
    """Try once to send each status that waits. Gives what happened, in fixed words.

    A status that was sent is moved to the folder of the sent ones, so it is sent once. One that has waited for
    longer than KEEP_TRYING_DAYS, or that this script did not write, is moved aside and tried no more. When GitHub
    does not answer, the others wait for the next start too.
    """
    waiting = sorted((args.dir / WAITING).glob("*.json")) if (args.dir / WAITING).is_dir() else []
    if folder_fault(args):
        # Nothing of such a folder is sent, and nothing of it is moved.
        return f"not sent: {folder_fault(args)}; the status of a run is then in its report only"
    fault, sent, aside, other = key_fault(args), 0, 0, ""
    for path in waiting:
        status = waiting_status(path, now)
        if status is None:
            set_aside(args, path, GIVEN_UP)
            aside += 1
        elif not fault:
            try:
                other = send_one(args, status) or other
            except NotSent as reason:
                fault = str(reason)
                continue
            set_aside(args, path, SENT)
            sent += 1
    left = len(waiting) - sent - aside
    said = [f"sent: {sent}"] if sent else []
    said += [f"not sent: {fault}; waits: {left}"] if left else []
    said += [f"set aside after {KEEP_TRYING_DAYS} days or as not written by this script: {aside}"] if aside else []
    return ("; ".join(said) or "nothing waits") + other


def send_what_waits(args, status: dict | None = None, stamp: str = "") -> str:
    """Keep the status of a run for sending, and try once to send each status that waits. Gives what happened.

    A fault of this step itself is given as words too: sending never stops a run, its report or a look of the timer.
    """
    try:
        if status:
            keep_for_sending(args, status, stamp)
        return send_waiting(args, dt.datetime.now(dt.timezone.utc))
    except Exception as error:  # the caller says so, and goes on
        return f"not sent: an error of the sending step itself ({type(error).__name__})"


def to_github(args, report: dict, folder: Path, stamp: str) -> None:
    """Write the report, and send the status of the run. Sending never changes the result of the run or stops its
    report: the report is written first, and a status that cannot go out now waits."""
    report["sent"] = "not tried"
    try:
        status, why_none = status_of(report)
        report["status"] = status or why_none
    except Exception as error:  # a report of another shape gets no status, and is written all the same
        status, report["status"] = None, f"no status: an error of the run itself ({type(error).__name__})"
    (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    report["sent"] = send_what_waits(args, status, stamp)
    (folder / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")


# --- one look of the timer ----------------------------------------------------------------------------------------

def reports_of(args) -> list:
    """The reports of the newest runs of this folder that can be read, oldest first."""
    found = []
    for path in sorted(args.dir.glob("reports/*/report.json"))[-200:]:
        try:
            report = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        found += [report] if isinstance(report, dict) else []
    return found


def smoke_tips() -> list:
    """The tips of the smoke branches at the repository's own address, by name: (name, commit). No key is needed."""
    printed = must("the smoke branches of the repository", ["git", "ls-remote", "--heads", REPO_URL, f"refs/heads/{SMOKE}*"], 120)
    tips = []
    for line in printed.splitlines():
        commit, _, name = line.partition("\trefs/heads/")
        if re.fullmatch(r"[0-9a-f]{40}", commit) and SMOKE_BRANCH.fullmatch(name):
            tips.append((name, commit))
    return sorted(tips)


def has_a_status(commit: str, name: str, writer: int) -> bool:
    """Whether the app wrote a status of this name on the commit. A status of that name by anybody else counts for nothing."""
    listed = github("GET", f"/repos/{REPOSITORY}/commits/{urllib.parse.quote(commit, safe='')}/statuses?per_page=100")
    return any(status.get("context") == name and (status.get("creator") or {}).get("id") == writer for status in listed)


def not_pushed_by_the_list(args, name: str, tip: str) -> str:
    """Why the tip of a smoke branch is not run, or nothing. GitHub's record of the branch has to say that a person
    on the server's own list moved the branch to this commit. This guard does not rest on the rules of the repository."""
    moves = github("GET", f"/repos/{REPOSITORY}/activity?ref={urllib.parse.quote('refs/heads/' + name, safe='')}&per_page=5")
    newest = moves[0] if moves else {}
    actor = newest.get("actor") or {}
    if newest.get("after") != tip:
        return "GitHub's record of the branch does not end at its tip"
    if actor.get("type") != "User" or str(actor.get("login", "")).lower() not in {login.lower() for login in args.smoke_by}:
        return "the account that moved the branch to its tip is not on the list of this server"
    return ""


def not_our_own_code(tip: str) -> str:
    """Why the timer does not run a commit by itself, or nothing. The commit has to be the head of an open pull
    request from a branch of this repository, and not from a `smoke/` branch: a person can push another's commit
    there. The look knows who pushed a branch, not whose code the commit is."""
    pulls = github("GET", f"/repos/{REPOSITORY}/commits/{urllib.parse.quote(tip, safe='')}/pulls?per_page=100")
    for pull in pulls or []:
        head = pull.get("head") or {}
        ours = (head.get("repo") or {}).get("full_name") == REPOSITORY and not str(head.get("ref", "")).startswith(SMOKE)
        if pull.get("state") == "open" and head.get("sha") == tip and ours:
            return ""
    return ("its commit is not the head of an open pull request from a branch of this repository. The timer runs "
            "only such a commit by itself; another one is run by hand")


def settled(report: dict) -> bool:
    """Whether the run for a commit needs no second run: it made a status, or it did not take place for a reason that
    the commit itself carries. A run that found the card in use, or images that were not of the head yet, settles nothing."""
    result = str(report.get("result", ""))
    return isinstance(report.get("status"), dict) or (result.startswith("not run: ") and not result.startswith((NOT_RUN, STALE)))


def smoke_that_is_due(args, today: list, hour: int) -> tuple:
    """The tip of one smoke branch that has no result yet and may run, as (commit, why), or (None, why not)."""
    if not args.status_writer or not args.smoke_by:
        return None, "no smoke: the settings of the server name no writer of the statuses, or no person whose push counts"
    if not args.smoke_hours:
        return None, "no smoke: the settings of the server name no hours in which a look may start a smoke run (--smoke-hours)"
    if hour not in args.smoke_hours:
        return None, "no smoke in this hour: it is not one of the hours for smoke runs (--smoke-hours). A smoke branch waits"
    ran = {report.get("commit") for report in reports_of(args) if report.get("mode") == "one commit" and settled(report)}
    for name, tip in smoke_tips():
        if tip in ran or has_a_status(tip, STATUS["one commit"], args.status_writer):
            continue
        if len([report for report in today if report.get("mode") == "one commit"]) >= args.smokes_a_day:
            return None, f"no smoke: {args.smokes_a_day} were started today, which is the bound of this server"
        why = not_pushed_by_the_list(args, name, tip) or not_our_own_code(tip)
        if not why:
            return tip, f"the tip of `{name}` has no result yet"
        print(f"tick: `{name}` is not run: {why}")
    return None, "nothing is due"


def held_lately(args, now: dt.datetime) -> str:
    """The time, within the last QUIET_MINUTES, when a look found a process on the card with no lock. Else nothing."""
    try:
        said = (args.dir / CARD_HELD).read_text(encoding="utf-8").strip()
        since = now - dt.datetime.strptime(said, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)
    except (OSError, ValueError):
        return ""
    return said if dt.timedelta(0) <= since < dt.timedelta(minutes=QUIET_MINUTES) else ""


def card_is_free(args, now: dt.datetime) -> bool:
    """Whether a run could take the card now: nobody holds its lock, and no process computes on it.

    A look that starts nothing (--look) asks only for the lock. The usual stack of the server computes on the card
    and takes no lock, and the server stops it only when that look found a run due. When the look that runs then
    finds a process on the card all the same, it writes the time down: the looks of the next hour start nothing,
    so the usual stack is not stopped and started at each of them.
    """
    with lock(args.lock) as held:
        if not held or args.look:
            return bool(held)
        if card_use()[0]:
            (args.dir / CARD_HELD).write_text(now.strftime("%Y-%m-%dT%H:%M:%SZ") + "\n", encoding="utf-8")
            return False
        return True


def what_is_due(args, now: dt.datetime) -> tuple:
    """What this look of the timer runs, and why, in one line: ("night", None, why), ("one commit", <its id>, why) or
    (None, None, why not). A look that finds the card in use starts nothing, so it uses up no try of the day."""
    today = [report for report in reports_of(args) if str(report.get("started", "")).startswith(now.strftime("%Y-%m-%d"))]
    night_is_due = now.hour == args.night_hour and not [report for report in today if report.get("mode") == "night"]
    lately = held_lately(args, now)
    if lately:
        return None, None, (f"nothing is started: at {lately} a process held the card with no lock, and the looks of the "
                            f"{QUIET_MINUTES} minutes after that start nothing")
    if not card_is_free(args, now):
        return None, None, "nothing is started: the card is in use"
    if night_is_due:
        return "night", None, "the night is due"
    try:
        commit, why = smoke_that_is_due(args, today, now.hour)
    except (StepFailed, NotSent) as reason:
        return None, None, f"no smoke: the repository could not be read ({reason})"
    except (KeyError, TypeError, AttributeError, IndexError, ValueError):
        return None, None, "no smoke: GitHub gave an answer of another shape"
    return ("one commit" if commit else None), commit, why


def hours(given: str) -> frozenset:
    """The hours of a day that a setting names, in UTC: single hours and spans, as in `22-23,0-5`."""
    found = set()
    for part in given.split(","):
        first, dash, last = part.strip().partition("-")
        last = last if dash else first
        if not (first.isdigit() and last.isdigit() and int(first) <= int(last) <= 23):
            raise argparse.ArgumentTypeError("give hours from 0 to 23, in UTC, each alone or as a span: for example 22-23,0-5")
        found.update(range(int(first), int(last) + 1))
    return frozenset(found)


def full_id(given: str) -> str:
    """The full id of a commit. A short id or a name could mean another commit tomorrow."""
    if not re.fullmatch(r"[0-9a-f]{40}", given):
        raise argparse.ArgumentTypeError("give the full id of the commit: 40 characters, each 0-9 or a-f")
    return given


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
    parser.add_argument("--status-key", type=Path, default=None, help="the file with the key of the app that writes the statuses")
    parser.add_argument("--status-app", default="", help="the client id of that app")
    parser.add_argument("--status-writer", type=int, default=0, help="the id of the account that the statuses of that app are written by")
    parser.add_argument("--tick", action="store_true", help="one look by the timer: send what waits, then run what is due, if anything")
    parser.add_argument("--look", action="store_true", help="with --tick: say what is due and start nothing (status 0: a run is due; 3: none is)")
    parser.add_argument("--night-hour", type=int, default=8, choices=range(24), metavar="0-23", help="the hour, in UTC, in which a look starts the night")
    parser.add_argument("--smoke-by", action="append", default=[], metavar="LOGIN", help="a person whose push to a smoke branch the timer runs")
    parser.add_argument("--smokes-a-day", type=int, default=6, help="how many smoke runs the timer starts in one day")
    parser.add_argument("--smoke-hours", type=hours, default=frozenset(), metavar="HOURS",
                        help="the hours, in UTC, in which a look may start a smoke run, as in 22-23,0-5; with none, the timer starts no smoke run")
    parser.add_argument("--commit", type=full_id, default=None, help="run the three sessions for this commit of a branch, and nothing else")
    parser.add_argument("--build-seconds", type=int, default=1800, help="the time limit of one image build of a run for a commit")
    parser.set_defaults(own_images=None)
    args = parser.parse_args(argv)
    if args.tick and args.commit:
        parser.error("--tick chooses what to run by itself: give --tick or --commit, not both")
    if args.look and not args.tick:
        parser.error("--look is a look of the timer that starts nothing: give it with --tick")
    return args


def start_time() -> tuple:
    """The start time of the run, whether this is its first start, the open file of the folder's lock, and the commit
    that a look of the timer chose.

    A copy that a run started in its place gets that run's time, the number of the file that holds the lock and, for
    a run for one commit, the commit.
    """
    stamp, number, chosen = (os.environ.pop(STARTED_AGAIN, "").split(" ") + ["", ""])[:3]
    try:
        chosen = chosen if re.fullmatch(r"[0-9a-f]{40}", chosen) else ""
        return dt.datetime.strptime(stamp, STAMP).replace(tzinfo=dt.timezone.utc), False, int(number), chosen
    except ValueError:
        return dt.datetime.now(dt.timezone.utc), True, None, ""


def one_look(args, started: dt.datetime) -> tuple:
    """One look of the timer: send what waits, and say what is due. Gives the commit that the look chose, if any,
    and the status to end with now, or None when a run starts."""
    # What waits goes out first: a run can take long. A look with nothing to run leaves no report.
    print(f"tick: statuses: {send_what_waits(args)}")
    mode, chosen, why = what_is_due(args, started)
    print(f"tick: {why}")
    if args.look or not mode:
        return None, NOTHING_DUE if args.look and not mode else 0
    return chosen, None


def tell(args, report: dict, folder: Path) -> None:
    """Print the result of a run, for the person or the log that started it."""
    if not args.commit:
        print(f"nightly run: {report['result']}\nreport: {folder / 'report.json'}\nGitHub: {report['sent']}")
        return
    # The run deletes nothing on GitHub. A smoke branch has done its work when its run is done.
    remove = [f"to remove the branch: git push origin --delete {name}" for name in report.get("pushed_as") or []]
    print("\n".join(filter(None, [f"run for the commit {args.commit}: {report['result']}", report.get("line"),
                                  f"report: {folder / 'report.json'}", f"GitHub: {report['sent']}", *remove])))


def run(args, argv: list, started: dt.datetime, first_start: bool, folder: Path, held) -> dict:
    """The run itself, under the lock of the folder: the night or the run for one commit, then its report and its
    status. Gives the report."""
    report = {"started": started.strftime("%Y-%m-%dT%H:%M:%SZ"), "step": "start", "mode": "one commit" if args.commit else "night",
              "script": "as it was started" if first_start else "the new copy: taking the head changed the script"}
    folder.mkdir(parents=True, exist_ok=not first_start)
    try:
        with halt_on(signal.SIGTERM, "the run was told to stop"), failures_into(report):
            (one_commit if args.commit else one_night)(args, report, folder, first_start)
    except NewScript:
        start_the_new_copy(argv, started.strftime(STAMP), report, folder, held)
    report["ended"] = dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    to_github(args, report, folder, started.strftime(STAMP))
    return report


def main(argv: list | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    args = parse(argv)
    started, first_start, kept, chosen = start_time()
    if not args.dir.is_absolute() or not args.dir.is_dir():
        print(f"nightly run: --dir {args.dir} is not a full path of a folder that is there. Fix: make the folder and "
              "give its full path.", file=sys.stderr)
        return 2
    folder = args.dir / "reports" / started.strftime(STAMP)
    # The lock of the folder comes first: a second run that looked for "a stack that an earlier run left" before it
    # held this lock would stop the stack of a run that is in its middle.
    with lock(args.dir / "run.lock", kept) as alone:
        if not alone:
            print("nightly run: another run of this folder is going. This one did nothing.")
            return NOTHING_DUE if args.look else 0
        if args.tick and first_start:
            chosen, ends_with = one_look(args, started)
            if ends_with is not None:
                return ends_with
        args.commit = args.commit or chosen or None
        report = run(args, argv, started, first_start, folder, alone)
    tell(args, report, folder)
    return 0 if report["result"] == "passed" or report["result"].startswith("not run: ") else 1


if __name__ == "__main__":
    sys.exit(main())
