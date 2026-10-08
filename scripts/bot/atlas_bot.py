#!/usr/bin/env python3
"""atlas-bot: the contributor workflow for ATLAS. Standard library only.

Commands, one per workflow in .github/workflows/bot-*.yml:

  claim       `/claim` or `/unclaim` as the first line of an issue comment
  stale       daily: remind, then release, a /claim with no open pull request
              by the claimant that names the issue
  sync        hourly: mirror Ready/Blocked to labels, area labels on PRs,
              welcome first-time PR authors, close issues whose fix
              reached dev
  welcome     issue opened: welcome a first-time issue author
  rfc-dedupe  RFC or Feature opened: link open RFCs and Epics with similar titles

Settings come from .github/atlas-bot.yml. The bot reads and writes issue,
pull request and project metadata only. It never checks out or runs PR code,
and text from users (comments, titles) is only matched against fixed
patterns, never executed.

Environment: GH_TOKEN (the atlas-bot installation token), GITHUB_REPOSITORY,
GITHUB_EVENT_PATH for event commands, ATLAS_BOT_DRY_RUN=1 to print writes
instead of making them.
"""

from __future__ import annotations

import datetime as dt
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Iterable

CONFIG_PATH = os.path.join(os.path.dirname(__file__), "..", "..", ".github", "atlas-bot.yml")
COMMAND = re.compile(r"^/(claim|unclaim)\s*$")
MAINTAINER = ("OWNER", "MEMBER")
FIRST_PR = ("FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER")
STATUS_LABELS = {"Ready": "status/ready", "Blocked": "status/blocked"}
# GitHub's closing keywords. They act only on the default branch (main), and
# work merges into dev, so sync closes the issues itself.
ISSUE_REF = re.compile(r"(?<![\w/#])#(\d+)\b")
_CLOSES = r"(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?):?[ \t]+#(\d+)\b"
CLOSING = re.compile(_CLOSES, re.IGNORECASE)
# A closing line: the keyword starts the line. More issues follow only as
# ", closes #N" or "and fixes #N"; other text on the line closes nothing.
CLOSING_LINE = re.compile(rf"[ \t]*{_CLOSES}(?:[ \t]*(?:,|and)[ \t]*{_CLOSES})*", re.IGNORECASE)
FENCE = re.compile(r"[ \t]*(?:```|~~~)")
STOPWORDS = set("""about after again against also allow allows because before being
between could does doing during each from have into just like make more most need
needs only other over same should some such than that their them then there these
they this those through under until very want what when where which while with
without would your support add adds use uses using""".split())


# --- settings -----------------------------------------------------------------


def load_config(path: str = CONFIG_PATH) -> dict:
    """Read the small YAML subset atlas-bot.yml uses: nested mappings of
    scalars, `#` comments. No dependency on PyYAML."""
    root: dict = {}
    stack: list = [(-1, root)]
    with open(path, encoding="utf-8") as fh:
        for raw in fh:
            line = raw.split(" #", 1)[0].rstrip()
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            indent = len(line) - len(line.lstrip())
            body = line.strip()
            if body.endswith(":") and ": " not in body:
                key, value = body[:-1], None
            else:
                key, sep, value = body.partition(": ")
                if not sep:
                    raise ValueError(f"atlas-bot.yml: cannot read line: {raw!r}")
            while stack[-1][0] >= indent:
                stack.pop()
            parent = stack[-1][1]
            if value is None:
                parent[key] = {}
                stack.append((indent, parent[key]))
            else:
                value = value.strip().strip("\"'")
                parent[key] = int(value) if value.isdigit() else value
    return root


# --- GitHub API ---------------------------------------------------------------


