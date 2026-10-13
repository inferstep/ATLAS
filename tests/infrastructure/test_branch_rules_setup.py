"""scripts/setup/rulesets.sh writes branch rules that no account can get
around, and its dry run says whether the live rules are the same.

The script is run for real in dry-run mode. A stand-in `gh` answers its
reads and refuses every write, so no test reaches GitHub.
"""

import importlib.util
import json
import os
import re
import shutil
import stat
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO = Path(__file__).resolve().parents[2]
SETUP = REPO / "scripts" / "setup"
BRANCHES = ("dev", "staging", "main")

STAND_IN_GH = r'''#!/usr/bin/env python3
"""Answers the reads rulesets.sh makes. Logs every call. Refuses every write."""
import glob, json, os, re, sys

args = sys.argv[1:]
with open(os.environ["STAND_IN_LOG"], "a") as log:
    log.write("\t".join(args) + "\n")
if args[:2] == ["auth", "status"]:
    sys.exit(0)
if "-X" in args:
    sys.exit("stand-in gh: a write was attempted: " + " ".join(args))
path = args[1]
live = [json.load(open(f)) for f in sorted(glob.glob(os.path.join(os.environ["STAND_IN_LIVE"], "*.json")))]
fixed = {"apps/github-actions": "15368", "apps/dependabot": "29110"}
if path in fixed:
    print(fixed[path])
elif path.endswith("/teams/maintainers"):
    print("4242")
elif path.endswith("/teams"):
    print("maintainers=maintain\nreviewers=push\ntriagers=triage")
elif path.endswith("/protection"):
    sys.exit(1)
elif path.endswith("/rulesets"):
    name = re.search(r'\.name == "(.*)"\) \|', args[-1]).group(1)
    for ruleset in live:
        if ruleset["name"] == name:
            print(ruleset["id"])
elif "/rulesets/" in path:
    print(json.dumps(next(r for r in live if str(r["id"]) == path.rsplit("/", 1)[1])))
else:
    print("merge commit=false squash=true rebase=true delete-on-merge=true update-branch=true")
'''


