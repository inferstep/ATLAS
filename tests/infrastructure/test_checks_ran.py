"""The "checks ran" job fails a change whose checks did not really run.

A workflow that fails to start shows no check, and a skipped job reports
success, so both look like a pass. These tests pin what the script expects
to have run for a change, and that each way of not running is reported.
"""
import importlib.util
import json
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "checks_ran.py"
TESTS = ".github/workflows/tests.yml"


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

def test_the_newest_run_of_a_workflow_for_the_event_counts(ran):
    runs = [run(TESTS, "cancelled", created="2000-01-01T00:00:00Z", run_id=1),
            run(TESTS, "success", created="2000-01-01T00:05:00Z", run_id=2),
            run(TESTS, "failure", created="2000-01-01T00:09:00Z", run_id=3, event="push"),
            run(".github/workflows/own.yml", None, status="in_progress", run_id=9)]
    latest = ran.latest_runs(runs, "pull_request", own_id=9)
    assert list(latest) == [TESTS]
    assert latest[TESTS]["id"] == 2


def test_a_job_is_found_under_each_name_form(ran):
    # A name that is all expression fits every job, so it gets only what no other job claims.
    jobs = {"go": {"name": "go test (${{ matrix.module }})"}, "trivy": {"strategy": {"matrix": {}}}, "lint": {},
            "zz-any": {"name": "${{ matrix.service }}"}}
    reported = [job("go test (proxy)"), job("go test (tui)"), job("trivy (proxy)"), job("lint"), job("sandbox")]
    found = {job_id: [r["name"] for r in mine] for job_id, mine in ran.jobs_by_definition(jobs, reported).items()}
    assert found == {"go": ["go test (proxy)", "go test (tui)"], "trivy": ["trivy (proxy)"], "lint": ["lint"],
                     "zz-any": ["sandbox"]}


def test_a_job_may_skip_only_by_a_condition_of_its_own_or_of_a_job_it_needs(ran):
    jobs = {"build": {"if": "github.event_name == 'push'"}, "publish": {"needs": "build"},
            "sign": {"needs": ["publish"]}, "test": {"needs": ["lint"]}, "lint": {}}
    assert [job_id for job_id in jobs if ran.conditional(job_id, jobs)] == ["build", "publish", "sign"]


# --- what is reported ----------------------------------------------------------

def test_a_change_whose_checks_all_ran_has_no_finding(ran, change):
    workflows = {TESTS: workflow(go={"name": "go test (${{ matrix.module }})"}, lint={"name": "lint"},
                                 publish={"name": "publish", "if": "github.event_name == 'push'"})}
    reported = [job("go test (proxy)"), job("go test (tui)", "failure"), job("lint"), job("publish", "skipped")]
    findings, by_condition, count = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: reported,
                                              ["go test (proxy)", "go test (tui)", "lint"], change)
    assert findings == []
    assert by_condition == ["publish"]
    assert count == 3


def test_a_workflow_with_no_run_is_reported(ran, change):
    findings, _, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], {}, lambda _: [], [], change)
    assert "has no run for this commit" in messages(findings)
    assert "Fix:" in messages(findings)


def test_a_workflow_that_failed_to_start_is_reported_with_its_run(ran, change):
    runs = {TESTS: run(TESTS, "startup_failure", run_id=7)}
    findings, _, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], runs, lambda _: [], [], change)
    assert "failed to start" in messages(findings)
    assert "https://example.invalid/runs/7" in messages(findings)
    assert findings[0].path == TESTS


@pytest.mark.parametrize("reported, words", [
    ([], "reported nothing: `lint`"),
    ([job("lint", "skipped")], "were skipped: `lint`"),
    ([job("lint", "cancelled")], "were cancelled: `lint`"),
])
def test_a_job_with_no_condition_that_did_not_run_is_reported(ran, change, reported, words):
    findings, by_condition, _ = ran.check({TESTS: workflow(lint={})}, [TESTS], {TESTS: run(TESTS)},
                                          lambda _: reported, [], change)
    assert words in messages(findings)
    assert "Fix:" in messages(findings)
    assert by_condition == []


def test_the_jobs_of_one_workflow_share_one_finding(ran, change):
    workflows = {TESTS: workflow(go={"name": "go test (${{ matrix.module }})"}, lint={})}
    reported = [job("go test (proxy)", "cancelled"), job("go test (tui)", "cancelled"), job("lint", "cancelled")]
    findings, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS, "cancelled")}, lambda _: reported, [], change)
    assert len(findings) == 1
    assert "3 job(s)" in findings[0].message


def test_a_required_check_that_was_skipped_is_reported_even_when_its_job_has_a_condition(ran, change):
    workflows = {TESTS: workflow(lint={"name": "lint", "if": "github.event_name == 'push'"})}
    findings, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: [job("lint", "skipped")],
                               ["lint"], change)
    assert "1 required check(s) were skipped: `lint`" in messages(findings)
    assert "count a skipped job as passed" in messages(findings)


def test_a_required_check_that_no_job_reported_is_reported(ran, change):
    workflows = {TESTS: workflow(lint={"name": "python lint"})}
    findings, _, _ = ran.check(workflows, [TESTS], {TESTS: run(TESTS)}, lambda _: [job("python lint")],
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
