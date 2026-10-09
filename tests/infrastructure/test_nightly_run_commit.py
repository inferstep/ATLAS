"""A run for one commit takes only what a maintainer has pushed under the name `smoke/` and what stands on the head
of `dev`, runs no file of it on the machine itself, builds under names of its own, and removes what it built.

git is the real one here, on repositories made for the test in a temporary folder: which commits a branch holds is
git's own work, and a stand-in would only repeat the rule. It may use the `file` protocol only, so no test reaches
the network. docker, the card and the Python of the run are the stand-ins of the nightly tests, with the parts that
this mode needs. Every run has a time limit.
"""
import ast
import fcntl
import json
import os
import re
import shutil
import signal
import subprocess
import time
from pathlib import Path

import pytest
import yaml

from tests.infrastructure import test_nightly_run as base
from tests.infrastructure.test_nightly_run import ROOT, SCRIPT, nightly

MARK = "0123456789ab"
# The line as the check of a pull request reads it (scripts/smoke_result.py).
READ_BY_THE_CHECK = re.compile(r"^Smoke run on ([0-9a-f]{7,40}): ([0-3]) of 3 sessions with no harness defect, (\d+) s; "
                               r"the changed text is part of every request \(text ([0-9a-f]{12})\)\.[ \t]*$", re.MULTILINE)
# What the stand-in of docker does for an image of the run's own: a build writes down what it was given, and an
# image is there from the end of its build until it is removed.
OWN_IMAGES = '''
own = [word for word in args if word.startswith("localhost/")]
if args[0] == "build":
    dockerfile, context = args[args.index("--file") + 1], args[-1]
    if not os.path.isfile(dockerfile):
        print("no Dockerfile at " + dockerfile)
        sys.exit(1)
    with open(home + "/built.log", "a") as log:
        log.write(json.dumps({"name": args[args.index("--tag") + 1], "context": context, "dockerfile": open(dockerfile).read(),
                              "holds": sorted(os.listdir(context))}) + "\\n")
    sys.exit(0)
if args[:2] == ["image", "inspect"] and own:
    built = [json.loads(line)["name"] for line in open(home + "/built.log")] if os.path.exists(home + "/built.log") else []
    removed = [word for c in calls[:-1] if c["args"][:2] == ["image", "rm"] for word in c["args"][2:]]
    sys.exit(0 if own[0] in built and own[0] not in removed else 1)
if args[:2] == ["image", "rm"]:
    sys.exit(0)
'''
THE_MARK = '''
if args[:2] == ["scripts/smoke_result.py", "--mark"]:
    with open(home + "/mark.log", "a") as log:
        log.write(json.dumps({"from": os.getcwd(), "holds": open("scripts/smoke_result.py").read()}) + "\\n")
    print(plan.get("marks", {}).get(args[2], plan.get("mark", "0123456789ab")))
    sys.exit(plan.get("mark_status", 0))
'''
GIT_ENV = {"GIT_AUTHOR_NAME": "a test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "a test",
           "GIT_COMMITTER_EMAIL": "test@example.invalid", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_CONFIG_SYSTEM": os.devnull,
           "GIT_ALLOW_PROTOCOL": "file"}
# The repository as `dev` has it, in the parts that the run reads.
ON_DEV = {
    "docker-compose.yml": "services: {}\n", "docker-compose.vulkan.yml": "services: {}\n", "docker-compose.cpu.yml": "services: {}\n",
    ".dockerignore": "proxy/\n",
    "scripts/e2e-reliability.py": "# the driver of dev\n", "scripts/code_quality.py": "# of dev\n",
    "scripts/reliability_report.py": "# of dev\n", "scripts/reliability_stream.py": "# of dev\n",
    "scripts/smoke_result.py": "# the mark script of dev\n",
    "proxy/Dockerfile": "FROM scratch\n# the proxy of dev\n", "proxy/prompt.go": "package main\n",
    "v3-service/Dockerfile": "FROM scratch\n# v3 of dev\n", "v3-service/main.py": "# of dev\n",
    "geometric-lens/Dockerfile": "FROM scratch\n# the lens of dev\n", "geometric-lens/main.py": "# of dev\n",
    "geometric-lens/geometric_lens/models/gx_weights.json": "{}\n",
    "sandbox/Dockerfile": "FROM scratch\n# the sandbox of dev\n", "sandbox/executor_server.py": "# of dev\n",
    "inference/Dockerfile.v31": "FROM scratch\n", "inference/entrypoint-v3.1.sh": "#!/bin/sh\n",
    "tests/replay/recordings/one.json": "{}\n", "docs/API.md": "the page\n",
}
SHORT = {"atlas-llama": "the model server", "atlas-lens": "the lens", "atlas-v3": "v3-service", "atlas-sandbox": "the sandbox",
         "atlas-proxy": "the proxy"}
NOT_BUILT = ["pull the model server", "pull the lens", "pull v3-service", "pull the sandbox", "pull the proxy"]
SESSIONS = ["card", "stack up", "ask", "driver", "ask", "stack down"]
# The mark of the commit, the mark of the head of `dev`, and the look for a stack that an earlier run left.
START = ["mark", "mark", "look"]


def git(where, *args, check=True):
    done = subprocess.run(["git", "-C", str(where), *args], capture_output=True, text=True, timeout=60, env={**os.environ, **GIT_ENV})
    assert not check or done.returncode == 0, done.stdout + done.stderr
    return done.stdout.strip()


