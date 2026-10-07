"""The nightly run does its steps in order, says the truth in its report, and always stops its stack.

No test here reaches git, docker, a model or the network. The run is started
with a path that holds only stand-ins made for the test: `git`, `docker` and
the Python that would run the driver and the tests. Each stand-in writes down
how it was called and answers from a small plan. Every run has a time limit.
"""
import fcntl
import http.server
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "nightly_run.py"
sys.path.insert(0, str(ROOT / "scripts"))
import nightly_run as nightly  # noqa: E402

HEAD = "1" * 40
OLD = "2" * 40
# Words that are in one call only, for the plan of a stand-in: the start of the stack (and not its stop), the driver,
# the tests.
UP, DRIVER, TESTS = "up -d --wait", "scripts/e2e-reliability.py", "-m pytest"

STAND_IN = r'''#!{python}
"""A stand-in for {name}: it writes down its call and answers from the plan."""
import json, os, sys, time
home = {home!r}
plan = json.load(open(home + "/plan.json"))
args = sys.argv[1:]
call = " ".join(args)
with open(home + "/calls.log", "a") as log:
    log.write(json.dumps({{"tool": {name!r}, "args": args,
                          "env": {{k: v for k, v in os.environ.items() if k.endswith("_URL") or k == "ATLAS_SERVICE_TOKEN_FILE"}}}}) + "\n")
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
elif "rev-parse" in args:
    print(plan["head"])
'''
DOCKER = '''
if args[:2] == ["image", "inspect"]:
    name = args[2].split("/")[-1].split(":")[0]
    print(json.dumps([{"RepoDigests": [args[2].split(":")[0] + "@sha256:" + name.encode().hex()],
                       "Config": {"Labels": {"org.opencontainers.image.revision": plan["built_from"].get(name, plan["head"])}}}]))
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
        open(out, "w").write('<?xml version="1.0"?><testsuites><testsuite name="pytest" errors="%d" failures="%d" skipped="%d" '
                             'tests="%d" time="1.0"><testcase name="tests=9"/></testsuite></testsuites>'
                             % (t.get("errors", 0), t.get("failures", 0), t.get("skipped", 0), t["tests"]))
    print("the tests ran")
    sys.exit(plan.get("tests_status", 0))
'''


def rows(*defects):
    return [{"task": task, "wall_s": 10.5 + n, "task_passed": True, "defects": ["d"] * count}
            for n, (task, count) in enumerate(zip(nightly.TASKS, defects or (0, 0, 0)))]


class Night:
    """A folder for one run with its stand-ins, and what the run did."""

    def __init__(self, root, **plan):
        self.root = root
        self.dir = root / "nightly"
        self.lock = root / "card.lock"
        (root / "bin").mkdir()
        self.dir.mkdir()
        (self.dir / "nightly.env").write_text("ATLAS_MODEL_FILE=model.gguf\nATLAS_MODEL_NAME=model\nATLAS_PROXY_PORT=8090\n")
        for name, body in (("git", GIT), ("docker", DOCKER), ("python-for-the-run", PYTHON)):
            path = root / "bin" / name
            path.write_text(STAND_IN.format(python=sys.executable, name=name, home=str(root), body=body))
            path.chmod(0o755)
        self.plan = {"head": HEAD, "built_from": {}, "driver_rows": rows(), "tests": {"tests": 125, "skipped": 4}, **plan}
        (root / "plan.json").write_text(json.dumps(self.plan))

    def command(self, *more):
        return [sys.executable, str(SCRIPT), "--dir", str(self.dir), "--lock", str(self.lock),
                "--python", str(self.root / "bin" / "python-for-the-run"), "--api", "http://127.0.0.1:9", *more]

    def env(self, **more):
        # Only the stand-ins are on the path: the real git and docker cannot be reached.
        return {"PATH": str(self.root / "bin"), "HOME": str(self.root), **more}

    def run(self, *more, **env):
        self.done = subprocess.run(self.command(*more), env=self.env(**env), capture_output=True, text=True, timeout=60)
        return self

    @property
    def report(self):
        (path,) = self.dir.glob("reports/*/report.json")
        return json.loads(path.read_text())

    def calls(self):
        log = self.root / "calls.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def did(self):
        """What was called, in order, in a few words each."""
        short = []
        for call in self.calls():
            args = call["args"]
            if call["tool"] == "docker" and args[0] == "compose":
                short.append("stack " + next(word for word in args if word in ("up", "down")))
            elif call["tool"] == "docker":
                short.append("image " + args[0])
            elif call["tool"] == "git":
                short.append("git " + next(word for word in args if word in ("clone", "fetch", "checkout", "rev-parse")))
            else:
                short.append("driver" if "e2e-reliability" in args[0] else "tests")
        return short


