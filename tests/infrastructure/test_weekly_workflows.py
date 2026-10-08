"""The two weekly workflows have the smallest rights that work, start only the scripts, and use no action that is new.

A scheduled job runs with nobody looking. So what each job may do is held here, by the workflow files themselves:
the canary check can only read, and of the weekly cleanup only the job that runs no fixer, no build and no test can
write an issue.
"""
import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
CANARY, CLEANUP = "canary-check.yml", "weekly-cleanup.yml"
SCRIPT = {CANARY: "scripts/canary.py", CLEANUP: "scripts/weekly_cleanup.py"}
# A scheduled run and a run by hand read `dev`; a pull request reads itself.
READS = "${{ github.event_name != 'pull_request' && 'dev' || '' }}"


def text(name):
    return (WORKFLOWS / name).read_text(encoding="utf-8")


def workflow(name):
    loaded = yaml.safe_load(text(name))
    # YAML reads the key `on` as the value true.
    loaded["on"] = loaded.pop(True)
    return loaded


def steps(name, job):
    return workflow(name)["jobs"][job]["steps"]


def rights(name):
    """Every right that the workflow or one of its jobs gives, as (job, right, how far)."""
    loaded = workflow(name)
    given = [("the workflow", right, level) for right, level in loaded["permissions"].items()]
    return given + [(job_id, right, level) for job_id, job in loaded["jobs"].items() for right, level in job["permissions"].items()]


def uses(name):
    return re.findall(r"^\s*-? ?uses: (\S+)@(\S+)(.*)$", text(name), re.MULTILINE)


BOTH = pytest.mark.parametrize("name", [CANARY, CLEANUP])


@BOTH
def test_it_starts_on_a_schedule_by_hand_and_for_a_pull_request_that_changes_its_own_two_files(name):
    on = workflow(name)["on"]
    assert set(on) == {"schedule", "workflow_dispatch", "pull_request"}
    assert on["pull_request"] == {"branches": ["dev"], "paths": [f".github/workflows/{name}", SCRIPT[name]]}
    assert on["workflow_dispatch"] in ({}, None)


@BOTH
def test_it_does_not_start_with_the_rights_of_the_base_branch_for_a_pull_request(name):
    assert "pull_request_target" not in text(name)
    assert "workflow_run" not in text(name)


@pytest.mark.parametrize("name, cron", [(CANARY, "47 2 * * 1"), (CLEANUP, "17 3 * * 1")])
def test_it_runs_once_a_week_on_monday_and_not_at_a_full_hour(name, cron):
    assert workflow(name)["on"]["schedule"] == [{"cron": cron}]
    minute, hour, day, month, weekday = cron.split()
    assert minute != "0"
    assert (day, month, weekday) == ("*", "*", "1")
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    assert f"each Monday at\n{int(hour):02}:{int(minute):02} UTC" in page


@BOTH
def test_the_workflow_gives_no_right_by_itself_and_each_job_names_its_own(name):
    loaded = workflow(name)
    assert loaded["permissions"] == {}
    for job in loaded["jobs"].values():
        assert isinstance(job["permissions"], dict)
        assert job["permissions"]
        assert job["timeout-minutes"] <= 90
        assert job["runs-on"] == "ubuntu-24.04"


def test_the_canary_check_can_only_read(name=CANARY):
    (job,) = workflow(name)["jobs"].values()
    assert job["permissions"] == {"contents": "read", "pull-requests": "read", "checks": "read", "actions": "read"}
    assert {level for _job, _right, level in rights(name)} == {"read"}
    assert job["name"] == "canary check (reads only)"


def test_of_the_weekly_cleanup_only_the_job_that_writes_the_issue_can_write_and_only_an_issue():
    jobs = workflow(CLEANUP)["jobs"]
    assert list(jobs) == ["text", "issue"]
    assert jobs["text"]["permissions"] == {"contents": "read"}
    assert jobs["issue"]["permissions"] == {"contents": "read", "issues": "write"}
    assert [given for given in rights(CLEANUP) + rights(CANARY) if given[2] != "read"] == [("issue", "issues", "write")]
    assert jobs["text"]["name"] == "weekly cleanup (make the text)"
    assert jobs["issue"]["name"] == "weekly cleanup (write the issue)"


def test_the_job_that_can_write_runs_no_fixer_no_build_and_no_test_and_takes_the_text_as_data():
    job = workflow(CLEANUP)["jobs"]["issue"]
    assert job["needs"] == "text"
    assert [step.get("uses", "run").split("@")[0] for step in job["steps"]] == ["actions/checkout", "actions/download-artifact", "run"]
    assert job["steps"][2]["run"] == 'python3 scripts/weekly_cleanup.py write --from "$RUNNER_TEMP/weekly-cleanup"'
    assert job["steps"][2]["env"] == {"GITHUB_TOKEN": "${{ github.token }}"}
    # Of the repository it has the one script, from `dev`: no other file of a branch is on its disk.
    assert job["steps"][0]["with"] == {"ref": "dev", "sparse-checkout": "scripts/weekly_cleanup.py", "sparse-checkout-cone-mode": False,
                                       "persist-credentials": False}
    for word in ("setup-go", "setup-python", "pip ", "go ", "pytest", "make --", "ruff"):
        assert word not in "\n".join(str(step) for step in job["steps"]), word


