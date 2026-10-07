"""A run for a pushed commit of dev or main is not cancelled by the next push, where its results are kept.

Each workflow that starts on a push to dev or main is on one of two lists
here: the run of every pushed commit ends, or the newest push cancels the
older run, with the reason. A new workflow fails this test until it is on a
list.
"""
from pathlib import Path

import pytest
import yaml

WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
# Their results are kept for each commit: coverage and the result of each test.
# Each with its concurrency group. A workflow that also runs for pull
# requests has the commit in its group only for a push.
BY_COMMIT_FOR_A_PUSH = "${{ github.event_name == 'push' && github.sha || github.ref }}"
EVERY_PUSH_ENDS = {
    "test.yml": ("tests-" + BY_COMMIT_FOR_A_PUSH, "${{ github.event_name != 'push' }}"),
    "vscode-extension.yml": ("vscode-extension-" + BY_COMMIT_FOR_A_PUSH, "${{ github.event_name != 'push' }}"),
    # It starts on a push only, so its group is the commit and it never cancels.
    "dev-results.yml": ("dev-results-${{ github.sha }}", False),
}
# The result for the newest commit takes the place of an older one.
NEWEST_PUSH_CANCELS = {
    "build-images.yml": "the images of the newest commit take the place of the older ones",
    "codeql.yml": "the analysis of the newest commit holds every finding of the older ones",
    "install-test.yml": "it tests the installer as it is now; an older commit's result is not kept",
    "sonar.yml": "the analysis of the newest commit takes the place of the older one",
}


# No concurrency group at all: no run of it cancels another.
NO_GROUP = {
    "scorecard.yml": "it starts on a push to main only, once a week, and by hand; its runs do not get in each other's way",
}


def read(name: str) -> dict:
    return yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))


def starts_on_a_push_to_a_branch(workflow: dict) -> bool:
    """Whether the workflow starts on a push to dev or to main."""
    # PyYAML reads the key `on` as the boolean True.
    push = (workflow.get(True) or {}).get("push")
    return isinstance(push, dict) and bool({"dev", "main"} & set(push.get("branches") or []))


@pytest.mark.parametrize("name", sorted(EVERY_PUSH_ENDS))
def test_a_pushed_commit_has_a_group_of_its_own_and_is_not_cancelled(name):
    concurrency = read(name)["concurrency"]
    group, cancels = EVERY_PUSH_ENDS[name]
    assert concurrency["group"] == group
    assert concurrency["cancel-in-progress"] == cancels


@pytest.mark.parametrize("name", sorted(NEWEST_PUSH_CANCELS))
def test_where_the_newest_push_cancels_the_group_is_the_branch(name):
    concurrency = read(name)["concurrency"]
    assert concurrency["group"].endswith("-${{ github.ref }}")
    assert concurrency["cancel-in-progress"] is True


@pytest.mark.parametrize("name", sorted(NO_GROUP))
def test_a_workflow_listed_with_no_group_has_none(name):
    assert "concurrency" not in read(name)
    assert NO_GROUP[name].strip()


def test_every_workflow_that_starts_on_a_push_to_dev_or_main_is_on_one_of_the_lists():
    on_push = sorted(path.name for path in WORKFLOWS.glob("*.yml") if starts_on_a_push_to_a_branch(read(path.name)))
    undecided = [name for name in on_push if name not in {**EVERY_PUSH_ENDS, **NEWEST_PUSH_CANCELS, **NO_GROUP}]
    assert undecided == [], (
        f"{undecided} start on a push to dev or main and are on no list in this test. Fix: add each to EVERY_PUSH_ENDS when "
        "its results are kept for each commit (then give it the group by commit), or to NEWEST_PUSH_CANCELS with the "
        "reason why an older run can be cancelled.")
    assert sorted([*EVERY_PUSH_ENDS, *NEWEST_PUSH_CANCELS, *NO_GROUP]) == on_push
