"""atlas-bot decisions, against a fake GitHub.

The bot's GitHub calls go through one small API class; FakeAPI keeps issues,
comments and board cards in memory, so every rule in .github/atlas-bot.yml
(claim limits, reminder and release days, label mirroring, area labels,
welcomes, RFC links) is checked without the network.
"""

import datetime as dt
import importlib.util
import os

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
_spec = importlib.util.spec_from_file_location("atlas_bot", os.path.join(ROOT, "scripts", "bot", "atlas_bot.py"))
bot_mod = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(bot_mod)

NOW = dt.datetime(2026, 10, 1, 12, 0, tzinfo=dt.timezone.utc)
BOT = "inferstep-atlas-bot[bot]"


def stamp(days_ago: float) -> str:
    return (NOW - dt.timedelta(days=days_ago)).strftime("%Y-%m-%dT%H:%M:%SZ")


class FakeAPI:
    def __init__(self):
        self.issues = {}      # number -> {state, assignees, labels, type, title}
        self.cards = {}       # number -> {id, status, shepherd}
        self.threads = {}     # number -> [comment]
        self.counts = {}      # search query -> count
        self.results = {}     # search query -> [issue]
        self.pulls = []       # open PRs
        self.files = {}       # PR number -> [path]
        self.linked = {}      # issue number -> [PR author]
        self.assigned = {}    # (issue number, login) -> when they were last assigned
        self.commits = []     # commits on dev: {sha, message}
        self.merged = []      # PRs merged into dev: {number, body}
        self.assignable = True
        self.log = []

    def issue(self, n):
        if n not in self.issues:
            raise RuntimeError(f"GET /issues/{n}: HTTP 404")
        i = self.issues[n]
        return {"number": n, **i, "assignees": [{"login": x} for x in i["assignees"]]}

    def comments(self, n):
        return list(self.threads.get(n, []))

    def comment(self, n, body):
        self.log.append(("comment", n, body))
        self.threads.setdefault(n, []).append(
            {"user": {"login": BOT, "type": "Bot"}, "body": body, "created_at": stamp(0)})

    def assign(self, n, login):
        self.log.append(("assign", n, login))
        if self.assignable:
            self.issues[n]["assignees"].append(login)
        return self.assignable

    def unassign(self, n, login):
        self.log.append(("unassign", n, login))
        self.issues[n]["assignees"].remove(login)

    def add_labels(self, n, labels):
        self.log.append(("add_labels", n, tuple(labels)))

    def remove_label(self, n, label):
        self.log.append(("remove_label", n, label))

    def search_count(self, q):
        return self.counts.get(q, 0)

    def search(self, q):
        return self.results.get(q, [])

    def open_pulls(self):
        # As GitHub answers `state=open`: a closed or merged pull request is not in the list.
        return [p for p in self.pulls if p.get("state", "open") == "open"]

    def pull_files(self, n):
        return self.files.get(n, [])

    def branch_commits(self, branch, since):
        assert branch == "dev"
        return self.commits

    def merged_pulls(self, branch, since_day):
        assert branch == "dev"
        return self.merged

    def close_issue(self, n):
        self.log.append(("close", n, None))
        self.issues[n]["state"] = "closed"

    def linked_open_pr_authors(self, n):
        return self.linked.get(n, [])

    def assigned_at(self, n, login):
        return self.assigned.get((n, login))

    def item(self, cfg, n):
        return self.cards.get(n)

    def set_status(self, cfg, item_id, status):
        self.log.append(("status", item_id, status))
        for card in self.cards.values():
            if card["id"] == item_id:
                card["status"] = status

    def items(self, cfg):
        for n, card in self.cards.items():
            i = self.issues[n]
            yield {"number": n, "state": i["state"], "status": card["status"],
                   "labels": list(i["labels"]), "assignees": list(i["assignees"])}

    def said(self, n):
        return [b for (kind, num, b) in self.log if kind == "comment" and num == n]


@pytest.fixture
def cfg():
    return bot_mod.load_config()


