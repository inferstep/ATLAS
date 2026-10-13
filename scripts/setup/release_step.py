#!/usr/bin/env python3
"""The release step: one push of an already-checked commit to dev, staging or main.

No account can push to those branches (scripts/setup/rulesets.sh), and the
merge button of a pull request makes new commits. Two things need the commit
itself to land:
  - a fast-forward of staging or main, so that the released commit is the
    one that was tested and built;
  - a merge commit on dev, which brings main back into dev after a hotfix.

  release_step.py <dev|staging|main> <full commit id>            check, change nothing
  release_step.py <dev|staging|main> <full commit id> --apply    open the rule, push, close the rule

It refuses unless the commit is the head of an open pull request to that
branch, every required check is green on it, the push is a fast-forward, and
the rulesets are in their normal closed state. With --apply it opens only
the rules that stop such a push, pushes, and closes them again, also when
the push fails. The required checks are never opened: GitHub refuses an
unchecked commit even while the step is open.

Needs `gh` logged in as a repository admin. Run it with --apply only with
the release owner's approval for this push (docs/RELEASE.md).
"""

from __future__ import annotations

import argparse
import json
import pathlib
import subprocess
import sys
from dataclasses import dataclass, field

# The ruleset names that scripts/setup/rulesets.sh writes.
REVIEW = "Release branches: review"
RELEASE_MERGE = "Release branches: pull request and linear history"
DEV_MERGE = "Integration branch: pull request, linear history and merge queue"

RULESET_KEYS = ("name", "target", "enforcement", "conditions", "rules")
GREEN = {"success", "skipped", "neutral"}


class GitHubError(Exception):
    pass


# What a call to GitHub can fail with: the command, its timeout, or its answer.
CLOSE_ERRORS = (GitHubError, subprocess.SubprocessError, OSError, ValueError, KeyError)


class GitHub:
    """The few GitHub API calls the step makes, through the `gh` command."""

    def __init__(self, repo: str) -> None:
        self.repo = repo

    def _api(self, args: list, body=None):
        proc = subprocess.run(
            ["gh", "api", *args] + (["--input", "-"] if body is not None else []),
            input=json.dumps(body) if body is not None else None,
            capture_output=True, text=True, timeout=120, check=False,
        )
        if proc.returncode:
            raise GitHubError(f"gh api {' '.join(args)}: {proc.stdout.strip()} {proc.stderr.strip()}")
        return json.loads(proc.stdout) if proc.stdout.strip() else None

    def get(self, path: str):
        return self._api([f"repos/{self.repo}/{path}"])

    def put(self, path: str, body: dict):
        return self._api([f"repos/{self.repo}/{path}", "-X", "PUT"], body)


def admins(mode: str) -> list:
    """Organization admins and the repository admin role, as bypass actors."""
    return [{"actor_id": None, "actor_type": "OrganizationAdmin", "bypass_mode": mode},
            {"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": mode}]


def modes(actors: list) -> list:
    return sorted(f"{a['actor_type']}:{a['bypass_mode']}" for a in actors or [])


@dataclass
class Plan:
    branch: str
    sha: str
    pull_requests: list = field(default_factory=list)
    ahead: int = 0
    merge_commits: int = 0
    required: list = field(default_factory=list)
    not_green: dict = field(default_factory=dict)
    to_open: list = field(default_factory=list)   # (ruleset id, name, its closed bypass list)
    problems: list = field(default_factory=list)


def check_results(gh, sha: str) -> dict:
    """The latest result of each check and status on the commit, by name."""
    results = {}
    runs = gh.get(f"commits/{sha}/check-runs?per_page=100")["check_runs"]
    for run in sorted(runs, key=lambda r: r["started_at"] or ""):
        results[run["name"]] = run["conclusion"] or run["status"]
    for status in reversed(gh.get(f"commits/{sha}/status?per_page=100")["statuses"]):
        results.setdefault(status["context"], status["state"])
    return results


