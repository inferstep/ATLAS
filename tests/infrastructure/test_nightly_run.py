"""The nightly run does its steps in order, says the truth in its report, and always stops its stack.

No test here reaches git, docker, a graphics card, a model or the network. The
run is started with a path that holds only stand-ins made for the test: `git`,
`docker`, `nvidia-smi` and the Python that would run the driver and the tests.
Each stand-in writes down how it was called and answers from a small plan.
Every run has a time limit.
"""
import ast
import contextlib
import fcntl
import http.server
import json
import os
import re
import shlex
import shutil
import signal
import subprocess
import sys
import threading
import time
import venv
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "nightly_run.py"
sys.path.insert(0, str(ROOT / "scripts"))
import nightly_run as nightly  # noqa: E402

HEAD = "1" * 40
OLD = "2" * 40
NEWER = "3" * 40
# Words that are in one call only, for the plan of a stand-in: the first pull, the start of the stack (and not its
# stop), the driver, the tests.
PULL, UP, DRIVER, TESTS = "pull --quiet ghcr.io/inferstep/atlas-llama:dev", "up -d --wait", "scripts/e2e-reliability.py", "-m pytest"

# What whole services answer: the status of the reply, and its body.
WHOLE = {
    "proxy/ready": [200, {"ready": True, "inference": True, "lens_ready": True, "sandbox": True, "v3": True}],
    "lens/ready": [200, {"ready": True, "llama_server": True, "lens_self_test": True, "fingerprint_ok": True,
                         "embed_capacity_tokens": 4096, "reason": None}],
    "lens/health": [200, {"service": "geometric-lens", "status": "healthy", "subsystems": {
        "llama_server": {"reachable": True},
        "lens": {"cost_field_loaded": True, "gx_loaded": True, "cx_calibrated": True, "gx_calibrated": True,
                 "self_test_pass": True, "self_test_error": None}}}],
}


def proxy_says(**parts):
    """The proxy's answer about the services, with these parts not as they are for a whole stack."""
    return [503, {**WHOLE["proxy/ready"][1], "ready": False, **parts}]


# A stand-in is two files. The first line of the one on the path is fixed: a line that named the path of Python would
# not start where that path holds a space.
ON_THE_PATH = '#!/bin/sh\nexec {python} -S {code} "$@"\n'
STAND_IN = r'''"""A stand-in for {name}: it writes down its call and answers from the plan."""
import json, os, sys, time
home = {home!r}
plan = json.load(open(home + "/plan.json"))
args = sys.argv[1:]
call = " ".join(args)
with open(home + "/calls.log", "a") as log:
    log.write(json.dumps({{"tool": {name!r}, "args": args, "has_the_token": "ATLAS_NIGHTLY_TOKEN" in os.environ,
                          "env": {{k: v for k, v in os.environ.items() if k.endswith("_URL") or k in ("ATLAS_SERVICE_TOKEN_FILE", "ATLAS_MODEL_NAME")}}}}) + "\n")
calls = [json.loads(line) for line in open(home + "/calls.log")]


def this_time(rounds, same):
    """The answer of the plan for this call: a list gives one answer for each time, and its last one stays."""
    return rounds[min(sum(1 for c in calls if same(c)), len(rounds)) - 1]


def head_now():
    heads = plan["head"] if isinstance(plan["head"], list) else [plan["head"]]
    return heads[max(min(sum(1 for c in calls if c["tool"] == "git" and "rev-parse" in c["args"]), len(heads)), 1) - 1]


for words, seconds in plan.get("sleep", {{}}).items():
    if words in {name!r} + " " + call:
        time.sleep(seconds)
for words in plan.get("fail", []):
    if words in {name!r} + " " + call:
        print("a planted failure of: " + words)
        sys.exit(1)
{body}
'''
GIT = '''
if args[0] == "clone":
    tree = args[-1]
    os.makedirs(tree + "/.git"); os.makedirs(tree + "/scripts")
    open(tree + "/docker-compose.yml", "w").write("services: {}\\n")
elif "checkout" in args and plan.get("checkout_adds"):
    open(args[1] + "/scripts/nightly_run.py", "a").write(plan["checkout_adds"])
elif "checkout" in args and plan.get("checkout_asks_for_the_lock"):
    # The new copy of the script first tries to take the lock of the folder through a file that it opens itself, as
    # another run would, and writes down whether it could.
    script = args[1] + "/scripts/nightly_run.py"
    first = "from __future__ import annotations" + chr(10)
    asks = chr(10).join([
        "import fcntl as _fcntl",
        "with open(%r, 'a') as _other:" % (args[1][:-len("/tree")] + "/run.lock"),
        "    try:",
        "        _fcntl.flock(_other, _fcntl.LOCK_EX | _fcntl.LOCK_NB)",
        "        _said = 'free'",
        "    except OSError:",
        "        _said = 'held'",
        "open(%r, 'a').write(_said + chr(10))" % (home + "/the_lock_when_the_new_copy_starts.txt"),
        ""])
    text = open(script).read()
    if "_fcntl" not in text:
        open(script, "w").write(text.replace(first, first + asks, 1))
elif "rev-parse" in args:
    print(head_now())
'''
DOCKER = '''
if args[0] == "ps":
    print("\\n".join(plan.get("left", [])))
if args[:2] == ["image", "inspect"]:
    name = args[2].split("/")[-1].split(":")[0]
    rounds = plan["built_from"] if isinstance(plan["built_from"], list) else [plan["built_from"]]
    built = this_time(rounds, lambda c: c["args"] == args)
    print(json.dumps([{"RepoDigests": [args[2].split(":")[0] + "@sha256:" + name.encode().hex()],
                       "Config": {"Labels": {"org.opencontainers.image.revision": built.get(name, head_now())}}}]))
if "exec" in args:
    url = args[-1]
    said = plan["answers"][("proxy" if ":8090" in url else "lens") + url[url.rindex("/"):]]
    status, body = this_time(said if isinstance(said[0], list) else [said], lambda c: c["args"] == args)
    sys.stdout.write((body if isinstance(body, str) else json.dumps(body)) + "\\n" + str(status))
'''
PYTHON = '''
if args and args[0] == "scripts/e2e-reliability.py":
    if plan.get("driver_rows") is not None:
        out = args[args.index("--json") + 1]
        json.dump(plan["driver_rows"], open(out, "w"))
    print("the driver ran")
    sys.exit(plan.get("driver_status", 0))
if args[:2] == ["-m", "pytest"]:
    if plan.get("tests") is not None:
        out = [a for a in args if a.startswith("--junitxml=")][0].split("=", 1)[1]
        t = plan["tests"]
        skipped = "".join('<testcase name="t%d"><skipped type="pytest.skip" message="%s">where</skipped></testcase>' % (n, reason)
                          for n, reason in enumerate(t.get("reasons", [])))
        open(out, "w").write('<?xml version="1.0"?><testsuites><testsuite name="pytest" errors="%d" failures="%d" skipped="%d" '
                             'tests="%d" time="1.0"><testcase name="tests=9"/>%s</testsuite></testsuites>'
                             % (t.get("errors", 0), t.get("failures", 0), t.get("skipped", 0), t["tests"], skipped))
    print("the tests ran")
    sys.exit(plan.get("tests_status", 0))
'''
CARD = '''
print("\\n".join(plan.get("card", [])))
'''


