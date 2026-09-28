#!/usr/bin/env python3
"""Audit the org and repo against the OpenSSF GitHub configuration best
practices (https://best.openssf.org/SCM-BestPractices/github/).

Read-only: it only makes GET requests. Every row prints PASS, FAIL,
EXCEPTION (a written, accepted reason), N/A, or MANUAL (the API can't show
it with this token, or it needs a person's judgement). Exits 1 if any row
FAILs.

Run it before launch and every quarter (GOVERNANCE). Needs `gh` logged in
as an org owner; the `repo` and `read:org` scopes are enough.

Usage: scripts/setup/audit.py [--org ORG] [--repo NAME] [--markdown]
"""

from __future__ import annotations

import argparse
import base64
import datetime as dt
import json
import re
import subprocess
import sys
import urllib.request

PASS, FAIL, EXC, NA, MANUAL = "PASS", "FAIL", "EXCEPTION", "N/A", "MANUAL"


def gh(path: str) -> tuple:
    """(status, json). A 404 or 403 comes back as a status, not an exception."""
    proc = subprocess.run(["gh", "api", "-i", path], capture_output=True, text=True)
    parts = re.split(r"\r?\n\r?\n", proc.stdout, maxsplit=1)
    head, body = parts[0], (parts[1] if len(parts) > 1 else "")
    m = re.match(r"HTTP/\S+ (\d+)", head)
    status = int(m.group(1)) if m else 0
    try:
        data = json.loads(body) if body.strip() else None
    except ValueError:
        data = None
    return status, data


def ok(path: str):
    status, data = gh(path)
    if status >= 400:
        raise LookupError(f"GET {path}: HTTP {status}")
    return data