def test_the_job_that_can_write_does_not_run_for_a_pull_request():
    assert workflow(CLEANUP)["jobs"]["issue"]["if"] == "${{ github.event_name != 'pull_request' }}"
    assert "if" not in workflow(CLEANUP)["jobs"]["text"]
    for step in steps(CLEANUP, "text") + steps(CLEANUP, "issue") + steps(CANARY, "check"):
        assert "if" not in step, step
        assert "continue-on-error" not in step, step


def test_the_job_that_runs_the_fixers_and_the_tests_has_no_token():
    for step in steps(CLEANUP, "text"):
        assert "token" not in str(step).lower().replace("persist-credentials", ""), step
        assert "env" not in step, step
    assert "secrets." not in text(CLEANUP) + text(CANARY)


@pytest.mark.parametrize("name, job, runs", [
    (CANARY, "check", "python scripts/canary.py check"),
    (CLEANUP, "text", 'python scripts/weekly_cleanup.py make --out "$RUNNER_TEMP/weekly-cleanup"'),
])
def test_the_workflow_only_starts_the_script_and_the_script_judges(name, job, runs):
    commands = [step["run"] for step in steps(name, job) if "run" in step]
    assert commands == ["pip install --require-hashes -r .github/requirements/ci.txt", runs]
    # No shell around the script could turn its status into a pass.
    for command in commands:
        assert not re.search(r"\|\||&&|\bif\b|\bset \+e\b|\|", command), command


def test_the_token_of_the_canary_check_is_the_jobs_own_and_only_the_script_gets_it():
    with_a_token = [step for step in steps(CANARY, "check") if "token" in str(step).lower().replace("persist-credentials", "")]
    assert with_a_token == [step for step in steps(CANARY, "check") if step.get("run") == "python scripts/canary.py check"]
    assert with_a_token[0]["env"] == {"GITHUB_TOKEN": "${{ github.token }}"}


@pytest.mark.parametrize("name, job", [(CANARY, "check"), (CLEANUP, "text")])
def test_on_a_schedule_and_by_hand_the_job_reads_dev_and_for_a_pull_request_it_reads_the_pull_request(name, job):
    checkout = steps(name, job)[0]
    assert checkout["uses"].startswith("actions/checkout@")
    assert checkout["with"] == {"ref": READS, "persist-credentials": False}


@BOTH
def test_every_checkout_leaves_no_token_on_the_disk(name):
    checkouts = [step for job in workflow(name)["jobs"].values() for step in job["steps"] if step.get("uses", "").startswith("actions/checkout@")]
    assert checkouts
    for step in checkouts:
        assert step["with"]["persist-credentials"] is False


@BOTH
def test_every_action_is_pinned_by_its_commit_and_is_one_that_another_workflow_uses_at_that_commit(name):
    elsewhere = {(action, commit) for other in WORKFLOWS.glob("*.yml") if other.name not in (CANARY, CLEANUP)
                 for action, commit, _rest in uses(other.name)}
    mine = uses(name)
    assert len(mine) >= 2
    for action, commit, rest in mine:
        assert re.fullmatch(r"[0-9a-f]{40}", commit), f"{action} is not pinned by a full commit id"
        assert re.fullmatch(r"\s+# v\d+\.\d+\.\d+", rest), f"{action} has no version beside its commit"
        assert (action, commit) in elsewhere, f"{action}@{commit} is used by no other workflow: it would be a new action"
    assert {action for action, _commit, _rest in uses(CANARY) + uses(CLEANUP)} == {
        "actions/checkout", "actions/setup-python", "actions/setup-go", "actions/upload-artifact", "actions/download-artifact"}


def test_the_text_is_kept_under_one_name_and_a_run_that_made_none_fails():
    upload = steps(CLEANUP, "text")[-1]
    download = steps(CLEANUP, "issue")[1]
    assert upload["with"] == {"name": "weekly-cleanup", "path": "${{ runner.temp }}/weekly-cleanup/", "if-no-files-found": "error"}
    assert download["with"] == {"name": "weekly-cleanup", "path": "${{ runner.temp }}/weekly-cleanup"}


def test_the_go_of_the_weekly_cleanup_is_the_go_of_the_go_test_jobs():
    (mine,) = [step["with"] for step in steps(CLEANUP, "text") if step.get("uses", "").startswith("actions/setup-go@")]
    tests = yaml.safe_load(text("test.yml"))["jobs"]
    theirs = {step["with"]["go-version"] for job in tests.values() for step in job.get("steps", []) if step.get("uses", "").startswith("actions/setup-go@")}
    assert theirs == {mine["go-version"]}
    assert mine["cache"] is False


@BOTH
def test_a_newer_run_of_the_same_branch_ends_the_older_one(name):
    assert workflow(name)["concurrency"] == {"group": name.removesuffix(".yml") + "-${{ github.ref }}", "cancel-in-progress": True}


def test_the_gates_page_lists_both_jobs_that_run_on_a_pull_request_and_the_job_with_a_condition():
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    not_required = page.split("### Not required", 1)[1].split("\n## ", 1)[0]
    assert "| `canary check (reads only)` |" in not_required
    assert "| `weekly cleanup (make the text)` |" in not_required
    assert "| `weekly-cleanup.yml` | `issue` |" in page
    for words in ("GitHub starts a scheduled run from the workflow file of the default branch\nonly",
                  "GitHub sends its mail for a\n  failed scheduled run to the account that last changed the `cron` line",
                  "\"Not judged\" is\n  never a pass", "A canary on the head of `dev` is not old"):
        assert words in page, words