class Repository:
    """The repository on GitHub, as a folder: a bare one that the run fetches from, and a work copy that pushes to it."""

    def __init__(self, root, script=None):
        self.bare, self.work = root / "origin.git", root / "work"
        subprocess.run(["git", "init", "--quiet", "--bare", "--initial-branch", "dev", str(self.bare)], check=True, timeout=60,
                       env={**os.environ, **GIT_ENV})
        subprocess.run(["git", "init", "--quiet", "--initial-branch", "dev", str(self.work)], check=True, timeout=60,
                       env={**os.environ, **GIT_ENV})
        git(self.work, "remote", "add", "origin", str(self.bare))
        # The real script loads the judge of the tests that lies beside it, so a tree that runs it has both.
        judge = {"scripts/tests_counted.py": (ROOT / "scripts" / "tests_counted.py").read_text(encoding="utf-8")} if script else {}
        self.head = self.commit({**ON_DEV, "scripts/nightly_run.py": script or "# the script of dev\n", **judge}, "refs/heads/dev")

    def commit(self, files, to, on="dev"):
        """A commit on top of a branch that changes these files (None takes a file away), pushed to a name. Gives its id."""
        if git(self.work, "rev-parse", "--verify", "--quiet", on, check=False):
            git(self.work, "checkout", "--quiet", "--detach", on)
        for name, text in files.items():
            path = self.work / name
            if text is None:
                path.unlink()
                continue
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)
        git(self.work, "add", "--all")
        git(self.work, "commit", "--quiet", "--allow-empty", "-m", f"a change for {to}")
        made = git(self.work, "rev-parse", "HEAD")
        if to == "refs/heads/dev":
            git(self.work, "update-ref", "refs/heads/dev", made)
        git(self.work, "push", "--quiet", "--force", "origin", f"{made}:{to}")
        return made

    def on_a_branch(self, files, name="smoke/a-change", on="dev"):
        """A commit with these changes, pushed as the tip of a branch: by default one for a smoke run."""
        return self.commit(files, f"refs/heads/{name}", on)


class OneCommit(base.Night):
    """A folder for a run for one commit: the tree is a real copy of the made repository, at the head of `dev`."""

    def __init__(self, root, repository, **plan):
        super().__init__(root, **{"head": repository.head, "mark": MARK, **plan})
        self.repository = repository
        for name, body in (("docker", OWN_IMAGES + base.DOCKER), ("python-for-the-run", THE_MARK + base.PYTHON)):
            (root / "stand-ins" / f"{name}.py").write_text(base.STAND_IN.format(name=name, home=str(root), body=body))
        (root / "bin" / "git").unlink()
        for tool in ("git", "tar"):
            (root / "bin" / tool).symlink_to(shutil.which(tool))
        subprocess.run(["git", "clone", "--quiet", "--branch", "dev", str(repository.bare), str(self.dir / "tree")], check=True,
                       timeout=60, env={**os.environ, **GIT_ENV})

    def env(self, **more):
        return {**super().env(**more), **GIT_ENV}

    def run(self, commit, *more, **env):
        self.commit = commit
        return super().run("--commit", commit, *more, **env)

    def start(self, commit):
        self.commit = commit
        return subprocess.Popen(self.command("--commit", commit), env=self.env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)

    @property
    def folder(self):
        (path,) = self.dir.glob("reports/*")
        return path

    def name_of(self, service):
        return f"localhost/atlas-nightly/{nightly.IMAGES[service]}:{self.folder.name}-{self.commit[:12]}"

    def built(self):
        log = self.root / "built.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def did(self):
        """What was called, in order, in a few words each. git is not a stand-in here, so its calls are not in the list."""
        short = []
        for call in self.calls():
            args = call["args"]
            image = SHORT.get(next((word.split("/")[-1].split(":")[0] for word in args if "/atlas-" in word), ""), "")
            if call["tool"] == "docker" and args[0] == "compose":
                step = "ask" if "exec" in args else "stack " + next(word for word in args if word in ("up", "down"))
            elif call["tool"] == "docker" and args[:2] == ["image", "inspect"]:
                continue
            elif call["tool"] == "docker":
                step = {"ps": "look", "pull": f"pull {image}", "build": f"build {image}", "image": f"remove {image}"}[args[0]]
            elif call["tool"] == "nvidia-smi":
                step = "card"
            else:
                step = {"scripts/e2e-reliability.py": "driver", "scripts/smoke_result.py": "mark"}.get(args[0], "tests")
            if not (step == "ask" and short and short[-1] == "ask"):
                short.append(step)
        return short


@pytest.fixture
def repository(tmp_path):
    return Repository(tmp_path)


def without(steps, *taken_out):
    return [step for step in steps if step not in taken_out]


# --- a commit that the run takes ----------------------------------------------------------------------------------

