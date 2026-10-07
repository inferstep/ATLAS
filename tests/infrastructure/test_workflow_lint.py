"""The workflow files keep the rules that zizmor and actionlint check in CI.

The two tools run in their own jobs. These tests hold what does not need the
tools: each tool is a download held against a recorded checksum, and every
checkout step drops the job's token unless its line says why it keeps it.
"""
import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = sorted((ROOT / ".github" / "workflows").glob("*.yml"))
LINT = ROOT / ".github" / "workflows" / "workflow-lint.yml"
KEEPS_TOKEN = "# zizmor: ignore[artipacked]"


def checkout_steps(text: str) -> list[dict]:
    """Every actions/checkout step of a workflow, as the mapping of its keys."""
    jobs = (yaml.safe_load(text) or {}).get("jobs") or {}
    return [step for job in jobs.values() for step in job.get("steps") or []
            if str(step.get("uses", "")).startswith("actions/checkout@")]


def keeps_token_with_a_reason(text: str) -> int:
    """How many lines keep the token and carry the marker, with a comment line of reason above."""
    lines = text.splitlines()
    return sum(1 for n, line in enumerate(lines)
               if re.match(r"\s*persist-credentials: true\s+" + re.escape(KEEPS_TOKEN), line)
               and n > 0 and lines[n - 1].strip().startswith("# "))


def checkout_problems(name: str, text: str) -> list[str]:
    steps = checkout_steps(text)
    kept = [step for step in steps if (step.get("with") or {}).get("persist-credentials") is not False]
    if len(kept) == keeps_token_with_a_reason(text):
        return []
    return [(f"{name}: {len(kept)} checkout step(s) keep the job's token in the checkout's git settings, and "
             f"{keeps_token_with_a_reason(text)} of them say why. Fix: add `persist-credentials: false` under `with:` "
             f"of the step. If a later step must push with the token, write `persist-credentials: true  {KEEPS_TOKEN}` "
             "with a comment line above it that gives the reason.")]


def test_there_are_workflows_to_read():
    assert len(WORKFLOWS) >= 20


@pytest.mark.parametrize("path", WORKFLOWS, ids=lambda path: path.name)
def test_every_checkout_step_drops_the_token_or_says_why_it_keeps_it(path):
    assert checkout_problems(path.name, path.read_text(encoding="utf-8")) == []


STEP = "jobs:\n  one:\n    steps:\n      - uses: actions/checkout@0123  # v1\n"


@pytest.mark.parametrize("step, found", [
    (STEP, 1),
    (STEP + "        with:\n          fetch-depth: 0\n", 1),
    (STEP + "        with:\n          persist-credentials: true\n", 1),
    (STEP + "        with:\n          persist-credentials: true  # zizmor: ignore[artipacked]\n", 1),
    (STEP + "        with:\n          persist-credentials: false\n", 0),
    (STEP + "        with:\n          # The job pushes to this branch.\n"
            "          persist-credentials: true  # zizmor: ignore[artipacked]\n", 0),
    ("jobs:\n  one:\n    steps:\n      - uses: actions/setup-go@0123  # v1\n", 0),
])
def test_the_checkout_rule_names_a_step_that_keeps_the_token(step, found):
    assert len(checkout_problems("x.yml", step)) == found


def test_a_checkout_problem_says_how_to_fix_it():
    message = checkout_problems("x.yml", STEP)[0]
    assert message.startswith("x.yml: 1 checkout step(s) keep the job's token")
    assert "Fix: add `persist-credentials: false`" in message


@pytest.mark.parametrize("tool, version", [("ZIZMOR", r"v\d+\.\d+\.\d+"), ("ACTIONLINT", r"\d+\.\d+\.\d+")])
def test_each_tool_is_held_against_a_recorded_checksum(tool, version):
    text = LINT.read_text(encoding="utf-8")
    assert re.search(rf"^ +{tool}_VERSION: {version}$", text, re.MULTILINE)
    assert re.search(rf"^ +{tool}_SHA256: [0-9a-f]{{64}}$", text, re.MULTILINE)
    assert re.search(rf'echo "\$\{{{tool}_SHA256\}}  \S+" \| sha256sum --check --status', text)


def test_a_finding_fails_each_job():
    jobs = yaml.safe_load(LINT.read_text(encoding="utf-8"))["jobs"]
    assert set(jobs) == {"zizmor", "actionlint"}
    for name, binary in (("zizmor", "zizmor"), ("actionlint", "actionlint")):
        run = jobs[name]["steps"][-1]["run"]
        assert re.search(rf'if ! "\$RUNNER_TEMP/bin/{binary}" [^\n]*"\$\{{files\[@\]\}}"; then', run)
        assert run.count("exit 1") == 2
        assert "continue-on-error" not in jobs[name]
        assert "continue-on-error" not in jobs[name]["steps"][-1]


def test_the_jobs_read_the_workflow_files_and_no_folder_below_them():
    text = LINT.read_text(encoding="utf-8")
    assert text.count("files=(.github/workflows/*.yml .github/workflows/*.yaml)") == 1
    assert text.count("files=(.github/workflows/*.yml .github/workflows/*.yaml .github/actions/*/action.yml)") == 1


def test_zizmor_reads_the_actions_of_this_repository_too():
    """A workflow runs the steps of a local action as its own, so the same rules hold for them.

    actionlint reads workflow files only. What it would check in an action
    file is held by zizmor and by tests/infrastructure/test_dev_results.py.
    """
    jobs = yaml.safe_load(LINT.read_text(encoding="utf-8"))["jobs"]
    assert ".github/actions/*/action.yml" in jobs["zizmor"]["steps"][-1]["run"]
    assert ".github/actions" not in jobs["actionlint"]["steps"][-1]["run"]
    assert sorted((ROOT / ".github" / "actions").glob("*/action.yml")), "no action file is there to read"


def test_the_lint_jobs_have_a_read_only_token():
    workflow = yaml.safe_load(LINT.read_text(encoding="utf-8"))
    assert workflow["permissions"] == {"contents": "read"}
    assert all("permissions" not in job for job in workflow["jobs"].values())