@pytest.fixture
def api():
    a = FakeAPI()
    a.issues[7] = {"state": "open", "assignees": [], "labels": [], "type": None, "title": "t"}
    a.cards[7] = {"id": "ITEM7", "status": "Ready", "shepherd": "@maint"}
    return a


def run(api, cfg, now=NOW):
    return bot_mod.Bot(api, cfg, now)


def claim_event(api, n, body, user="alice", assoc="NONE", pr=False):
    issue = {"number": n, "state": api.issues[n]["state"],
             "assignees": [{"login": x} for x in api.issues[n]["assignees"]]}
    if pr:
        issue["pull_request"] = {}
    return {"issue": issue, "comment": {"body": body, "user": {"login": user, "type": "User"},
                                        "author_association": assoc}}


# --- settings ---------------------------------------------------------------


def test_config_reads_the_repo_settings(cfg):
    assert cfg["claims"] == {"ping_after_days": 10, "release_after_days": 14,
                             "max_open": 2, "max_open_first_timer": 1}
    assert cfg["project"] == {"owner": "inferstep", "number": 1}
    assert cfg["bot"]["login"] == BOT
    assert cfg["links"]["start_here"].startswith("https://github.com/orgs/inferstep/projects/1/")
    assert cfg["areas"]["proxy/"] == "area/proxy" and cfg["areas"][".github/"] == "area/ci"


def test_every_area_label_is_one_the_label_script_creates(cfg):
    with open(os.path.join(ROOT, "scripts", "setup", "labels.sh"), encoding="utf-8") as fh:
        script = fh.read()
    for label in set(cfg["areas"].values()):
        assert f'"{label}|' in script, label


# --- /claim -----------------------------------------------------------------


def test_claim_a_ready_issue(api, cfg):
    api.counts["is:pr is:merged author:alice"] = 1
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert ("assign", 7, "alice") in api.log
    assert ("status", "ITEM7", "In Progress") in api.log
    reply = api.said(7)[-1]
    assert bot_mod.marker("claim", "alice") in reply and "@maint" in reply and "Closes #7" in reply


@pytest.mark.parametrize("body", ["/claim please", "I'll /claim this", "/claimed", ""])
def test_only_an_exact_first_line_is_a_command(api, cfg, body):
    run(api, cfg).claim(claim_event(api, 7, body))
    assert api.log == []


def test_claim_ignores_pull_request_comments(api, cfg):
    run(api, cfg).claim(claim_event(api, 7, "/claim", pr=True))
    assert api.log == []


@pytest.mark.parametrize("status", ["Triage", "Backlog", "In Progress", None])
def test_only_ready_issues_can_be_claimed(api, cfg, status):
    api.cards[7]["status"] = status
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert not [x for x in api.log if x[0] in ("assign", "status")]
    assert "only issues marked Ready" in api.said(7)[-1]


def test_an_issue_off_the_board_cannot_be_claimed(api, cfg):
    del api.cards[7]
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert "isn't on the board" in api.said(7)[-1]


def test_one_claimant_per_issue(api, cfg):
    api.issues[7]["assignees"] = ["bob"]
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert "already claimed by @bob" in api.said(7)[-1]
    assert not [x for x in api.log if x[0] == "assign"]


@pytest.mark.parametrize("merged,open_claims,allowed", [(0, 0, True), (0, 1, False), (3, 1, True), (3, 2, False)])
def test_claim_limits(api, cfg, merged, open_claims, allowed):
    api.counts["is:pr is:merged author:alice"] = merged
    api.counts["is:issue is:open assignee:alice"] = open_claims
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert (("assign", 7, "alice") in api.log) is allowed


def test_maintainers_skip_the_limits(api, cfg):
    api.counts["is:issue is:open assignee:isaac"] = 9
    run(api, cfg).claim(claim_event(api, 7, "/claim", user="isaac", assoc="OWNER"))
    assert ("assign", 7, "isaac") in api.log


def test_a_failed_assignment_does_not_move_the_card(api, cfg):
    api.assignable = False
    run(api, cfg).claim(claim_event(api, 7, "/claim"))
    assert not [x for x in api.log if x[0] == "status"]
    assert "wouldn't assign" in api.said(7)[-1]