class API:
    """The GitHub calls the bot makes. Tests replace this with a fake."""

    def __init__(self, token: str, repo: str, dry_run: bool = False,
                 base: str = "https://api.github.com") -> None:
        self.token, self.repo, self.dry_run, self.base = token, repo, dry_run, base
        self.owner = repo.split("/")[0]
        self._project: dict | None = None

    # transport
    def _call(self, method: str, path: str, body: Any = None, mutate: bool | None = None) -> Any:
        # GraphQL reads are POSTs too, so the caller says whether a call writes.
        if (method != "GET" if mutate is None else mutate) and self.dry_run:
            print(f"DRY-RUN {method} {path} {json.dumps(body) if body else ''}".rstrip())
            return {}
        url = path if path.startswith("http") else f"{self.base}{path}"
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(url, data=data, method=method, headers={
            "Authorization": f"Bearer {self.token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "atlas-bot"})
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                raw = resp.read()
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"{method} {path}: HTTP {e.code} {e.read()[:300]!r}") from None

    def _pages(self, path: str) -> Iterable[dict]:
        sep = "&" if "?" in path else "?"
        page = 1
        while True:
            batch = self._call("GET", f"{path}{sep}per_page=100&page={page}")
            items = batch.get("items", batch) if isinstance(batch, dict) else batch
            yield from items
            if len(items) < 100:
                return
            page += 1

    def graphql(self, query: str, write: bool = False, **variables: Any) -> dict:
        if write and self.dry_run:
            print(f"DRY-RUN graphql {json.dumps(variables)}")
            return {}
        out = self._call("POST", "/graphql", {"query": query, "variables": variables}, mutate=write)
        if out.get("errors"):
            raise RuntimeError(f"graphql: {out['errors']}")
        return out["data"]

    # issues and pull requests
    def issue(self, number: int) -> dict:
        return self._call("GET", f"/repos/{self.repo}/issues/{number}")

    def comments(self, number: int) -> list:
        return list(self._pages(f"/repos/{self.repo}/issues/{number}/comments"))

    def comment(self, number: int, body: str) -> None:
        self._call("POST", f"/repos/{self.repo}/issues/{number}/comments", {"body": body})

    def assign(self, number: int, login: str) -> bool:
        out = self._call("POST", f"/repos/{self.repo}/issues/{number}/assignees",
                         {"assignees": [login]})
        # GitHub drops logins it can't assign without an error; check the result.
        return self.dry_run or login in [a["login"] for a in out.get("assignees", [])]

    def unassign(self, number: int, login: str) -> None:
        self._call("DELETE", f"/repos/{self.repo}/issues/{number}/assignees",
                   {"assignees": [login]})

    def add_labels(self, number: int, labels: list) -> None:
        self._call("POST", f"/repos/{self.repo}/issues/{number}/labels", {"labels": labels})

    def remove_label(self, number: int, label: str) -> None:
        self._call("DELETE", f"/repos/{self.repo}/issues/{number}/labels/"
                   f"{urllib.parse.quote(label, safe='')}")

    def search_count(self, query: str) -> int:
        q = urllib.parse.quote(f"repo:{self.repo} {query}")
        return int(self._call("GET", f"/search/issues?q={q}&per_page=1")["total_count"])

    def search(self, query: str) -> list:
        q = urllib.parse.quote(f"repo:{self.repo} {query}")
        return self._call("GET", f"/search/issues?q={q}&per_page=30").get("items", [])

    def open_pulls(self) -> list:
        return list(self._pages(f"/repos/{self.repo}/pulls?state=open"))

    def branch_commits(self, branch: str, since: str) -> list:
        """Commits on a branch since an ISO time: [{sha, message}]."""
        q = urllib.parse.urlencode({"sha": branch, "since": since})
        return [{"sha": c["sha"], "message": c["commit"]["message"]}
                for c in self._pages(f"/repos/{self.repo}/commits?{q}")]

    def merged_pulls(self, branch: str, since_day: str) -> list:
        """Pull requests merged into a branch since a date: [{number, body}]."""
        return [{"number": i["number"], "body": i.get("body") or ""}
                for i in self.search(f"is:pr is:merged base:{branch} merged:>={since_day}")]

    def close_issue(self, number: int) -> None:
        self._call("PATCH", f"/repos/{self.repo}/issues/{number}",
                   {"state": "closed", "state_reason": "completed"})

    def pull_files(self, number: int) -> list:
        return [f["filename"] for f in self._pages(f"/repos/{self.repo}/pulls/{number}/files")]

    def assigned_at(self, number: int, login: str) -> str | None:
        """When this person was last assigned to the issue (an ISO time), by
        the bot or by hand. None when the issue's events show no assignment."""
        times = [e["created_at"] for e in self._pages(f"/repos/{self.repo}/issues/{number}/events")
                 if e.get("event") == "assigned" and (e.get("assignee") or {}).get("login") == login]
        return max(times) if times else None

    def linked_open_pr_authors(self, number: int) -> list:
        owner, name = self.repo.split("/")
        data = self.graphql("""query($o: String!, $n: String!, $num: Int!) {
            repository(owner: $o, name: $n) { issue(number: $num) {
              closedByPullRequestsReferences(first: 20, includeClosedPrs: false) {
                nodes { author { login } } } } } }""", o=owner, n=name, num=number)
        nodes = data["repository"]["issue"]["closedByPullRequestsReferences"]["nodes"]
        return [n["author"]["login"] for n in nodes if n.get("author")]

    # project board
    def project(self, cfg: dict) -> dict:
        if self._project is None:
            data = self.graphql("""query($o: String!, $n: Int!) { organization(login: $o) {
                projectV2(number: $n) { id field(name: "Status") {
                  ... on ProjectV2SingleSelectField { id options { id name } } } } } }""",
                                o=cfg["project"]["owner"], n=int(cfg["project"]["number"]))
            p = data["organization"]["projectV2"]
            self._project = {"id": p["id"], "status_field": p["field"]["id"],
                             "options": {o["name"]: o["id"] for o in p["field"]["options"]}}
        return self._project

    def item(self, cfg: dict, number: int) -> dict | None:
        """This issue's card on the board: {id, status, shepherd}, or None."""
        owner, name = self.repo.split("/")
        data = self.graphql("""query($o: String!, $n: String!, $num: Int!) {
            repository(owner: $o, name: $n) { issue(number: $num) { projectItems(first: 20) {
              nodes { id project { number owner { ... on Organization { login } } }
                status: fieldValueByName(name: "Status") {
                  ... on ProjectV2ItemFieldSingleSelectValue { name } }
                shepherd: fieldValueByName(name: "Shepherd") {
                  ... on ProjectV2ItemFieldTextValue { text } } } } } } }""",
                            o=owner, n=name, num=number)
        for node in data["repository"]["issue"]["projectItems"]["nodes"]:
            proj = node["project"]
            if (proj["number"] == int(cfg["project"]["number"])
                    and proj["owner"].get("login") == cfg["project"]["owner"]):
                return {"id": node["id"],
                        "status": (node.get("status") or {}).get("name"),
                        "shepherd": ((node.get("shepherd") or {}).get("text") or "").strip()}
        return None

    def set_status(self, cfg: dict, item_id: str, status: str) -> None:
        p = self.project(cfg)
        self.graphql("""mutation($p: ID!, $i: ID!, $f: ID!, $o: String!) {
            updateProjectV2ItemFieldValue(input: {projectId: $p, itemId: $i, fieldId: $f,
              value: {singleSelectOptionId: $o}}) { projectV2Item { id } } }""",
                     write=True, p=p["id"], i=item_id, f=p["status_field"], o=p["options"][status])

    def items(self, cfg: dict) -> Iterable[dict]:
        """Every issue card on the board from this repo: {number, state,
        status, labels, assignees}."""
        cursor = None
        while True:
            data = self.graphql("""query($o: String!, $n: Int!, $c: String) { organization(login: $o) {
                projectV2(number: $n) { items(first: 100, after: $c) {
                  pageInfo { hasNextPage endCursor }
                  nodes { status: fieldValueByName(name: "Status") {
                      ... on ProjectV2ItemFieldSingleSelectValue { name } }
                    content { ... on Issue { number state repository { nameWithOwner }
                      labels(first: 50) { nodes { name } }
                      assignees(first: 10) { nodes { login } } } } } } } } }""",
                                o=cfg["project"]["owner"], n=int(cfg["project"]["number"]), c=cursor)
            page = data["organization"]["projectV2"]["items"]
            for node in page["nodes"]:
                c = node.get("content") or {}
                if c.get("repository", {}).get("nameWithOwner") != self.repo:
                    continue
                yield {"number": c["number"], "state": c["state"].lower(),
                       "status": (node.get("status") or {}).get("name"),
                       "labels": [x["name"] for x in c["labels"]["nodes"]],
                       "assignees": [x["login"] for x in c["assignees"]["nodes"]]}
            if not page["pageInfo"]["hasNextPage"]:
                return
            cursor = page["pageInfo"]["endCursor"]


# --- the bot ------------------------------------------------------------------


def closed_by(text: str) -> list:
    """The issues that a commit message or the text of a pull request closes.

    A closing keyword counts only where it starts a line, outside fenced
    blocks. So a sentence that quotes the closing line of another change
    closes nothing, in code marks or not, and neither does a quoted line
    (`> Closes #7`) or a line that starts with a code mark."""
    numbers: list = []
    fenced = False
    for line in (text or "").splitlines():
        if FENCE.match(line):
            fenced = not fenced
        elif not fenced:
            found = CLOSING_LINE.match(line)
            if found:
                numbers += [int(n) for n in CLOSING.findall(found.group(0))]
    return numbers


def marker(kind: str, login: str = "") -> str:
    return f"<!-- atlas-bot:{kind}{' @' + login if login else ''} -->"


def parse_time(stamp: str) -> dt.datetime:
    return dt.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc)


