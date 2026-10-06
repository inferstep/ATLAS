"""scripts/setup/release_step.py pushes only a commit that passed every
required check, opens only the rule that stops the push, and always closes
it again.

GitHub is an in-memory stand-in and the push is a function, so no test
reaches GitHub or runs git.
"""

import importlib.util
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location("release_step", REPO / "scripts" / "setup" / "release_step.py")
step = importlib.util.module_from_spec(_spec)
sys.modules["release_step"] = step   # dataclasses look their module up by name
_spec.loader.exec_module(step)

SHA = "a" * 40
CHECKS_RULESET = "Release branches: required checks"


class StandInGitHub:
    """A repository with one green pull request to `branch` whose head is SHA."""

    def __init__(self, branch="main", merge_commits=0):
        self.branch = branch
        self.pulls = [{"number": 12, "head": {"sha": SHA}}]
        commits = [{"parents": ["p"]}] * 3 + [{"parents": ["p", "q"]}] * merge_commits
        self.compare = {"ahead_by": len(commits), "behind_by": 0, "commits": commits}
        self.results = {"unit tests": "success", "lint": "success"}
        self.rulesets = {
            1: self._ruleset(step.REVIEW, step.admins("pull_request")),
            2: self._ruleset(step.RELEASE_MERGE, []),
            3: self._ruleset(step.DEV_MERGE, []),
            4: self._ruleset(CHECKS_RULESET, []),
        }
        self.writes = []            # (ruleset id, the bypass modes written)
        self.fail_closing = set()   # ruleset ids whose closing write fails

    @staticmethod
    def _ruleset(name, bypass):
        return {"name": name, "target": "branch", "enforcement": "active",
                "conditions": {}, "rules": [], "bypass_actors": bypass}

    def get(self, path):
        if path.startswith("pulls?"):
            return self.pulls
        if path.startswith("compare/"):
            return self.compare
        if path.startswith("rules/branches/"):
            return [{"type": "required_status_checks", "parameters": {
                "required_status_checks": [{"context": name} for name in ("unit tests", "lint")]}}]
        if path.endswith("/check-runs?per_page=100"):
            return {"check_runs": [{"name": n, "conclusion": c, "status": "completed", "started_at": "t"}
                                   for n, c in self.results.items()]}
        if path.endswith("/status?per_page=100"):
            return {"statuses": []}
        if path.startswith("rulesets?"):
            return [{"id": i, "name": r["name"]} for i, r in self.rulesets.items()]
        if path.startswith("rulesets/"):
            return dict(self.rulesets[int(path.split("/")[1])])
        raise AssertionError(f"unexpected read: {path}")

    def put(self, path, body):
        ruleset_id = int(path.split("/")[1])
        written = step.modes(body["bypass_actors"])
        closing = "always" not in " ".join(written)
        if closing and ruleset_id in self.fail_closing:
            raise step.GitHubError("the write failed")
        self.writes.append((ruleset_id, written))
        self.rulesets[ruleset_id] = dict(body)
        return dict(body)

    def open_now(self):
        return sorted(r["name"] for r in self.rulesets.values()
                      if "always" in " ".join(step.modes(r["bypass_actors"])))


def names(plan):
    return [name for _id, name, _closed in plan.to_open]


def test_a_green_fast_forward_of_main_opens_only_the_review_rule():
    plan = step.make_plan(StandInGitHub("main"), "main", SHA)
    assert plan.problems == []
    assert names(plan) == [step.REVIEW]


def test_a_range_with_a_merge_commit_also_opens_the_linear_history_rule():
    plan = step.make_plan(StandInGitHub("staging", merge_commits=1), "staging", SHA)
    assert plan.problems == []
    assert names(plan) == [step.REVIEW, step.RELEASE_MERGE]


def test_a_merge_back_into_dev_opens_only_the_dev_rule():
    plan = step.make_plan(StandInGitHub("dev", merge_commits=1), "dev", SHA)
    assert plan.problems == []
    assert names(plan) == [step.DEV_MERGE]