def test_unclaim_returns_the_issue_to_ready(api, cfg):
    api.issues[7]["assignees"] = ["alice"]
    api.cards[7]["status"] = "In Progress"
    run(api, cfg).claim(claim_event(api, 7, "/unclaim"))
    assert ("unassign", 7, "alice") in api.log and ("status", "ITEM7", "Ready") in api.log


def test_claim_reads_the_current_assignee_not_the_event_copy(api, cfg):
    event = claim_event(api, 7, "/claim")  # made while unassigned
    api.issues[7]["assignees"] = ["bob"]   # bob's /claim ran first
    run(api, cfg).claim(event)
    assert not [x for x in api.log if x[0] == "assign"]
    assert "already claimed by @bob" in api.said(7)[-1]


def test_unclaim_by_someone_else_changes_nothing(api, cfg):
    api.issues[7]["assignees"] = ["alice"]
    run(api, cfg).claim(claim_event(api, 7, "/unclaim", user="mallory"))
    assert not [x for x in api.log if x[0] in ("unassign", "status")]


# --- stale claims -----------------------------------------------------------


def claimed(api, days_ago, login="alice", by=BOT):
    api.issues[7]["assignees"] = [login]
    api.cards[7]["status"] = "In Progress"
    api.threads[7] = [{"user": {"login": by, "type": "Bot" if by == BOT else "User"},
                       "body": bot_mod.marker("claim", login), "created_at": stamp(days_ago)}]


def test_contributing_states_the_periods_of_the_settings(cfg):
    with open(os.path.join(ROOT, "CONTRIBUTING.md"), encoding="utf-8") as fh:
        text = " ".join(fh.read().split())
    assert f"Open a pull request within **{cfg['claims']['ping_after_days']} days**" in text
    assert f"After **{cfg['claims']['release_after_days']} days** without one, the claim is released" in text


@pytest.mark.parametrize("days,action", [(9, None), (10, "ping"), (13, "ping"), (14, "release"), (30, "release")])
def test_reminder_then_release(api, cfg, days, action):
    claimed(api, days)
    run(api, cfg).stale()
    said = " ".join(api.said(7))
    assert (bot_mod.marker("ping", "alice") in said) is (action == "ping")
    assert (("unassign", 7, "alice") in api.log) is (action == "release")
    if action == "release":
        assert ("status", "ITEM7", "Ready") in api.log


def test_one_reminder_per_claim(api, cfg):
    claimed(api, 10)
    run(api, cfg).stale()
    run(api, cfg).stale()
    assert len([b for b in api.said(7) if bot_mod.marker("ping", "alice") in b]) == 1


def test_a_linked_pull_request_keeps_the_claim(api, cfg):
    claimed(api, 30)
    api.linked[7] = ["alice"]
    run(api, cfg).stale()
    assert api.log == []


@pytest.mark.parametrize("author,text,kept", [
    ("alice", "Closes #7", True),              # PR into dev: GitHub makes no link, the mention counts
    ("alice", "Refs #7", True),                # the issue stays open after the merge; the claim is still worked on
    ("alice", "Fixes #7", True),
    ("alice", "Resolves #7", True),
    ("alice", "docs: resync (issue #7)", True),
    ("bob", "Closes #7", False),               # someone else's PR does not keep alice's claim
    ("alice", "Closes #70", False),            # another issue
    ("alice", "see inferstep/ATLAS#7", False),  # a qualified ref is not taken for this repo's #7
])
def test_an_open_pull_request_that_names_the_issue_keeps_the_claim(api, cfg, author, text, kept):
    claimed(api, 30)
    api.pulls = [{"number": 246, "title": "docs(zh-cn): resync", "body": text, "labels": [],
                  "author_association": "CONTRIBUTOR", "created_at": stamp(2),
                  "user": {"login": author, "type": "User"}}]
    run(api, cfg).stale()
    assert (("unassign", 7, "alice") in api.log) is (not kept)


