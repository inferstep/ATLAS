"""A job takes an action of this repository from the commit that its workflow file comes from.

For a pull request GitHub runs the workflow file of the merge commit. A job
that checks out another commit, as the two upload jobs do with the head
commit, has that commit's tree in its workspace. An action that is used from
that tree is the other commit's action: it can be older than the workflow
file, or not be there at all. So such a job takes its actions from a second
checkout: the workflow's own commit, in a folder of its own.
"""
from pathlib import Path

import pytest
import yaml

WORKFLOWS = Path(__file__).resolve().parents[2] / ".github" / "workflows"
OWN_COMMIT = (None, "${{ github.sha }}")
FIX = ("Fix: after that checkout, check out the workflow's own commit into a folder of its own (a checkout step with "
       "`path:` and no `ref:`, and `sparse-checkout: .github/actions`), and use the action from that folder.")


def folder_of(step: dict) -> str:
    """The folder a checkout step fills, as a path from the workspace: "" is the workspace itself."""
    return str((step.get("with") or {}).get("path") or "").strip("/")


def not_from_the_own_commit(job: dict) -> list[str]:
    """Each use of a local action in this job that is not taken from the workflow's own commit, with the reason."""
    found, folders = [], {}
    for number, step in enumerate(job.get("steps") or [], 1):
        uses = str(step.get("uses") or "")
        if uses.startswith("actions/checkout@"):
            folder, ref = folder_of(step), (step.get("with") or {}).get("ref")
            if not folder:
                # A checkout into the workspace clears it, and the folders that earlier steps filled with it.
                folders = {}
            folders[folder] = (number, ref)
        elif uses.startswith("./"):
            path = uses[2:].strip("/")
            holders = [folder for folder in folders if not folder or path == folder or path.startswith(folder + "/")]
            if not holders:
                found.append(f"step {number} uses {uses}, and no step before it checks out a commit")
                continue
            checkout, ref = folders[max(holders, key=len)]
            if ref not in OWN_COMMIT:
                found.append(f"step {number} uses {uses} from the tree that step {checkout} checks out with `ref: {ref}`")
    return found


def job(*steps):
    return {"steps": list(steps)}


def checkout(ref=None, path=None):
    settings = {key: value for key, value in (("ref", ref), ("path", path)) if value is not None}
    return {"uses": "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "with": settings}


HEAD = "${{ github.event.pull_request.head.sha || github.sha }}"
ACTION = {"uses": "./.github/actions/upload-coverage"}
IN_A_FOLDER = {"uses": "./workflow-commit/.github/actions/upload-coverage"}


@pytest.mark.parametrize("steps", [
    (checkout(), ACTION),
    (checkout(ref="${{ github.sha }}"), ACTION),
    (checkout(ref=HEAD), checkout(path="workflow-commit"), IN_A_FOLDER),
    (checkout(ref=HEAD), checkout(path="workflow-commit/"), {"uses": "actions/setup-go@b7ad1dad"}, IN_A_FOLDER),
    (checkout(ref=HEAD), {"uses": "codecov/codecov-action@303a32d7"}),
    (checkout(ref=HEAD), {"run": "ls ./.github/actions"}),
])
def test_an_action_from_the_workflows_own_commit_is_not_named(steps):
    assert not_from_the_own_commit(job(*steps)) == []


@pytest.mark.parametrize("steps, says", [
    ((checkout(ref=HEAD), ACTION), "step 2 uses ./.github/actions/upload-coverage from the tree that step 1 checks out with `ref: "),
    ((checkout(ref="star-history"), ACTION), "from the tree that step 1 checks out with `ref: star-history`"),
    # The folder with the workflow's own commit is filled first, and the checkout of the head then clears it.
    ((checkout(path="workflow-commit"), checkout(ref=HEAD), IN_A_FOLDER), "from the tree that step 2 checks out"),
    # The second checkout is there, and the action is still taken from the head's tree.
    ((checkout(ref=HEAD), checkout(path="workflow-commit"), ACTION), "from the tree that step 1 checks out"),
    # The folder of its own holds another commit too.
    ((checkout(ref=HEAD), checkout(ref=HEAD, path="workflow-commit"), IN_A_FOLDER), "from the tree that step 2 checks out"),
    # A folder whose name only starts the same is not the folder of the action.
    ((checkout(ref=HEAD), checkout(path="workflow"), IN_A_FOLDER), "from the tree that step 1 checks out"),
    ((ACTION,), "no step before it checks out a commit"),
    ((checkout(path="other"), ACTION), "no step before it checks out a commit"),
])
def test_an_action_from_another_commits_tree_is_named(steps, says):
    found = not_from_the_own_commit(job(*steps))
    assert len(found) == 1 and says in found[0], found


def local_action_jobs():
    """Each job of the repository's workflows that uses an action of the repository: (file, job, the job's settings)."""
    jobs = []
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        for name, settings in (yaml.safe_load(path.read_text(encoding="utf-8")).get("jobs") or {}).items():
            if any(str(step.get("uses") or "").startswith("./") for step in settings.get("steps") or []):
                jobs.append((path.name, name, settings))
    return jobs


def test_every_job_takes_its_local_actions_from_the_commit_of_its_workflow_file():
    jobs = local_action_jobs()
    assert {(file, name) for file, name, _settings in jobs} >= {
        ("test.yml", "coverage-upload"), ("test.yml", "test-results-upload")}, "the two upload jobs are not read"
    wrong = [f"{file}, job {name}: {finding}" for file, name, settings in jobs for finding in not_from_the_own_commit(settings)]
    assert not wrong, (
        "a job uses an action of this repository from the tree of another commit than the one its workflow file comes "
        "from:\n  " + "\n  ".join(wrong) + "\nOn a pull request whose branch does not have that action, or has an older "
        "one, the job fails or runs the other action. " + FIX)


@pytest.mark.parametrize("name", ["coverage-upload", "test-results-upload"])
def test_the_upload_jobs_keep_the_head_commit_in_the_workspace_and_take_only_the_actions_beside_it(name):
    steps = yaml.safe_load((WORKFLOWS / "test.yml").read_text(encoding="utf-8"))["jobs"][name]["steps"]
    checkouts = [step["with"] for step in steps if str(step.get("uses") or "").startswith("actions/checkout@")]
    assert len(checkouts) == 2, checkouts
    assert checkouts[0] == {"ref": HEAD, "persist-credentials": False}
    assert checkouts[1] == {"sparse-checkout": ".github/actions", "sparse-checkout-cone-mode": False,
                            "path": "workflow-commit", "persist-credentials": False}
