#!/usr/bin/env python3
"""Monthly contributor-flow review for inferstep/ATLAS. Read-only.

New contributors leave when nobody answers. This collects the numbers that
show a slow queue early: issues waiting in Triage, pull requests waiting for
a first maintainer response, claims with no pull request, the stock of
starter issues, open Dependabot pull requests and security alerts, and the
OpenSSF Scorecard result. It prints Markdown, ready to paste into an issue.

Exit status 1 when a promise from CONTRIBUTING is broken: an outside pull
request with no maintainer response after 5 business days.

Usage: scripts/setup/flow_review.py [--repo inferstep/ATLAS] [--project 1]
Needs the GitHub CLI (gh), logged in. Makes only GET and GraphQL-read calls.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import subprocess
import sys
import urllib.request

MAINTAINER = ("OWNER", "MEMBER", "COLLABORATOR")
RESPONSE_DAYS = 5  # business days, as CONTRIBUTING promises
ISSUE_REF = re.compile(r"(?<![\w/#])#(\d+)\b")  # as scripts/bot/atlas_bot.py


def gh(*args: str, body: dict | None = None):
    cmd = ["gh", "api", *args]
    if "--paginate" in args:
        cmd.append("--slurp")
    if body is not None:
        cmd += ["--input", "-"]
    out = subprocess.run(cmd, input=json.dumps(body) if body else None,
                         capture_output=True, text=True, check=True).stdout
    data = json.loads(out) if out.strip() else None
    if "--paginate" in args:
        data = [x for page in data for x in page]
    return data


def gql(query: str, **variables):
    data = gh("graphql", body={"query": query, "variables": variables})
    if data.get("errors"):
        raise SystemExit(json.dumps(data["errors"]))
    return data["data"]


def parse(stamp: str) -> dt.datetime:
    return dt.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)


def business_days(start: dt.datetime, end: dt.datetime) -> int:
    """Whole weekdays from start to end, not counting the start day."""
    days, day = 0, start.date()
    while day < end.date():
        day += dt.timedelta(days=1)
        if day.weekday() < 5:
            days += 1
    return days


def board_items(owner: str, number: int) -> list:
    items, cursor = [], None
    while True:
        page = gql("""query($o: String!, $n: Int!, $c: String) { organization(login: $o) {
            projectV2(number: $n) { items(first: 100, after: $c) { pageInfo { hasNextPage endCursor }
              nodes { status: fieldValueByName(name: "Status") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
                level: fieldValueByName(name: "Contributor Level") { ... on ProjectV2ItemFieldSingleSelectValue { name } }
                content { __typename
                  ... on Issue { number title state createdAt issueType { name } assignees(first: 5) { nodes { login } }
                    closedByPullRequestsReferences(first: 5, includeClosedPrs: false) { totalCount } }
                  ... on PullRequest { number title state createdAt } } } } } } }""",
                   o=owner, n=number, c=cursor)["organization"]["projectV2"]["items"]
        items += page["nodes"]
        if not page["pageInfo"]["hasNextPage"]:
            return items
        cursor = page["pageInfo"]["endCursor"]


def first_response(repo: str, pr: dict) -> str | None:
    """Time of the first review or comment by a maintainer, if any."""
    n = pr["number"]
    times = [r["submitted_at"] for r in gh(f"repos/{repo}/pulls/{n}/reviews", "--paginate")
             if r.get("author_association") in MAINTAINER and r.get("submitted_at")]
    times += [c["created_at"] for c in gh(f"repos/{repo}/issues/{n}/comments", "--paginate")
              if c.get("author_association") in MAINTAINER]
    return min(times) if times else None


def scorecard(repo: str) -> str:
    try:
        with urllib.request.urlopen(f"https://api.securityscorecards.dev/projects/github.com/{repo}",
                                    timeout=20) as resp:
            d = json.load(resp)
        return f"{d['score']} (scan of {d['date'][:10]}, commit {d['repo']['commit'][:7]})"
    except Exception as e:  # the score is informational; never fail the review on it
        return f"unavailable ({e.__class__.__name__})"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--repo", default="inferstep/ATLAS")
    ap.add_argument("--project", type=int, default=1)
    a = ap.parse_args()
    owner = a.repo.split("/")[0]
    now = dt.datetime.now(dt.timezone.utc)
    broken = 0

    items = [i for i in board_items(owner, a.project) if i.get("content")]
    open_issues = [i for i in items if i["content"]["__typename"] == "Issue" and i["content"]["state"] == "OPEN"]
    status = lambda i: (i.get("status") or {}).get("name")

    print(f"# Contributor flow review, {now:%Y-%m-%d}\n")

    triage = sorted((i for i in items if status(i) == "Triage" and i["content"]["state"] == "OPEN"),
                    key=lambda i: i["content"]["createdAt"])
    print(f"## Triage queue: {len(triage)}\n")
    for i in triage:
        c = i["content"]
        print(f"- #{c['number']} ({(now - parse(c['createdAt'])).days} days) {c['title']}")

    print("\n## Outside pull requests\n")
    print("| PR | Author | Open | First maintainer response |\n|---|---|---|---|")
    for pr in gh(f"repos/{a.repo}/pulls?state=open", "--paginate"):
        if pr["user"].get("type") == "Bot" or pr.get("author_association") in MAINTAINER:
            continue
        opened = parse(pr["created_at"])
        first = first_response(a.repo, pr)
        if first:
            answer = f"after {business_days(opened, parse(first))} business days"
        else:
            waited = business_days(opened, now)
            late = waited > RESPONSE_DAYS
            broken += late
            answer = f"**none after {waited} business days**" if late else f"none yet ({waited} business days)"
        draft = " (draft)" if pr.get("draft") else ""
        print(f"| #{pr['number']}{draft} | {pr['user']['login']} | {(now - opened).days} days | {answer} |")

    # GitHub links "Closes #N" only for pull requests into main, and ours go
    # into dev, so an open pull request by the assignee that names the issue
    # counts too (the claim bot uses the same rule).
    pulls = gh(f"repos/{a.repo}/pulls?state=open", "--paginate")
    named: dict = {}
    for pr in pulls:
        for num in set(ISSUE_REF.findall(f"{pr.get('title') or ''}\n{pr.get('body') or ''}")):
            named.setdefault(int(num), set()).add(pr["user"]["login"])

    def has_pr(i: dict) -> bool:
        c = i["content"]
        who = {x["login"] for x in c["assignees"]["nodes"]}
        return c["closedByPullRequestsReferences"]["totalCount"] > 0 or bool(who & named.get(c["number"], set()))

    claims = [i for i in open_issues if status(i) == "In Progress" and i["content"]["assignees"]["nodes"]
              and not has_pr(i)]
    print(f"\n## Claims with no linked pull request: {len(claims)}\n")
    for i in claims:
        c = i["content"]
        who = ", ".join("@" + x["login"] for x in c["assignees"]["nodes"])
        print(f"- #{c['number']} {who}: {c['title']}")

    starters = [i for i in open_issues if status(i) == "Ready" and (i.get("level") or {}).get("name") == "Starter"
                and not i["content"]["assignees"]["nodes"] and (i["content"].get("issueType") or {}).get("name") != "Epic"]
    print(f"\n## Start Here stock: {len(starters)} unclaimed starter issues"
          + (" (low: write more)" if len(starters) < 3 else "") + "\n")

    dep_prs = [p for p in pulls if p["user"]["login"] == "dependabot[bot]"]
    alerts = len(gh(f"repos/{a.repo}/dependabot/alerts?state=open", "--paginate"))
    scans = gh(f"repos/{a.repo}/code-scanning/alerts?state=open", "--paginate")
    by_tool: dict = {}
    for s in scans:
        by_tool[s["tool"]["name"]] = by_tool.get(s["tool"]["name"], 0) + 1
    leak_alerts = len(gh(f"repos/{a.repo}/secret-scanning/alerts?state=open", "--paginate"))
    print("## Security and upkeep\n")
    print(f"- Open Dependabot pull requests: {len(dep_prs)}")
    print(f"- Open Dependabot alerts: {alerts}")
    print(f"- Open code scanning alerts: {len(scans)}"
          + (f" ({', '.join(f'{k} {v}' for k, v in sorted(by_tool.items()))})" if by_tool else ""))
    print(f"- Open secret scanning alerts: {leak_alerts}")
    print(f"- OpenSSF Scorecard: {scorecard(a.repo)}")

    if broken:
        print(f"\n**{broken} pull request(s) waited more than {RESPONSE_DAYS} business days"
              " for a first response.**")
    return 1 if broken else 0


if __name__ == "__main__":
    sys.exit(main())