def test_a_claim_with_a_draft_pull_request_into_dev_gets_no_reminder_and_no_release(api, cfg):
    # The claim, a draft pull request into dev 13 hours later, and the daily run 15 days after the claim.
    claimed(api, 15)
    api.pulls = [{"number": 272, "title": "docs(zh-cn): translate the page", "body": "Closes #7", "labels": [],
                  "draft": True, "base": {"ref": "dev"}, "author_association": "CONTRIBUTOR",
                  "created_at": stamp(15 - 13 / 24), "user": {"login": "alice", "type": "User"}}]
    run(api, cfg).stale()
    assert api.log == []
    api.pulls = []
    run(api, cfg).stale()
    assert ("unassign", 7, "alice") in api.log


@pytest.mark.parametrize("state,kept", [("open", True), ("closed", False)])
def test_a_closed_pull_request_does_not_keep_the_claim(api, cfg, state, kept):
    claimed(api, 30)
    api.pulls = [{"number": 272, "title": "t", "body": "Closes #7", "labels": [], "state": state,
                  "user": {"login": "alice", "type": "User"}}]
    run(api, cfg).stale()
    assert (("unassign", 7, "alice") in api.log) is (not kept)


@pytest.mark.parametrize("assigned_days_ago,action", [(3, None), (9, None), (10, "ping"), (13, "ping"), (14, "release")])
def test_a_hand_assignment_after_the_claim_starts_the_count_again(api, cfg, assigned_days_ago, action):
    # Claimed 30 days ago and reminded on day 10; a maintainer assigned the claimant again later.
    claimed(api, 30)
    api.threads[7].append({"user": {"login": BOT, "type": "Bot"}, "body": bot_mod.marker("ping", "alice"),
                           "created_at": stamp(20)})
    api.assigned[(7, "alice")] = stamp(assigned_days_ago)
    run(api, cfg).stale()
    said = " ".join(api.said(7))
    assert (bot_mod.marker("ping", "alice") in said) is (action == "ping")
    assert (("unassign", 7, "alice") in api.log) is (action == "release")


def test_an_assignment_older_than_the_claim_does_not_move_the_count_back(api, cfg):
    claimed(api, 10)
    api.assigned[(7, "alice")] = stamp(40)
    run(api, cfg).stale()
    assert bot_mod.marker("ping", "alice") in " ".join(api.said(7))


def test_the_events_are_not_asked_for_a_claim_that_is_young_or_has_a_pull_request(api, cfg):
    asked = []
    api.assigned_at = lambda n, login: asked.append((n, login))
    claimed(api, 9)
    run(api, cfg).stale()
    claimed(api, 30)
    api.pulls = [{"number": 9, "title": "t", "body": "Refs #7", "labels": [], "user": {"login": "alice", "type": "User"}}]
    run(api, cfg).stale()
    assert asked == [] and api.log == []


class Recorded(bot_mod.API):
    """The real API class with the network replaced: it records each path and answers from a table."""

    def __init__(self, answers):
        super().__init__("token", "inferstep/ATLAS")
        self.answers, self.paths = answers, []

    def _call(self, method, path, body=None, mutate=None):
        self.paths.append((method, path))
        return self.answers.get(path.split("?")[0], [])


def test_only_open_pull_requests_are_asked_for():
    api = Recorded({"/repos/inferstep/ATLAS/pulls": [{"number": 272}]})
    assert api.open_pulls() == [{"number": 272}]
    assert api.paths == [("GET", "/repos/inferstep/ATLAS/pulls?state=open&per_page=100&page=1")]


def test_the_newest_assignment_of_that_person_is_read_from_the_issues_events():
    events = [{"event": "assigned", "assignee": {"login": "alice"}, "created_at": "2026-09-29T08:00:00Z"},
              {"event": "unassigned", "assignee": {"login": "alice"}, "created_at": "2026-10-06T06:00:00Z"},
              {"event": "assigned", "assignee": {"login": "alice"}, "created_at": "2026-10-06T15:00:00Z"},
              {"event": "unassigned", "assignee": {"login": "alice"}, "created_at": "2026-10-07T08:00:00Z"},
              {"event": "assigned", "assignee": {"login": "bob"}, "created_at": "2026-10-07T09:00:00Z"},
              {"event": "labeled", "created_at": "2026-10-07T10:00:00Z"}]
    api = Recorded({"/repos/inferstep/ATLAS/issues/253/events": events})
    assert api.assigned_at(253, "alice") == "2026-10-06T15:00:00Z"
    assert api.assigned_at(253, "carol") is None
    assert api.paths[0] == ("GET", "/repos/inferstep/ATLAS/issues/253/events?per_page=100&page=1")


