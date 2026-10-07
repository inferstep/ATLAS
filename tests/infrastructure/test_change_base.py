"""The one rule for the commit a check compares a change with.

Each case is a small repository made for the test: a base branch that moves
on after a branch began, and the merge of the branch into it, as a job of a
pull request checks it out.
"""
import importlib.util
import json
import re
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
USERS = ("integrity.yml", "checks-ran.yml", "golangci-lint.yml", "hadolint.yml")


def load(name):
    spec = importlib.util.spec_from_file_location(f"atlas_{name}", ROOT / "scripts" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


@pytest.fixture(scope="module")
def rule():
    return load("change_base")


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args],
                          capture_output=True, text=True, check=True).stdout.strip()


def commit(root, files, message):
    for path, text in files.items():
        (root / path).parent.mkdir(parents=True, exist_ok=True)
        (root / path).write_text(text, encoding="utf-8")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", message)
    return git(root, "rev-parse", "HEAD")


@pytest.fixture
def moved(tmp_path):
    """A base branch that got a change after a branch began, checked out as the merge of the two.

    Returns the root, the base when the branch began, the base as it is now, and the head of the branch.
    """
    root = tmp_path / "repo"
    root.mkdir()
    git(root, "init", "-q", "-b", "dev")
    began = commit(root, {"proxy/agent.go": "package main\n", "docs/API.md": "api\n"}, "base")
    git(root, "checkout", "-q", "-b", "work")
    head = commit(root, {"docs/API.md": "api, with the branch's own line\n"}, "the branch's own change")
    git(root, "checkout", "-q", "dev")
    now = commit(root, {"proxy/agent.go": "package main\n\n// the base branch moved on\n",
                        ".github/workflows/other.yml": "name: other\n"}, "another change lands on the base")
    git(root, "checkout", "-q", "--detach", "dev")
    git(root, "merge", "-q", "--no-ff", "--no-edit", "work")
    return root, began, now, head


def pull_request(head, base):
    return {"action": "synchronize", "pull_request": {"head": {"sha": head}, "base": {"sha": base, "ref": "dev"}}}


def test_the_base_of_a_pull_request_is_the_base_branch_as_it_is_now(rule, moved):
    root, began, now, head = moved
    assert rule.base_of(root, "pull_request", pull_request(head, began)) == now
    assert now != began


def test_compared_with_that_base_a_check_sees_only_the_branchs_own_files(rule, moved):
    root, began, _, head = moved
    base = rule.base_of(root, "pull_request", pull_request(head, began))
    assert git(root, "diff", "--name-only", f"{base}...HEAD").split() == ["docs/API.md"]
    # The base the event names gives the base branch's later changes as well: the fault this rule removes.
    assert git(root, "diff", "--name-only", f"{began}...HEAD").split() == [
        ".github/workflows/other.yml", "docs/API.md", "proxy/agent.go"]


def test_the_integrity_check_names_only_the_branchs_own_files(rule, moved):
    root, began, _, head = moved
    check = load("integrity_check")
    base = rule.base_of(root, "pull_request", pull_request(head, began))

    def named(commit_to_compare):
        diff = git(root, "diff", "--no-color", "--unified=0", "--no-renames", f"{commit_to_compare}...HEAD", "--")
        return sorted({finding.path for finding in check.check(diff + "\n", set())})
    assert named(base) == []
    assert named(began) == [".github/workflows/other.yml"]


def test_checks_ran_does_not_expect_a_workflow_for_a_path_only_the_base_branch_changed(rule, moved):
    root, began, _, head = moved
    ran = load("checks_ran")
    base = rule.base_of(root, "pull_request", pull_request(head, began))
    replay = {True: {"pull_request": {"branches": ["dev"], "paths": ["proxy/**"]}}, "jobs": {"replay": {}}}
    change = ran.Change("pull_request", "synchronize", head, began, "dev")
    path = ".github/workflows/replay.yml"
    expected, _ = ran.expected_workflows({path: replay}, change, ran.changed_files(root, base), "")
    assert expected == []
    expected, _ = ran.expected_workflows({path: replay}, change, ran.changed_files(root, began), "")
    assert expected == [path]