def rows(*defects):
    return [{"task": task, "wall_s": 10.5 + n, "task_passed": True, "defects": ["d"] * count}
            for n, (task, count) in enumerate(zip(nightly.TASKS, defects or (0, 0, 0)))]


class Night:
    """A folder for one run with its stand-ins, and what the run did."""

    def __init__(self, root, python=sys.executable, from_the_tree=False, **plan):
        self.root = root
        self.dir = root / "nightly"
        self.lock = root / "card.lock"
        self.script = SCRIPT
        (root / "bin").mkdir()
        (root / "stand-ins").mkdir()
        self.dir.mkdir()
        (self.dir / "nightly.env").write_text("ATLAS_MODEL_FILE=model.gguf\nATLAS_MODEL_NAME=model\nATLAS_PROXY_PORT=8090\n")
        for name, body in (("git", GIT), ("docker", DOCKER), ("python-for-the-run", PYTHON), ("nvidia-smi", CARD)):
            code = root / "stand-ins" / f"{name}.py"
            code.write_text(STAND_IN.format(name=name, home=str(root), body=body))
            path = root / "bin" / name
            path.write_text(ON_THE_PATH.format(python=shlex.quote(python), code=shlex.quote(str(code))))
            path.chmod(0o755)
        if from_the_tree:
            # The run is started from its own tree, as the timer starts it: the tree is there from an earlier night.
            tree = self.dir / "tree"
            (tree / ".git").mkdir(parents=True)
            (tree / "scripts").mkdir()
            (tree / "docker-compose.yml").write_text("services: {}\n")
            self.script = Path(shutil.copy(SCRIPT, tree / "scripts" / "nightly_run.py"))
        self.plan = {"head": HEAD, "built_from": {}, "driver_rows": rows(), "tests": {"tests": 125}, **plan,
                     "answers": {**WHOLE, **plan.get("answers", {})}}
        (root / "plan.json").write_text(json.dumps(self.plan))

    def command(self, *more):
        # No wait in a test unless the test asks for one: an argument that is given again holds.
        return [sys.executable, str(self.script), "--dir", str(self.dir), "--lock", str(self.lock),
                "--python", str(self.root / "bin" / "python-for-the-run"), "--api", "http://127.0.0.1:9",
                "--image-wait-minutes", "0", "--settle-seconds", "0", *more]

    def env(self, **more):
        # Only the stand-ins are on the path: the real git and docker cannot be reached.
        return {"PATH": str(self.root / "bin"), "HOME": str(self.root), **more}

    def run(self, *more, **env):
        self.done = subprocess.run(self.command(*more), env=self.env(**env), capture_output=True, text=True, timeout=90)
        return self

    def start(self):
        return subprocess.Popen(self.command(), env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

    def wait_for(self, step):
        deadline = time.monotonic() + 20
        while step not in self.did() and time.monotonic() < deadline:
            time.sleep(0.1)
        assert step in self.did(), self.did()

    @property
    def report(self):
        (path,) = self.dir.glob("reports/*/report.json")
        return json.loads(path.read_text())

    def calls(self):
        log = self.root / "calls.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def did(self):
        """What was called, in order, in a few words each. The questions of one round to the services are one step."""
        short = []
        for call in self.calls():
            args = call["args"]
            if call["tool"] == "docker" and args[0] == "compose":
                step = "ask" if "exec" in args else "stack " + next(word for word in args if word in ("up", "down"))
            elif call["tool"] == "docker":
                step = {"ps": "look", "pull": "image pull", "image": "image inspect"}[args[0]]
            elif call["tool"] == "git":
                step = "git " + next(word for word in args if word in ("clone", "fetch", "checkout", "rev-parse"))
            elif call["tool"] == "nvidia-smi":
                step = "card"
            else:
                step = "driver" if "e2e-reliability" in args[0] else "tests"
            if not (step == "ask" and short and short[-1] == "ask"):
                short.append(step)
        return short


ONE_HEAD = ["git fetch", "git checkout", "git rev-parse"]
IMAGES = ["image pull", "image inspect"] * 5
WITH_THE_CARD = ["card", "stack up", "ask", "driver", "tests", "ask", "stack down"]


def test_a_night_does_its_steps_in_order_and_the_report_holds_what_it_measured(tmp_path):
    night = Night(tmp_path).run()
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.did() == ["git clone"] + ONE_HEAD + ["look"] + IMAGES + WITH_THE_CARD
    report = night.report
    assert report["result"] == "passed" and report["commit"] == HEAD and report["stale"] == []
    assert report["left_stack"] == "none" and report["script"] == "as it was started"
    assert report["waited_for_images"] == "0 s" and report["card"] == "no process computes on it"
    assert set(report["images"]) == set(nightly.IMAGES)
    for service, about in report["images"].items():
        assert about["image"] == f"ghcr.io/inferstep/{nightly.IMAGES[service]}:dev"
        assert about["digest"].startswith("sha256:") and about["commit"] == HEAD
    assert report["tasks"] == [{"task": task, "seconds": 10.5 + n, "passed": True, "defects": 0} for n, task in enumerate(nightly.TASKS)]
    assert report["tests"] == {"expected": 125, "collected": 125, "passed": 125, "failed": 0, "skipped": 0, "skip_reasons": {}}
    for when in ("at the start", "at the end"):
        assert report["services"][when] == {
            "the proxy's /ready": {"status": 200, "answer": WHOLE["proxy/ready"][1]},
            "the lens's /ready": {"status": 200, "answer": WHOLE["lens/ready"][1]},
            "the lens's /health": {"status": 200, "answer": WHOLE["lens/health"][1]}}
    assert report["not_whole_at_the_end"] == [] and report["stack_stopped"] == "yes"
    assert report["sent"] == "not sent: no token or no issue was given"
    text = nightly.as_text(report)
    assert "- The services at the end of the run: whole" in text
    assert "- The lens about itself: self test passed: yes; C(x) calibrated: yes; G(x) calibrated: yes" in text
    assert "- Waited for the images of that commit: 0 s" in text and "- The card before the start: no process computes on it" in text


def test_the_stack_has_a_name_and_ports_of_its_own_and_the_driver_and_the_tests_are_sent_to_it(tmp_path):
    night = Night(tmp_path).run()
    calls = night.calls()
    for call in calls:
        if call["tool"] == "docker" and call["args"][0] == "compose":
            args = call["args"]
            assert args[args.index("-p") + 1] == "atlas-nightly"
            assert args[args.index("--env-file") + 1] == str(night.dir / "run.env")
            assert args[args.index("-f") + 1] == str(night.dir / "tree" / "docker-compose.yml")
    settings = (night.dir / "run.env").read_text().splitlines()
    # The server's own settings come first, and the run's settings after them, so a port of the server's file does not hold.
    assert settings[0] == "ATLAS_MODEL_FILE=model.gguf" and settings.index("ATLAS_PROXY_PORT=8090") < settings.index("ATLAS_PROXY_PORT=18090")
    assert "ATLAS_IMAGE_TAG=dev" in settings and f"ATLAS_PROJECT_DIR={night.dir / 'workspace'}" in settings
    assert f"ATLAS_SECRETS_DIR={night.dir / 'secrets'}" in settings
    # The proxy and the sandbox mount the same workspace: both run as the user of the run.
    for name in ("ATLAS_PROXY_UID", "ATLAS_SANDBOX_UID"):
        assert f"{name}={os.getuid()}" in settings and f"{name.replace('UID', 'GID')}={os.getgid()}" in settings
    driver = next(call for call in calls if call["tool"] == "python-for-the-run" and "e2e" in call["args"][0])
    said = dict(zip(driver["args"][1::2], driver["args"][2::2]))
    assert said["--url"] == "http://127.0.0.1:18090" and said["--tasks"] == ",".join(nightly.TASKS) and said["--reps"] == "1"
    assert said["--compose-project"] == "atlas-nightly" and said["--sandbox-container"] == "atlas-nightly-sandbox-1"
    assert said["--workspace"] == str(night.dir / "workspace" / "_reliability") and said["--commit"] == HEAD
    tests = next(call for call in calls if call["args"][:2] == ["-m", "pytest"])
    assert tests["args"][2:5] == ["-m", "integration", "tests/infrastructure"]
    assert tests["env"]["SANDBOX_URL"] == "http://127.0.0.1:18020" and tests["env"]["LLAMA_URL"] == "http://127.0.0.1:18080"
    assert tests["env"]["ATLAS_PROXY_URL"] == "http://127.0.0.1:18090"
    # One of the tests compares the served model with this name, and skips when it has none.
    assert tests["env"]["ATLAS_MODEL_NAME"] == "model" and driver["env"]["ATLAS_MODEL_NAME"] == "model"
    assert [call["env"] for call in calls if call["tool"] in ("git", "docker", "nvidia-smi") and "ATLAS_MODEL_NAME" in call["env"]] == []


def test_the_stack_of_the_run_has_no_service_token(tmp_path):
    # The driver and the tests send no token, so a stack that asks for one would refuse every session and most tests.
    night = Night(tmp_path).run()
    assert night.report["result"] == "passed"
    assert list((night.dir / "secrets").iterdir()) == []
    assert [call for call in night.calls() if "ATLAS_SERVICE_TOKEN_FILE" in call["env"]] == []
    driver = (ROOT / "scripts" / "e2e-reliability.py").read_text(encoding="utf-8")
    assert "Authorization" not in driver, (
        "the driver now sends a token. Then the stack of the nightly run can have one: see write_settings in "
        "scripts/nightly_run.py, which refuses a token file today.")


def test_beside_a_token_file_in_its_folder_the_run_starts_no_stack_and_says_how_to_fix_it(tmp_path):
    night = Night(tmp_path)
    (night.dir / "secrets").mkdir()
    (night.dir / "secrets" / "service-token").write_text("a token of an earlier setup")
    night.run()
    assert night.done.returncode == 1
    assert "secrets/service-token is there" in night.report["result"] and "Fix: remove that file." in night.report["result"]
    assert "stack up" not in night.did()


# --- the images of the head ---------------------------------------------------------------------------------------

def stale(image, built_from=OLD[:12]):
    return f"not run: the images are not of the head of the branch yet (ghcr.io/inferstep/{image}:dev was built from {built_from})"


def test_with_an_image_that_was_not_built_from_the_head_the_night_is_not_run_and_that_is_no_failure(tmp_path):
    night = Night(tmp_path, built_from={"atlas-sandbox": OLD}).run()
    assert night.done.returncode == 0
    assert night.report["result"] == stale("atlas-sandbox")
    did = night.did()
    assert "card" not in did and "stack up" not in did and "driver" not in did and "tests" not in did
    assert "tasks" not in night.report and "tests" not in night.report and "stack_stopped" not in night.report


@pytest.mark.parametrize("service", sorted(nightly.IMAGES))
def test_each_of_the_five_images_is_compared_with_the_head(service):
    images = {name: {"image": f"ghcr.io/inferstep/{image}:dev", "digest": "sha256:0", "commit": HEAD}
              for name, image in nightly.IMAGES.items()}
    assert nightly.stale_images(images, HEAD) == []
    images[service]["commit"] = OLD
    assert nightly.stale_images(images, HEAD) == [f"ghcr.io/inferstep/{nightly.IMAGES[service]}:dev was built from {OLD[:12]}"]
    assert nightly.judge({"stale": nightly.stale_images(images, HEAD)}) == stale(nightly.IMAGES[service])


def test_an_image_that_names_no_commit_is_not_of_the_head(tmp_path):
    night = Night(tmp_path, built_from={"atlas-proxy": ""}).run()
    assert night.report["result"] == stale("atlas-proxy", "no named commit")


def test_images_that_come_some_minutes_after_the_push_are_waited_for(tmp_path):
    # The first look finds the images of the commit before; the second finds the images of the head.
    night = Night(tmp_path, built_from=[dict.fromkeys(nightly.IMAGES.values(), OLD), {}]).run("--image-wait-minutes", "0.02")
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert night.report["waited_for_images"] == "0.12 s"
    assert night.did() == ["git clone"] + ONE_HEAD + ["look"] + IMAGES + ONE_HEAD + IMAGES + WITH_THE_CARD


def test_images_that_do_not_come_in_the_time_of_the_wait_leave_the_night_not_run(tmp_path):
    night = Night(tmp_path, built_from={"atlas-v3": OLD}).run("--image-wait-minutes", "0.02")
    assert night.done.returncode == 0
    assert night.report["result"] == stale("atlas-v3")
    assert night.report["waited_for_images"] == "1.2 s" and night.did().count("image pull") == 5 * 11
    assert "- Waited for the images of that commit: 1.2 s" in nightly.as_text(night.report)


def test_when_the_branch_moves_during_the_wait_the_run_measures_its_new_head(tmp_path):
    # The images are of a newer push than the head that was taken first.
    night = Night(tmp_path, head=[HEAD, NEWER], built_from=dict.fromkeys(nightly.IMAGES.values(), NEWER)).run("--image-wait-minutes", "0.02")
    assert night.report["result"] == "passed" and night.report["commit"] == NEWER
    driver = next(call for call in night.calls() if call["tool"] == "python-for-the-run" and "e2e" in call["args"][0])
    assert driver["args"][driver["args"].index("--commit") + 1] == NEWER


# --- the card -----------------------------------------------------------------------------------------------------

def test_when_another_run_holds_the_lock_the_run_starts_no_stack_and_says_so(tmp_path):
    night = Night(tmp_path)
    with open(night.lock, "a") as held:
        fcntl.flock(held, fcntl.LOCK_EX)
        night.run()
    assert night.done.returncode == 0
    assert night.report["result"] == "not run: the card was in use"
    # What needs no card was done; the card was not read and no stack was started or stopped.
    assert night.did() == ["git clone"] + ONE_HEAD + ["look"] + IMAGES
    assert "stack_stopped" not in night.report


def test_when_a_process_holds_the_card_the_run_starts_no_stack_and_gives_the_amount(tmp_path):
    # A stack that simply runs on the server holds the card and takes no lock.
    night = Night(tmp_path, card=["4242, 13200", "4250, 300"]).run()
    assert night.done.returncode == 0
    assert night.report["result"] == "not run: the card was in use (2 process(es) hold 13500 MiB)"
    assert night.did()[-1] == "card" and "stack_stopped" not in night.report
    assert nightly.NOT_RUN == "not run: the card was in use"


@pytest.mark.parametrize("status, printed, in_use, says", [
    (0, "", False, "no process computes on it"),
    (0, "4242, 13200\n", True, "1 process(es) hold 13200 MiB"),
    (0, "4242, 13200\n4250, 300\n", True, "2 process(es) hold 13500 MiB"),
    # A process whose amount the card does not give still holds it.
    (0, "4242, [N/A]\n", True, "1 process(es) hold 0 MiB"),
    (0, "a warning of the driver, with a comma\n", False, "no process computes on it"),
    (9, "NVIDIA-SMI has failed\n", False, "not read: nvidia-smi ended with status 9"),
])
def test_how_the_answer_of_the_card_is_read(monkeypatch, status, printed, in_use, says):
    asked = []
    monkeypatch.setattr(nightly, "command", lambda argv, limit: asked.append(argv) or subprocess.CompletedProcess(argv, status, printed, ""))
    assert nightly.card_use() == (in_use, says)
    assert asked == [["nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"]]


def test_a_card_that_cannot_be_read_counts_as_free_and_the_report_says_why(tmp_path):
    night = Night(tmp_path, fail=["nvidia-smi"]).run()
    assert night.report["result"] == "passed" and night.report["card"] == "not read: nvidia-smi ended with status 1"


def test_without_nvidia_smi_the_run_goes_on_and_the_report_says_that_the_card_was_not_read(tmp_path):
    night = Night(tmp_path)
    (tmp_path / "bin" / "nvidia-smi").unlink()
    night.run()
    assert night.report["result"] == "passed" and night.report["card"].startswith("not read: nvidia-smi gave no answer")


def test_the_card_is_read_after_a_left_stack_is_stopped_and_before_the_stack_is_started(tmp_path):
    did = Night(tmp_path, left=["0a1b2c3d4e5f"]).run().did()
    assert did.index("stack down") < did.index("card") < did.index("stack up")


def test_a_run_holds_the_lock_while_its_stack_runs(tmp_path):
    night = Night(tmp_path, sleep={UP: 4})
    process = night.start()
    try:
        night.wait_for("stack up")
        with open(night.lock, "a") as other:
            with pytest.raises(OSError):
                fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
    finally:
        process.communicate(timeout=60)
    with open(night.lock, "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)


def test_the_pulls_come_before_the_lock_and_the_time_limit_does_not_count_them(tmp_path):
    # A pull of 4 s and a limit of 3 s for the run: the run passes, and the lock of the card is free during the pull.
    night = Night(tmp_path, sleep={PULL: 4})
    process = subprocess.Popen(night.command("--limit-minutes", "0.05"), env=night.env(), stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, text=True)
    try:
        night.wait_for("image pull")
        with open(night.lock, "a") as other:
            fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
            fcntl.flock(other, fcntl.LOCK_UN)
    finally:
        process.communicate(timeout=60)
    assert process.returncode == 0 and night.report["result"] == "passed"


def test_a_lock_file_that_cannot_be_used_fails_the_run_and_the_report_says_in_which_step(tmp_path):
    # For example a lock file that another user made and that this user cannot write.
    night = Night(tmp_path)
    night.lock.mkdir()
    night.run()
    assert night.done.returncode == 1
    assert night.report["result"].startswith("failed: an error of the run itself, in the step: take the lock of the card: IsADirectoryError(")
    assert "stack up" not in night.did()


def test_a_second_run_does_not_stop_the_stack_of_a_run_that_is_in_its_middle(tmp_path):
    # For Docker the stack of the first run is "a stack of this name that is there": the look of a second run would
    # find it. So the lock of the folder is taken before that look.
    night = Night(tmp_path, sleep={DRIVER: 6}, left=["0a1b2c3d4e5f"])
    first = night.start()
    try:
        night.wait_for("driver")
        before = night.did()
        second = subprocess.run(night.command(), env=night.env(), capture_output=True, text=True, timeout=60, check=False)
        assert second.returncode == 0 and "another run of this folder is going" in second.stdout
        assert night.did() == before, "the second run called a tool while the first run was in its middle"
    finally:
        first.communicate(timeout=60)
    assert night.report["result"] == "passed"
    assert night.did()[-3:] == ["tests", "ask", "stack down"]


def test_only_one_run_of_a_folder_goes_at_a_time(tmp_path):
    night = Night(tmp_path)
    with open(night.dir / "run.lock", "a") as going:
        fcntl.flock(going, fcntl.LOCK_EX)
        night.run()
    assert night.done.returncode == 0 and "another run of this folder is going" in night.done.stdout
    assert night.calls() == [] and not (night.dir / "reports").exists()


# --- what fails a run ---------------------------------------------------------------------------------------------

@pytest.mark.parametrize("plan, says, last_step", [
    ({"fail": ["fetch --quiet"]}, "failed: the newest commit of the branch: status 1: a planted failure of: fetch --quiet", "git fetch"),
    ({"fail": ["ps --all"]}, "failed: the look for a stack that an earlier run left: status 1", "look"),
    ({"fail": ["pull --quiet ghcr.io/inferstep/atlas-lens:dev"]}, "failed: the pull of ghcr.io/inferstep/atlas-lens:dev: status 1", "image pull"),
    ({"fail": [UP]}, "failed: the start of the stack: status 1", "stack up"),
    ({"driver_rows": None, "driver_status": 2}, "failed: the driver left no result (status 2): the driver ran", "driver"),
    ({"driver_rows": rows()[:2]}, "failed: the driver gave no result for ask_explain (status 0)", "driver"),
    ({"tests": None, "tests_status": 4}, "failed: the tests left no result (status 4): the tests ran", "tests"),
])
def test_a_step_that_fails_ends_the_run_with_its_reason_and_the_stack_is_stopped(tmp_path, plan, says, last_step):
    night = Night(tmp_path, **plan).run()
    assert night.done.returncode == 1
    assert night.report["result"].startswith(says), night.report["result"]
    did = night.did()
    before_the_stop = [step for step in did if step != "stack down"]
    assert before_the_stop[-1] == last_step, did
    if "stack up" in did:
        assert did[-1] == "stack down" and did.count("stack down") == 1 and night.report["stack_stopped"] == "yes"
    else:
        assert "stack down" not in did and "stack_stopped" not in night.report


def measured(defects=(0, 0, 0), collected=125, failed=0, at_the_end=(), skipped=None):
    """What a run that came to its end holds in its report."""
    skipped = skipped or {}
    return {"stale": [], "tasks": [{"task": task, "seconds": 10.5, "passed": True, "defects": count}
                                   for task, count in zip(nightly.TASKS, defects)],
            "tests": {"expected": 125, "collected": collected, "passed": collected - failed - sum(skipped.values()), "failed": failed,
                      "skipped": sum(skipped.values()), "skip_reasons": skipped},
            "not_whole_at_the_end": list(at_the_end)}


@pytest.mark.parametrize("report, says", [
    (measured(), "passed"),
    (measured(defects=(0, 2, 0)), "failed: offbyone: 2 defect(s) of the harness"),
    (measured(collected=13), "failed: 13 tests were collected, and there are 125"),
    (measured(collected=126), "failed: 126 tests were collected, and there are 125"),
    (measured(failed=3), "failed: 3 test(s) failed"),
    # A skipped test counts as collected: all 125 are there, and 8 of them were not run.
    (measured(skipped={"the TUI is not built": 8}), "failed: 8 test(s) were skipped (8: the TUI is not built)"),
    (measured(skipped={"javac": 9, "kotlinc": 8, "ruby": 8, "php": 8, "the TUI": 8, "one more": 1, "another": 1}),
     "failed: 43 test(s) were skipped (9: javac; 8: kotlinc; 8: ruby; 8: php; 8: the TUI; and 2 more reason(s))"),
    ({**measured(), "tests": {"expected": 125, "collected": 125, "passed": 121, "failed": 0, "skipped": 4}},
     "failed: 4 test(s) were skipped (the result file gives no reason)"),
    (measured(at_the_end=["the proxy says that the lens is not ready"]),
     "failed: at the end of the run the proxy says that the lens is not ready"),
])
def test_each_fault_that_a_run_measured_fails_it_and_is_named(report, says):
    assert nightly.judge(report) == says


def test_a_run_that_measured_faults_fails_and_names_each_fault(tmp_path):
    night = Night(tmp_path, driver_rows=rows(1, 0, 0), tests={"tests": 13, "failures": 1, "errors": 1}).run()
    assert night.done.returncode == 1
    assert night.report["result"] == ("failed: add_function: 1 defect(s) of the harness; 13 tests were collected, and there "
                                      "are 125; 2 test(s) failed")
    assert night.did()[-4:] == ["driver", "tests", "ask", "stack down"]


def test_a_night_with_skipped_tests_fails_and_the_result_gives_the_reasons(tmp_path):
    # The result file says tests="125" all the same: a skipped test counts as collected.
    reasons = ["javac is not on the path"] * 3 + ["tui/atlas-tui not built (cd tui &amp;&amp; go build -o atlas-tui .)"] * 2 + ["ruby is not on the path"]
    night = Night(tmp_path, tests={"tests": 125, "skipped": 6, "reasons": reasons}).run()
    assert night.done.returncode == 1
    assert night.report["tests"]["skip_reasons"] == {
        "javac is not on the path": 3, "tui/atlas-tui not built (cd tui && go build -o atlas-tui .)": 2, "ruby is not on the path": 1}
    assert night.report["result"] == ("failed: 6 test(s) were skipped (3: javac is not on the path; 2: tui/atlas-tui not built "
                                      "(cd tui && go build -o atlas-tui .); 1: ruby is not on the path)")
    assert night.report["tests"]["passed"] == 119


def test_a_task_whose_change_did_not_land_is_reported_and_does_not_fail_the_run(tmp_path):
    landed = rows()
    landed[1]["task_passed"] = False
    night = Night(tmp_path, driver_rows=landed).run()
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert [task["passed"] for task in night.report["tasks"]] == [True, False, True]


@pytest.mark.parametrize("slow, step", [(UP, "start the stack"), (DRIVER, "the smoke run"),
                                        (TESTS, "the tests that the plain jobs leave out")])
def test_at_the_time_limit_the_run_ends_and_the_stack_is_stopped(tmp_path, slow, step):
    night = Night(tmp_path, sleep={slow: 30})
    start = time.monotonic()
    night.run("--limit-minutes", "0.05")
    assert time.monotonic() - start < 25, "the run did not end at its limit: the slow step went on"
    assert night.done.returncode == 1
    assert night.report["result"] == f"failed: the time limit of 0.05 minutes was reached, in the step: {step}"
    assert night.did()[-1] == "stack down" and night.report["stack_stopped"] == "yes"


def test_a_run_that_is_told_to_stop_stops_its_stack_and_says_so(tmp_path):
    night = Night(tmp_path, sleep={DRIVER: 30})
    process = night.start()
    night.wait_for("driver")
    process.send_signal(signal.SIGTERM)
    process.communicate(timeout=30)
    assert process.returncode == 1
    assert night.report["result"] == "failed: the run was told to stop, in the step: the smoke run"
    assert night.did()[-1] == "stack down"


def test_a_run_that_is_told_to_stop_before_it_took_the_card_writes_its_report(tmp_path):
    night = Night(tmp_path, sleep={PULL: 30})
    process = night.start()
    night.wait_for("image pull")
    process.send_signal(signal.SIGTERM)
    process.communicate(timeout=30)
    assert process.returncode == 1
    assert night.report["result"] == "failed: the run was told to stop, in the step: pull the images"
    assert "stack up" not in night.did() and "stack down" not in night.did()


def test_an_error_of_the_run_itself_is_in_the_report_and_the_stack_is_stopped(tmp_path):
    # A row of the driver that lacks a field: the run did not expect it.
    night = Night(tmp_path, driver_rows=[{"task": "add_function"}]).run()
    assert night.done.returncode == 1
    assert night.report["result"].startswith("failed: an error of the run itself, in the step: the smoke run: KeyError(")
    assert night.did()[-1] == "stack down" and night.report["stack_stopped"] == "yes"


def test_a_stack_that_could_not_be_stopped_fails_the_run_and_the_result_says_so_first(tmp_path):
    night = Night(tmp_path, fail=["down --volumes"]).run()
    assert night.done.returncode == 1
    assert night.report["stack_stopped"] == "no: status 1: a planted failure of: down --volumes"
    assert night.report["result"] == ("failed: the stack was not stopped (status 1: a planted failure of: down --volumes); "
                                      "before that: passed")


def test_without_the_servers_settings_file_the_run_fails_before_it_starts_a_stack(tmp_path):
    night = Night(tmp_path)
    (night.dir / "nightly.env").unlink()
    night.run()
    assert night.done.returncode == 1
    assert "nightly.env is not there" in night.report["result"] and "Fix: write it once" in night.report["result"]
    assert "stack up" not in night.did()


@pytest.mark.parametrize("settings, name", [
    ("ATLAS_MODEL_NAME=first\nATLAS_MODEL_FILE=m.gguf\nATLAS_MODEL_NAME=the-last-one\n", "the-last-one"),
    ('ATLAS_MODEL_FILE=m.gguf\nexport ATLAS_MODEL_NAME = "a quoted name"  \n', "a quoted name"),
    ("# ATLAS_MODEL_NAME=in-a-comment\nATLAS_MODEL_NAME='single'\n", "single"),
])
def test_the_name_of_the_model_is_read_from_the_servers_settings_file(tmp_path, settings, name):
    night = Night(tmp_path)
    (night.dir / "nightly.env").write_text(settings)
    night.run()
    assert night.report["result"] == "passed", night.report["result"]
    tests = next(call for call in night.calls() if call["args"][:2] == ["-m", "pytest"])
    assert tests["env"]["ATLAS_MODEL_NAME"] == name


@pytest.mark.parametrize("settings", ["ATLAS_MODEL_FILE=m.gguf\n", "ATLAS_MODEL_NAME=\n", "# ATLAS_MODEL_NAME=model\n", "XATLAS_MODEL_NAME=model\n"])
def test_a_settings_file_with_no_name_of_the_model_stops_the_run_before_the_stack(tmp_path, settings):
    # With no name the one test that compares the served model would skip, and a skipped test fails the night.
    night = Night(tmp_path)
    (night.dir / "nightly.env").write_text(settings)
    night.run()
    assert night.done.returncode == 1
    assert "nightly.env has no line `ATLAS_MODEL_NAME=<name>`" in night.report["result"] and "Fix: add the line" in night.report["result"]
    assert "stack up" not in night.did() and "driver" not in night.did()


@pytest.mark.parametrize("folder", ["relative/folder", "{root}/not-there"])
def test_a_folder_that_cannot_be_used_ends_the_run_with_status_2_and_nothing_is_called(tmp_path, folder):
    night = Night(tmp_path)
    done = subprocess.run([sys.executable, str(SCRIPT), "--dir", folder.replace("{root}", str(tmp_path))], env=night.env(),
                          cwd=tmp_path, capture_output=True, text=True, timeout=60)
    assert done.returncode == 2 and "is not a full path of a folder that is there" in done.stderr
    assert night.calls() == []


def test_the_run_writes_only_inside_its_own_folder(tmp_path):
    night = Night(tmp_path)
    before = sorted(str(path.relative_to(tmp_path)) for path in tmp_path.rglob("*") if night.dir not in path.parents and path != night.dir)
    night.run()
    after = sorted(str(path.relative_to(tmp_path)) for path in tmp_path.rglob("*") if night.dir not in path.parents and path != night.dir)
    # Outside the folder: the lock file of the card, at the place the run was given, and the stand-ins' own list of calls.
    assert sorted(set(after) - set(before)) == ["calls.log", "card.lock"]


def test_the_stand_ins_start_where_the_path_of_python_holds_a_space(tmp_path):
    root = tmp_path / "a folder"
    root.mkdir()
    venv.create(root / "a python", with_pip=False, symlinks=True)
    night = Night(root, python=str(root / "a python" / "bin" / "python")).run()
    assert night.done.returncode == 0 and night.report["result"] == "passed", night.report["result"]


# --- whether the services are whole -------------------------------------------------------------------------------

def answers(**other):
    """The answers of whole services, as the run keeps them, with these other ones in their place."""
    whole = {name: {"status": WHOLE[key][0], "answer": WHOLE[key][1]}
             for name, key in zip(nightly.QUESTIONS, ("proxy/ready", "lens/ready", "lens/health"))}
    return {**whole, **{name: {"status": said[0], "answer": said[1]} for name, said in other.items()}}


@pytest.mark.parametrize("said, faults", [
    (answers(), []),
    (answers(**{"the proxy's /ready": proxy_says(lens_ready=False, lens_reason="it has no C(x) model loaded")}),
     ["the proxy says that the lens is not ready (it has no C(x) model loaded)"]),
    (answers(**{"the proxy's /ready": proxy_says(sandbox=False)}), ["the proxy says that the sandbox is not ready"]),
    (answers(**{"the proxy's /ready": proxy_says(inference=False, v3=False)}),
     ["the proxy says that the model server is not ready", "the proxy says that v3-service is not ready"]),
    # A part that the proxy does not give is not taken as ready.
    (answers(**{"the proxy's /ready": [200, {"ready": True, "inference": True, "sandbox": True, "v3": True}]}),
     ["the proxy says that the lens is not ready"]),
    (answers(**{"the proxy's /ready": [0, "no answer: timed out"]}), ["the proxy gave no answer about the services (no answer: timed out)"]),
    (answers(**{"the lens's /ready": [503, {"detail": {"ready": False, "reason": "the cost field has another dimension than the model"}}]}),
     ["the lens says that it cannot score (the cost field has another dimension than the model)"]),
    (answers(**{"the lens's /ready": [0, "no answer: timed out"]}), ["the lens says that it cannot score (no answer: timed out)"]),
    # The lens's /health is kept for the reader and is not judged: a lens with no calibration scores.
    (answers(**{"the lens's /health": [200, {"status": "degraded"}]}), []),
])
def test_what_the_answers_of_the_services_say_is_wrong(said, faults):
    assert nightly.not_whole(said) == faults


@pytest.mark.parametrize("plan, says", [
    ({"answers": {"lens/ready": [503, {"detail": {"ready": False, "reason": "the cost field has another dimension than the model"}}]}},
     "the lens says that it cannot score (the cost field has another dimension than the model)"),
    ({"fail": ["exec -T atlas-proxy"]},
     "the proxy gave no answer about the services (no answer that can be read: a planted failure of: exec -T atlas-proxy)"),
])
def test_a_service_that_is_not_whole_after_the_start_fails_the_run_and_no_session_is_run(tmp_path, plan, says):
    # The stack is "healthy" for compose all the same: the health check of the lens asks whether the process serves.
    night = Night(tmp_path, **plan).run()
    assert night.done.returncode == 1
    assert night.report["result"] == "failed: a service was not whole when the stack had started: " + says
    did = night.did()
    assert did[-3:] == ["stack up", "ask", "stack down"] and "driver" not in did and "tests" not in did
    assert set(night.report["services"]["at the start"]) == set(nightly.QUESTIONS)


def test_a_service_that_is_whole_a_moment_after_the_start_is_waited_for(tmp_path):
    night = Night(tmp_path, answers={"proxy/ready": [proxy_says(lens_ready=False), WHOLE["proxy/ready"]]}).run("--settle-seconds", "1")
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    asked = [call for call in night.calls() if "exec" in call["args"] and call["args"][-1].endswith(":8090/ready")]
    assert len(asked) == 3, "twice at the start, once at the end"


def test_a_service_that_is_not_whole_at_the_end_fails_the_run_and_the_result_names_it(tmp_path):
    # Whole at the start, and not when the sessions and the tests are over.
    night = Night(tmp_path, answers={"proxy/ready": [WHOLE["proxy/ready"], proxy_says(v3=False)]}).run()
    assert night.done.returncode == 1
    assert night.report["result"] == "failed: at the end of the run the proxy says that v3-service is not ready"
    assert night.did()[-4:] == ["driver", "tests", "ask", "stack down"]
    assert "- The services at the end of the run: the proxy says that v3-service is not ready" in nightly.as_text(night.report)


def test_a_lens_with_no_calibration_is_in_the_report_and_does_not_fail_the_run(tmp_path):
    lens = {**WHOLE["lens/health"][1]["subsystems"]["lens"], "cx_calibrated": False}
    night = Night(tmp_path, answers={"lens/health": [200, {"status": "healthy", "subsystems": {"lens": lens}}]}).run()
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert "- The lens about itself: self test passed: yes; C(x) calibrated: no; G(x) calibrated: yes" in nightly.as_text(night.report)


def test_each_service_is_asked_inside_its_own_container_of_the_runs_own_stack(tmp_path):
    night = Night(tmp_path).run()
    asked = [call["args"] for call in night.calls() if "exec" in call["args"]]
    assert len(asked) == 2 * len(nightly.QUESTIONS)
    for args, (service, url) in zip(asked, list(nightly.QUESTIONS.values()) * 2):
        assert args[args.index("-p") + 1] == "atlas-nightly"
        assert args[args.index("exec"):] == ["exec", "-T", service, "curl", "-s", "-m", "60", "-w", "\n%{http_code}", url]


@pytest.mark.parametrize("printed, status, answer", [
    ('{"ready": true}\n200', 200, {"ready": True}),
    ('{"ready": true}\n\n200', 200, {"ready": True}),
    ('time="2026" level=warning msg="The \\"X\\" variable is not set"\n{"ready": false}\n503', 503, {"ready": False}),
    ("curl: (7) Failed to connect\n000", 0, "no answer that can be read: curl: (7) Failed to connect\n000"),
    ("", 0, "no answer that can be read: "),
])
def test_the_answer_of_a_service_is_read_from_its_last_two_lines(printed, status, answer):
    assert nightly.read_answer(printed) == {"status": status, "answer": answer}


def test_each_question_goes_to_a_service_of_the_compose_file_at_the_port_of_its_health_check():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    for service, url in nightly.QUESTIONS.values():
        block = re.search(rf"^  {re.escape(service)}:\n(.*?)(?=^  [\w-]+:\n|\Z)", compose, re.M | re.S).group(1)
        (checked,) = re.findall(r'test: \["CMD", "curl", "-sf", "(http://localhost:\d+)/health"\]', block)
        assert url.startswith(checked + "/"), (service, url, checked)
    proxy = (ROOT / "proxy" / "main.go").read_text(encoding="utf-8")
    said = set(re.findall(r'^\t\t"(\w+)":\s+\w+,$', proxy.split("func handleReady(", 1)[1].split("\n}\n", 1)[0], re.M))
    assert said == {"ready", *nightly.PROXY_PARTS}, (
        f"the proxy's /ready gives {sorted(said)}, and the nightly run judges {sorted(nightly.PROXY_PARTS)}. Fix: "
        "PROXY_PARTS in scripts/nightly_run.py.")


# --- the report on GitHub -----------------------------------------------------------------------------------------

class Issue(http.server.BaseHTTPRequestHandler):
    seen: list = []

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        Issue.seen.append((self.path, self.headers["Authorization"], json.loads(body)))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *_args):
        pass


TOKEN = "a-token-made-for-this-test-0123456789"


@contextlib.contextmanager
def a_stand_in_for_github():
    """A server on this machine that takes the change of an issue's text. Gives its port."""
    Issue.seen = []
    server = http.server.HTTPServer(("127.0.0.1", 0), Issue)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server.server_port
    finally:
        server.shutdown()
        thread.join(timeout=10)


def test_with_a_token_the_report_replaces_the_text_of_the_one_issue_and_the_token_is_written_nowhere(tmp_path):
    with a_stand_in_for_github() as port:
        night = Night(tmp_path).run("--issue", "7", "--api", f"http://127.0.0.1:{port}", ATLAS_NIGHTLY_TOKEN=TOKEN)
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    ((path, sent_with, body),) = Issue.seen
    assert path == "/repos/inferstep/ATLAS/issues/7" and sent_with == f"Bearer {TOKEN}"
    assert body["body"].startswith("**Nightly run of `dev`: passed**") and HEAD in body["body"]
    assert night.report["sent"] == "sent to issue 7 (status 200)"
    assert TOKEN not in night.done.stdout + night.done.stderr
    for path in tmp_path.rglob("*"):
        if path.is_file():
            assert TOKEN not in path.read_text(errors="replace"), path


def test_no_command_that_the_run_starts_has_the_token(tmp_path):
    # The driver and the tests are code of the tree, and what they print is kept in a log.
    with a_stand_in_for_github() as port:
        night = Night(tmp_path).run("--issue", "7", "--api", f"http://127.0.0.1:{port}", ATLAS_NIGHTLY_TOKEN=TOKEN)
    calls = night.calls()
    assert {"git", "docker", "python-for-the-run", "nvidia-smi"} == {call["tool"] for call in calls} and len(calls) >= 25
    assert [call for call in calls if call["has_the_token"]] == []
    assert len(Issue.seen) == 1, "the run itself still has the token for the report"


NOT_SENT = "not sent: the token goes only to https://api.github.com, and --api names another address"


@pytest.mark.parametrize("address", ["http://localhost:{port}", "http://127.0.0.1:{port}/elsewhere", "http://127.0.0.1:{port}?",
                                     "http://127.0.0.1:{port}.example.invalid", "https://127.0.0.1:{port}",
                                     "https://api.github.com.example.invalid", "http://api.github.com", "https://api.github.com/"])
def test_the_token_is_sent_to_no_other_address_than_githubs(monkeypatch, address):
    monkeypatch.setenv("ATLAS_NIGHTLY_TOKEN", TOKEN)
    with a_stand_in_for_github() as port:
        args = nightly.parse(["--dir", "/not-used", "--issue", "7", "--api", address.replace("{port}", str(port))])
        assert nightly.publish(args, "a report") == NOT_SENT
    assert Issue.seen == []


def test_a_run_whose_address_is_another_one_sends_nothing_and_keeps_its_result(tmp_path):
    with a_stand_in_for_github() as port:
        night = Night(tmp_path).run("--issue", "7", "--api", f"http://localhost:{port}", ATLAS_NIGHTLY_TOKEN=TOKEN)
    assert Issue.seen == [] and night.report["sent"] == NOT_SENT
    assert night.done.returncode == 0 and night.report["result"] == "passed"


@pytest.mark.parametrize("argument", ["--repo-url", "--branch", "--registry", "--tag", "--repository"])
def test_no_argument_points_the_run_at_another_repository_registry_or_branch(tmp_path, argument):
    night = Night(tmp_path).run(argument, "another")
    assert night.done.returncode == 2 and "unrecognized arguments" in night.done.stderr
    assert night.calls() == []


def test_a_token_with_no_issue_sends_nothing(tmp_path):
    night = Night(tmp_path).run(ATLAS_NIGHTLY_TOKEN="a-token")
    assert night.report["sent"] == "not sent: no token or no issue was given"


def test_a_report_that_cannot_be_sent_says_so_and_the_run_keeps_its_result(tmp_path):
    night = Night(tmp_path).run("--issue", "7", ATLAS_NIGHTLY_TOKEN="a-token")
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert night.report["sent"].startswith("not sent: ") and "a-token" not in json.dumps(night.report)


# --- a stack that an earlier run left -----------------------------------------------------------------------------

def test_a_stack_that_an_earlier_run_left_is_stopped_first_and_the_report_says_so(tmp_path):
    night = Night(tmp_path, left=["0a1b2c3d4e5f", "6a7b8c9d0e1f"]).run()
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert night.report["left_stack"] == "2 container(s) of an earlier run were stopped first"
    assert night.did() == ["git clone"] + ONE_HEAD + ["look", "stack down"] + IMAGES + WITH_THE_CARD
    look = next(call["args"] for call in night.calls() if call["args"][0] == "ps")
    assert look == ["ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project=atlas-nightly"]
    assert "- A stack that an earlier run left: 2 container(s) of an earlier run were stopped first" in nightly.as_text(night.report)


def test_a_left_stack_that_cannot_be_stopped_fails_the_run_before_anything_is_pulled_or_started(tmp_path):
    night = Night(tmp_path, left=["0a1b2c3d4e5f"], fail=["down --volumes"]).run()
    assert night.done.returncode == 1
    assert night.report["result"].startswith("failed: the stop of the stack that an earlier run left: status 1")
    assert "image pull" not in night.did() and "stack up" not in night.did()


def test_a_left_stack_is_stopped_also_in_a_night_whose_images_are_stale(tmp_path):
    night = Night(tmp_path, left=["0a1b2c3d4e5f"], built_from={"atlas-proxy": OLD}).run()
    assert night.report["result"] == stale("atlas-proxy")
    assert night.did()[4:6] == ["look", "stack down"] and "stack up" not in night.did()


# --- a new copy of the script -------------------------------------------------------------------------------------

THE_REST = ["look"] + IMAGES + WITH_THE_CARD


def test_when_taking_the_head_changes_the_script_the_new_copy_does_the_run_and_only_once(tmp_path):
    # Each checkout changes the file again: the new copy still does not start a third one.
    night = Night(tmp_path, from_the_tree=True, checkout_adds="\n# one more line\n").run()
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.did() == ONE_HEAD + ONE_HEAD + THE_REST
    assert night.report["result"] == "passed" and night.report["script"] == "the new copy: taking the head changed the script"
    assert len(list(night.dir.glob("reports/*"))) == 1, "the new copy writes into the folder of the run that started it"
    for name in (night.lock, night.dir / "run.lock"):
        with open(name, "a") as other:
            fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)


