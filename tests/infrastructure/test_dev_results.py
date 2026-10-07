"""A push to dev takes the results of the merge queue's run and does not run the tests again.

The workflow that does this runs only on a push to dev, so most of what can
go wrong there is held here: the lookup of the queue's run, the shape of the
workflows, and the steps the two upload actions share.
"""
import importlib.util
import re
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
ACTIONS = ROOT / ".github" / "actions"
SHA = "a" * 40


def load(name):
    spec = importlib.util.spec_from_file_location(f"atlas_{name}", ROOT / "scripts" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


@pytest.fixture(scope="module")
def lookup():
    return load("queue_run")


def workflow(name):
    return yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))


def triggers(name):
    # PyYAML reads the key `on` as the boolean True.
    return workflow(name)[True]


# --- the lookup of the queue's run ------------------------------------------------------

def a_run(run_id=7, status="completed", conclusion="success", sha=SHA, created="2000-01-01T00:00:00Z"):
    return {"id": run_id, "status": status, "conclusion": conclusion, "head_sha": sha, "created_at": created}


def github(runs, jobs=()):
    """A stand-in for GitHub's API: the runs of a commit, and the jobs of a run. It keeps the paths asked for."""
    asked = []

    def read(path):
        asked.append(path)
        if "/jobs" in path:
            return [{"jobs": [{"name": name, "conclusion": conclusion} for name, conclusion in jobs]}]
        return [{"workflow_runs": list(runs)}]
    read.asked = asked
    return read


def test_the_queues_run_of_a_commit_is_found_by_commit_and_event(lookup):
    read = github([a_run(7)])
    assert lookup.queue_run(read, "o/r", SHA)["id"] == 7
    assert read.asked == [f"repos/o/r/actions/workflows/test.yml/runs?head_sha={SHA}&event=merge_group&per_page=100"]


def test_the_newest_of_several_runs_counts_and_a_run_of_another_commit_does_not(lookup):
    runs = [a_run(7), a_run(9, created="2000-01-02T00:00:00Z"), a_run(11, sha="b" * 40, created="2000-01-03T00:00:00Z")]
    assert lookup.queue_run(github(runs), "o/r", SHA)["id"] == 9


def test_a_commit_with_no_queue_run_fails_with_the_words_that_say_what_to_do(lookup):
    with pytest.raises(lookup.NoRun) as none:
        lookup.queue_run(github([]), "o/r", SHA)
    message = str(none.value)
    assert message.startswith("this commit did not come through the merge queue; start `tests` for it by hand")
    assert "merge-back" in message
    assert "Fix:" in message and "gh workflow run test.yml --ref" in message
    assert "artifact" not in message.lower()


class Clock:
    """A clock for the tests. It stops a wait that goes on past any limit a test sets, so that a lookup that
    lost its limit fails the test and does not run without end."""

    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now

    def pause(self, seconds):
        self.now += seconds
        assert self.now <= 86_400, "the lookup went on waiting for more than a day of its own clock"


def test_the_lookup_waits_until_the_queues_run_has_ended(lookup):
    answers = [[a_run(status="in_progress", conclusion=None)], [a_run(status="in_progress", conclusion=None)], [a_run()]]
    clock = Clock()

    def read(path):
        return [{"workflow_runs": answers.pop(0) if len(answers) > 1 else answers[0]}]
    run = lookup.ended(read, "o/r", SHA, limit=600, pause=clock.pause, clock=clock)
    assert (run["status"], clock.now) == ("completed", 2 * lookup.POLL_SECONDS)


def test_a_run_that_does_not_end_in_time_fails_and_says_so(lookup):
    clock = Clock()
    with pytest.raises(lookup.NoRun, match=r"has not ended after 10 minutes \(run 7\).*Fix:"):
        lookup.ended(github([a_run(status="in_progress", conclusion=None)]), "o/r", SHA, limit=600,
                     pause=clock.pause, clock=clock)
    assert clock.now == 600