def rules_to_open(branch: str, merge_commits: int) -> list:
    """(ruleset name, its closed bypass list) for the rules that stop the push.

    On staging and main the review rule stops it, because nobody approves a
    release owner's own pull request. A range with a merge commit also needs
    the linear-history rule opened. On dev the merge queue rule stops every
    push, so the dev rule is opened.
    """
    if branch == "dev":
        return [(DEV_MERGE, [])]
    wanted = [(REVIEW, admins("pull_request"))]
    if merge_commits:
        wanted.append((RELEASE_MERGE, []))
    return wanted


def inspect_range(gh, plan: Plan) -> None:
    """Is the push a fast-forward, and does its range hold merge commits?"""
    try:
        cmp_ = gh.get(f"compare/{plan.branch}...{plan.sha}")
    except GitHubError:
        plan.problems.append("cannot compare the branch with the commit")
        return
    plan.ahead = cmp_["ahead_by"]
    if cmp_["behind_by"] != 0:
        plan.problems.append(
            f"not a fast-forward: {plan.branch} has {cmp_['behind_by']} commit(s) that the new commit lacks")
    if cmp_["ahead_by"] == 0:
        plan.problems.append(f"{plan.branch} already has this commit")
    plan.merge_commits = sum(1 for c in cmp_["commits"] if len(c["parents"]) > 1)
    if cmp_["ahead_by"] > len(cmp_["commits"]):
        # GitHub cut the list short, so the range may hold a merge commit.
        plan.merge_commits = max(plan.merge_commits, 1)


def inspect_checks(gh, plan: Plan) -> None:
    rules = gh.get(f"rules/branches/{plan.branch}?per_page=100")
    plan.required = [c["context"] for r in rules if r["type"] == "required_status_checks"
                     for c in r["parameters"]["required_status_checks"]]
    try:
        results = check_results(gh, plan.sha)
    except GitHubError:
        plan.problems.append(f"cannot read the checks of {plan.sha[:7]}: is the commit on GitHub?")
        plan.not_green = dict.fromkeys(plan.required, "unknown")
        return
    plan.not_green = {c: results.get(c, "missing") for c in plan.required if results.get(c) not in GREEN}
    if not plan.required:
        plan.problems.append(f"no required checks found on {plan.branch}: the rulesets are not as rulesets.sh writes them")
    if plan.not_green:
        shown = ", ".join(f"{k}={v}" for k, v in sorted(plan.not_green.items())[:6])
        plan.problems.append(f"{len(plan.not_green)} of {len(plan.required)} required checks are not green: {shown}")


def inspect_rulesets(gh, plan: Plan) -> None:
    by_name = {r["name"]: r["id"] for r in gh.get("rulesets?per_page=100")}
    for name, closed in rules_to_open(plan.branch, plan.merge_commits):
        if name not in by_name:
            plan.problems.append(f"no ruleset named '{name}': run scripts/setup/rulesets.sh --dry-run")
            continue
        plan.to_open.append((by_name[name], name, closed))
        live = modes(gh.get(f"rulesets/{by_name[name]}").get("bypass_actors"))
        if live != modes(closed):
            plan.problems.append(
                f"ruleset '{name}' is not in its closed state: bypass is {live or 'empty'}, expected {modes(closed) or 'empty'}")


def make_plan(gh, branch: str, sha: str) -> Plan:
    plan = Plan(branch, sha)
    plan.pull_requests = [p["number"] for p in gh.get(f"pulls?state=open&base={branch}&per_page=100")
                          if p["head"]["sha"] == sha]
    if not plan.pull_requests:
        plan.problems.append(f"no open pull request to {branch} has {sha[:7]} as its head")
    inspect_range(gh, plan)
    inspect_checks(gh, plan)
    inspect_rulesets(gh, plan)
    return plan


def set_bypass(gh, ruleset_id: int, actors: list) -> list:
    """Write the ruleset back with this bypass list. Returns the list GitHub then reports."""
    current = gh.get(f"rulesets/{ruleset_id}")
    body = {k: current[k] for k in RULESET_KEYS}
    body["bypass_actors"] = actors
    return gh.put(f"rulesets/{ruleset_id}", body).get("bypass_actors")


