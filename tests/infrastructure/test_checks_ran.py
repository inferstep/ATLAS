"""The "checks ran" job fails a change whose checks did not really run.

A workflow that fails to start shows no check, and a skipped job reports
success, so both look like a pass. These tests pin what the script expects
to have run for a change, and that each way of not running is reported.
"""
import http.server
import importlib.util
import json
import re
import subprocess
import sys
import threading
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "checks_ran.py"
TESTS = ".github/workflows/tests.yml"
TITLE = ".github/workflows/pr-title.yml"


@pytest.fixture(scope="module")
def ran():
    spec = importlib.util.spec_from_file_location("atlas_checks_ran", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


@pytest.fixture
def change(ran):
    return ran.Change("pull_request", "synchronize", "head", "base", "dev")


def workflow(**jobs):
    return {True: {"pull_request": {"branches": ["main", "dev"]}}, "jobs": jobs}


def job(name, conclusion="success"):
    return {"name": name, "conclusion": conclusion}


def run(path, conclusion="success", status="completed", created="2000-01-01T00:00:00Z", run_id=1, event="pull_request"):
    return {"id": run_id, "path": path, "event": event, "status": status, "conclusion": conclusion,
            "created_at": created, "html_url": f"https://example.invalid/runs/{run_id}"}


def messages(findings):
    return " | ".join(finding.message for finding in findings)


# --- which workflows must start ------------------------------------------------

def test_the_event_gives_the_commit_the_base_and_the_branch(ran):
    pull = {"action": "opened", "pull_request": {"head": {"sha": "h"}, "base": {"sha": "b", "ref": "dev"}}}
    assert ran.change_from_event("pull_request", pull) == ("pull_request", "opened", "h", "b", "dev")
    group = {"merge_group": {"head_sha": "h", "base_sha": "b", "base_ref": "refs/heads/dev"}}
    assert ran.change_from_event("merge_group", group) == ("merge_group", "checks_requested", "h", "b", "dev")
    with pytest.raises(ValueError):
        ran.change_from_event("push", {})


@pytest.mark.parametrize("on", [
    pytest.param("pull_request", id="a name"),
    pytest.param(["push", "pull_request"], id="a list"),
    pytest.param({"pull_request": None}, id="a mapping"),
])
def test_every_form_of_the_on_block_is_read(ran, change, on):
    assert ran.starts_on({True: on}, change, ["a.py"]) is True
    assert ran.starts_on({"on": on}, change, ["a.py"]) is True


# `change` is a pull request into `dev` that was updated (`synchronize`).
@pytest.mark.parametrize("on, changed, starts", [
    pytest.param({"push": {}}, ["a.py"], False, id="another event"),
    pytest.param({"pull_request": {"types": ["labeled"]}}, ["a.py"], False, id="another action"),
    pytest.param({"pull_request": {"types": ["opened", "synchronize"]}}, ["a.py"], True, id="an action it lists"),
    pytest.param({"pull_request": {"branches": ["main"]}}, ["a.py"], False, id="a branch filter without dev"),
    pytest.param({"pull_request": {"branches": ["main", "dev"]}}, ["a.py"], True, id="a branch filter with dev"),
    pytest.param({"pull_request": {"branches": ["release/**"]}}, ["a.py"], False, id="a pattern dev does not fit"),
    pytest.param({"pull_request": {"branches-ignore": ["dev"]}}, ["a.py"], False, id="dev ignored"),
    pytest.param({"pull_request": {"paths": ["extensions/vscode/**", ".github/workflows/ui.yml"]}},
                 ["proxy/agent.go"], False, id="no changed file in its paths"),
    pytest.param({"pull_request": {"paths": ["extensions/vscode/**", ".github/workflows/ui.yml"]}},
                 ["proxy/agent.go", "extensions/vscode/src/a/b.ts"], True, id="one changed file in its paths"),
    pytest.param({"pull_request": {"paths-ignore": ["docs/**", "*.md"]}}, ["docs/a/b.md", "README.md"], False,
                 id="every changed file ignored"),
    pytest.param({"pull_request": {"paths-ignore": ["docs/**", "*.md"]}}, ["docs/a.md", "proxy/agent.go"], True,
                 id="one changed file not ignored"),
    # One star stays inside a folder; two stars cross folders.
    pytest.param({"pull_request": {"paths": ["*.md"]}}, ["docs/a.md"], False, id="one star in another folder"),
    pytest.param({"pull_request": {"paths": ["*.md"]}}, ["README.md"], True, id="one star at the top"),
    pytest.param({"pull_request": {"paths": ["**/README.md"]}}, ["README.md"], True, id="two stars at the top"),
    pytest.param({"pull_request": {"paths": ["**/README.md"]}}, ["a/b/README.md"], True, id="two stars down folders"),
])
def test_the_event_the_action_and_the_filters_decide_whether_a_workflow_starts(ran, change, on, changed, starts):
    assert ran.starts_on({True: on}, change, changed) is starts


def test_a_filter_it_cannot_read_is_said_not_guessed(ran, change):
    assert ran.starts_on({True: {"pull_request": {"paths": ["src/**", "!src/docs/**"]}}}, change, ["src/a.py"]) is None
    workflows = {"a.yml": {True: {"pull_request": {"paths": ["[ab].py"]}}}, "b.yml": {True: "pull_request"},
                 "own.yml": {True: "pull_request"}}
    assert ran.expected_workflows(workflows, change, ["a.py"], own="own.yml") == (["b.yml"], ["a.yml"])


def test_every_workflow_of_this_repository_can_be_judged(ran, change):
    workflows = ran.read_workflows(ROOT)
    assert workflows, "no workflow files were read"
    expected, unread = ran.expected_workflows(workflows, change, ["README.md"], own="")
    assert unread == [], f"`on:` filters in a form scripts/checks_ran.py does not read: {unread}"
    assert ".github/workflows/test.yml" in expected
    # A workflow with a path filter starts only for its own files.
    assert ".github/workflows/vscode-extension.yml" not in expected
    with_ui, _ = ran.expected_workflows(workflows, change, ["extensions/vscode/src/extension.ts"], own="")
    assert ".github/workflows/vscode-extension.yml" in with_ui


# --- which run and which jobs --------------------------------------------------

def test_the_replay_workflow_is_expected_only_when_its_paths_changed(ran, change):
    workflows = ran.read_workflows(ROOT)
    replay = ".github/workflows/replay.yml"
    assert replay in ran.expected_workflows(workflows, change, ["proxy/agent.go"], "")[0]
    assert replay in ran.expected_workflows(workflows, change, ["tests/replay/recordings/normal_edit.json"], "")[0]
    expected, unread = ran.expected_workflows(workflows, change, ["docs/README.md"], "")
    assert replay not in expected
    assert replay not in unread
    in_the_queue = change._replace(event="merge_group", action="checks_requested")
    assert replay in ran.expected_workflows(workflows, in_the_queue, ["docs/README.md"], "")[0]


def test_the_newest_run_of_a_workflow_for_the_event_counts(ran):
    runs = [run(TESTS, "cancelled", created="2000-01-01T00:00:00Z", run_id=1),
            run(TESTS, "success", created="2000-01-01T00:05:00Z", run_id=2),
            run(TESTS, "failure", created="2000-01-01T00:09:00Z", run_id=3, event="push"),
            run(".github/workflows/own.yml", None, status="in_progress", run_id=9)]
    latest = ran.latest_runs(runs, "pull_request", own_id=9)
    assert list(latest) == [TESTS]
    assert latest[TESTS]["id"] == 2


def test_a_job_is_found_under_each_name_form(ran):
    # A name that is all expression fits every name, so a name proves nothing for it: it gets only the one
    # name GitHub gives it when it is skipped, the text of the expression.
    jobs = {"go": {"name": "go test (${{ matrix.module }})"}, "trivy": {"strategy": {"matrix": {}}}, "lint": {},
            "zz-any": {"name": "${{ matrix.service }}"}}
    reported = [job("go test (proxy)"), job("go test (tui)"), job("trivy (proxy)"), job("lint"), job("sandbox"),
                job("matrix.service", "skipped")]
    found = {job_id: [r["name"] for r in mine] for job_id, mine in ran.jobs_by_definition(jobs, reported).items()}
    assert found == {"go": ["go test (proxy)", "go test (tui)"], "trivy": ["trivy (proxy)"], "lint": ["lint"],
                     "zz-any": ["matrix.service"]}


def test_a_job_named_by_an_expression_alone_is_not_present_because_some_other_job_reported(ran, change):
    workflows = {TESTS: workflow(lint={}, **{"zz-any": {"name": "${{ matrix.service }}"}})}
    reported = [job("lint"), job("a job that no definition fits")]
    findings, _, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: reported, [], change)
    assert "1 job(s) in .github/workflows/tests.yml reported nothing: `${{ matrix.service }}`" in messages(findings)


