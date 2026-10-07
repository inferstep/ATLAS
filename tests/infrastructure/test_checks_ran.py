"""The "checks ran" job fails a change whose checks did not really run.

A workflow that fails to start shows no check, and a skipped job reports
success, so both look like a pass. These tests pin what the script expects
to have run for a change, and that each way of not running is reported.
"""
import importlib.util
import json
import re
import subprocess
import sys
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


@pytest.mark.parametrize("on", ["pull_request", ["push", "pull_request"], {"pull_request": None}])
def test_every_form_of_the_on_block_is_read(ran, change, on):
    assert ran.starts_on({True: on}, change, ["a.py"]) is True
    assert ran.starts_on({"on": on}, change, ["a.py"]) is True


def test_a_workflow_for_another_event_or_action_is_not_expected(ran, change):
    assert ran.starts_on({True: {"push": {}}}, change, ["a.py"]) is False
    assert ran.starts_on({True: {"pull_request": {"types": ["labeled"]}}}, change, ["a.py"]) is False
    assert ran.starts_on({True: {"pull_request": {"types": ["opened", "synchronize"]}}}, change, ["a.py"]) is True


def test_branch_filters_decide(ran, change):
    assert ran.starts_on({True: {"pull_request": {"branches": ["main"]}}}, change, ["a.py"]) is False
    assert ran.starts_on({True: {"pull_request": {"branches": ["main", "dev"]}}}, change, ["a.py"]) is True
    assert ran.starts_on({True: {"pull_request": {"branches": ["release/**"]}}}, change, ["a.py"]) is False
    assert ran.starts_on({True: {"pull_request": {"branches-ignore": ["dev"]}}}, change, ["a.py"]) is False


def test_path_filters_decide(ran, change):
    only_ui = {True: {"pull_request": {"paths": ["extensions/vscode/**", ".github/workflows/ui.yml"]}}}
    assert ran.starts_on(only_ui, change, ["proxy/agent.go"]) is False
    assert ran.starts_on(only_ui, change, ["proxy/agent.go", "extensions/vscode/src/a/b.ts"]) is True
    not_docs = {True: {"pull_request": {"paths-ignore": ["docs/**", "*.md"]}}}
    assert ran.starts_on(not_docs, change, ["docs/a/b.md", "README.md"]) is False
    assert ran.starts_on(not_docs, change, ["docs/a.md", "proxy/agent.go"]) is True
    # One star stays inside a folder; two stars cross folders.
    top_level = {True: {"pull_request": {"paths": ["*.md"]}}}
    assert ran.starts_on(top_level, change, ["docs/a.md"]) is False
    assert ran.starts_on(top_level, change, ["README.md"]) is True
    anywhere = {True: {"pull_request": {"paths": ["**/README.md"]}}}
    assert ran.starts_on(anywhere, change, ["README.md"]) is True
    assert ran.starts_on(anywhere, change, ["a/b/README.md"]) is True


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
    ([], "reported nothing: `lint`"),
    ([job("lint", "skipped")], "were skipped: `lint`"),
    ([job("lint", "cancelled")], "were cancelled: `lint`"),
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


def test_a_required_check_that_was_skipped_is_reported_even_when_its_job_has_a_condition(ran, change):
    workflows = {TESTS: workflow(lint={"name": "lint", "if": "github.event_name == 'push'"})}
    findings, _, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: [job("lint", "skipped")],
                               ["lint"], change)
    assert "1 required check(s) were skipped: `lint`" in messages(findings)
    assert "count a skipped job as passed" in messages(findings)


def test_a_required_check_that_no_job_reported_is_reported(ran, change):
    workflows = {TESTS: workflow(lint={"name": "python lint"})}
    findings, _, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: [job("python lint")],
                               ["lint", "python lint"], change)
    assert "1 required check(s) were reported by no job: `lint`" in messages(findings)


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