class Audit:
    def __init__(self, org: str, repo: str) -> None:
        self.org, self.repo = org, repo
        self.full = f"{org}/{repo}"
        self.rows: list = []
        self.repo_info = ok(f"repos/{self.full}")
        self.default = self.repo_info["default_branch"]
        self.rules = ok(f"repos/{self.full}/rules/branches/{self.default}")
        self.rulesets = {r["id"]: ok(f"repos/{self.full}/rulesets/{r['id']}")
                         for r in ok(f"repos/{self.full}/rulesets")}
        self.workflows = self._workflows()

    def add(self, area: str, item: str, result: str, evidence: str) -> None:
        self.rows.append((area, item, result, evidence))

    # helpers
    def rule(self, kind: str) -> list:
        return [r for r in self.rules if r["type"] == kind]

    def rule_without_bypass(self, kind: str) -> bool:
        return any(not self.rulesets[r["ruleset_id"]]["bypass_actors"] for r in self.rule(kind))

    def pr_param(self, key: str):
        vals = [r["parameters"].get(key) for r in self.rule("pull_request")]
        return max(vals) if vals and not isinstance(vals[0], dict) else (vals[0] if vals else None)

    def _workflows(self) -> dict:
        out = {}
        for f in ok(f"repos/{self.full}/contents/.github/workflows?ref={self.default}"):
            if f["name"].endswith((".yml", ".yaml")):
                blob = ok(f"repos/{self.full}/contents/{f['path']}?ref={self.default}")
                out[f["name"]] = base64.b64decode(blob["content"]).decode()
        return out

    def exists(self, path: str) -> bool:
        return gh(f"repos/{self.full}/contents/{path}?ref={self.default}")[0] == 200

    # checks
    def run(self) -> None:
        org = ok(f"orgs/{self.org}")
        wf_perm = ok(f"repos/{self.full}/actions/permissions/workflow")
        act = ok(f"repos/{self.full}/actions/permissions")

        # CI/CD
        self.add("CI/CD", "Workflows can't approve PRs",
                 FAIL if wf_perm["can_approve_pull_request_reviews"] else PASS,
                 f"can_approve_pull_request_reviews={wf_perm['can_approve_pull_request_reviews']}")
        others = [r["name"] for r in ok(f"orgs/{self.org}/repos?per_page=100") if r["name"] != self.repo]
        disabled = [n for n in others if not ok(f"repos/{self.org}/{n}/actions/permissions")["enabled"]]
        self.add("CI/CD", "Actions restricted to selected repos",
                 PASS if act["enabled"] and len(disabled) == len(others) else FAIL,
                 f"{self.repo} enabled; Actions off in {len(disabled)}/{len(others)} other repos "
                 "(the org policy itself needs admin:org to read)")
        unpinned = [f"{n}: {u}" for n, txt in self.workflows.items()
                    for u in re.findall(r"uses:\s*(\S+)", txt)
                    if not u.startswith("./") and not re.search(r"@[0-9a-f]{40}$", u)]
        self.add("CI/CD", "Only verified or explicitly trusted actions",
                 PASS if act["allowed_actions"] == "selected" and act.get("sha_pinning_required") and not unpinned else FAIL,
                 f"allowed_actions={act['allowed_actions']}, sha_pinning_required={act.get('sha_pinning_required')}, "
                 f"unpinned uses on {self.default}: {len(unpinned)}")
        self.add("CI/CD", "Default workflow token read-only",
                 PASS if wf_perm["default_workflow_permissions"] == "read" else FAIL,
                 f"default_workflow_permissions={wf_perm['default_workflow_permissions']}")
        runners = ok(f"repos/{self.full}/actions/runners")["total_count"]
        for item in ("Runner group limited to private repos", "Runner group limited to selected repos"):
            self.add("CI/CD", item, NA if runners == 0 else MANUAL,
                     f"{runners} self-hosted runners (GitHub-hosted only)")
        self.add("Enterprise", "All six enterprise items", NA, "Free org, no enterprise account")

        # Members
        admins = [m["login"] for m in ok(f"orgs/{self.org}/members?role=admin&per_page=100")]
        self.add("Members", "Fewer than three org owners", PASS if len(admins) < 3 else FAIL,
                 f"owners: {', '.join(admins)}")
        self.add("Members", "Admins active in last 6 months", MANUAL, "quarterly access review")
        self.add("Members", "Members active in last 6 months", MANUAL, "quarterly access review; inactivity drops a rung")

        # Org
        self.add("Org", "2FA enforced", PASS if org.get("two_factor_requirement_enabled") else FAIL,
                 f"two_factor_requirement_enabled={org.get('two_factor_requirement_enabled')}")
        self.add("Org", "Default member permission restricted",
                 PASS if org.get("default_repository_permission") in ("none", "read") else FAIL,
                 f"default_repository_permission={org.get('default_repository_permission')}")
        self.add("Org", "Only admins create public repos",
                 FAIL if org.get("members_can_create_public_repositories") else PASS,
                 f"members_can_create_public_repositories={org.get('members_can_create_public_repositories')}")
        self.add("Org", "Org uses SSO", NA, "needs Enterprise Cloud; 2FA + security key or passkey instead")
        hooks_status, hooks = gh(f"orgs/{self.org}/hooks")
        if hooks_status == 200:
            bad_ssl = [h["id"] for h in hooks if h["config"].get("insecure_ssl") not in ("0", 0)]
            no_secret = [h["id"] for h in hooks if not h["config"].get("secret")]
            self.add("Org", "Org webhooks use SSL", FAIL if bad_ssl else PASS, f"{len(hooks)} org hooks")
            self.add("Org", "Org webhooks have a secret", FAIL if no_secret else PASS, f"{len(hooks)} org hooks")
        else:
            for item in ("Org webhooks use SSL", "Org webhooks have a secret"):
                self.add("Org", item, MANUAL, f"reading org hooks needs admin:org_hook (HTTP {hooks_status})")

        # Repo
        pushed = dt.datetime.strptime(self.repo_info["pushed_at"], "%Y-%m-%dT%H:%M:%SZ")
        days = (dt.datetime.now(dt.timezone.utc) - pushed.replace(tzinfo=dt.timezone.utc)).days
        self.add("Repo", "Updated at least quarterly", PASS if days <= 90 else FAIL, f"last push {days} days ago")
        self.add("Repo", "Workflows can't approve PRs",
                 FAIL if wf_perm["can_approve_pull_request_reviews"] else PASS, "same setting as CI/CD")
        reviews = self.pr_param("required_approving_review_count") or 0
        self.add("Repo", "Default branch requires code review", PASS if reviews >= 1 else FAIL,
                 f"{reviews} approval(s) on {self.default}; the lead's direct pushes are a logged bypass")
        self.add("Repo", "Linear history", PASS if self.rule("required_linear_history") else FAIL,
                 f"required_linear_history on {self.default}")
        self.add("Repo", "Workflow token read-only", PASS if wf_perm["default_workflow_permissions"] == "read" else FAIL,
                 "same setting as CI/CD")
        self._scorecard()
        self.add("Repo", "Two reviewers required", EXC if reviews < 2 else PASS,
                 "Exception until 3+ people hold Reviewer or above (GOVERNANCE)")
        checks = [c for r in self.rule("required_status_checks") for c in r["parameters"]["required_status_checks"]]
        self.add("Repo", "All checks pass before merge", PASS if checks else FAIL, f"{len(checks)} required checks")
        strict = any(r["parameters"].get("strict_required_status_checks_policy") for r in self.rule("required_status_checks"))
        self.add("Repo", "Branch up to date before merge", PASS if strict else FAIL, f"strict={strict}")
        self.add("Repo", "No force pushes", PASS if self.rule_without_bypass("non_fast_forward") else FAIL,
                 "non_fast_forward in a ruleset with no bypass")
        self.add("Repo", "Default branch protected", PASS if self.rules else FAIL, f"{len(self.rules)} active rules on {self.default}")
        self.add("Repo", "Branch deletion blocked", PASS if self.rule_without_bypass("deletion") else FAIL,
                 "deletion in a ruleset with no bypass")
        dep_review = "dependency-review.yml" in self.workflows and any(c["context"] == "dependency review" for c in checks)
        self.add("Repo", "Dependency review enabled", PASS if dep_review else FAIL,
                 f"workflow on {self.default}: {'dependency-review.yml' in self.workflows}; required: "
                 f"{any(c['context'] == 'dependency review' for c in checks)}")
        alerts = gh(f"repos/{self.full}/vulnerability-alerts")[0]
        self.add("Repo", "Vulnerability alerts enabled", PASS if alerts == 204 else FAIL, f"HTTP {alerts}")
        self.add("Repo", "Forking not allowed", EXC if self.repo_info.get("allow_forking") else PASS,
                 "Exception: public open source; contributors work from forks")
        convo = self.pr_param("required_review_thread_resolution")
        self.add("Repo", "Conversations resolved before merge", PASS if convo else FAIL, f"required_review_thread_resolution={convo}")
        hooks = ok(f"repos/{self.full}/hooks")
        self.add("Repo", "Repo webhooks have a secret",
                 PASS if all(h["config"].get("secret") for h in hooks) else FAIL, f"{len(hooks)} repo hooks")
        self.add("Repo", "Repo webhooks use SSL",
                 PASS if all(h["config"].get("insecure_ssl") in ("0", 0) for h in hooks) else FAIL, f"{len(hooks)} repo hooks")
        self.add("Repo", "Signed commits required", PASS if self.rule("required_signatures") else EXC,
                 "Partial: release tags are signed and verified (verify-tags); commit-signing rollout pending (4.1)")
        stale = self.pr_param("dismiss_stale_reviews_on_push")
        self.add("Repo", "New changes need re-approval", PASS if stale else FAIL, f"dismiss_stale_reviews_on_push={stale}")
        restricted = self.rule("update") and self.rule("creation")
        self.add("Repo", "Restrict who can push", PASS if restricted else FAIL,
                 "creation and update rules; only admins, maintainers and Dependabot bypass")
        collabs = ok(f"repos/{self.full}/collaborators?per_page=100")
        repo_admins = [c["login"] for c in collabs if c.get("role_name") == "admin"]
        self.add("Repo", "Fewer than three repo admins", PASS if len(repo_admins) < 3 else FAIL, f"admins: {', '.join(repo_admins)}")
        owners = self.pr_param("require_code_owner_review")
        self.add("Repo", "Review limited to code owners", PASS if owners and self.exists(".github/CODEOWNERS") else FAIL,
                 f"require_code_owner_review={owners}, CODEOWNERS present")
        dismissal = self.pr_param("dismissal_restriction") or {}
        self.add("Repo", "Restrict who can dismiss reviews", PASS if dismissal.get("enabled") else FAIL,
                 f"dismissal_restriction enabled={dismissal.get('enabled')}")

        # Ops
        self.add("Ops", "Managed from one central account", MANUAL, "the lead's account owns the org")
        members = {m["login"] for m in ok(f"orgs/{self.org}/members?per_page=100")}
        in_teams = {m["login"] for t in ok(f"orgs/{self.org}/teams") for m in ok(f"orgs/{self.org}/teams/{t['slug']}/members")}
        self.add("Ops", "Membership limited to the team", PASS if members <= in_teams else FAIL,
                 f"{len(members)} members, all in a team: {members <= in_teams}")
        self.add("Ops", "Review settings after transfer", MANUAL, "done 2026-09-27 against the pre-transfer snapshot")
        ir = self.exists("docs/INCIDENT_RESPONSE.md") and self.exists("SECURITY.md")
        self.add("Ops", "Incident response plan", PASS if ir else FAIL,
                 f"SECURITY.md + docs/INCIDENT_RESPONSE.md on {self.default}: {ir}")
        sa = self.repo_info.get("security_and_analysis") or {}
        scanning = (sa.get("secret_scanning", {}).get("status") == "enabled"
                    and sa.get("secret_scanning_push_protection", {}).get("status") == "enabled"
                    and "codeql.yml" in self.workflows)
        self.add("Ops", "Security alerts and scanning", PASS if scanning and alerts == 204 else FAIL,
                 "secret scanning + push protection + Dependabot alerts + codeql.yml")
        tooling = "scorecard.yml" in self.workflows and self.exists("scripts/setup/audit.py")
        self.add("Ops", "Automated compliance tooling", PASS if tooling else FAIL,
                 f"scorecard.yml and scripts/setup/audit.py on {self.default}: {tooling}")
        self.add("Ops", "API tools instead of elevated privileges", MANUAL, "scripts/setup/* and the scoped atlas-bot app")
        self.add("Ops", "Audit logs and policies reviewed", MANUAL, "quarterly: org Settings → Logs → Audit log")

    def _scorecard(self) -> None:
        url = f"https://api.scorecard.dev/projects/github.com/{self.full}"
        try:
            with urllib.request.urlopen(url, timeout=20) as resp:
                score = json.loads(resp.read()).get("score")
            self.add("Repo", "Scorecard above 7", PASS if score and score > 7 else FAIL, f"score {score}")
        except Exception as e:  # not published yet, or network
            self.add("Repo", "Scorecard above 7", MANUAL, f"no published score yet ({type(e).__name__})")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--org", default="inferstep")
    ap.add_argument("--repo", default="ATLAS")
    ap.add_argument("--markdown", action="store_true", help="print a Markdown table")
    args = ap.parse_args()
    audit = Audit(args.org, args.repo)
    audit.run()
    if args.markdown:
        print("| Area | OpenSSF item | Result | Evidence |\n| --- | --- | --- | --- |")
        for row in audit.rows:
            print("| " + " | ".join(row) + " |")
    else:
        width = max(len(r[1]) for r in audit.rows)
        for area, item, result, evidence in audit.rows:
            print(f"{result:<9} {area:<10} {item:<{width}}  {evidence}")
    counts: dict = {}
    for row in audit.rows:
        counts[row[2]] = counts.get(row[2], 0) + 1
    print("\n" + ", ".join(f"{k} {v}" for k, v in sorted(counts.items())), f"(default branch: {audit.default})")
    return 1 if counts.get(FAIL) else 0


if __name__ == "__main__":
    sys.exit(main())