@pytest.mark.parametrize("jobs, passed", [
    ([("go test (proxy)", "success"), ("go test (tui)", "success"), ("pytest (tests/v3)", "success")], True),
    ([("go test (proxy)", "success"), ("pytest (tests/v3)", "failure")], False),
    ([("go test (proxy)", "cancelled"), ("pytest (tests/v3)", "success")], False),
    # A job that makes no report does not decide whether the coverage is real.
    ([("go test (proxy)", "success"), ("pytest (tests/v3)", "success"), ("java sandbox smoke (containerized)", "failure")], True),
    # No report job at all: nothing shows that the tests passed.
    ([("shellcheck", "success")], False),
])
def test_coverage_counts_as_real_only_when_every_job_that_makes_a_report_passed(lookup, jobs, passed):
    assert lookup.tests_passed(github([], jobs), "o/r", a_run()) is passed


def test_the_jobs_that_did_not_pass_are_named_with_how_they_ended(lookup):
    jobs = [("go test (proxy)", "success"), ("pytest (tests/perf)", "failure"), ("go test (tui)", "cancelled"),
            ("java sandbox smoke (containerized)", "failure")]
    assert lookup.not_passed(github([], jobs), "o/r", a_run()) == ["go test (tui) (cancelled)", "pytest (tests/perf) (failure)"]
    assert lookup.not_passed(github([], [("go test (proxy)", "success")]), "o/r", a_run()) == []
    assert lookup.not_passed(github([], [("shellcheck", "success")]), "o/r", a_run()) == [
        "no job of that run makes a coverage report"]


def test_the_report_jobs_the_lookup_reads_are_the_jobs_the_coverage_upload_needs(lookup):
    jobs = workflow("test.yml")["jobs"]
    needed = [str(jobs[job_id]["name"]).split("${{")[0] for job_id in jobs["coverage-upload"]["needs"]]
    assert sorted(needed) == sorted(lookup.REPORT_JOBS)
    assert lookup.WORKFLOW == "test.yml"


# --- the tests workflow -------------------------------------------------------------------

def test_the_tests_workflow_does_not_start_on_a_push_to_dev_and_can_be_started_by_hand():
    on = triggers("test.yml")
    assert on["push"]["branches"] == ["main"]
    assert "dev" in on["pull_request"]["branches"] and "dev" in on["merge_group"]["branches"]
    assert "workflow_dispatch" in on


def test_no_test_job_has_a_condition():
    """A job with a condition is not judged for absence by `checks ran`. The two upload jobs are the only ones."""
    jobs = workflow("test.yml")["jobs"]
    assert sorted(job_id for job_id, job in jobs.items() if "if" in job) == ["coverage-upload", "test-results-upload"]


def test_a_run_started_by_hand_sends_its_results_and_saves_the_cache_as_a_push_run_does():
    jobs = workflow("test.yml")["jobs"]
    assert jobs["coverage-upload"]["if"] == "github.event_name != 'merge_group'"
    assert jobs["test-results-upload"]["if"] == "${{ !cancelled() && github.event_name != 'merge_group' }}"
    cache = next(step for step in jobs["go-tests"]["steps"] if str(step.get("uses", "")).startswith("actions/cache@"))
    assert cache["if"] == "github.event_name != 'merge_group'"


# --- the workflow of a push to dev --------------------------------------------------------

def test_the_results_workflow_runs_on_a_push_to_dev_and_on_nothing_else():
    assert triggers("dev-results.yml") == {"push": {"branches": ["dev"]}}
    assert workflow("dev-results.yml")["concurrency"] == {"group": "dev-results-${{ github.sha }}",
                                                         "cancel-in-progress": False}


def steps_of(name, job):
    return workflow(name)["jobs"][job]["steps"]


def test_the_results_job_finds_the_run_then_takes_its_reports_then_sends_them():
    steps = steps_of("dev-results.yml", "queue-results")
    order = [step.get("id") or step.get("uses", "").split("@")[0] or step.get("name") for step in steps]
    assert order == ["actions/checkout", "run", "actions/download-artifact", "the reports of that run are here",
                     "./.github/actions/upload-coverage", "coverage is not sent", "./.github/actions/upload-test-results"]
    assert steps[1]["run"] == 'python3 scripts/queue_run.py --commit "$GITHUB_SHA" --wait-minutes 30'
    assert steps[2]["with"]["run-id"] == "${{ steps.run.outputs.run-id }}"
    assert steps[2]["with"]["pattern"] == "coverage-*"
    assert steps[4]["if"] == "steps.run.outputs.tests-passed == 'true'"
    assert "if" not in steps[6]
    permissions = workflow("dev-results.yml")["jobs"]["queue-results"]["permissions"]
    assert permissions == {"contents": "read", "actions": "read", "id-token": "write"}