def test_a_job_may_skip_only_by_a_condition_of_its_own_or_of_a_job_it_needs(ran):
    jobs = {"build": {"if": "github.event_name == 'push'"}, "publish": {"needs": "build"},
            "sign": {"needs": ["publish"]}, "test": {"needs": ["lint"]}, "lint": {}}
    assert [job_id for job_id in jobs if ran.conditional(job_id, jobs)] == ["build", "publish", "sign"]


# --- what is reported ----------------------------------------------------------

def test_a_change_whose_checks_all_ran_has_no_finding(ran, change):
    workflows = {TESTS: workflow(go={"name": "go test (${{ matrix.module }})"}, lint={"name": "lint"},
                                 publish={"name": "publish", "if": "github.event_name == 'push'"})}
    reported = [job("go test (proxy)"), job("go test (tui)", "failure"), job("lint"), job("publish", "skipped")]
    findings, by_condition, count, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: reported,
                                              ["go test (proxy)", "go test (tui)", "lint"], change)
    assert findings == []
    assert by_condition == ["publish"]
    assert count == 3


def test_a_workflow_with_no_run_is_reported(ran, change):
    findings, _, _, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], {}, lambda _: [], [], change)
    assert "has no run for this commit" in messages(findings)
    assert "Fix:" in messages(findings)