def test_checks_ran_takes_its_base_from_the_rule_and_from_nowhere_else(monkeypatch):
    ran = load("checks_ran")
    monkeypatch.setenv("CHANGE_BASE", "abc123")
    assert ran.comparison_base() == "abc123"
    monkeypatch.delenv("CHANGE_BASE")
    with pytest.raises(RuntimeError, match="CHANGE_BASE is not set"):
        ran.comparison_base()
    source = (ROOT / "scripts" / "checks_ran.py").read_text(encoding="utf-8")
    assert "changed_files(root, comparison_base())" in source
    assert "changed_files(root, change.base_sha)" not in source


def test_a_checkout_that_is_not_the_merge_of_the_pull_request_stops_with_a_fix(rule, moved):
    root, began, _, head = moved
    with pytest.raises(rule.NotTheExpectedCheckout, match="the second of them the head of the pull request") as other:
        rule.base_of(root, "pull_request", pull_request("0" * 40, began))
    assert "no other base is taken in its place" in str(other.value)
    assert "Fix: let the checkout step take the event's own commit" in str(other.value)
    git(root, "checkout", "-q", "--detach", head)
    with pytest.raises(rule.NotTheExpectedCheckout, match=r"it has 1 parent\(s\)"):
        rule.base_of(root, "pull_request", pull_request(head, began))


def test_in_the_merge_queue_the_base_is_the_one_parent_the_queue_built_on(rule, moved):
    root, began, now, _ = moved
    git(root, "checkout", "-q", "--detach", now)
    group = commit(root, {"docs/API.md": "api, squashed by the queue\n"}, "the queue's commit")
    payload = {"merge_group": {"head_sha": group, "base_sha": now, "base_ref": "refs/heads/dev"}}
    assert rule.base_of(root, "merge_group", payload) == now
    payload["merge_group"]["base_sha"] = began
    with pytest.raises(rule.NotTheExpectedCheckout, match="is not built on the base the merge queue names"):
        rule.base_of(root, "merge_group", payload)


def test_any_other_event_stops(rule, moved):
    with pytest.raises(rule.NotTheExpectedCheckout, match="is not one this rule reads"):
        rule.base_of(moved[0], "push", {})


def test_the_command_prints_the_base_or_ends_with_a_status_that_stops_the_step(moved, tmp_path):
    root, began, now, head = moved
    event = tmp_path / "event.json"

    def run(payload):
        event.write_text(json.dumps(payload), encoding="utf-8")
        return subprocess.run([sys.executable, str(ROOT / "scripts" / "change_base.py")], cwd=root, capture_output=True,
                              text=True, check=False, env={"PATH": "/usr/bin:/bin", "GITHUB_EVENT_NAME": "pull_request",
                                                           "GITHUB_EVENT_PATH": str(event)})
    good = run(pull_request(head, began))
    assert (good.returncode, good.stdout.strip()) == (0, now)
    bad = run(pull_request("0" * 40, began))
    assert (bad.returncode, bad.stdout) == (1, "")
    assert bad.stderr.startswith("FAIL change base: the checked-out commit")


def base_step():
    """The commands of the step that reads the base, as the integrity workflow has them."""
    workflow = yaml.safe_load((WORKFLOWS / "integrity.yml").read_text(encoding="utf-8"))
    return next(step["run"] for step in workflow["jobs"]["integrity"]["steps"] if step.get("id") == "base")


def base_step_run(root, payload, tmp_path):
    """Run that step in a checkout, as a job of a pull request does. Gives the ended command and what it wrote."""
    event, output = tmp_path / "event.json", tmp_path / "output"
    event.write_text(json.dumps(payload), encoding="utf-8")
    output.write_text("", encoding="utf-8")
    (tmp_path / "runner").mkdir()
    done = subprocess.run(["bash", "-e", "-c", base_step()], cwd=root, capture_output=True, text=True, check=False,
                          env={"PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin",
                               "GITHUB_EVENT_NAME": "pull_request", "GITHUB_EVENT_PATH": str(event),
                               "GITHUB_OUTPUT": str(output), "RUNNER_TEMP": str(tmp_path / "runner")})
    return done, output.read_text(encoding="utf-8")


def run_base_step(root, payload, tmp_path):
    """The same, as the status of the step and what it wrote."""
    done, wrote = base_step_run(root, payload, tmp_path)
    return done.returncode, wrote