def test_it_waits_until_every_expected_run_has_ended(ran):
    clock, answers = Clock(), [{}, {TESTS: run(TESTS, None, status="in_progress")}, {TESTS: run(TESTS)}]
    runs, running = ran.settle(lambda: answers.pop(0) if len(answers) > 1 else answers[0], [TESTS], limit=3600,
                               pause=clock.pause, clock=clock)
    assert (runs[TESTS]["status"], running) == ("completed", [])
    assert clock.now == 2 * ran.POLL_SECONDS


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


def test_each_listing_is_put_in_the_log(ran, capsys):
    clock, answers = Clock(), [{}, {TESTS: run(TESTS, None, status="in_progress")}, {TESTS: run(TESTS)}]
    ran.settle(lambda: answers.pop(0) if len(answers) > 1 else answers[0], [TESTS], limit=3600,
               pause=clock.pause, clock=clock)
    lines = [line for line in capsys.readouterr().out.splitlines() if line.startswith("note listing")]
    assert lines == ["note listing 1 after 0s: 0 of 1 expected workflow(s) listed, 0 running, 1 not listed",
                     "note listing 2 after 60s: 1 of 1 expected workflow(s) listed, 1 running, 0 not listed",
                     "note listing 3 after 120s: 1 of 1 expected workflow(s) listed, 0 running, 0 not listed"]


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


def waiting_run(path, **more):
    return run(path, conclusion="action_required", **more)


def test_a_run_that_waits_for_approval_is_named_as_waiting_and_not_as_a_job_that_reported_nothing(ran, change):
    workflows = {TESTS: workflow(unit={"name": "unit"}), TITLE: workflow(title={"name": "pr title"})}
    runs = {TESTS: run(TESTS), TITLE: waiting_run(TITLE)}
    jobs = {TESTS: [job("unit")], TITLE: []}
    findings, waiting, _, ran_to_an_end, _ = ran.judge(workflows, [TESTS, TITLE], runs, [],
                                                    lambda r: jobs[r["path"]], ["unit", "pr title"], change)
    assert waiting == [TITLE]
    assert messages(findings) == ""
    assert ran_to_an_end == 1


def test_without_the_rule_the_same_run_reads_as_a_job_that_reported_nothing(ran, change):
    workflows = {TITLE: workflow(title={"name": "pr title"})}
    findings, _, _, _ = ran.check(workflows, [TITLE], {TITLE: waiting_run(TITLE)}, lambda r: [], ["pr title"], change)
    assert "reported nothing: `pr title`" in messages(findings)


def test_no_required_check_is_judged_while_a_run_waits(ran, change):
    workflows = {TESTS: workflow(unit={"name": "unit"}), TITLE: workflow(title={"name": "pr title"})}
    runs = {TESTS: run(TESTS), TITLE: waiting_run(TITLE)}
    jobs = {TESTS: [job("unit", "skipped")], TITLE: []}
    asked = []

    def required_spy(required, reported, branch, explained=()):
        asked.append(list(required))
        return []
    original, ran.required_findings = ran.required_findings, required_spy
    try:
        ran.judge(workflows, [TESTS, TITLE], runs, [], lambda r: jobs[r["path"]], ["unit", "pr title"], change)
    finally:
        ran.required_findings = original
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


def test_the_wait_for_the_other_runs_does_not_wait_for_an_approval(ran):
    listings = iter([{TITLE: waiting_run(TITLE)}])
    runs, running = ran.settle(lambda: next(listings), [TITLE], limit=600, pause=lambda s: None, clock=lambda: 0.0)
    assert running == []
    assert runs[TITLE]["conclusion"] == "action_required"


def test_a_run_that_starts_after_its_approval_is_waited_for_like_any_other(ran):
    clock = type("Clock", (), {"now": 0.0})()
    listings = iter([{TITLE: run(TITLE, status="in_progress", conclusion=None)}, {TITLE: run(TITLE)}])

    def pause(seconds):
        clock.now += seconds
    runs, running = ran.settle(lambda: next(listings), [TITLE], limit=600, pause=pause, clock=lambda: clock.now)
    assert running == []
    assert runs[TITLE]["conclusion"] == "success"