def _load(name):
    spec = importlib.util.spec_from_file_location(name, SETUP / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module   # dataclasses look their module up by name
    spec.loader.exec_module(module)
    return module


diff = _load("ruleset_diff")


class DryRun:
    """One dry run of rulesets.sh against a folder of 'live' rulesets."""

    def __init__(self, root: Path, live: Path) -> None:
        root.mkdir(parents=True)
        bin_dir, tmp = root / "bin", root / "tmp"
        bin_dir.mkdir()
        tmp.mkdir()
        gh = bin_dir / "gh"
        gh.write_text(STAND_IN_GH)
        gh.chmod(gh.stat().st_mode | stat.S_IXUSR)
        log = root / "calls.log"
        log.write_text("")
        env = dict(os.environ, PATH=f"{bin_dir}{os.pathsep}{os.environ['PATH']}", TMPDIR=str(tmp),
                   STAND_IN_LOG=str(log), STAND_IN_LIVE=str(live))
        proc = subprocess.run(["bash", str(SETUP / "rulesets.sh"), "--dry-run", "--repo", "example-org/example-repo"],
                              capture_output=True, text=True, env=env, timeout=120, check=False)
        assert proc.returncode == 0, proc.stdout + proc.stderr
        self.stdout = proc.stdout
        self.calls = log.read_text().splitlines()
        out = next(tmp.glob("atlas-rulesets.*"))
        self.files = sorted(out.glob("*.json"))
        self.rulesets = [json.loads(f.read_text()) for f in self.files]


def as_github_returns(ruleset: dict, ruleset_id: int) -> dict:
    """The ruleset with what GitHub adds to an answer: an id, links, and defaults."""
    live = json.loads(json.dumps(ruleset))
    live.update(id=ruleset_id, source="example-org/example-repo", _links={"self": {"href": "x"}})
    for actor in live["bypass_actors"]:
        if actor["actor_type"] == "OrganizationAdmin":
            actor["actor_id"] = None
    for rule in live["rules"]:
        if rule["type"] == "pull_request":
            rule["parameters"].setdefault("required_reviewers", [])
            rule["parameters"].setdefault("dismissal_restriction", {"enabled": False, "allowed_actors": []})
            rule["parameters"]["require_extra_approval_for_unattributed_changes"] = True
    return live


@pytest.fixture(scope="module")
def first_run(tmp_path_factory):
    root = tmp_path_factory.mktemp("rulesets")
    live = root / "live-empty"
    live.mkdir()
    return DryRun(root / "run", live)


@pytest.fixture
def live_copy(first_run, tmp_path):
    """A 'live' folder that holds exactly what the script wants."""
    live = tmp_path / "live"
    live.mkdir()
    for number, ruleset in enumerate(first_run.rulesets, start=101):
        (live / f"{number}.json").write_text(json.dumps(as_github_returns(ruleset, number)))
    return live


def applies_to(ruleset: dict, branch: str) -> bool:
    ref = f"refs/heads/{branch}"
    names = ruleset["conditions"]["ref_name"]
    return ruleset["target"] == "branch" and ref not in names["exclude"] and (ref in names["include"] or "~ALL" in names["include"])


def rule_kinds(ruleset: dict) -> set:
    return {rule["type"] for rule in ruleset["rules"]}


def unskippable(rulesets: list, branch: str, kind: str) -> list:
    """The rulesets that put this rule on the branch and let nobody around it."""
    return [r for r in rulesets if applies_to(r, branch) and kind in rule_kinds(r) and not r["bypass_actors"]]


def required_checks(rulesets: list, branch: str) -> set:
    return {check["context"] for r in rulesets if applies_to(r, branch) for rule in r["rules"]
            if rule["type"] == "required_status_checks" for check in rule["parameters"]["required_status_checks"]}


def test_a_dry_run_writes_nothing_to_github(first_run):
    assert first_run.calls, "the stand-in gh was not used"
    assert not [call for call in first_run.calls if "-X" in call.split("\t")]
    assert len(first_run.rulesets) == 9


def test_no_account_can_skip_a_required_check(first_run):
    with_checks = [r for r in first_run.rulesets if "required_status_checks" in rule_kinds(r)]
    assert with_checks
    for ruleset in with_checks:
        assert ruleset["bypass_actors"] == [], ruleset["name"]
    for branch in BRANCHES:
        assert required_checks(first_run.rulesets, branch), f"{branch} requires no check"


@pytest.mark.parametrize("kind", ["pull_request", "required_linear_history", "non_fast_forward", "deletion"])
def test_no_account_can_get_around_the_rules_on_dev_staging_and_main(first_run, kind):
    for branch in BRANCHES:
        assert unskippable(first_run.rulesets, branch, kind), f"{kind} on {branch} is missing or has a bypass"


def test_a_bypass_never_lets_anyone_push_to_staging_or_main(first_run):
    review = [r for r in first_run.rulesets if r["name"] == "Release branches: review"]
    assert review
    assert review[0]["bypass_actors"]
    assert {actor["bypass_mode"] for actor in review[0]["bypass_actors"]} == {"pull_request"}


def test_dev_takes_changes_only_through_the_merge_queue(first_run):
    assert unskippable(first_run.rulesets, "dev", "merge_queue")
    # The queue never uses a bypass, so a ruleset with a bypass list must
    # leave dev and the queue's own branches alone.
    for ruleset in first_run.rulesets:
        if ruleset["target"] == "branch" and "update" in rule_kinds(ruleset):
            excluded = ruleset["conditions"]["ref_name"]["exclude"]
            assert "refs/heads/dev" in excluded
            assert "refs/heads/gh-readonly-queue/**/*" in excluded


def test_every_check_required_on_dev_is_reported_by_a_job(first_run):
    patterns = []
    for workflow in sorted((REPO / ".github" / "workflows").glob("*.yml")):
        for job_id, job in (yaml.safe_load(workflow.read_text()).get("jobs") or {}).items():
            name = str(job.get("name", job_id))
            literal = re.sub(r"\$\{\{.*?\}\}", "", name)
            if literal.strip():   # a name that is only a matrix value would match anything
                parts = [re.escape(part) for part in re.split(r"\$\{\{.*?\}\}", name)]
                patterns.append(re.compile(".+".join(parts)))
    for check in sorted(required_checks(first_run.rulesets, "dev")):
        assert any(p.fullmatch(check) for p in patterns), f"no workflow job reports the required check {check!r}"


def test_staging_and_main_require_only_checks_that_dev_requires(first_run):
    dev = required_checks(first_run.rulesets, "dev")
    for branch in ("staging", "main"):
        assert required_checks(first_run.rulesets, branch) <= dev


def test_the_release_step_names_rulesets_that_the_script_writes(first_run):
    step = _load("release_step")
    written = {r["name"]: r for r in first_run.rulesets}
    for name in (step.REVIEW, step.RELEASE_MERGE, step.DEV_MERGE):
        assert name in written
    assert not any("required_status_checks" in rule_kinds(written[n])
                   for n in (step.REVIEW, step.RELEASE_MERGE, step.DEV_MERGE))


def test_a_dry_run_says_same_when_the_live_rules_match(first_run, live_copy, tmp_path):
    run = DryRun(tmp_path / "run", live_copy)
    assert "9 of 9 are the same as the live rulesets" in run.stdout
    assert not [line for line in run.stdout.splitlines() if line.startswith(("  update  ", "  create  "))]


def test_a_dry_run_shows_a_bypass_that_was_added_to_the_checks(first_run, live_copy, tmp_path):
    for path in live_copy.glob("*.json"):
        ruleset = json.loads(path.read_text())
        if ruleset["name"] == "Integration branch: required checks":
            ruleset["bypass_actors"] = [{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always"}]
            path.write_text(json.dumps(ruleset))
    run = DryRun(tmp_path / "run", live_copy)
    assert "update  Integration branch: required checks" in run.stdout
    assert "bypass: wants nobody, live has ['RepositoryRole:5:always']" in run.stdout
    assert "8 of 9 are the same as the live rulesets" in run.stdout


def _pair():
    wanted = {"name": "n", "target": "branch", "enforcement": "active",
              "conditions": {"ref_name": {"include": ["refs/heads/a", "refs/heads/b"], "exclude": []}},
              "rules": [{"type": "deletion"},
                        {"type": "required_status_checks", "parameters": {
                            "strict_required_status_checks_policy": True,
                            "required_status_checks": [{"context": "one", "integration_id": 1},
                                                       {"context": "two", "integration_id": 1}]}}],
              "bypass_actors": []}
    return wanted, as_github_returns(wanted, 7)


def test_order_and_empty_defaults_are_not_differences():
    wanted, live = _pair()
    live["conditions"]["ref_name"]["include"].reverse()
    live["rules"].reverse()
    live["rules"][0]["parameters"]["required_status_checks"].reverse()
    live["rules"][0]["parameters"]["do_not_enforce_on_create"] = False
    assert diff.differences(wanted, live) == []


def test_a_missing_check_and_a_live_extra_setting_are_differences():
    wanted, live = _pair()
    checks = next(r for r in live["rules"] if r["type"] == "required_status_checks")["parameters"]
    checks["required_status_checks"].pop()
    checks["do_not_enforce_on_create"] = True
    found = diff.differences(wanted, live)
    assert any("required_status_checks.required_status_checks" in line for line in found)
    assert any("do_not_enforce_on_create" in line and "does not set" in line for line in found)


def test_a_rule_on_one_side_only_is_a_difference():
    wanted, live = _pair()
    live["rules"] = [r for r in live["rules"] if r["type"] != "deletion"] + [{"type": "creation"}]
    assert diff.differences(wanted, live) == ["rule deletion: wanted, not live", "rule creation: live, not wanted"]


def test_the_audit_reads_the_strictest_pull_request_rule_on_a_branch():
    audit = _load("audit")
    reader = audit.Audit.__new__(audit.Audit)
    restricted = {"enabled": True, "allowed_actors": [{"id": 1, "type": "Team"}]}
    reader.rules = [
        {"type": "pull_request", "ruleset_id": 1, "parameters": {
            "required_approving_review_count": 0, "require_code_owner_review": False,
            "dismissal_restriction": {"enabled": False, "allowed_actors": []}}},
        {"type": "pull_request", "ruleset_id": 2, "parameters": {
            "required_approving_review_count": 1, "require_code_owner_review": True,
            "dismissal_restriction": restricted}},
    ]
    assert reader.pr_param("required_approving_review_count") == 1
    assert reader.pr_param("require_code_owner_review") is True
    assert reader.pr_param("dismissal_restriction") == restricted
    assert reader.pr_param("a parameter no rule has") is None


def test_bash_is_there_for_these_tests():
    assert shutil.which("bash")