def test_a_failed_report_job_of_the_queues_run_is_a_notice_with_its_name_and_no_red_of_this_job():
    step = steps_of("dev-results.yml", "queue-results")[5]
    assert step["if"] == "steps.run.outputs.tests-passed != 'true'"
    assert step["env"] == {"NOT_PASSED": "${{ steps.run.outputs.not-passed }}"}
    assert step["run"].startswith('echo "::notice title=coverage not uploaded::')
    assert "$NOT_PASSED" in step["run"] and "exit" not in step["run"]
    assert "The test results are sent." in step["run"]


def test_no_report_of_the_queues_run_fails_the_job_and_does_not_read_as_nothing_to_send():
    step = steps_of("dev-results.yml", "queue-results")[3]
    assert 'if [ "${#found[@]}" -eq 0 ]; then' in step["run"]
    assert "::error title=no reports to send::" in step["run"] and "exit 1" in step["run"]
    assert "Fix:" in step["run"]
    assert "if" not in step and "continue-on-error" not in step


def cache_step(steps):
    return next(step for step in steps if str(step.get("uses", "")).startswith("actions/cache@"))


def test_the_cache_job_saves_under_the_key_and_folders_that_the_pull_request_jobs_read():
    mine, theirs = steps_of("dev-results.yml", "go-cache"), steps_of("test.yml", "go-tests")
    assert cache_step(mine)["with"] == cache_step(theirs)["with"]
    assert cache_step(mine)["uses"] == cache_step(theirs)["uses"]
    setup = [next(step for step in steps if str(step.get("uses", "")).startswith("actions/setup-go@")) for steps in (mine, theirs)]
    assert setup[0]["with"] == setup[1]["with"] and setup[0]["uses"] == setup[1]["uses"]
    points = [next(step["run"] for step in steps if "GOCACHE=" in str(step.get("run", ""))) for steps in (mine, theirs)]
    assert points[0] == points[1]
    jobs = workflow("dev-results.yml")["jobs"]
    assert jobs["go-cache"]["strategy"]["matrix"] == workflow("test.yml")["jobs"]["go-tests"]["strategy"]["matrix"]


def test_the_cache_job_builds_only_when_no_cache_was_found():
    build = steps_of("dev-results.yml", "go-cache")[-1]
    assert build["if"] == "steps.cache.outputs.cache-hit != 'true'"
    assert build["run"] == 'python3 scripts/go_test_build.py "$MODULE"'
    assert cache_step(steps_of("dev-results.yml", "go-cache"))["id"] == "cache"


def test_the_build_for_the_cache_is_the_test_gates_own_command_with_no_test_to_run(tmp_path):
    build, gates = load("go_test_build"), load("production-readiness")
    for module in ("proxy", "tui"):
        command, folder, env = build.build_command(module, str(tmp_path))
        gate = gates._gates(())[f"go-{module}-test"]
        assert command[:2] == ["go", "test"] and command[-3:] == ["-run", "^$", "./..."]
        assert "-json" not in command
        without = [part for part in command[:-3] + ["./..."] if not part.startswith("-cover")]
        assert without == list(gate.command), "the build flags differ from the gate's"
        assert f"-coverprofile={tmp_path.resolve() / ('go-' + module + '.out')}" in command and "-covermode=atomic" in command
        assert folder == ROOT / module
        assert "GOCACHE" in env


# --- the upload steps, in one place ---------------------------------------------------------

def uses_of(name):
    return [str(step.get("uses", "")) for job in workflow(name)["jobs"].values() for step in job.get("steps") or []]


def test_both_workflows_send_through_the_same_two_actions():
    for name in ("test.yml", "dev-results.yml"):
        local = [use for use in uses_of(name) if use.startswith("./")]
        assert local == ["./.github/actions/upload-coverage", "./.github/actions/upload-test-results"], name
        assert not [use for use in uses_of(name) if use.startswith("codecov/")], f"{name} sends by itself"