def test_a_night_does_its_steps_in_order_and_the_report_holds_what_it_measured(tmp_path):
    night = Night(tmp_path).run()
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.did() == (["git clone", "git fetch", "git checkout", "git rev-parse"] + ["image pull", "image image"] * 5
                           + ["stack up", "driver", "tests", "stack down"])
    report = night.report
    assert report["result"] == "passed" and report["commit"] == HEAD and report["stale"] == []
    assert set(report["images"]) == set(nightly.IMAGES)
    for service, about in report["images"].items():
        assert about["image"] == f"ghcr.io/inferstep/{nightly.IMAGES[service]}:dev"
        assert about["digest"].startswith("sha256:") and about["commit"] == HEAD
    assert report["tasks"] == [{"task": task, "seconds": 10.5 + n, "passed": True, "defects": 0} for n, task in enumerate(nightly.TASKS)]
    assert report["tests"] == {"expected": 125, "collected": 125, "passed": 121, "failed": 0, "skipped": 4}
    assert report["stack_stopped"] == "yes"
    assert report["sent"] == "not sent: no token or no issue was given"


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
    driver = next(call for call in calls if call["tool"] != "git" and call["tool"] != "docker" and "e2e" in call["args"][0])
    said = dict(zip(driver["args"][1::2], driver["args"][2::2]))
    assert said["--url"] == "http://127.0.0.1:18090" and said["--tasks"] == ",".join(nightly.TASKS) and said["--reps"] == "1"
    assert said["--compose-project"] == "atlas-nightly" and said["--sandbox-container"] == "atlas-nightly-sandbox-1"
    assert said["--workspace"] == str(night.dir / "workspace" / "_reliability") and said["--commit"] == HEAD
    tests = next(call for call in calls if call["args"][:2] == ["-m", "pytest"])
    assert tests["args"][2:5] == ["-m", "integration", "tests/infrastructure"]
    assert tests["env"]["SANDBOX_URL"] == "http://127.0.0.1:18020" and tests["env"]["LLAMA_URL"] == "http://127.0.0.1:18080"
    assert tests["env"]["ATLAS_PROXY_URL"] == "http://127.0.0.1:18090"
    assert tests["env"]["ATLAS_SERVICE_TOKEN_FILE"] == str(night.dir / "secrets" / "service-token")
    token = night.dir / "secrets" / "service-token"
    assert len(token.read_text()) == 64 and (token.stat().st_mode & 0o777) == 0o600


@pytest.mark.parametrize("image", sorted(nightly.IMAGES.values()))
def test_an_image_that_was_not_built_from_the_head_makes_the_report_stale_and_nothing_is_run(tmp_path, image):
    night = Night(tmp_path, built_from={image: OLD}).run()
    assert night.done.returncode == 1
    assert night.report["result"] == f"stale: ghcr.io/inferstep/{image}:dev was built from {OLD[:12]}"
    assert "stack up" not in night.did() and "driver" not in night.did() and "tests" not in night.did()
    assert "tasks" not in night.report and "tests" not in night.report


def test_an_image_that_names_no_commit_is_stale(tmp_path):
    night = Night(tmp_path, built_from={"atlas-proxy": ""}).run()
    assert night.report["result"] == "stale: ghcr.io/inferstep/atlas-proxy:dev was built from no named commit"


def test_when_the_card_is_in_use_the_run_does_nothing_and_says_so(tmp_path):
    night = Night(tmp_path)
    with open(night.lock, "a") as held:
        fcntl.flock(held, fcntl.LOCK_EX)
        night.run()
    assert night.done.returncode == 0
    assert night.report["result"] == "not run: the card was in use"
    assert night.calls() == []
    assert "stack_stopped" not in night.report