def test_a_workflow_that_failed_to_start_is_reported_with_its_run(ran, change):
    runs = {TESTS: run(TESTS, "startup_failure", run_id=7)}
    findings, _, _, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], runs, lambda _: [], [], change)
    assert "failed to start" in messages(findings)
    assert "https://example.invalid/runs/7" in messages(findings)
    assert findings[0].path == TESTS


@pytest.mark.parametrize("reported, words", [
    pytest.param([], "reported nothing: `lint`", id="absent"),
    pytest.param([job("lint", "skipped")], "were skipped: `lint`", id="skipped"),
    pytest.param([job("lint", "cancelled")], "were cancelled: `lint`", id="cancelled"),
])
def test_a_job_with_no_condition_that_did_not_run_is_reported(ran, change, reported, words):
    findings, by_condition, _, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], {TESTS: run(TESTS)},
                                          lambda _: reported, [], change)
    assert words in messages(findings)
    assert "Fix:" in messages(findings)
    assert by_condition == []


def test_the_jobs_of_one_workflow_share_one_finding(ran, change):
    workflows = {TESTS: workflow(go={"name": "go test (${{ matrix.module }})"}, lint={})}
    reported = [job("go test (proxy)", "cancelled"), job("go test (tui)", "cancelled"), job("lint", "cancelled")]
    findings, _, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS, "cancelled")}, lambda _: reported, [], change)
    assert len(findings) == 1
    assert "3 job(s)" in findings[0].message


