"""A run for a pushed commit of dev or main is not cancelled by the next push, where its results are kept.

Each workflow that starts on a push to dev is on one of two lists here: the
run of every pushed commit ends, or the newest push cancels the older run,
with the reason. A new workflow fails this test until it is on a list.
"""
from pathlib import Path

import pytest
import yaml

WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
# Their results are kept for each commit: coverage and the result of each test.
# Each with the first part of its group's name.
EVERY_PUSH_ENDS = {"test.yml": "tests", "vscode-extension.yml": "vscode-extension"}
# The result for the newest commit takes the place of an older one.
NEWEST_PUSH_CANCELS = {
    "build-images.yml": "the images of the newest commit take the place of the older ones",
    "codeql.yml": "the analysis of the newest commit holds every finding of the older ones",
    "install-test.yml": "it tests the installer as it is now; an older commit's result is not kept",
    "sonar.yml": "the analysis of the newest commit takes the place of the older one",
}


def read(name: str) -> dict:
    return yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))


def starts_on_a_push_to_dev(workflow: dict) -> bool:
    # PyYAML reads the key `on` as the boolean True.
    push = (workflow.get(True) or {}).get("push")
    return isinstance(push, dict) and "dev" in (push.get("branches") or [])


@pytest.mark.parametrize("name", sorted(EVERY_PUSH_ENDS))
def test_a_pushed_commit_has_a_group_of_its_own_and_is_not_cancelled(name):
    concurrency = read(name)["concurrency"]
    by_commit = "${{ github.event_name == 'push' && github.sha || github.ref }}"
    assert concurrency["group"] == f"{EVERY_PUSH_ENDS[name]}-{by_commit}"
    assert concurrency["cancel-in-progress"] == "${{ github.event_name != 'push' }}"


@pytest.mark.parametrize("name", sorted(NEWEST_PUSH_CANCELS))
def test_where_the_newest_push_cancels_the_group_is_the_branch(name):
    concurrency = read(name)["concurrency"]
    assert concurrency["group"].endswith("-${{ github.ref }}")
    assert concurrency["cancel-in-progress"] is True


def test_every_workflow_that_starts_on_a_push_to_dev_is_on_one_of_the_two_lists():
    on_push = sorted(path.name for path in WORKFLOWS.glob("*.yml") if starts_on_a_push_to_dev(read(path.name)))
    undecided = [name for name in on_push if name not in EVERY_PUSH_ENDS and name not in NEWEST_PUSH_CANCELS]
    assert undecided == [], (
        f"{undecided} start on a push to dev and are on no list in this test. Fix: add each to EVERY_PUSH_ENDS when "
        "its results are kept for each commit (then give it the group by commit), or to NEWEST_PUSH_CANCELS with the "
        "reason why an older run can be cancelled.")
    assert sorted([*EVERY_PUSH_ENDS, *NEWEST_PUSH_CANCELS]) == on_push