@pytest.mark.parametrize("action, flags, kind", [
    ("upload-coverage", ["go-proxy", "go-tui", "python"], None),
    ("upload-test-results", ["go-proxy", "go-tui", "python"], "test_results"),
])
def test_an_upload_action_sends_each_report_with_its_flag_and_fails_when_an_upload_is_refused(action, flags, kind):
    steps = yaml.safe_load((ACTIONS / action / "action.yml").read_text(encoding="utf-8"))["runs"]["steps"]
    uploads = [step for step in steps if str(step.get("uses", "")).startswith("codecov/codecov-action@")]
    assert [step["with"]["flags"] for step in uploads] == flags
    for step in uploads:
        assert step["with"]["fail_ci_if_error"] is True and step["with"]["use_oidc"] is True
        assert step["with"].get("report_type") == kind
        assert "if" in step, "an upload with no report to send must be left out, and say so"


@pytest.mark.parametrize("path", sorted(ACTIONS.glob("*/action.yml")), ids=lambda path: path.parent.name)
def test_an_action_of_this_repository_keeps_the_rules_of_a_workflow(path):
    """What actionlint and the checkout rule hold for a workflow file, for a file they do not read."""
    text = path.read_text(encoding="utf-8")
    steps = yaml.safe_load(text)["runs"]["steps"]
    for step in steps:
        use = str(step.get("uses", ""))
        if use:
            assert re.fullmatch(r"[\w.-]+/[\w./-]+@[0-9a-f]{40}", use), f"{use} is not pinned to a commit"
            assert not use.startswith("actions/checkout@"), "an action must not check out: its job does, with its own rule"
        else:
            assert step.get("shell") == "bash", f"the step `{step.get('name')}` names no shell"
    for line in text.splitlines():
        if re.search(r"uses: \S+@[0-9a-f]{40}", line):
            assert re.search(r"#\s*v\d+\.\d+\.\d+\s*$", line), f"the pin has no version beside it: {line.strip()}"
    assert "${{ inputs." not in "".join(str(step.get("run", "")) for step in steps), (
        "an input is pasted into a shell line. Fix: give it to the step in `env:` and use the variable.")


# --- what else depends on a tests run for a commit -----------------------------------------

def test_the_promotion_finds_the_tests_run_by_commit_and_names_no_event():
    """On dev that run is the merge queue's. A lookup by event or by branch would find none."""
    promote = workflow("build-images.yml")["jobs"]["promote"]
    wait = next(step["run"] for step in promote["steps"] if "gh run list" in str(step.get("run", "")))
    assert '--workflow test.yml' in wait and '--commit "${GITHUB_SHA}"' in wait
    assert "--event" not in wait and "--branch" not in wait
    assert 'if [ "$conclusion" = "success" ]; then' in wait
    assert "start the workflow tests by hand" in wait


def test_the_extensions_results_job_has_nothing_to_send_when_no_file_was_made_and_is_not_red_for_it():
    steps = steps_of("vscode-extension.yml", "test-results-upload")
    download = next(step for step in steps if str(step.get("uses", "")).startswith("actions/download-artifact@"))
    assert download["with"].get("pattern") == "coverage-typescript" and "name" not in download["with"]
    assert download["with"].get("merge-multiple") is True
    check = next(step for step in steps if step.get("id") == "results")
    assert "::notice title=no test results for typescript::" in check["run"]
    upload = next(step for step in steps if str(step.get("uses", "")).startswith("codecov/"))
    assert upload["if"] == "steps.results.outputs.typescript != ''"


# --- the check of a pull request that changes any of this ----------------------------------

def test_a_pull_request_that_changes_the_results_path_tries_the_lookup_and_sends_nothing():
    on = triggers("dev-results-check.yml")
    assert sorted(on) == ["pull_request"]
    assert sorted(on["pull_request"]["paths"]) == sorted([
        ".github/workflows/dev-results.yml", ".github/workflows/dev-results-check.yml", ".github/actions/**",
        "scripts/queue_run.py"])
    job = workflow("dev-results-check.yml")["jobs"]["lookup"]
    assert job["permissions"] == {"contents": "read", "actions": "read"}
    uses = [str(step.get("uses", "")) for step in job["steps"]]
    assert not [use for use in uses if use.startswith(("codecov/", "./"))], "the check must send nothing"
    lookup_step = next(step for step in job["steps"] if step.get("id") == "run")
    assert "scripts/queue_run.py" in lookup_step["run"] and "--plain" in lookup_step["run"]
    assert 'git rev-list --first-parent -n 5 "$BASE"' in lookup_step["run"]