@pytest.mark.parametrize("jobs, reported, required, words", [
    pytest.param({"lint": {"name": "lint", "if": "github.event_name == 'push'"}}, [job("lint", "skipped")], ["lint"],
                 "1 required check(s) were skipped: `lint`. The rules of `dev` count a skipped job as passed",
                 id="skipped, even when its job has a condition"),
    pytest.param({"lint": {"name": "python lint"}}, [job("python lint")], ["lint", "python lint"],
                 "1 required check(s) were reported by no job: `lint`", id="reported by no job"),
])
def test_a_required_check_that_did_not_run_is_reported(ran, change, jobs, reported, required, words):
    findings, _, _, _ = ran.check({TESTS: workflow(**jobs)}, [TESTS], {TESTS: run(TESTS)}, lambda _: reported,
                                  required, change)
    assert words in messages(findings)


def test_the_required_names_come_from_the_branch_rules_without_this_check_itself(ran):
    rules = [{"type": "pull_request", "parameters": {}},
             {"type": "required_status_checks",
              "parameters": {"required_status_checks": [{"context": "lint"}, {"context": "checks ran"}]}}]
    own = {"jobs": {"checks-ran": {"name": "checks ran"}}}
    # Its own run is the one that is still going, so it can never have ended.
    assert ran.required_names(rules, own) == ["lint"]
    assert ran.required_names(rules, {}) == ["lint", "checks ran"]
    assert ran.required_names([], own) == []


# --- waiting -------------------------------------------------------------------

class Clock:
    """Time that moves only when the code under test pauses."""

    def __init__(self):
        self.now = 0.0

    def pause(self, seconds):
        self.now += seconds

    def __call__(self):
        return self.now


def test_it_waits_until_every_expected_run_has_ended_and_logs_each_listing(ran, capsys):
    clock, answers = Clock(), [{}, {TESTS: run(TESTS, None, status="in_progress")}, {TESTS: run(TESTS)}]
    runs, running = ran.settle(lambda: answers.pop(0) if len(answers) > 1 else answers[0], [TESTS], limit=3600,
                               pause=clock.pause, clock=clock)
    assert (runs[TESTS]["status"], running) == ("completed", [])
    assert clock.now == 2 * ran.POLL_SECONDS
    lines = [line for line in capsys.readouterr().out.splitlines() if line.startswith("note listing")]
    assert lines == ["note listing 1 after 0s: 0 of 1 expected workflow(s) listed, 0 running, 1 not listed",
                     "note listing 2 after 60s: 1 of 1 expected workflow(s) listed, 1 running, 0 not listed",
                     "note listing 3 after 120s: 1 of 1 expected workflow(s) listed, 0 running, 0 not listed"]


def test_a_workflow_that_never_gets_a_run_ends_the_wait_after_the_grace_time(ran):
    clock = Clock()
    runs, running = ran.settle(dict, [TESTS], limit=3600, pause=clock.pause, clock=clock)
    assert (runs, running) == ({}, [])
    assert ran.GRACE_SECONDS <= clock.now < ran.GRACE_SECONDS + ran.POLL_SECONDS


def test_a_run_an_earlier_listing_showed_is_not_missing_when_a_later_listing_leaves_it_out(ran, capsys):
    clock = Clock()
    answers = [{TESTS: run(TESTS, None, status="in_progress")}] * 4 + [{}, {TESTS: run(TESTS)}]
    runs, running = ran.settle(lambda: answers.pop(0) if len(answers) > 1 else answers[0], [TESTS], limit=3600,
                               pause=clock.pause, clock=clock)
    assert (runs[TESTS]["status"], running) == ("completed", [])
    assert clock.now == 5 * ran.POLL_SECONDS
    assert "left out 1 run(s) that an earlier listing showed" in capsys.readouterr().out