def test_the_folder_stays_locked_between_the_run_and_the_new_copy_that_it_starts(tmp_path):
    # A lock on an open file is dropped when the program is replaced, unless the file stays open across that.
    night = Night(tmp_path, from_the_tree=True, checkout_asks_for_the_lock=True).run()
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.report["result"] == "passed" and night.report["script"].startswith("the new copy")
    assert (tmp_path / "the_lock_when_the_new_copy_starts.txt").read_text() == "held\n"
    with open(night.dir / "run.lock", "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)


@pytest.mark.parametrize("given", ["20261008T040000Z 987654", "20261008T040000Z 0", "20261008T040000Z", "not a time 5"])
def test_a_start_time_with_a_number_that_is_not_the_kept_lock_file_is_a_run_like_any_other(tmp_path, given):
    # The number names no open file, or a file that is not the lock file: the lock file is opened afresh and taken.
    night = Night(tmp_path).run(ATLAS_NIGHTLY_STARTED_AGAIN=given)
    assert night.done.returncode == 0 and night.report["result"] == "passed", night.done.stdout + night.done.stderr
    with open(night.dir / "run.lock", "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)


def test_a_script_that_the_head_did_not_change_is_not_started_again(tmp_path):
    night = Night(tmp_path, from_the_tree=True).run()
    assert night.did() == ONE_HEAD + THE_REST and night.report["script"] == "as it was started"


def test_a_run_that_was_not_started_from_its_tree_is_never_replaced_by_the_trees_copy(tmp_path):
    night = Night(tmp_path, from_the_tree=True, checkout_adds="\n# one more line\n")
    night.script = SCRIPT
    night.run()
    assert night.did() == ONE_HEAD + THE_REST and night.report["script"] == "as it was started"


def test_the_new_copy_has_the_token_for_the_report_and_its_commands_do_not(tmp_path):
    with a_stand_in_for_github() as port:
        night = Night(tmp_path, from_the_tree=True, checkout_adds="\n# one more line\n").run(
            "--issue", "7", "--api", f"http://127.0.0.1:{port}", ATLAS_NIGHTLY_TOKEN=TOKEN)
    assert night.report["script"].startswith("the new copy") and night.report["sent"] == "sent to issue 7 (status 200)"
    assert [sent_with for _path, sent_with, _body in Issue.seen] == [f"Bearer {TOKEN}"]
    assert [call for call in night.calls() if call["has_the_token"]] == []
    for path in tmp_path.rglob("*"):
        if path.is_file():
            assert TOKEN not in path.read_text(errors="replace"), path


def test_a_new_copy_that_does_not_start_leaves_a_report_that_says_so(tmp_path):
    night = Night(tmp_path, from_the_tree=True, checkout_adds="\nthis is not Python (\n").run()
    assert night.done.returncode != 0 and "SyntaxError" in night.done.stderr
    assert night.did() == ONE_HEAD
    assert night.report["result"] == ("failed: taking the head changed the script of the run, the run started the new copy in "
                                      "its place, and that copy left no report")


# --- what the run takes from the repository -----------------------------------------------------------------------

def test_the_three_tasks_are_tasks_of_the_driver():
    driver = (ROOT / "scripts" / "e2e-reliability.py").read_text(encoding="utf-8")
    known = set(re.findall(r'^    "(\w+)": Task\(', driver, re.M)) | set(re.findall(r'^TASKS\["(\w+)"\] = Task\(', driver, re.M))
    assert len(nightly.TASKS) == 3 and set(nightly.TASKS) <= known, (nightly.TASKS, sorted(known))


def test_the_number_of_tests_is_the_number_of_test_functions_in_the_marked_files_and_the_gates_page_has_each():
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    table = dict(re.findall(r"^\| `(tests/infrastructure/test_\w+\.py)` \| (\d+) \|", page, re.M))
    marked = re.findall(r'"/(tests/infrastructure/test_\w+\.py)"', (ROOT / "tests" / "conftest.py").read_text(encoding="utf-8"))
    assert len(table) == 7 and sorted(table) == sorted(marked)
    in_the_files = {}
    for file in marked:
        tree = ast.parse((ROOT / file).read_text(encoding="utf-8"))
        functions = [node for node in ast.walk(tree) if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith("test")]
        many = [node.name for node in functions if any("parametrize" in ast.unparse(mark) for mark in node.decorator_list)]
        assert not many, (
            f"{file}: {many} now runs once for each of its values, so the number of its tests is no longer the number "
            "of its functions. Fix: count its values in this test, then give the new number to the page and to the run.")
        in_the_files[file] = len(functions)
    wrong = {file: (count, int(table[file])) for file, count in in_the_files.items() if count != int(table[file])}
    assert not wrong, (
        f"these files have another number of tests than their row of docs/quality/gates.md says (in the file, on the page): "
        f"{wrong}. The nightly run fails a night whose number of collected tests is not EXPECTED_TESTS. Fix: give the "
        "new number to the row of the page and to EXPECTED_TESTS in scripts/nightly_run.py.")
    assert sum(in_the_files.values()) == nightly.EXPECTED_TESTS


def test_every_port_and_every_image_of_the_compose_file_is_one_the_run_knows():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    published = set(re.findall(r'"127\.0\.0\.1:\$\{(ATLAS_\w+_PORT):-\d+\}:\d+"', compose))
    assert published and published <= set(nightly.PORTS), (
        f"docker-compose.yml publishes {sorted(published - set(nightly.PORTS))}, and the nightly run gives it no port of "
        "its own, so its stack would take the usual port. Fix: add it to PORTS in scripts/nightly_run.py.")
    assert len(set(nightly.PORTS.values())) == len(nightly.PORTS)
    images = set(re.findall(r"image: ghcr\.io/\$\{ATLAS_GHCR_OWNER:-inferstep\}/([\w-]+):", compose))
    assert images == set(nightly.IMAGES.values()), (images, nightly.IMAGES)