def keywords(title: str, limit: int = 5) -> list:
    words = re.findall(r"[a-z0-9][a-z0-9+.#-]*[a-z0-9+#]|[a-z0-9]", title.lower())
    seen: list = []
    for w in sorted(words, key=len, reverse=True):
        if len(w) >= 4 and w not in STOPWORDS and w not in seen:
            seen.append(w)
    return seen[:limit]


class Bot:
    def __init__(self, api: API, cfg: dict, now: dt.datetime | None = None) -> None:
        self.api, self.cfg = api, cfg
        self.now = now or dt.datetime.now(dt.timezone.utc)
        self.bot_login = cfg.get("bot", {}).get("login", "")

    def _mine(self, c: dict) -> bool:
        """A comment the bot wrote. Markers are only trusted from the bot,
        so a user can't forge a claim time by pasting one."""
        user = c.get("user") or {}
        if self.bot_login:
            return user.get("login") == self.bot_login
        return user.get("type") == "Bot"

    # /claim and /unclaim
    def claim(self, event: dict) -> None:
        issue, comment = event["issue"], event["comment"]
        if "pull_request" in issue or (comment.get("user") or {}).get("type") == "Bot":
            return
        lines = (comment.get("body") or "").strip().splitlines()
        m = COMMAND.match(lines[0].strip()) if lines else None
        if not m:
            return
        user = comment["user"]["login"]
        maintainer = comment.get("author_association") in MAINTAINER
        # Re-read the issue: the event's copy is from when the comment was
        # made, and two /claim comments close together would both see it
        # unassigned. The workflow also queues runs per issue.
        current = self.api.issue(issue["number"])
        if m.group(1) == "claim":
            self._claim(current, user, maintainer)
        else:
            self._unclaim(current, user)

    def _claim(self, issue: dict, user: str, maintainer: bool) -> None:
        n, c, links = issue["number"], self.cfg["claims"], self.cfg["links"]
        assignees = [a["login"] for a in issue.get("assignees") or []]
        if issue.get("state") != "open":
            self.api.comment(n, f"@{user} this issue is closed, so it can't be claimed.")
            return
        if user in assignees:
            self.api.comment(n, f"@{user} this issue is already yours.")
            return
        if assignees:
            self.api.comment(n, f"@{user} this issue is already claimed by "
                             f"@{assignees[0]}. Find another in [Start Here]({links['start_here']}).")
            return
        item = self.api.item(self.cfg, n)
        if item is None or item["status"] != "Ready":
            where = f"its status is {item['status']}" if item and item["status"] else "it isn't on the board yet"
            self.api.comment(n, f"@{user} only issues marked Ready can be claimed, and {where}. "
                             f"Ready issues are in [Start Here]({links['start_here']}).")
            return
        if not maintainer:
            open_claims = self.api.search_count(f"is:issue is:open assignee:{user}")
            merged = self.api.search_count(f"is:pr is:merged author:{user}")
            limit = int(c["max_open"] if merged else c["max_open_first_timer"])
            if open_claims >= limit:
                kind = "" if merged else " before your first merged pull request"
                self.api.comment(n, f"@{user} you can hold {limit} open claim{'s' if limit > 1 else ''}"
                                 f"{kind}, and you have {open_claims}. Finish or `/unclaim` one first.")
                return
        if not self.api.assign(n, user):
            self.api.comment(n, f"@{user} GitHub wouldn't assign this issue to you. A maintainer will look.")
            return
        self.api.set_status(self.cfg, item["id"], "In Progress")
        shepherd = item["shepherd"].lstrip("@")
        who = f"Your shepherd is @{shepherd}: ask them anything. " if shepherd else ""
        self.api.comment(n, f"{marker('claim', user)}\n@{user} it's yours. {who}Next:\n"
                         f"1. Fork the repo and branch from `dev`.\n"
                         f"2. Open a **draft pull request** against `dev` within {c['ping_after_days']} days, "
                         f"with `Closes #{n}` in its description.\n"
                         f"3. Without a linked pull request, the claim is released after "
                         f"{c['release_after_days']} days. Comment `/unclaim` any time to let it go.\n\n"
                         f"The steps are in [CONTRIBUTING]({links['contributing']}).")

    def _unclaim(self, issue: dict, user: str) -> None:
        n = issue["number"]
        if user not in [a["login"] for a in issue.get("assignees") or []]:
            self.api.comment(n, f"@{user} you don't hold a claim on this issue.")
            return
        self.api.unassign(n, user)
        item = self.api.item(self.cfg, n)
        if item and item["status"] == "In Progress":
            self.api.set_status(self.cfg, item["id"], "Ready")
        self.api.comment(n, f"{marker('unclaim', user)}\n@{user} released. The issue is Ready for someone else.")

    # daily
    def stale(self) -> None:
        c = self.cfg["claims"]
        # GitHub links a pull request to an issue ("Closes #N") only when the
        # pull request targets main, and ours target dev. So an open pull
        # request by the claimant that names the issue also keeps the claim.
        mentions: dict = {}
        for pr in self.api.open_pulls():
            text = f"{pr.get('title') or ''}\n{pr.get('body') or ''}"
            for num in set(ISSUE_REF.findall(text)):
                mentions.setdefault(int(num), set()).add((pr.get("user") or {}).get("login"))
        for it in self.api.items(self.cfg):
            if it["state"] != "open" or it["status"] != "In Progress" or not it["assignees"]:
                continue
            n = it["number"]
            comments = [x for x in self.api.comments(n) if self._mine(x)]
            for login in it["assignees"]:
                claims = [x for x in comments if marker("claim", login) in x.get("body", "")]
                if not claims:
                    continue  # assigned by hand, not by /claim: not the bot's to release
                since = parse_time(claims[-1]["created_at"])
                if (self.now - since).days < int(c["ping_after_days"]):
                    continue
                if login in mentions.get(n, ()) or login in self.api.linked_open_pr_authors(n):
                    continue
                # A maintainer who assigns the claimant again, after a release
                # or to give more time, starts the count again: the days run
                # from the newest of the claim and the assignment.
                assigned = self.api.assigned_at(n, login)
                since = max(since, parse_time(assigned)) if assigned else since
                days = (self.now - since).days
                if days >= int(c["release_after_days"]):
                    self.api.unassign(n, login)
                    item = self.api.item(self.cfg, n)
                    if item:
                        self.api.set_status(self.cfg, item["id"], "Ready")
                    self.api.comment(n, f"{marker('release', login)}\n@{login} this claim is released: "
                                     f"no linked pull request after {c['release_after_days']} days. "
                                     f"The issue is Ready again. You can `/claim` it again if it's still free.")
                elif days >= int(c["ping_after_days"]):
                    pinged = any(marker("ping", login) in x.get("body", "")
                                 and parse_time(x["created_at"]) > since for x in comments)
                    if not pinged:
                        left = int(c["release_after_days"]) - days
                        self.api.comment(n, f"{marker('ping', login)}\n@{login} how is it going? Open a draft "
                                         f"pull request with `Closes #{n}` to keep this claim. Without one, "
                                         f"it's released in {left} day{'s' if left != 1 else ''}. "
                                         "Stuck? Ask here, or `/unclaim`.")

    # hourly
    def sync(self) -> None:
        for it in self.api.items(self.cfg):
            want = {STATUS_LABELS[it["status"]]} if it["state"] == "open" and it["status"] in STATUS_LABELS else set()
            have = set(it["labels"]) & set(STATUS_LABELS.values())
            if want - have:
                self.api.add_labels(it["number"], sorted(want - have))
            for label in sorted(have - want):
                self.api.remove_label(it["number"], label)
        areas = self.cfg.get("areas", {})
        for pr in self.api.open_pulls():
            n = pr["number"]
            files = self.api.pull_files(n)
            want = {label for prefix, label in areas.items() if any(f.startswith(prefix) for f in files)}
            have = {x["name"] for x in pr.get("labels") or []}
            if want - have:
                self.api.add_labels(n, sorted(want - have))
            if self._first_pr(pr):
                if not any(self._mine(x) and marker("welcome") in x.get("body", "")
                           for x in self.api.comments(n)):
                    self.api.comment(n, self._welcome_pr(pr["user"]["login"]))
        self._close_fixed()

    def _close_fixed(self) -> None:
        """Close the open issues that a commit or merged pull request on the
        work branch names in a closing line (see closed_by). The board then marks
        them Done by itself, and the milestone names the release that ships them."""
        d = self.cfg["done"]
        since = self.now - dt.timedelta(days=int(d["lookback_days"]))
        where: dict = {}
        for c in self.api.branch_commits(d["branch"], since.strftime("%Y-%m-%dT%H:%M:%SZ")):
            for num in closed_by(c["message"]):
                where.setdefault(num, c["sha"][:7])
        for pr in self.api.merged_pulls(d["branch"], since.strftime("%Y-%m-%d")):
            for num in closed_by(pr["body"]):
                where.setdefault(num, f"#{pr['number']}")
        for n, ref in sorted(where.items()):
            try:
                issue = self.api.issue(n)
            except RuntimeError:  # no such issue: a typo in a commit message
                continue
            if "pull_request" in issue or issue.get("state") != "open":
                continue
            if not any(self._mine(x) and marker("done") in x.get("body", "")
                       for x in self.api.comments(n)):
                self.api.comment(n, f"{marker('done')}\nFixed on `{d['branch']}` in {ref}. "
                                    "It ships in the next release.")
            self.api.close_issue(n)

    def _first_pr(self, pr: dict) -> bool:
        # author_association is not enough: under the bot's app token GitHub
        # reported a first-time contributor's PR (#246) as something else, so
        # also count the author's pull requests, as the issue welcome does.
        user = pr.get("user") or {}
        if user.get("type") == "Bot" or not self._recent(pr["created_at"], days=7):
            return False
        assoc = pr.get("author_association")
        if assoc in FIRST_PR:
            return True
        if assoc != "NONE":  # CONTRIBUTOR, COLLABORATOR, MEMBER, OWNER: not new
            return False
        return self.api.search_count(f"is:pr author:{user.get('login')}") <= 1

    def _recent(self, stamp: str, days: int) -> bool:
        return (self.now - parse_time(stamp)).days < days

    def _welcome_pr(self, login: str) -> str:
        links = self.cfg["links"]
        return (f"{marker('welcome')}\nThanks for your first pull request to ATLAS, @{login}! "
                "A maintainer replies within 5 business days. Meanwhile: the title must look like "
                "`type(scope): summary`, the description should say `Closes #<issue>`, and "
                f"CI must pass. The rest is in [CONTRIBUTING]({links['contributing']}).")

    # issue opened
    def welcome(self, event: dict) -> None:
        issue = event["issue"]
        user = issue["user"]
        if user.get("type") == "Bot" or issue.get("author_association") in MAINTAINER + ("COLLABORATOR",):
            return
        if self.api.search_count(f"is:issue author:{user['login']}") > 1:
            return
        links = self.cfg["links"]
        self.api.comment(issue["number"], f"{marker('welcome')}\nThanks for your first issue, "
                         f"@{user['login']}! A maintainer triages new issues. Want to write code too? "
                         f"Pick a Ready issue from [Start Here]({links['start_here']}) and comment "
                         f"`/claim`. How it works: [CONTRIBUTING]({links['contributing']}).")

    def rfc_dedupe(self, event: dict) -> None:
        n = event["issue"]["number"]
        issue = self.api.issue(n)
        kind = (issue.get("type") or {}).get("name")
        if kind not in ("RFC", "Feature"):
            return
        words = keywords(issue.get("title") or "")
        if not words:
            return
        hits: dict = {}
        for t in ("RFC", "Epic"):
            for found in self.api.search(f'is:issue is:open type:"{t}" in:title {" OR ".join(words)}'):
                if found["number"] == n:
                    continue
                title = found.get("title") or ""
                score = sum(1 for w in words if w in title.lower())
                if score:
                    hits[found["number"]] = (score, t, found)
        if not hits:
            return
        best = sorted(hits.values(), key=lambda h: (-h[0], h[2]["number"]))
        lines = []
        for _score, t, found in best[: int(self.cfg["rfc_dedupe"]["max_links"])]:
            owners = ", ".join(f"@{a['login']}" for a in found.get("assignees") or []) or "unassigned"
            lines.append(f"- #{found['number']} {found['title']} ({t}, {owners})")
        self.api.comment(n, f"{marker('rfc-dedupe')}\nThese open RFCs and Epics look related. "
                         "If one already covers this, say so here and a maintainer will link or close:\n"
                         + "\n".join(lines))


def main(argv: list) -> int:
    commands = ("claim", "stale", "sync", "welcome", "rfc-dedupe")
    if len(argv) != 2 or argv[1] not in commands:
        print(f"usage: atlas_bot.py {{{'|'.join(commands)}}}", file=sys.stderr)
        return 2
    cmd = argv[1]
    api = API(os.environ["GH_TOKEN"], os.environ["GITHUB_REPOSITORY"],
              dry_run=os.environ.get("ATLAS_BOT_DRY_RUN") == "1")
    bot = Bot(api, load_config())
    if cmd in ("claim", "welcome", "rfc-dedupe"):
        with open(os.environ["GITHUB_EVENT_PATH"], encoding="utf-8") as fh:
            event = json.load(fh)
        getattr(bot, cmd.replace("-", "_"))(event)
    else:
        getattr(bot, cmd)()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