def test_a_listing_with_only_an_older_run_does_not_replace_the_newer_one(ran):
    clock = Clock()
    newer = run(TESTS, None, status="in_progress", created="2000-01-02T00:00:00Z", run_id=2)
    answers = [{TESTS: newer}, {TESTS: run(TESTS)}, {TESTS: dict(newer, status="completed", conclusion="failure")}]
    runs, _ = ran.settle(lambda: answers.pop(0) if len(answers) > 1 else answers[0], [TESTS], limit=3600,
                         pause=clock.pause, clock=clock)
    assert (runs[TESTS]["id"], runs[TESTS]["conclusion"]) == (2, "failure")


def test_a_new_attempt_of_a_run_is_read_from_the_newest_listing(ran):
    seen = ran.merge_runs({TESTS: run(TESTS, "failure")}, {TESTS: run(TESTS, None, status="in_progress")})
    assert seen[TESTS]["status"] == "in_progress"


def test_a_run_that_does_not_end_is_named_when_the_time_is_up(ran):
    clock = Clock()
    _, running = ran.settle(lambda: {TESTS: run(TESTS, None, status="in_progress")}, [TESTS], limit=600,
                            pause=clock.pause, clock=clock)
    assert running == [TESTS]
    assert clock.now == 600


# --- the command ---------------------------------------------------------------

def test_outside_a_pull_request_job_it_stops_with_a_fix(tmp_path):
    event = tmp_path / "event.json"
    event.write_text(json.dumps({}), encoding="utf-8")
    env = {"PATH": "/usr/bin:/bin", "GITHUB_EVENT_PATH": str(event), "GITHUB_EVENT_NAME": "push",
           "GITHUB_REPOSITORY": "example/example"}
    done = subprocess.run([sys.executable, str(SCRIPT), "--root", str(ROOT)], capture_output=True, text=True,
                          env=env, check=False)
    assert done.returncode == 2
    assert "fix:" in done.stderr


def test_the_report_prints_each_finding_and_an_annotation(ran, capsys):
    ran.report([ran.Finding(TESTS, "workflow x failed to start. Fix: y.")], ["7 workflow(s) expected"], github=True)
    out = capsys.readouterr().out
    assert "note 7 workflow(s) expected" in out
    assert "FAIL workflow x failed to start. Fix: y." in out
    assert f"::error file={TESTS},title=a check did not run::workflow x failed to start. Fix: y." in out
    assert "1 check(s) did not run" in out


def test_a_run_that_waits_for_approval_is_named_as_waiting_and_not_as_a_job_that_reported_nothing(ran, change):
    workflows = {TESTS: workflow(unit={"name": "unit"}), TITLE: workflow(title={"name": "pr title"})}
    runs = {TESTS: run(TESTS), TITLE: run(TITLE, "action_required")}
    jobs = {TESTS: [job("unit")], TITLE: []}
    findings, waiting, _, ran_to_an_end, _ = ran.judge(workflows, [TESTS, TITLE], runs, [],
                                                    lambda r: jobs[r["path"]], ["unit", "pr title"], change)
    assert waiting == [TITLE]
    assert messages(findings) == ""
    assert ran_to_an_end == 1


def test_without_the_rule_the_same_run_reads_as_a_job_that_reported_nothing(ran, change):
    workflows = {TITLE: workflow(title={"name": "pr title"})}
    findings, _, _, _ = ran.check(workflows, [TITLE], {TITLE: run(TITLE, "action_required")}, lambda r: [],
                                  ["pr title"], change)
    assert "reported nothing: `pr title`" in messages(findings)