# --- a job that was skipped behind a failed job ----------------------------------------

BEHIND = workflow(a={}, b={"needs": "a"})


def skips(ran, change, jobs, reported, required=()):
    """The findings and the jobs skipped behind a failure, for one workflow."""
    findings, by_condition, _, behind = ran.check({TESTS: workflow(**jobs)}, [TESTS], {TESTS: run(TESTS)},
                                                  lambda _: reported, list(required), change)
    return messages(findings), behind, by_condition


def test_a_job_that_is_not_required_and_was_skipped_behind_a_failed_job_is_a_note(ran, change):
    found, behind, _ = skips(ran, change, BEHIND["jobs"], [job("a", "failure"), job("b", "skipped")])
    assert found == ""
    assert behind == {"b": ["a"]}
    found, behind, _ = skips(ran, change, BEHIND["jobs"], [job("a", "failure"), job("b", "skipped")], required=["a"])
    assert (found, behind) == ("", {"b": ["a"]})


@pytest.mark.parametrize("needed", ["success", "cancelled", "skipped"])
def test_a_job_skipped_with_no_failed_job_above_it_is_a_finding(ran, change, needed):
    found, behind, _ = skips(ran, change, BEHIND["jobs"], [job("a", needed), job("b", "skipped")])
    assert "were skipped: `b`" in found or "were skipped: `a`, `b`" in found
    assert "No failed job that it `needs` explains the skip" in found
    assert behind == {}


def test_a_required_check_skipped_behind_a_failed_job_that_is_not_required_is_a_finding(ran, change):
    found, behind, _ = skips(ran, change, BEHIND["jobs"], [job("a", "failure"), job("b", "skipped")], required=["b"])
    assert "1 job(s) in .github/workflows/tests.yml were skipped: `b`" in found
    assert "1 required check(s) were skipped: `b`" in found
    assert behind == {}


def test_a_required_check_skipped_behind_a_failed_required_check_is_a_note(ran, change):
    found, behind, _ = skips(ran, change, BEHIND["jobs"], [job("a", "failure"), job("b", "skipped")],
                             required=["a", "b"])
    assert found == ""
    assert behind == {"b": ["a"]}


CHAIN = {"a": {}, "b": {"needs": "a"}, "c": {"needs": ["b"]}}
CHAIN_RUN = [job("a", "failure"), job("b", "skipped"), job("c", "skipped")]


def test_a_chain_of_skipped_jobs_is_explained_by_the_job_that_failed_at_its_root(ran, change):
    found, behind, _ = skips(ran, change, CHAIN, CHAIN_RUN)
    assert found == ""
    assert behind == {"b": ["a"], "c": ["a"]}


def test_a_required_check_down_a_chain_is_a_finding_unless_the_root_that_failed_is_required(ran, change):
    found, behind, _ = skips(ran, change, CHAIN, CHAIN_RUN, required=["c"])
    assert "were skipped: `c`" in found and "1 required check(s) were skipped: `c`" in found
    assert behind == {"b": ["a"]}
    found, behind, _ = skips(ran, change, CHAIN, CHAIN_RUN, required=["a", "c"])
    assert (found, behind) == ("", {"b": ["a"], "c": ["a"]})


def test_with_two_failed_jobs_above_a_required_check_one_required_one_is_enough(ran, change):
    jobs = {"a": {}, "d": {}, "c": {"needs": ["a", "d"]}}
    reported = [job("a", "failure"), job("d", "failure"), job("c", "skipped")]
    found, behind, _ = skips(ran, change, jobs, reported, required=["c", "d"])
    assert (found, behind) == ("", {"c": ["a", "d"]})
    found, behind, _ = skips(ran, change, jobs, reported, required=["c"])
    assert "1 required check(s) were skipped: `c`" in found
    assert behind == {}


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
    assert lines == ["skipped by their own `if:` condition (not judged): publish",
                     "2 job(s) skipped because a job they need failed (the failed job holds the result): "
                     "`b` behind `a`, `d`; `c` behind `a`"]
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