def test_a_run_holds_the_lock_while_it_runs(tmp_path):
    night = Night(tmp_path, sleep={UP: 4})
    process = subprocess.Popen(night.command(), env=night.env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    try:
        deadline = time.monotonic() + 20
        while "stack up" not in night.did() and time.monotonic() < deadline:
            time.sleep(0.1)
        assert "stack up" in night.did()
        with open(night.lock, "a") as other:
            with pytest.raises(OSError):
                fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
    finally:
        process.communicate(timeout=60)
    with open(night.lock, "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)


@pytest.mark.parametrize("plan, says, last_step", [
    ({"fail": ["fetch --quiet"]}, "failed: the newest commit of the branch: status 1: a planted failure of: fetch --quiet", "git fetch"),
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
        assert did[-1] == "stack down" and did.count("stack down") == 1
    else:
        assert night.report["stack_stopped"].startswith("there was none")


@pytest.mark.parametrize("plan, says", [
    ({"driver_rows": rows(0, 2, 0)}, "failed: offbyone: 2 defect(s) of the harness"),
    ({"tests": {"tests": 13, "skipped": 0}}, "failed: 13 tests were collected, and there are 125"),
    ({"tests": {"tests": 126, "skipped": 0}}, "failed: 126 tests were collected, and there are 125"),
    ({"tests": {"tests": 125, "failures": 2, "errors": 1, "skipped": 4}}, "failed: 3 test(s) failed"),
    ({"driver_rows": rows(1, 0, 0), "tests": {"tests": 13, "failures": 1}},
     "failed: add_function: 1 defect(s) of the harness; 13 tests were collected, and there are 125; 1 test(s) failed"),
])
def test_a_run_that_measured_a_fault_fails_and_names_each_fault(tmp_path, plan, says):
    night = Night(tmp_path, **plan).run()
    assert night.done.returncode == 1
    assert night.report["result"] == says
    assert night.did()[-3:] == ["driver", "tests", "stack down"]


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
    process = subprocess.Popen(night.command(), env=night.env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    deadline = time.monotonic() + 20
    while "driver" not in night.did() and time.monotonic() < deadline:
        time.sleep(0.1)
    process.send_signal(signal.SIGTERM)
    process.communicate(timeout=30)
    assert process.returncode == 1
    assert night.report["result"] == "failed: the run was told to stop, in the step: the smoke run"
    assert night.did()[-1] == "stack down"


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
    # Outside the folder: the lock file, at the place the run was given, and the stand-ins' own list of calls.
    assert sorted(set(after) - set(before)) == ["calls.log", "card.lock"]


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


def test_with_a_token_the_report_replaces_the_text_of_the_one_issue_and_the_token_is_written_nowhere(tmp_path):
    Issue.seen = []
    server = http.server.HTTPServer(("127.0.0.1", 0), Issue)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    token = "a-token-made-for-this-test-0123456789"
    try:
        night = Night(tmp_path).run("--issue", "7", "--repository", "owner/name", "--api", f"http://127.0.0.1:{server.server_port}",
                                    ATLAS_NIGHTLY_TOKEN=token)
    finally:
        server.shutdown()
        thread.join(timeout=10)
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    ((path, sent_with, body),) = Issue.seen
    assert path == "/repos/owner/name/issues/7" and sent_with == f"Bearer {token}"
    assert body["body"].startswith("**Nightly run of `dev`: passed**") and HEAD in body["body"]
    assert night.report["sent"] == "sent to issue 7 (status 200)"
    assert token not in night.done.stdout + night.done.stderr
    for path in tmp_path.rglob("*"):
        if path.is_file():
            assert token not in path.read_text(errors="replace"), path


def test_a_token_with_no_issue_sends_nothing(tmp_path):
    night = Night(tmp_path).run(ATLAS_NIGHTLY_TOKEN="a-token")
    assert night.report["sent"] == "not sent: no token or no issue was given"


def test_a_report_that_cannot_be_sent_says_so_and_the_run_keeps_its_result(tmp_path):
    night = Night(tmp_path).run("--issue", "7", ATLAS_NIGHTLY_TOKEN="a-token")
    assert night.done.returncode == 0 and night.report["result"] == "passed"
    assert night.report["sent"].startswith("not sent: ") and "a-token" not in json.dumps(night.report)


# --- what the run takes from the repository ---------------------------------------------------------------------

def test_the_three_tasks_are_tasks_of_the_driver():
    driver = (ROOT / "scripts" / "e2e-reliability.py").read_text(encoding="utf-8")
    known = set(re.findall(r'^    "(\w+)": Task\(', driver, re.M)) | set(re.findall(r'^TASKS\["(\w+)"\] = Task\(', driver, re.M))
    assert len(nightly.TASKS) == 3 and set(nightly.TASKS) <= known, (nightly.TASKS, sorted(known))


def test_the_number_of_tests_is_the_sum_of_the_table_of_the_gates_page_and_the_files_are_the_marked_ones():
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    table = re.findall(r"^\| `(tests/infrastructure/test_\w+\.py)` \| (\d+) \|", page, re.M)
    assert len(table) == 7 and sum(int(count) for _file, count in table) == nightly.EXPECTED_TESTS
    marked = re.findall(r'"/(tests/infrastructure/test_\w+\.py)"', (ROOT / "tests" / "conftest.py").read_text(encoding="utf-8"))
    assert sorted(file for file, _count in table) == sorted(marked)


def test_every_port_and_every_image_of_the_compose_file_is_one_the_run_knows():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    published = set(re.findall(r'"127\.0\.0\.1:\$\{(ATLAS_\w+_PORT):-\d+\}:\d+"', compose))
    assert published and published <= set(nightly.PORTS), (
        f"docker-compose.yml publishes {sorted(published - set(nightly.PORTS))}, and the nightly run gives it no port of "
        "its own, so its stack would take the usual port. Fix: add it to PORTS in scripts/nightly_run.py.")
    assert len(set(nightly.PORTS.values())) == len(nightly.PORTS)
    images = set(re.findall(r"image: ghcr\.io/\$\{ATLAS_GHCR_OWNER:-inferstep\}/([\w-]+):", compose))
    assert images == set(nightly.IMAGES.values()), (images, nightly.IMAGES)