def with_the_rule(tmp_path, on_the_base, on_the_branch):
    """A base branch and a branch, each with its own copy of the rule (None: no copy), checked out as their merge."""
    root = tmp_path / "repo"
    root.mkdir()
    git(root, "init", "-q", "-b", "dev")
    rule_path = "scripts/change_base.py"
    began = commit(root, {"docs/API.md": "api\n", **({rule_path: on_the_base} if on_the_base else {})}, "base")
    git(root, "checkout", "-q", "-b", "work")
    head = commit(root, {rule_path: on_the_branch}, "the branch's copy of the rule")
    git(root, "checkout", "-q", "--detach", "dev")
    git(root, "merge", "-q", "--no-ff", "--no-edit", "work")
    return root, began, head


def test_the_base_branchs_copy_of_the_rule_gives_the_base_when_a_change_rewrites_the_rule(tmp_path):
    real = (ROOT / "scripts" / "change_base.py").read_text(encoding="utf-8")
    root, base, head = with_the_rule(tmp_path, real, "print('0' * 40)\n")
    assert run_base_step(root, pull_request(head, base), tmp_path) == (0, f"sha={base}\n")


def test_the_changes_copy_of_the_rule_runs_only_while_the_base_has_none(tmp_path):
    real = (ROOT / "scripts" / "change_base.py").read_text(encoding="utf-8")
    root, base, head = with_the_rule(tmp_path, None, real)
    assert run_base_step(root, pull_request(head, base), tmp_path) == (0, f"sha={base}\n")


def test_the_step_stops_and_gives_no_base_when_the_rule_stops(tmp_path):
    real = (ROOT / "scripts" / "change_base.py").read_text(encoding="utf-8")
    root, base, _ = with_the_rule(tmp_path, real, real + "# the branch's own line\n")
    status, wrote = run_base_step(root, pull_request("0" * 40, base), tmp_path)
    assert status != 0
    assert wrote == ""


def test_in_a_checkout_without_the_parent_the_step_stops_and_runs_no_copy_of_the_rule(tmp_path):
    real = (ROOT / "scripts" / "change_base.py").read_text(encoding="utf-8")
    root, base, head = with_the_rule(tmp_path, real, "print('0' * 40)\n")
    shallow = tmp_path / "shallow"
    git(tmp_path, "clone", "-q", "--depth", "1", root.as_uri(), str(shallow))
    done, wrote = base_step_run(shallow, pull_request(head, base), tmp_path)
    assert done.returncode == 1
    assert wrote == ""
    assert "::error::The parent of the checked-out commit is not in this checkout" in done.stdout
    assert "Fix: give the checkout step of this job fetch-depth 0 (or 2 or more) and no ref." in done.stdout


@pytest.mark.parametrize("name", USERS)
def test_the_checkout_that_the_base_step_reads_from_has_the_parent_commits(name):
    workflow = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    for job_name, job in workflow["jobs"].items():
        steps = job["steps"]
        if not any(step.get("id") == "base" for step in steps):
            continue
        settings = next(step for step in steps if str(step.get("uses", "")).startswith("actions/checkout@")).get("with") or {}
        depth = settings.get("fetch-depth", 1)
        assert depth == 0 or depth >= 2, (
            f"{name}, job {job_name}: the checkout has fetch-depth {depth}, so the parent of the checked-out commit is "
            "not there and the base step stops. Fix: set fetch-depth to 0, or to 2 or more.")
        assert "ref" not in settings, (
            f"{name}, job {job_name}: the checkout names a ref, so the checked-out commit is not the merge that the "
            "base is read from. Fix: let the checkout take the event's own commit.")


@pytest.mark.parametrize("name", USERS)
def test_each_workflow_that_compares_with_the_base_takes_it_from_the_one_rule(name):
    workflow = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    for job in workflow["jobs"].values():
        steps = job["steps"]
        using = [n for n, step in enumerate(steps) if "steps.base.outputs.sha" in json.dumps(step)]
        if not using:
            continue
        rule_step = next(n for n, step in enumerate(steps) if step.get("id") == "base")
        assert steps[rule_step]["run"] == base_step()
        assert rule_step < min(using)


def test_no_workflow_takes_the_base_of_a_comparison_from_the_event():
    users = [path.name for path in sorted(WORKFLOWS.glob("*.yml"))
             if re.search(r"pull_request\.base\.sha|merge_group\.base_sha", path.read_text(encoding="utf-8"))]
    assert users == []
    with_rule = [path.name for path in sorted(WORKFLOWS.glob("*.yml"))
                 if "scripts/change_base.py" in path.read_text(encoding="utf-8")]
    assert with_rule == sorted(USERS)