def test_a_commit_that_is_not_the_head_of_an_open_pull_request_is_refused():
    gh = StandInGitHub()
    gh.pulls = [{"number": 12, "head": {"sha": "b" * 40}}]
    assert any("no open pull request" in p for p in step.make_plan(gh, "main", SHA).problems)


@pytest.mark.parametrize("result", ["failure", "in_progress", None])
def test_a_required_check_that_is_not_green_is_refused(result):
    gh = StandInGitHub()
    if result is None:
        del gh.results["lint"]
    else:
        gh.results["lint"] = result
    plan = step.make_plan(gh, "main", SHA)
    assert any("1 of 2 required checks are not green" in p for p in plan.problems)


def test_a_push_that_is_not_a_fast_forward_is_refused():
    gh = StandInGitHub()
    gh.compare["behind_by"] = 2
    assert any("not a fast-forward" in p for p in step.make_plan(gh, "main", SHA).problems)


def test_a_rule_that_is_already_open_is_refused():
    gh = StandInGitHub()
    gh.rulesets[1]["bypass_actors"] = step.admins("always")
    assert any("not in its closed state" in p for p in step.make_plan(gh, "main", SHA).problems)


def test_the_rules_are_closed_again_after_the_push():
    gh = StandInGitHub("staging", merge_commits=1)
    plan = step.make_plan(gh, "staging", SHA)
    seen_open = []

    def push():
        seen_open.extend(gh.open_now())
        return True

    assert step.apply(gh, plan, push) == 0
    assert seen_open == sorted([step.REVIEW, step.RELEASE_MERGE])
    assert gh.open_now() == []
    assert step.modes(gh.rulesets[1]["bypass_actors"]) == step.modes(step.admins("pull_request"))


def test_the_required_checks_are_never_opened():
    gh = StandInGitHub("staging", merge_commits=1)
    step.apply(gh, step.make_plan(gh, "staging", SHA), lambda: True)
    assert gh.writes
    assert 4 not in {ruleset_id for ruleset_id, _modes in gh.writes}


def test_the_rules_are_closed_when_github_refuses_the_push():
    gh = StandInGitHub()
    assert step.apply(gh, step.make_plan(gh, "main", SHA), lambda: False) == 1
    assert gh.open_now() == []


def test_the_rules_are_closed_when_the_push_breaks():
    gh = StandInGitHub()

    def push():
        raise RuntimeError("the network went away")

    plan = step.make_plan(gh, "main", SHA)
    with pytest.raises(RuntimeError):
        step.apply(gh, plan, push)
    assert gh.open_now() == []


def test_a_rule_that_cannot_be_closed_is_reported_and_the_others_are_still_closed(capsys):
    gh = StandInGitHub("staging", merge_commits=1)
    gh.fail_closing = {1}
    assert step.apply(gh, step.make_plan(gh, "staging", SHA), lambda: True) == 2
    assert gh.open_now() == [step.REVIEW]
    assert "STILL OPEN" in capsys.readouterr().out


def run_main(monkeypatch, gh, *args):
    monkeypatch.setattr(step, "GitHub", lambda repo: gh)
    monkeypatch.setattr(step, "git_push", lambda checkout, sha, branch: (lambda: True))
    monkeypatch.setattr(sys, "argv", ["release_step.py", *args])
    return step.main()


def test_without_apply_nothing_is_written(monkeypatch, capsys):
    gh = StandInGitHub()
    assert run_main(monkeypatch, gh, "main", SHA) == 0
    assert gh.writes == []
    assert "Nothing changed" in capsys.readouterr().out


def test_with_a_problem_apply_writes_nothing(monkeypatch, capsys):
    gh = StandInGitHub()
    gh.results["lint"] = "failure"
    assert run_main(monkeypatch, gh, "main", SHA, "--apply") == 1
    assert gh.writes == []
    assert "STOP, nothing changed" in capsys.readouterr().out


def test_a_short_commit_id_is_refused(monkeypatch):
    gh = StandInGitHub()
    assert run_main(monkeypatch, gh, "main", "abc1234", "--apply") == 2
    assert gh.writes == []