def test_no_required_check_is_judged_while_a_run_waits(ran, change, monkeypatch):
    workflows = {TESTS: workflow(unit={"name": "unit"}), TITLE: workflow(title={"name": "pr title"})}
    runs = {TESTS: run(TESTS), TITLE: run(TITLE, "action_required")}
    jobs = {TESTS: [job("unit", "skipped")], TITLE: []}
    asked = []

    def required_spy(required, reported, branch, explained=()):
        asked.append(list(required))
        return []
    monkeypatch.setattr(ran, "required_findings", required_spy)
    ran.judge(workflows, [TESTS, TITLE], runs, [], lambda r: jobs[r["path"]], ["unit", "pr title"], change)
    assert asked == [[]]


def test_the_check_does_not_pass_while_a_run_waits(ran):
    assert ran.outcome([], []) == (0, "checks ran: every check ran")
    status, line = ran.outcome([], [TITLE])
    assert status == 1
    assert line == "checks ran: no verdict yet, 1 workflow(s) wait for a maintainer's approval"
    status, line = ran.outcome([ran.Finding(TESTS, "x")], [TITLE])
    assert status == 1
    assert "1 check(s) did not run, and 1 workflow(s) wait" in line


def test_the_waiting_message_says_what_each_person_does(ran, capsys):
    status = ran.report([], [], True, [TITLE])
    out = capsys.readouterr().out
    assert status == 1
    assert f"WAIT workflow {TITLE} waits for a maintainer's approval" in out
    assert "If you opened the pull request, there is nothing for you to do." in out
    assert 'A maintainer: approve the waiting runs on the pull request ("Approve and run"), then run this check again.' in out
    assert f"::warning file={TITLE},title=waiting for a maintainer's approval::" in out
    assert "::error" not in out
    assert "reported nothing" not in out


# Each listing is given once; asking for one more fails the test.
@pytest.mark.parametrize("listings, conclusion", [
    pytest.param([{TITLE: run(TITLE, "action_required")}], "action_required", id="a run that waits for approval"),
    pytest.param([{TITLE: run(TITLE, status="in_progress", conclusion=None)}, {TITLE: run(TITLE)}], "success",
                 id="a run that started after its approval"),
])
def test_the_wait_does_not_wait_for_an_approval_but_for_a_run_that_started_after_it(ran, listings, conclusion):
    clock, answers = Clock(), iter(listings)
    runs, running = ran.settle(lambda: next(answers), [TITLE], limit=600, pause=clock.pause, clock=clock)
    assert running == []
    assert runs[TITLE]["conclusion"] == conclusion


# --- a job that was skipped behind a failed job ----------------------------------------

BEHIND = {"a": {}, "b": {"needs": "a"}}
BEHIND_RUN = [job("a", "failure"), job("b", "skipped")]
CHAIN = {"a": {}, "b": {"needs": "a"}, "c": {"needs": ["b"]}}
CHAIN_RUN = [job("a", "failure"), job("b", "skipped"), job("c", "skipped")]
TWO_ABOVE = {"a": {}, "d": {}, "c": {"needs": ["a", "d"]}}
TWO_ABOVE_RUN = [job("a", "failure"), job("d", "failure"), job("c", "skipped")]


def skips(ran, change, jobs, reported, required=()):
    """The findings and the jobs skipped behind a failure, for one workflow."""
    findings, by_condition, _, behind = ran.check({TESTS: workflow(**jobs)}, [TESTS], {TESTS: run(TESTS)},
                                                  lambda _: reported, list(required), change)
    return messages(findings), behind, by_condition


@pytest.mark.parametrize("jobs, reported, required, behind", [
    pytest.param(BEHIND, BEHIND_RUN, [], {"b": ["a"]}, id="no check required"),
    pytest.param(BEHIND, BEHIND_RUN, ["a"], {"b": ["a"]}, id="the failed job required"),
    pytest.param(BEHIND, BEHIND_RUN, ["a", "b"], {"b": ["a"]}, id="the failed job and the skipped one required"),
    pytest.param(CHAIN, CHAIN_RUN, [], {"b": ["a"], "c": ["a"]}, id="a chain, explained by the failed job at its root"),
    pytest.param(CHAIN, CHAIN_RUN, ["a", "c"], {"b": ["a"], "c": ["a"]}, id="a chain whose failed root is required"),
    pytest.param(TWO_ABOVE, TWO_ABOVE_RUN, ["c", "d"], {"c": ["a", "d"]}, id="two failed above, one of them required"),
])
def test_a_job_skipped_behind_a_failed_job_is_a_note(ran, change, jobs, reported, required, behind):
    found, skipped_behind, _ = skips(ran, change, jobs, reported, required)
    assert (found, skipped_behind) == ("", behind)