def test_hand_assignments_are_not_released(api, cfg):
    api.issues[7]["assignees"] = ["alice"]
    api.cards[7]["status"] = "In Progress"
    run(api, cfg).stale()
    assert api.log == []


def test_a_forged_claim_marker_is_ignored(api, cfg):
    claimed(api, 30, by="mallory")
    run(api, cfg).stale()
    assert api.log == []


# --- hourly sync ------------------------------------------------------------


def test_ready_and_blocked_are_mirrored_to_labels(api, cfg):
    api.issues[8] = {"state": "open", "assignees": [], "labels": ["status/ready"], "type": None, "title": "t"}
    api.cards[8] = {"id": "ITEM8", "status": "Blocked", "shepherd": ""}
    api.issues[9] = {"state": "closed", "assignees": [], "labels": ["status/blocked"], "type": None, "title": "t"}
    api.cards[9] = {"id": "ITEM9", "status": "Blocked", "shepherd": ""}
    run(api, cfg).sync()
    assert ("add_labels", 7, ("status/ready",)) in api.log
    assert ("add_labels", 8, ("status/blocked",)) in api.log and ("remove_label", 8, "status/ready") in api.log
    assert ("remove_label", 9, "status/blocked") in api.log


def test_area_labels_from_changed_paths(api, cfg):
    api.cards.clear()
    api.pulls = [{"number": 40, "labels": [{"name": "area/tui"}], "author_association": "CONTRIBUTOR",
                  "created_at": stamp(1), "user": {"login": "bob"}}]
    api.files[40] = ["proxy/agent.go", "tui/main.go", "docs/SETUP.md", "docker-compose.rocm.yml"]
    run(api, cfg).sync()
    assert api.log == [("add_labels", 40, ("area/docs", "area/install", "area/proxy"))]


def test_first_pull_requests_are_welcomed_once(api, cfg):
    api.cards.clear()
    api.pulls = [{"number": 41, "labels": [], "author_association": "FIRST_TIME_CONTRIBUTOR",
                  "created_at": stamp(1), "user": {"login": "newbie"}},
                 {"number": 42, "labels": [], "author_association": "FIRST_TIME_CONTRIBUTOR",
                  "created_at": stamp(30), "user": {"login": "old"}}]
    run(api, cfg).sync()
    run(api, cfg).sync()
    assert len(api.said(41)) == 1 and "first pull request" in api.said(41)[0]
    assert api.said(42) == []


@pytest.mark.parametrize("assoc,prs,user_type,welcomed", [
    ("NONE", 1, "User", True),          # first PR, association not reported as first-time
    ("CONTRIBUTOR", 1, "User", False),  # has contributed before
    ("NONE", 3, "User", False),         # returning author
    ("MEMBER", 1, "User", False),
    ("COLLABORATOR", 1, "User", False),
    ("NONE", 1, "Bot", False),          # Dependabot and other apps
])
def test_first_pr_is_found_by_counting_the_authors_prs(api, cfg, assoc, prs, user_type, welcomed):
    api.cards.clear()
    api.pulls = [{"number": 43, "labels": [], "author_association": assoc,
                  "created_at": stamp(1), "user": {"login": "newbie", "type": user_type}}]
    api.counts["is:pr author:newbie"] = prs
    run(api, cfg).sync()
    assert bool(api.said(43)) is welcomed


# --- welcome and RFC links --------------------------------------------------


def issue_event(n, login="newbie", assoc="NONE"):
    return {"issue": {"number": n, "user": {"login": login, "type": "User"}, "author_association": assoc}}