def close_rules(gh, plan: Plan, opened: list) -> list:
    """Close every rule that was opened. Returns the names that are still open."""
    still_open = []
    for ruleset_id, name, closed in plan.to_open:
        if ruleset_id not in opened:
            continue
        try:
            if modes(set_bypass(gh, ruleset_id, closed)) == modes(closed):
                print(f"  closed '{name}'")
            else:
                still_open.append(name)
        except CLOSE_ERRORS as e:
            # One failure must not leave the other rules open, so go on.
            still_open.append(name)
            print(f"  ERROR closing '{name}': {e}")
    return still_open


def apply(gh, plan: Plan, push) -> int:
    """Open the rules, push, close the rules. Returns 0 only when the push landed and every rule is closed."""
    opened, code = [], 1
    try:
        for ruleset_id, name, _closed in plan.to_open:
            opened.append(ruleset_id)
            set_bypass(gh, ruleset_id, admins("always"))
            print(f"  opened '{name}'")
        code = 0 if push() else 1
        print("  push: " + ("accepted" if code == 0 else "REFUSED"))
    finally:
        still_open = close_rules(gh, plan, opened)
        if still_open:
            print(f"!!! STILL OPEN: {still_open}. Close them now: run scripts/setup/rulesets.sh, "
                  "which writes every ruleset back to its closed state.")
            code = 2
    return code


def git_push(checkout: str, sha: str, branch: str):
    """A function that pushes the commit to the branch and says whether GitHub took it."""
    git = ["git", "-C", checkout]

    def push() -> bool:
        proc = subprocess.run(git + ["push", "origin", f"{sha}:refs/heads/{branch}"],
                              capture_output=True, text=True, timeout=180, check=False)
        for line in (proc.stderr or "").splitlines():
            if "remote:" in line or "rejected" in line or "->" in line:
                print("     " + line.strip())
        return proc.returncode == 0

    subprocess.run(git + ["fetch", "-q", "origin", sha], check=True, timeout=180)
    return push


def report(plan: Plan, repo: str) -> None:
    print(f"release step: {repo} {plan.branch} <- {plan.sha[:7]}")
    print(f"  pull request: {', '.join('#' + str(n) for n in plan.pull_requests) or '-'}")
    print(f"  range: {plan.ahead} commit(s), {plan.merge_commits} merge commit(s)")
    print(f"  required checks: {len(plan.required) - len(plan.not_green)} of {len(plan.required)} green")
    print(f"  rules to open for one push: {', '.join(repr(n) for _i, n, _c in plan.to_open) or '-'}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("branch", choices=["dev", "staging", "main"])
    ap.add_argument("sha", help="the full 40-character commit id")
    ap.add_argument("--apply", action="store_true", help="open the rule, push, close the rule")
    ap.add_argument("--repo", default="inferstep/ATLAS")
    ap.add_argument("--checkout", default=str(pathlib.Path(__file__).resolve().parents[2]),
                    help="the local clone that pushes (default: this one)")
    args = ap.parse_args()
    if len(args.sha) != 40:
        print("error: give the full 40-character commit id", file=sys.stderr)
        return 2
    gh = GitHub(args.repo)
    try:
        plan = make_plan(gh, args.branch, args.sha)
    except GitHubError as e:
        print(f"error: {e}", file=sys.stderr)
        return 1
    report(plan, args.repo)
    if plan.problems:
        print("STOP, nothing changed:")
        for problem in plan.problems:
            print("  - " + problem)
        return 1
    if not args.apply:
        print("All conditions hold. Nothing changed. Add --apply with the release owner's approval.")
        return 0
    try:
        push = git_push(args.checkout, args.sha, args.branch)
    except (subprocess.SubprocessError, OSError) as e:
        print(f"error: cannot fetch the commit into {args.checkout}, nothing changed: {e}", file=sys.stderr)
        return 1
    code = apply(gh, plan, push)
    try:
        head = gh.get(f"branches/{args.branch}")["commit"]["sha"]
        print(f"  {args.branch} is now at {head[:7]}" + ("" if head == args.sha else ", which is NOT the commit"))
    except GitHubError as e:
        print(f"  could not read {args.branch} back: {e}")
    return code


if __name__ == "__main__":
    sys.exit(main())