@pytest.mark.parametrize("needed", ["success", "cancelled", "skipped"])
def test_a_job_skipped_with_no_failed_job_above_it_is_a_finding(ran, change, needed):
    found, behind, _ = skips(ran, change, BEHIND, [job("a", needed), job("b", "skipped")])
    assert "were skipped: `b`" in found or "were skipped: `a`, `b`" in found
    assert "No failed job that it `needs` explains the skip" in found
    assert behind == {}


# The rules count a skipped required check as passed, and a failed job that is not required holds no merge.
@pytest.mark.parametrize("jobs, reported, check, behind", [
    pytest.param(BEHIND, BEHIND_RUN, "b", {}, id="behind a failed job"),
    pytest.param(CHAIN, CHAIN_RUN, "c", {"b": ["a"]}, id="down a chain"),
    pytest.param(TWO_ABOVE, TWO_ABOVE_RUN, "c", {}, id="behind two failed jobs"),
])
def test_a_required_check_skipped_behind_failed_jobs_that_are_not_required_is_a_finding(ran, change, jobs, reported,
                                                                                       check, behind):
    found, skipped_behind, _ = skips(ran, change, jobs, reported, [check])
    assert f"1 job(s) in {TESTS} were skipped: `{check}`" in found
    assert f"1 required check(s) were skipped: `{check}`" in found
    assert skipped_behind == behind


def test_a_job_behind_one_that_its_own_condition_skipped_is_not_explained_by_a_failure(ran, change):
    jobs = {"a": {}, "b": {"needs": "a", "if": "github.event_name == 'push'"}, "c": {"needs": "b"}}
    reported = [job("a", "failure"), job("b", "skipped"), job("c", "skipped")]
    found, behind, by_condition = skips(ran, change, jobs, reported)
    assert behind == {}
    assert sorted(by_condition) == ["b", "c"]
    assert found == ""
    # The rules count a skipped required check as passed, so that one is named whatever skipped it.
    found, behind, _ = skips(ran, change, jobs, reported, required=["c"])
    assert "1 required check(s) were skipped: `c`" in found
    assert behind == {}


def test_one_fault_in_the_extensions_job_is_one_red_and_not_one_more_from_this_check(ran, change):
    path = ".github/workflows/vscode-extension.yml"
    workflows = ran.read_workflows(ROOT)
    reported = [job("lint + test + build", "failure"), job("coverage upload (extension)", "skipped"),
                job("test results upload (extension)", "failure")]
    findings, _, _, behind = ran.check(workflows, [path], {path: run(path)}, lambda _: reported, [], change)
    assert messages(findings) == ""
    assert behind == {"coverage upload (extension)": ["lint + test + build"]}


def test_the_jobs_skipped_behind_a_failure_are_always_printed(ran):
    lines = ran.skip_notes(["publish"], {"c": ["a"], "b": ["a", "d"]})
    behind = ("2 job(s) skipped because a job they need failed (the failed job holds the result): "
              "`b` behind `a`, `d`; `c` behind `a`")
    assert lines == ["skipped by their own `if:` condition (not judged): publish", behind]
    assert ran.skip_notes([], {}) == []