def test_first_issue_is_welcomed(api, cfg):
    api.counts["is:issue author:newbie"] = 1
    run(api, cfg).welcome(issue_event(7))
    assert "first issue" in api.said(7)[0]


@pytest.mark.parametrize("count,assoc", [(2, "NONE"), (1, "MEMBER"), (1, "COLLABORATOR")])
def test_returning_authors_and_members_are_not_welcomed(api, cfg, count, assoc):
    api.counts["is:issue author:newbie"] = count
    run(api, cfg).welcome(issue_event(7, assoc=assoc))
    assert api.said(7) == []


def test_keywords_drop_short_and_common_words():
    assert bot_mod.keywords("Add support for ROCm multi-GPU inference with llama.cpp") == \
        ["multi-gpu", "inference", "llama.cpp", "rocm"]


def test_rfc_links_related_open_rfcs_and_epics(api, cfg):
    api.issues[50] = {"state": "open", "assignees": [], "labels": [], "type": {"name": "RFC"},
                      "title": "Multi-GPU inference for larger models"}
    words = bot_mod.keywords(api.issues[50]["title"])
    q = " OR ".join(words)
    api.results[f'is:issue is:open type:"RFC" in:title {q}'] = [
        {"number": 50, "title": "Multi-GPU inference for larger models", "assignees": []},
        {"number": 34, "title": "Multi-GPU for larger models", "assignees": [{"login": "maint"}]}]
    api.results[f'is:issue is:open type:"Epic" in:title {q}'] = [
        {"number": 12, "title": "Unrelated epic", "assignees": []}]
    run(api, cfg).rfc_dedupe({"issue": {"number": 50}})
    reply = api.said(50)[0]
    assert "#34" in reply and "@maint" in reply and "#50" not in reply and "#12" not in reply


@pytest.mark.parametrize("kind", ["Bug", "Task", None])
def test_rfc_links_only_for_rfcs_and_features(api, cfg, kind):
    api.issues[50] = {"state": "open", "assignees": [], "labels": [],
                      "type": {"name": kind} if kind else None, "title": "Multi-GPU inference"}
    run(api, cfg).rfc_dedupe({"issue": {"number": 50}})
    assert api.said(50) == []


# --- close issues whose fix reached dev --------------------------------------


def closed(api):
    return [n for (kind, n, *_rest) in api.log if kind == "close"]


def test_a_commit_on_dev_closes_the_issue_it_names(api, cfg):
    api.commits = [{"sha": "d834340abc", "message": "fix(proxy): x\n\nCloses #7"}]
    run(api, cfg).sync()
    assert closed(api) == [7]
    assert "Fixed on `dev` in d834340" in api.said(7)[0]


def test_a_merged_pull_request_closes_the_issue_it_names(api, cfg):
    api.merged = [{"number": 246, "body": "Fixes #7\n\n## What changed"}]
    run(api, cfg).sync()
    assert closed(api) == [7] and "in #246" in api.said(7)[0]


def test_closing_twice_is_one_comment_and_one_close(api, cfg):
    api.commits = [{"sha": "aaaaaaa1", "message": "Closes #7"}, {"sha": "bbbbbbb2", "message": "resolves: #7"}]
    run(api, cfg).sync()
    run(api, cfg).sync()
    assert closed(api) == [7] and len(api.said(7)) == 1


@pytest.mark.parametrize("message", ["see (#7)", "refs #7", "Prefixes #7", "closes#7"])
def test_only_closing_keywords_close(api, cfg, message):
    api.commits = [{"sha": "ccccccc3", "message": message}]
    run(api, cfg).sync()
    assert closed(api) == []


def test_pull_requests_closed_and_missing_issues_are_skipped(api, cfg):
    api.issues[8] = {"state": "closed", "assignees": [], "labels": [], "type": None, "title": "t"}
    api.issues[9] = {"state": "open", "assignees": [], "labels": [], "type": None, "title": "t",
                     "pull_request": {}}
    api.commits = [{"sha": "ddddddd4", "message": "Closes #8, closes #9, closes #999"}]
    run(api, cfg).sync()
    assert closed(api) == [] and api.said(8) == api.said(9) == []