def test_a_commit_of_a_branch_runs_its_three_sessions_on_its_own_image_and_gives_the_one_line(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.done.returncode == 0, run.done.stdout + run.done.stderr
    # No tests: the run is the three sessions and nothing else.
    assert run.did() == START + without(NOT_BUILT, "pull the proxy") + ["build the proxy"] + SESSIONS + ["remove the proxy"]
    report, name = run.report, run.name_of("atlas-proxy")
    assert report["result"] == "passed"
    assert report["commit"] == commit
    assert report["head_of_dev"] == repository.head
    assert report["changed_services"] == ["atlas-proxy"]
    assert report["build"] == "built from the commit: atlas-proxy"
    assert report["built"] == {"atlas-proxy": name}
    assert report["built_removed"] == {name: "yes"}
    assert sorted(report["images"]) == sorted(set(nightly.IMAGES) - {"atlas-proxy"})
    assert "tests" not in report
    line = f"Smoke run on {commit}: 3 of 3 sessions with no harness defect, 34 s; the changed text is part of every request (text {MARK})."
    assert report["line"] == line
    assert line in run.done.stdout.splitlines()
    assert READ_BY_THE_CHECK.findall(run.done.stdout) == [(commit, "3", "34", MARK)]
    assert run.done.stdout.splitlines()[0] == f"run for the commit {commit}: passed"
    # The run deletes nothing on GitHub: it says how to remove the branch that was pushed for it.
    assert report["pushed_as"] == ["smoke/a-change"]
    assert run.done.stdout.splitlines()[-1] == "to remove the branch: git push origin --delete smoke/a-change"


def test_the_stack_runs_the_image_that_was_built_and_the_driver_is_told_the_commit(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    (up,) = [call["args"] for call in run.calls() if call["tool"] == "docker" and "up" in call["args"]]
    files = [up[n + 1] for n, word in enumerate(up) if word == "-f"]
    # The file of the run comes last, so its images hold over the ones of the compose file.
    assert files == [str(run.dir / "tree" / "docker-compose.yml"), str(run.folder / "own-images.yml")]
    assert yaml.safe_load((run.folder / "own-images.yml").read_text()) == {
        "services": {"atlas-proxy": {"image": run.name_of("atlas-proxy"), "pull_policy": "never"}}}
    (driver,) = [call["args"] for call in run.calls() if call["args"][0] == "scripts/e2e-reliability.py"]
    assert driver[driver.index("--commit") + 1] == commit
    # The stop of the stack names the same files: it is the same stack.
    (down,) = [call["args"] for call in run.calls() if call["tool"] == "docker" and "down" in call["args"]]
    assert [down[n + 1] for n, word in enumerate(down) if word == "-f"] == files


def test_the_image_is_built_from_the_commits_files_and_the_tree_of_the_run_stays_at_the_head_of_dev(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/Dockerfile": "FROM scratch\n# the proxy of the commit\n", "proxy/new.go": "package main\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    (built,) = run.built()
    assert built["dockerfile"] == "FROM scratch\n# the proxy of the commit\n"
    assert "new.go" in built["holds"]
    assert built["context"] == str(run.folder / "commit" / "proxy")
    assert built["name"] == run.name_of("atlas-proxy")
    tree = run.dir / "tree"
    assert git(tree, "rev-parse", "HEAD") == repository.head
    assert git(tree, "status", "--porcelain") == ""
    assert (tree / "proxy" / "Dockerfile").read_text() == ON_DEV["proxy/Dockerfile"]
    assert not (tree / "proxy" / "new.go").exists()


@pytest.mark.parametrize("service, changed, context, holds", [
    ("atlas-proxy", "proxy/prompt.go", "proxy", ["Dockerfile", "prompt.go"]),
    ("geometric-lens", "geometric-lens/main.py", "geometric-lens", ["Dockerfile", "geometric_lens", "main.py"]),
    ("sandbox", "sandbox/executor_server.py", "sandbox", ["Dockerfile", "executor_server.py"]),
    # The build of v3-service has the whole repository as its context. The copy holds only what that build reads.
    ("v3-service", "v3-service/main.py", ".", [".dockerignore", "v3-service"]),
    ("v3-service", ".dockerignore", ".", [".dockerignore", "v3-service"]),
])
def test_each_service_is_built_from_its_own_folder_and_only_the_others_are_pulled(tmp_path, repository, service, changed, context, holds):
    commit = repository.on_a_branch({changed: "a change\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.report["result"] == "passed", run.report["result"]
    assert run.report["changed_services"] == [service], run.report["result"]
    (built,) = run.built()
    assert built["name"] == run.name_of(service)
    assert built["holds"] == holds
    assert Path(built["context"]) == run.folder / "commit" / context
    (build,) = [call["args"] for call in run.calls() if call["args"][0] == "build"]
    assert build[build.index("--file") + 1] == str(run.folder / "commit" / nightly.BUILDS[service][1])
    assert build[build.index("--label") + 1] == f"org.opencontainers.image.revision={commit}"
    pulled = [call["args"][-1] for call in run.calls() if call["args"][0] == "pull"]
    assert pulled == [f"ghcr.io/inferstep/{image}:dev" for other, image in nightly.IMAGES.items() if other != service]


def test_a_commit_that_changes_two_services_gets_both_built_and_both_removed(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n", "sandbox/executor_server.py": "a change\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.report["changed_services"] == ["sandbox", "atlas-proxy"]
    assert run.did() == (START + without(NOT_BUILT, "pull the proxy", "pull the sandbox") + ["build the sandbox", "build the proxy"]
                         + SESSIONS + ["remove the sandbox", "remove the proxy"])
    assert set(yaml.safe_load((run.folder / "own-images.yml").read_text())["services"]) == {"sandbox", "atlas-proxy"}


def test_when_no_build_folder_differs_nothing_is_built_and_the_run_says_so(tmp_path, repository):
    commit = repository.on_a_branch({"tests/replay/recordings/one.json": '{"again": true}\n', "docs/API.md": "the page, changed\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.done.returncode == 0
    assert run.report["result"] == "passed"
    assert run.did() == START + NOT_BUILT + SESSIONS
    assert run.report["mark"] == run.report["mark_of_dev"]
    assert run.report["build"] == ("nothing was built: no build folder of a service differs between the commit and the head of "
                                   "`dev`, so each image is the one that was built from that head")
    assert run.report["built"] == {}
    assert run.report["built_removed"] == {}
    assert run.report["changed_services"] == []
    (up,) = [call["args"] for call in run.calls() if call["tool"] == "docker" and "up" in call["args"]]
    assert up.count("-f") == 1
    assert not (run.folder / "own-images.yml").exists()
    assert not (run.folder / "commit").exists()
    assert READ_BY_THE_CHECK.findall(run.done.stdout) == [(commit, "3", "34", MARK)]


def test_the_head_of_dev_itself_is_run_when_it_is_pushed_for_a_smoke_run(tmp_path, repository):
    git(repository.work, "push", "--quiet", "origin", f"{repository.head}:refs/heads/smoke/the-head-of-dev")
    run = OneCommit(tmp_path, repository).run(repository.head)
    assert run.report["result"] == "passed"
    assert run.report["changed_services"] == []


# --- a commit that the run does not take --------------------------------------------------------------------------

def not_run(run, starts_with):
    """The run did nothing with the commit: no pull, no build, no stack, no session. That is no failure."""
    assert run.done.returncode == 0, run.done.stdout + run.done.stderr
    assert run.report["result"].startswith("not run: " + starts_with), run.report["result"]
    assert [step for step in run.did() if step != "mark"] == []
    assert run.built() == []
    assert run.report["built"] == {}
    assert not run.report["line"]
    assert "Smoke run on" not in run.done.stdout
    assert run.done.stdout.splitlines()[0] == f"run for the commit {run.commit}: {run.report['result']}"


NOT_PUSHED = "the commit {commit} is not the tip of a branch `smoke/<name>` of the repository."


def not_pushed(run, commit):
    not_run(run, NOT_PUSHED.format(commit=commit))
    assert "so it takes only what a maintainer has pushed under the name `smoke/`" in run.report["result"]
    assert f"Fix: read the whole change; then push exactly this commit, `git push origin {commit}:refs/heads/smoke/<name>`" in run.report["result"]
    assert "pushed_as" not in run.report
    assert "to remove the branch" not in run.done.stdout


def test_a_commit_of_a_pull_request_from_outside_is_not_run_though_a_fetch_by_its_id_works(tmp_path, repository):
    # The head of a pull request from outside is under refs/pull/ of the repository, and on no branch of it.
    outside = repository.commit({"proxy/Dockerfile": "FROM scratch\nRUN anything\n"}, "refs/pull/7/head")
    git(repository.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
    run = OneCommit(tmp_path, repository)
    git(run.dir / "tree", "fetch", "--quiet", "origin", outside)
    assert git(run.dir / "tree", "cat-file", "-t", outside) == "commit", "the fetch by id has to work for this test to show anything"
    not_pushed(run.run(outside), outside)


def test_after_a_maintainer_pushed_that_head_under_the_name_smoke_it_is_run(tmp_path, repository):
    outside = repository.commit({"proxy/prompt.go": "a change from outside\n"}, "refs/pull/7/head")
    git(repository.work, "push", "--quiet", "origin", f"{outside}:refs/heads/smoke/pull-7")
    run = OneCommit(tmp_path, repository).run(outside)
    assert run.report["result"] == "passed", run.report["result"]
    assert run.report["pushed_as"] == ["smoke/pull-7"]


def test_a_commit_that_the_repository_does_not_have_is_not_run(tmp_path, repository):
    not_pushed(OneCommit(tmp_path, repository).run("4" * 40), "4" * 40)


@pytest.mark.parametrize("branch", [
    "fix/a-change", "dev-next", "star-history",
    # Branches that a program pushes. Such a one can stand on the head of `dev` and change a build folder.
    "dependabot/go_modules/proxy/golang.org/x/net-0.50.0", "gh-readonly-queue/dev/pr-431-63f33f70",
    # Names that only look like the one.
    "smokey/a-change", "fix/smoke/a-change", "smoke", "smoke-a-change",
])
def test_the_tip_of_a_branch_of_another_name_is_not_run(tmp_path, repository, branch):
    commit = repository.on_a_branch({"proxy/go.mod": "module a-change\n"}, name=branch)
    not_pushed(OneCommit(tmp_path, repository).run(commit), commit)


def test_a_commit_of_a_branch_of_another_name_is_run_when_it_is_also_the_tip_of_a_smoke_branch(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/go.mod": "module a-dependency\n"}, name="dependabot/go_modules/proxy/x")
    git(repository.work, "push", "--quiet", "origin", f"{commit}:refs/heads/smoke/read-and-pushed")
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.report["result"] == "passed", run.report["result"]
    assert run.report["pushed_as"] == ["smoke/read-and-pushed"]


def test_one_push_stands_for_one_commit_a_commit_further_down_the_branch_is_not_run(tmp_path, repository):
    first = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    tip = repository.commit({"proxy/prompt.go": "a change, and one more\n"}, "refs/heads/smoke/a-change", on=first)
    assert git(repository.bare, "rev-list", "--count", f"{first}..{tip}") == "1"
    assert git(repository.bare, "rev-parse", "refs/heads/smoke/a-change") == tip
    not_pushed(OneCommit(tmp_path, repository).run(first), first)
    (tmp_path / "for the tip").mkdir()
    assert OneCommit(tmp_path / "for the tip", repository).run(tip).report["result"] == "passed"


def test_a_commit_that_two_smoke_branches_have_as_their_tip_is_run_and_both_are_named(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    git(repository.work, "push", "--quiet", "origin", f"{commit}:refs/heads/smoke/a-second-name")
    run = OneCommit(tmp_path, repository).run(commit)
    assert run.report["pushed_as"] == ["smoke/a-change", "smoke/a-second-name"]
    assert run.done.stdout.splitlines()[-2:] == ["to remove the branch: git push origin --delete smoke/a-change",
                                                 "to remove the branch: git push origin --delete smoke/a-second-name"]


def test_a_smoke_branch_that_is_gone_no_longer_counts(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"}, name="smoke/gone-soon")
    run = OneCommit(tmp_path, repository)
    # An earlier run fetched the branch under the run's own names. Then the branch was deleted.
    git(run.dir / "tree", "fetch", "--quiet", "origin", "+refs/heads/smoke/*:refs/nightly/smoke/*")
    assert commit in git(run.dir / "tree", "for-each-ref", "--format=%(objectname)", "refs/nightly/smoke/")
    git(repository.bare, "update-ref", "-d", "refs/heads/smoke/gone-soon")
    not_pushed(run.run(commit), commit)


def test_a_name_of_the_tree_that_is_no_smoke_branch_of_the_repository_does_not_count(tmp_path, repository):
    outside = repository.commit({"proxy/prompt.go": "a change\n"}, "refs/pull/7/head")
    run = OneCommit(tmp_path, repository)
    tree = run.dir / "tree"
    git(tree, "fetch", "--quiet", "origin", "refs/pull/7/head:refs/heads/smoke/a-local-branch")
    git(tree, "update-ref", "refs/remotes/origin/smoke/looks-like-one", outside)
    git(tree, "update-ref", "refs/nightly/smoke/written-by-hand", outside)
    git(tree, "tag", "smoke/a-tag", outside)
    not_pushed(run.run(outside), outside)


def starts_on_a_push_to(workflow, branch):
    """Whether a workflow starts when this branch is pushed, made or deleted. Gives the reason, or nothing."""
    on = workflow.get(True, workflow.get("on")) or {}
    on = {on: {}} if isinstance(on, str) else dict.fromkeys(on, {}) if isinstance(on, list) else on
    for event in ("create", "delete"):
        if event in on:
            return f"it starts when a branch is {event}d"
    if "push" not in on:
        return ""
    push = on["push"] or {}
    if "branches" not in push and "branches-ignore" not in push:
        return "" if "tags" in push or "tags-ignore" in push else "it starts on a push to every branch"

    def fits(pattern):
        return re.fullmatch("".join(".*" if part == "**" else "[^/]*" if part == "*" else re.escape(part)
                                    for part in re.split(r"(\*\*|\*)", pattern)), branch) is not None
    if "branches" in push:
        return "its list of branches takes this one" if any(fits(pattern) for pattern in push["branches"]) else ""
    return "" if any(fits(pattern) for pattern in push["branches-ignore"]) else "its list of branches to leave out does not hold this one"


def test_no_workflow_starts_when_a_smoke_branch_is_pushed():
    workflows = sorted((ROOT / ".github" / "workflows").glob("*.y*ml"))
    assert len(workflows) >= 20
    for path in workflows:
        why = starts_on_a_push_to(yaml.safe_load(path.read_text(encoding="utf-8")), "smoke/a-name")
        assert not why, (f"{path.name}: {why}. A push under `smoke/` is the sign for a smoke run and has to start nothing: "
                         "the commit on it can be the head of a pull request from outside")


@pytest.mark.parametrize("on, starts", [
    ({"push": {"branches": ["main", "dev"]}}, False), ({"push": {"tags": ["v*"]}}, False), ({"pull_request": {}}, False),
    ({"push": {"branches": ["main"], "tags": ["v*"]}}, False), ({"push": {"branches-ignore": ["smoke/**"]}}, False),
    ({"push": None}, True), ({"push": {"paths": ["proxy/**"]}}, True), ({"push": {"branches": ["**"]}}, True),
    ({"push": {"branches": ["smoke/*"]}}, True), ({"push": {"branches": ["s*/**"]}}, True), ({"push": {"branches-ignore": ["main"]}}, True),
    ({"create": None}, True), ({"delete": None}, True), ("push", True), (["push", "pull_request"], True), (["pull_request"], False),
    ({"push": {"branches": ["*"]}}, False),
])
def test_the_reading_of_a_workflows_start_finds_each_way_that_a_push_starts_it(on, starts):
    assert bool(starts_on_a_push_to({True: on}, "smoke/a-name")) is starts


def test_a_commit_that_does_not_stand_on_the_head_of_dev_is_not_run_until_its_branch_is_updated(tmp_path, repository):
    behind = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    old_head, repository.head = repository.head, repository.commit({"docs/API.md": "dev went on\n"}, "refs/heads/dev")
    run = OneCommit(tmp_path, repository)
    not_run(run.run(behind), f"the commit {behind[:12]} does not stand on the head of `dev` ({repository.head[:12]}).")
    assert "Fix: update the branch onto `dev`, push, and start the run for the new commit" in run.report["result"]
    assert old_head != repository.head


def test_after_the_branch_is_updated_onto_dev_its_new_commit_is_run(tmp_path, repository):
    behind = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    repository.head = repository.commit({"docs/API.md": "dev went on\n"}, "refs/heads/dev")
    git(repository.work, "checkout", "--quiet", "--detach", behind)
    git(repository.work, "merge", "--quiet", "--no-edit", repository.head)
    updated = git(repository.work, "rev-parse", "HEAD")
    git(repository.work, "push", "--quiet", "origin", f"{updated}:refs/heads/smoke/a-change")
    run = OneCommit(tmp_path, repository).run(updated)
    assert run.report["result"] == "passed", run.report["result"]
    assert run.report["changed_services"] == ["atlas-proxy"], run.report["result"]


TAKEN_FROM_DEV = [
    ("scripts/nightly_run.py", "the script of this run"),
    ("scripts/server/nightly_look.sh", "the files of the timer, which run on the server itself"),
    ("scripts/server/atlas-nightly.service", "the files of the timer, which run on the server itself"),
    ("scripts/tests_counted.py", "the judge of the tests, which the script loads on the server itself"),
    ("scripts/e2e-reliability.py", "the driver of the sessions, which runs on the server itself"),
    ("scripts/code_quality.py", "a module of the driver, which runs on the server itself"),
    ("scripts/reliability_report.py", "a module of the driver, which runs on the server itself"),
    ("scripts/reliability_stream.py", "a module of the driver, which runs on the server itself"),
    ("scripts/smoke_result.py", "the script that makes the mark of the result line"),
    ("docker-compose.yml", "a compose file of the stack"),
    ("inference/Dockerfile.v31", "the model server, whose image is never built on the server and whose start script the stack mounts from the tree"),
    ("inference/entrypoint-v3.1.sh", "the model server, whose image is never built on the server and whose start script the stack mounts from the tree"),
    ("inference/a-new-file.sh", "the model server, whose image is never built on the server and whose start script the stack mounts from the tree"),
    ("geometric-lens/geometric_lens/models/gx_weights.json", "the model files of the lens, which the stack mounts"),
]


# Each of them with other text, and each that `dev` has also taken away.
CHANGES = [(name, what, "of the commit\n") for name, what in TAKEN_FROM_DEV] + [
    (name, what, None) for name, what in TAKEN_FROM_DEV if name in ON_DEV or name == "scripts/nightly_run.py"]


@pytest.mark.parametrize("name, what, text", CHANGES)
def test_a_commit_that_changes_what_the_run_takes_from_dev_is_not_run_and_the_file_is_named(tmp_path, repository, name, what, text):
    # The commit also changes a service: without the refusal the run would build and start it.
    commit = repository.on_a_branch({name: text, "proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    not_run(run, f"the commit changes `{name}`: {what}. The run takes that from `dev`, so it would run the copy of `dev` under "
                 "the name of the commit.")
    assert "Fix: bring that change to `dev` in a pull request of its own" in run.report["result"]


def test_a_file_that_is_moved_counts_under_its_old_name(tmp_path, repository):
    commit = repository.on_a_branch({"scripts/reliability_stream.py": None, "scripts/stream.py": ON_DEV["scripts/reliability_stream.py"]})
    not_run(OneCommit(tmp_path, repository).run(commit), "the commit changes `scripts/reliability_stream.py`")


def test_a_compose_file_that_the_run_was_given_is_taken_from_dev_too_and_one_that_it_does_not_use_is_not(tmp_path, repository):
    commit = repository.on_a_branch({"docker-compose.vulkan.yml": "services: {x: {}}\n", "docker-compose.cpu.yml": "services: {y: {}}\n"})
    run = OneCommit(tmp_path, repository).run(commit, "--compose-file", "docker-compose.vulkan.yml")
    not_run(run, "the commit changes `docker-compose.vulkan.yml`: a compose file of the stack.")
    (tmp_path / "a second server").mkdir()
    assert OneCommit(tmp_path / "a second server", repository).run(commit).report["result"] == "passed"


def test_with_more_than_one_such_file_the_result_names_the_first_and_counts_the_others(tmp_path, repository):
    commit = repository.on_a_branch({"scripts/code_quality.py": "x\n", "docker-compose.yml": "services: {z: {}}\n", "inference/x": "x\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    not_run(run, "the commit changes `docker-compose.yml`: a compose file of the stack.")
    assert "It changes 2 more such file(s)." in run.report["result"]


@pytest.mark.parametrize("name, path, lies_in_it", [
    ("proxy/prompt.go", "proxy/", True), ("proxy/a/b.go", "proxy/", True), ("proxy2/prompt.go", "proxy/", False),
    ("docs/proxy/prompt.go", "proxy/", False), ("docker-compose.yml", "docker-compose.yml", True),
    ("docker-compose.yml.orig", "docker-compose.yml", False), ("scripts/nightly_run.py/x", "scripts/nightly_run.py", False),
])
def test_a_folder_holds_the_files_under_it_and_a_file_is_only_itself(name, path, lies_in_it):
    assert nightly.under(name, path) is lies_in_it


@pytest.mark.parametrize("given", ["abc1234", "dev", "a-change", "HEAD", "1" * 39, "1" * 41, "G" * 40, "--upload-pack=touch /tmp/x", ""])
def test_the_commit_is_given_by_its_full_id_or_the_run_does_nothing(tmp_path, repository, given):
    run = OneCommit(tmp_path, repository)
    run.done = subprocess.run(run.command(f"--commit={given}"), env=run.env(), capture_output=True, text=True, timeout=60)
    assert run.done.returncode == 2
    assert "give the full id of the commit: 40 characters, each 0-9 or a-f" in run.done.stderr
    assert run.calls() == []
    assert not list(run.dir.glob("reports/*"))


# --- the images of the run -----------------------------------------------------------------------------------------

@pytest.mark.parametrize("service", sorted(nightly.BUILDS))
def test_an_image_of_the_run_has_a_name_of_this_machine_with_the_time_and_the_commit_and_never_the_tag_dev(service):
    name = nightly.own_image(service, "20261008T040000Z", "ab" * 20)
    assert name == f"localhost/atlas-nightly/{nightly.IMAGES[service]}:20261008T040000Z-abababababab"
    assert not name.endswith(f":{nightly.TAG}")
    assert nightly.REGISTRY not in name


def test_no_call_of_the_run_names_an_image_of_its_own_under_the_registry_or_removes_another_image(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n", "v3-service/main.py": "a change\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    own = {run.name_of("atlas-proxy"), run.name_of("v3-service")}
    docker = [call["args"] for call in run.calls() if call["tool"] == "docker"]
    assert {args[args.index("--tag") + 1] for args in docker if args[0] == "build"} == own
    removing = [args for args in docker if {"rm", "rmi", "prune"} & set(args)]
    assert sorted(removing) == sorted(["image", "rm", name] for name in own)
    # The only other thing that the run removes is its own stack.
    assert [args for args in docker if "down" in args and "-p" in args and args[args.index("-p") + 1] != "atlas-nightly"] == []
    for args in docker:
        if args[0] in ("build", "push", "tag"):
            assert not [word for word in args if word.startswith("ghcr.io/") or word.endswith(":dev")], args
    assert not [args for args in docker if args[0] in ("push", "tag", "login")]


def test_the_images_are_removed_after_the_stack_is_stopped(tmp_path, repository):
    run = OneCommit(tmp_path, repository).run(repository.on_a_branch({"proxy/prompt.go": "a change\n"}))
    did = run.did()
    assert did.index("stack down") < did.index("remove the proxy") == len(did) - 1


@pytest.mark.parametrize("plan, more, says, stack", [
    ({"fail": ["build --tag localhost/atlas-nightly/atlas-proxy"]}, (), "failed: the build of atlas-proxy from the commit: status 1", False),
    ({"card": ["4242, 13200"]}, (), "not run: the card was in use (1 process(es) hold 13200 MiB)", False),
    ({"fail": ["up -d --wait"]}, (), "failed: the start of the stack: status 1", True),
    ({"driver_rows": None}, (), "failed: the driver left no result", True),
    ({"sleep": {base.DRIVER: 30}}, ("--limit-minutes", "0.05"), "failed: the time limit of 0.05 minutes was reached, in the step: the smoke run", True),
])
def test_whatever_ends_the_run_the_images_that_it_built_are_removed(tmp_path, repository, plan, more, says, stack):
    commit = repository.on_a_branch({"sandbox/executor_server.py": "a change\n", "proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository, **plan).run(commit, *more)
    assert run.report["result"].startswith(says), run.report["result"]
    sandbox, proxy = run.name_of("sandbox"), run.name_of("atlas-proxy")
    # The sandbox is built first. The proxy's image is there when its build ended.
    proxy_built = "build" not in str(plan.get("fail"))
    assert run.report["built_removed"] == {sandbox: "yes", proxy: "yes" if proxy_built else "there was none: its build did not end"}
    assert [step for step in run.did() if step.startswith("remove")] == ["remove the sandbox"] + ["remove the proxy"] * proxy_built
    assert ("stack down" in run.did()) is stack
    assert not run.report["line"]


def test_when_another_run_holds_the_card_the_built_images_are_removed_and_no_stack_is_started(tmp_path, repository):
    run = OneCommit(tmp_path, repository)
    with open(run.lock, "a") as held:
        fcntl.flock(held, fcntl.LOCK_EX)
        run.run(repository.on_a_branch({"proxy/prompt.go": "a change\n"}))
    assert run.done.returncode == 0
    assert run.report["result"] == "not run: the card was in use"
    assert run.report["built_removed"] == {run.name_of("atlas-proxy"): "yes"}
    assert "stack up" not in run.did()


def test_a_run_that_is_told_to_stop_stops_its_stack_and_removes_its_images(tmp_path, repository):
    run = OneCommit(tmp_path, repository, sleep={base.DRIVER: 30})
    process = run.start(repository.on_a_branch({"proxy/prompt.go": "a change\n"}))
    run.wait_for("driver")
    process.send_signal(signal.SIGTERM)
    process.communicate(timeout=30)
    assert process.returncode == 1
    assert run.report["result"] == "failed: the run was told to stop, in the step: the smoke run"
    assert run.did()[-2:] == ["stack down", "remove the proxy"]
    assert run.report["built_removed"] == {run.name_of("atlas-proxy"): "yes"}


def test_each_build_has_a_time_limit_of_its_own(tmp_path, repository):
    run = OneCommit(tmp_path, repository, sleep={"build --tag": 40})
    start = time.monotonic()
    run.run(repository.on_a_branch({"proxy/prompt.go": "a change\n"}), "--build-seconds", "2")
    assert time.monotonic() - start < 30, "the build went on after its limit"
    assert run.done.returncode == 1
    assert run.report["result"] == "failed: the build of atlas-proxy from the commit: no end after 2 s"
    assert "stack up" not in run.did()
    assert run.report["built_removed"] == {run.name_of("atlas-proxy"): "there was none: its build did not end"}


def test_an_image_that_cannot_be_removed_is_named_in_the_report(tmp_path, repository):
    run = OneCommit(tmp_path, repository, fail=["image rm"]).run(repository.on_a_branch({"proxy/prompt.go": "a change\n"}))
    assert run.report["built_removed"] == {run.name_of("atlas-proxy"): "no: status 1: a planted failure of: image rm"}


def test_with_an_image_of_dev_that_was_not_built_from_the_head_the_commit_is_not_run_and_nothing_is_built(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository, built_from={"atlas-v3": base.OLD}).run(commit)
    assert run.done.returncode == 0
    assert run.report["result"] == ("not run: the images are not of the head of the branch yet "
                                    f"(ghcr.io/inferstep/atlas-v3:dev was built from {base.OLD[:12]})")
    assert run.built() == []
    assert "card" not in run.did()
    assert not run.report["line"]


def test_the_image_of_dev_for_a_service_that_the_commit_builds_is_not_asked_for(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository, built_from={"atlas-proxy": base.OLD}).run(commit)
    assert run.report["result"] == "passed"
    assert "pull the proxy" not in run.did()


# --- the mark, the line, the report ---------------------------------------------------------------------------------

def test_the_marks_are_made_by_the_copy_of_the_script_that_dev_has_before_anything_is_pulled_or_built(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository).run(commit)
    calls = [call["args"] for call in run.calls() if call["args"][0] == "scripts/smoke_result.py"]
    assert calls == [["scripts/smoke_result.py", "--mark", commit], ["scripts/smoke_result.py", "--mark", repository.head]]
    assert run.did()[:2] == ["mark", "mark"]
    for made in [json.loads(line) for line in (tmp_path / "mark.log").read_text().splitlines()]:
        assert Path(made["from"]).resolve() == (run.dir / "tree").resolve()
        assert made["holds"] == ON_DEV["scripts/smoke_result.py"]
    assert run.report["mark"] == MARK
    assert run.report["mark_of_dev"] == MARK


OF_DEV = "aaaaaaaaaaaa"


@pytest.mark.parametrize("changed", [
    {"tests/replay/recordings/one.json": '{"again": true}\n'},
    # Another service is built, and it does not send the text.
    {"tests/replay/recordings/one.json": '{"again": true}\n', "sandbox/executor_server.py": "a change\n"},
    {"tests/replay/recordings/one.json": '{"again": true}\n', "v3-service/main.py": "a change\n"},
])
def test_with_another_mark_than_dev_and_a_proxy_that_is_devs_the_commit_is_not_run(tmp_path, repository, changed):
    commit = repository.on_a_branch(changed)
    run = OneCommit(tmp_path, repository, marks={repository.head: OF_DEV}).run(commit)
    assert run.done.returncode == 0, run.done.stdout + run.done.stderr
    assert run.report["result"].startswith(
        f"not run: the recordings of the commit show another text than `dev` sends (text {MARK}; `dev` has {OF_DEV}), and the "
        "commit does not change the proxy, which sends that text. So no session of this run would carry the text of the commit.")
    assert "Fix: put the change of the text into the same commit as its recordings" in run.report["result"]
    assert run.did() == ["mark", "mark"]
    assert run.built() == []
    assert not run.report["line"]
    assert "Smoke run on" not in run.done.stdout


def test_with_another_mark_than_dev_and_a_proxy_built_from_the_commit_the_line_names_the_commits_text(tmp_path, repository):
    commit = repository.on_a_branch({"tests/replay/recordings/one.json": '{"again": true}\n', "proxy/prompt.go": "a new text\n"})
    run = OneCommit(tmp_path, repository, marks={repository.head: OF_DEV}).run(commit)
    assert run.report["result"] == "passed", run.report["result"]
    assert run.report["mark_of_dev"] == OF_DEV
    assert READ_BY_THE_CHECK.findall(run.done.stdout) == [(commit, "3", "34", MARK)]


@pytest.mark.parametrize("plan, says", [
    ({"mark_status": 2}, "failed: the mark of the commit's text: status 2"),
    ({"mark": "the head has no recordings"}, "failed: the mark of the commit's text: scripts/smoke_result.py printed no mark: the head has no recordings"),
])
def test_without_a_mark_the_run_fails_before_it_pulls_builds_or_starts_anything(tmp_path, repository, plan, says):
    run = OneCommit(tmp_path, repository, **plan).run(repository.on_a_branch({"proxy/prompt.go": "a change\n"}))
    assert run.done.returncode == 1, run.report["result"]
    assert run.report["result"].startswith(says), run.report["result"]
    assert run.did() == ["mark"]
    assert run.built() == []


def measured(result="passed", defects=(0, 0, 0), seconds=(10.2, 11.2, 12.2)):
    return {"result": result, "commit": "c" * 40, "mark": MARK,
            "tasks": [{"task": task, "seconds": took, "passed": True, "defects": count}
                      for task, took, count in zip(nightly.TASKS, seconds, defects)]}


@pytest.mark.parametrize("report, line", [
    (measured(), f"Smoke run on {'c' * 40}: 3 of 3 sessions with no harness defect, 34 s; the changed text is part of every request (text {MARK})."),
    # A red smoke run has its line too: the text of the pull request then shows that it was red.
    (measured("failed: offbyone: 2 defect(s) of the harness", (0, 2, 0)),
     f"Smoke run on {'c' * 40}: 2 of 3 sessions with no harness defect, 34 s; the changed text is part of every request (text {MARK})."),
    (measured("failed: three", (1, 1, 1), (100, 200.4, 300)),
     f"Smoke run on {'c' * 40}: 0 of 3 sessions with no harness defect, 600 s; the changed text is part of every request (text {MARK})."),
    # Three sessions with no defect in a run that failed for another reason: a line would say 3 of 3 for a failed run.
    (measured("failed: at the end of the run the proxy says that the lens is not ready"), ""),
    (measured("failed: the stack was not stopped (status 1); before that: passed"), ""),
    ({**measured("failed: the driver left no result"), "tasks": []}, ""),
    ({"result": "not run: the card was in use", "commit": "c" * 40, "mark": MARK}, ""),
])
def test_the_line_is_given_for_a_run_whose_sessions_ran_and_is_the_line_that_the_check_reads(report, line):
    assert nightly.result_line(report) == line
    assert bool(READ_BY_THE_CHECK.fullmatch(line)) is bool(line)


def test_a_run_with_a_harness_defect_fails_and_its_line_says_how_many_sessions_had_none(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository, driver_rows=base.rows(0, 2, 0)).run(commit)
    assert run.done.returncode == 1
    assert run.report["result"] == "failed: offbyone: 2 defect(s) of the harness"
    assert READ_BY_THE_CHECK.findall(run.done.stdout) == [(commit, "2", "34", MARK)]


def test_the_run_for_a_commit_writes_only_inside_its_own_folder(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run = OneCommit(tmp_path, repository)

    def outside():
        return sorted(str(path.relative_to(tmp_path)) for path in tmp_path.rglob("*")
                      if run.dir not in path.parents and path != run.dir and repository.work not in path.parents)

    before = outside()
    run.run(commit)
    assert sorted(set(outside()) - set(before)) == ["built.log", "calls.log", "card.lock", "mark.log"]


def test_when_taking_the_head_changes_the_script_the_copy_of_dev_does_the_run_for_the_commit(tmp_path):
    repository = Repository(tmp_path, script=SCRIPT.read_text())
    run = OneCommit(tmp_path, repository)
    run.script = run.dir / "tree" / "scripts" / "nightly_run.py"
    # `dev` goes on and changes the script; the commit stands on that new head and does not change the script.
    repository.head = repository.commit({"scripts/nightly_run.py": SCRIPT.read_text() + "\n# the copy of dev, one commit later\n"}, "refs/heads/dev")
    (tmp_path / "plan.json").write_text(json.dumps({**run.plan, "head": repository.head}))
    commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
    run.run(commit)
    assert run.done.returncode == 0, run.done.stdout + run.done.stderr
    assert run.report["script"] == "the new copy: taking the head changed the script"
    assert run.report["result"] == "passed"
    assert run.script.read_text().endswith("# the copy of dev, one commit later\n")
    assert run.report["head_of_dev"] == repository.head
    assert run.did().count("driver") == 1
    assert run.did().count("build the proxy") == 1


# --- the lists of the script, held against the repository ---------------------------------------------------------

def compose_file():
    return yaml.safe_load((ROOT / "docker-compose.yml").read_text())["services"]


def test_each_service_is_built_as_the_compose_file_builds_it_and_only_the_model_server_is_never_built():
    assert set(nightly.IMAGES) - set(nightly.BUILDS) == {"llama-server"}
    for service, (context, dockerfile, paths) in nightly.BUILDS.items():
        build = compose_file()[service]["build"]
        folder = build["context"].removeprefix("./")
        assert context == folder, service
        assert dockerfile == ("" if folder == "." else folder + "/") + build.get("dockerfile", "Dockerfile"), service
        assert (ROOT / dockerfile).is_file(), dockerfile
        if folder != ".":
            assert paths == (folder + "/",), service


def test_the_build_whose_context_is_the_whole_repository_reads_only_the_paths_that_the_script_names():
    (whole,) = [service for service, (context, _dockerfile, _paths) in nightly.BUILDS.items() if context == "."]
    _context, dockerfile, paths = nightly.BUILDS[whole]
    sources = [word for line in (ROOT / dockerfile).read_text().splitlines() if line.startswith(("COPY ", "ADD ")) and "--from=" not in line
               for word in line.split()[1:-1] if not word.startswith("--")]
    assert len(sources) >= 10
    for source in sources:
        assert any(nightly.under(source, path) for path in paths), f"{dockerfile} reads {source}, and the copy of the commit would not hold it"
    assert ".dockerignore" in paths
    assert (ROOT / ".dockerignore").is_file()


def modules_of_scripts(name, seen):
    """The files of scripts/ that a script loads, and what those load."""
    for node in ast.walk(ast.parse((ROOT / "scripts" / name).read_text())):
        loaded = ([alias.name for alias in node.names] if isinstance(node, ast.Import)
                  else [node.module] if isinstance(node, ast.ImportFrom) and node.module and not node.level else [])
        for module in loaded:
            file = module.split(".")[0] + ".py"
            if (ROOT / "scripts" / file).is_file() and file not in seen:
                seen.add(file)
                modules_of_scripts(file, seen)
    return seen


def test_every_file_that_the_run_starts_or_loads_on_the_server_itself_is_one_that_it_takes_from_dev():
    source = SCRIPT.read_text()
    started = set(re.findall(r'\[args\.python, "(scripts/[^"]+)"', source))
    assert started == {"scripts/e2e-reliability.py", "scripts/smoke_result.py"}, started
    loaded = {f"scripts/{name}" for name in modules_of_scripts("e2e-reliability.py", set())}
    assert len(loaded) >= 3
    held = {path.relative_to(ROOT.resolve()).as_posix() for path in nightly.AS_STARTED}
    assert held == {"scripts/nightly_run.py", "scripts/tests_counted.py"}, held
    for name in started | loaded | held:
        assert name in nightly.FROM_DEV, f"the run uses dev's copy of {name}; a commit that changes it has to be refused"


def test_every_file_of_the_repository_that_the_stack_reads_through_a_mount_is_one_that_the_run_takes_from_dev():
    # A mount that the stack may only read is an input of the stack. One that it may write is a place for its data.
    mounted = []
    for service in compose_file().values():
        for volume in service.get("volumes", []):
            assert isinstance(volume, str), f"a mount in another form than the short one is not read by this test: {volume}"
            found = re.search(r"\./[^}:]+", volume.split(":/")[0])
            if found and volume.endswith(":ro") and (ROOT / found.group()).exists():
                mounted.append(found.group()[2:] + ("/" if (ROOT / found.group()).is_dir() else ""))
    assert sorted(mounted) == ["geometric-lens/geometric_lens/models/", "inference/entrypoint-v3.1.sh"], mounted
    for path in mounted:
        assert any(path.startswith(folder) for folder in nightly.FROM_DEV if folder.endswith("/")), \
            f"the stack mounts {path} from the tree, which holds dev; a commit that changes it has to be refused"


def test_the_names_of_this_runs_own_are_in_no_registry_and_are_no_names_that_git_fetches_by_itself():
    assert nightly.OWN_IMAGES.startswith("localhost/")
    assert nightly.BRANCHES == "refs/nightly/smoke/"
    assert nightly.SMOKE == "smoke/"
    assert not nightly.BRANCHES.startswith(("refs/heads/", "refs/remotes/", "refs/tags/"))