def test_the_gates_page_names_the_jobs_this_check_does_not_judge(ran):
    with_a_condition = {}
    for path, workflow in ran.read_workflows(ROOT).items():
        jobs = workflow.get("jobs") or {}
        mine = sorted(job_id for job_id in jobs if ran.conditional(job_id, jobs))
        if mine:
            with_a_condition[Path(path).name] = mine
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    section = page.split("### What `checks ran` judges", 1)[1].split("\n## ", 1)[0]
    table = {name: sorted(re.findall(r"`([^`]+)`", listed))
             for name, listed in re.findall(r"^\| `([\w.-]+\.ya?ml)` \| (.+) \|$", section, re.MULTILINE)}
    assert table == with_a_condition, (
        "the gates page names other jobs with an `if:` of their own than the workflow files have. Fix: bring the "
        "table under \"What `checks ran` judges\" in docs/quality/gates.md in line with the workflow files, and the "
        "number in the text above it.")
    count = sum(len(jobs) for jobs in with_a_condition.values())
    assert f"- {count} job definitions have an `if:` of their own" in " ".join(section.split())


class _GitHub(http.server.BaseHTTPRequestHandler):
    """A stand-in for GitHub's API. It answers each request with the next status of `statuses`: a page for 200, and
    a short message for any other. "silent" ends the connection with no answer; "broken" begins the page and ends
    the connection in its middle."""

    statuses = []

    def do_GET(self):
        status = self.statuses.pop(0)
        if status == "silent":
            return
        if status == "broken":
            self.send_response(200)
            self.send_header("Content-Length", "100")
            self.end_headers()
            self.wfile.write(b'{"total_')
            return
        body = json.dumps({"total_count": 0} if status == 200 else {"message": "a planted answer"}).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def test_a_short_outage_of_github_is_waited_out_and_an_answer_that_says_no_is_not_asked_again(ran, monkeypatch):
    server = http.server.HTTPServer(("127.0.0.1", 0), _GitHub)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    slept = []
    monkeypatch.setattr(ran, "API", f"http://127.0.0.1:{server.server_port}")
    monkeypatch.setattr(ran.time, "sleep", slept.append)
    try:
        # Two answers 5xx, then the answer: three tries, with a wait that grows.
        monkeypatch.setattr(_GitHub, "statuses", [502, 503, 200])
        assert ran.api("repos/o/r/actions/runs", "a-token") == [{"total_count": 0}]
        assert slept == [10, 30] and _GitHub.statuses == []
        # A connection that ends with no answer, and an answer that breaks off in its middle, are asked again too.
        monkeypatch.setattr(_GitHub, "statuses", ["silent", "broken", 200])
        slept[:] = []
        assert ran.api("repos/o/r/actions/runs", "a-token") == [{"total_count": 0}]
        assert slept == [10, 30] and _GitHub.statuses == []
        # An answer that says no is not asked again, and the words say what GitHub answered.
        monkeypatch.setattr(_GitHub, "statuses", [404])
        slept[:] = []
        with pytest.raises(RuntimeError, match=r"^GitHub answered \S+ with 404 Not Found$"):
            ran.api("repos/o/r/actions/runs", "a-token")
        assert slept == [] and _GitHub.statuses == []
        # A longer outage: five tries, almost four minutes, and the words say that GitHub did not answer.
        monkeypatch.setattr(_GitHub, "statuses", [502] * 5)
        with pytest.raises(RuntimeError, match=r"^GitHub did not answer \S+ in 5 tries over 220 s: HTTP Error 502: Bad Gateway$"):
            ran.api("repos/o/r/actions/runs", "a-token")
        assert slept == [10, 30, 60, 120] and _GitHub.statuses == []
    finally:
        server.shutdown()
        server.server_close()
    # GitHub cannot be reached at all: the same tries, and the same words.
    slept[:] = []
    with pytest.raises(RuntimeError, match=r"^GitHub did not answer \S+ in 5 tries over 220 s: <urlopen error"):
        ran.api("repos/o/r/actions/runs", "a-token")
    assert slept == [10, 30, 60, 120]
