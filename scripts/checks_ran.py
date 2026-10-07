#!/usr/bin/env python3
"""Fail when a check of this change did not really run.

A workflow that fails to start shows no check at all, and a job that is
skipped reports success, so a change can look green with a check missing.
This reads the workflow files of the change, the runs GitHub recorded for its
commit and the checks the base branch requires, waits for the runs to end,
and reports:

  - a workflow that should have started and has no run, or failed to start;
  - a job with no condition that was skipped, was cancelled or is absent;
  - a required check that no job ran to an end.

Each finding says what did not run, why that matters and how to fix it.

Exit status: 0 when every check ran, 1 when one did not, 2 when the change,
the workflow files or the runs could not be read.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable, NamedTuple

import yaml

API = "https://api.github.com"
WORKFLOW_DIR = ".github/workflows"
DEFAULT_TYPES = {"pull_request": ("opened", "synchronize", "reopened"), "merge_group": ("checks_requested",)}
FILTER_KEYS = ("branches", "branches-ignore", "paths", "paths-ignore")
RAN_TO_AN_END = ("success", "failure", "timed_out")
# What GitHub gives a run of a pull request from a fork until a maintainer
# approves it. Such a run has no job yet.
WAITS_FOR_APPROVAL = "action_required"
EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
POLL_SECONDS = 60
# Runs of one commit are created within seconds of each other. A workflow
# with no run after this long is counted as not started.
GRACE_SECONDS = 180


class Change(NamedTuple):
    event: str
    action: str
    sha: str
    base_sha: str
    branch: str


class Finding(NamedTuple):
    path: str
    message: str


def change_from_event(event: str, payload: dict) -> Change:
    """The commit, base and action of a pull_request or merge_group event."""
    if event == "pull_request":
        pull = payload["pull_request"]
        return Change(event, payload["action"], pull["head"]["sha"], pull["base"]["sha"], pull["base"]["ref"])
    if event == "merge_group":
        group = payload["merge_group"]
        branch = group["base_ref"].removeprefix("refs/heads/")
        return Change(event, payload.get("action", "checks_requested"), group["head_sha"], group["base_sha"], branch)
    raise ValueError(f"event {event!r} is not one this check reads (pull_request, merge_group)")


def triggers(workflow: dict) -> dict:
    """The `on:` block as {event: its settings}. YAML reads a bare `on` as True."""
    on = workflow.get("on", workflow.get(True))
    if isinstance(on, str):
        return {on: {}}
    if isinstance(on, list):
        return {name: {} for name in on}
    return {name: settings or {} for name, settings in (on or {}).items()}


def glob_regex(pattern: str) -> re.Pattern | None:
    """A regex for a branch or path filter, or None for a form this does not read."""
    if re.search(r"[?+\[\]!]", pattern):
        return None
    out = ""
    for part in re.split(r"(\*\*/|\*\*|\*)", pattern):
        out += {"**/": "(?:.*/)?", "**": ".*", "*": "[^/]*"}.get(part, re.escape(part))
    return re.compile(out)


def as_list(value) -> list:
    return [value] if isinstance(value, str) else list(value or [])


def starts_on(workflow: dict, change: Change, changed: list[str]) -> bool | None:
    """Whether GitHub starts this workflow for the change. None: a filter this cannot read."""
    settings = triggers(workflow).get(change.event)
    if settings is None:
        return False
    if change.action not in as_list(settings.get("types", DEFAULT_TYPES.get(change.event, (change.action,)))):
        return False
    filters = {key: [glob_regex(p) for p in as_list(settings.get(key))] for key in FILTER_KEYS}
    if any(regex is None for regexes in filters.values() for regex in regexes):
        return None

    def hit(key: str, value: str) -> bool:
        return any(regex.fullmatch(value) for regex in filters[key])

    return ((not filters["branches"] or hit("branches", change.branch))
            and not hit("branches-ignore", change.branch)
            and (not filters["paths"] or any(hit("paths", path) for path in changed))
            and (not filters["paths-ignore"] or any(not hit("paths-ignore", path) for path in changed)))


def conditional(job_id: str, jobs: dict, seen: tuple = ()) -> bool:
    """A job that may skip by design: it has an `if:`, or it needs a job that has one."""
    job = jobs.get(job_id) or {}
    if "if" in job:
        return True
    return any(conditional(needed, jobs, seen + (job_id,)) for needed in as_list(job.get("needs")) if needed not in seen)


def name_regex(job_id: str, job: dict) -> tuple[re.Pattern, int]:
    """Matches the names GitHub reports for this job, one per matrix leg; and how much of a name it fixes."""
    name = job.get("name")
    if name:
        literal = EXPRESSION.split(str(name))
        return re.compile(".+".join(map(re.escape, literal)) + r"(?: / .+)?"), sum(map(len, literal))
    return re.compile(re.escape(job_id) + r"(?: \(.+\))?(?: / .+)?"), len(job_id)


def jobs_by_definition(jobs: dict, reported: list[dict]) -> dict[str, list[dict]]:
    """Each reported job under the job that defines it: the one whose name fixes most of it."""
    patterns = {job_id: name_regex(job_id, job or {}) for job_id, job in jobs.items()}
    out = {job_id: [] for job_id in jobs}
    for record in reported:
        owners = [(fixed, job_id) for job_id, (regex, fixed) in patterns.items() if regex.fullmatch(record["name"])]
        if owners:
            out[max(owners)[1]].append(record)
    return out


def latest_runs(runs: list[dict], event: str, own_id: int) -> dict[str, dict]:
    """The newest run of each workflow for this event, by workflow file."""
    latest = {}
    for run in sorted(runs, key=lambda r: (r["created_at"], r["id"])):
        if run["event"] == event and run["id"] != own_id:
            latest[run["path"].split("@")[0]] = run
    return latest


def workflow_findings(path: str, run: dict | None, event: str) -> list[Finding]:
    if run is None:
        return [Finding(path, f"workflow {path} has no run for this commit. It starts on {event}, so its checks are "
                              "missing, not passed. Fix: open the Actions tab for this commit. If the workflow is "
                              "listed with an error, fix the cause it names. If it is not listed, check its `on:` block.")]
    if run.get("conclusion") == "startup_failure":
        return [Finding(path, f"workflow {path} failed to start, so none of its jobs ran and it shows no check. The "
                              "usual causes are an action the repository does not allow and a syntax error in the "
                              f"file. Fix: open {run.get('html_url', 'the run')} and fix the cause it names.")]
    return []


def quoted(names) -> str:
    return ", ".join(f"`{name}`" for name in sorted(set(names)))


def job_findings(path: str, workflow: dict, reported: list[dict]) -> tuple[list[Finding], list[str]]:
    """Findings for the jobs with no condition, and the names of the jobs skipped by their own condition."""
    jobs = workflow.get("jobs") or {}
    absent, skipped, cancelled, by_condition = [], [], [], []
    for job_id, mine in jobs_by_definition(jobs, reported).items():
        if conditional(job_id, jobs):
            by_condition += [record["name"] for record in mine if record.get("conclusion") == "skipped"]
            continue
        if not mine:
            absent.append((jobs[job_id] or {}).get("name") or job_id)
        skipped += [record["name"] for record in mine if record.get("conclusion") == "skipped"]
        cancelled += [record["name"] for record in mine if record.get("conclusion") == "cancelled"]
    rule = "A job with no `if:` condition must run on every change."
    findings = []
    if absent:
        findings.append(Finding(path, f"{len(absent)} job(s) in {path} reported nothing: {quoted(absent)}. {rule} "
                                      "Fix: open the run and see why the job did not start."))
    if skipped:
        findings.append(Finding(path, f"{len(skipped)} job(s) in {path} were skipped: {quoted(skipped)}. {rule} Such a "
                                      "job is skipped when a job it `needs` did not pass. Fix: make that job pass, "
                                      "then run this check again."))
    if cancelled:
        findings.append(Finding(path, f"{len(cancelled)} job(s) in {path} were cancelled: {quoted(cancelled)}. {rule} "
                                      "Fix: run the workflow again, then run this check again."))
    return findings, by_condition


def required_findings(required: list[str], reported: list[dict], branch: str) -> list[Finding]:
    """A required check that was skipped, or that no job reported. The branch rules do not stop either."""
    states = {name: {record.get("conclusion") for record in reported if record["name"] == name} for name in required}
    skipped = [name for name, seen in states.items() if seen and seen <= {"skipped"}]
    absent = [name for name, seen in states.items() if not seen]
    findings = []
    if skipped:
        findings.append(Finding("", f"{len(skipped)} required check(s) were skipped: {quoted(skipped)}. The rules of "
                                    f"`{branch}` count a skipped job as passed, so the change could merge without "
                                    "them. Fix: remove the condition that skips the job for this event."))
    if absent:
        findings.append(Finding("", f"{len(absent)} required check(s) were reported by no job: {quoted(absent)}. The "
                                    f"rules of `{branch}` wait for exactly these names. Fix: give the job its name "
                                    "again; if a rename is intended, a repository admin must change the name in the rules."))
    return findings


def required_names(rules: list[dict], own_workflow: dict) -> list[str]:
    """The check names the branch rules require, without the jobs of the workflow that runs this check."""
    own = [name_regex(job_id, job or {})[0] for job_id, job in (own_workflow.get("jobs") or {}).items()]
    names = [item["context"] for rule in rules if rule.get("type") == "required_status_checks"
             for item in rule["parameters"]["required_status_checks"]]
    return [name for name in names if not any(regex.fullmatch(name) for regex in own)]


def merge_runs(seen: dict[str, dict], listed: dict[str, dict]) -> dict[str, dict]:
    """The runs the listings showed so far: a run once shown is kept, and the newer run of a workflow counts."""
    merged = dict(seen)
    for path, run in listed.items():
        known = merged.get(path)
        if known is None or (run["created_at"], run["id"]) >= (known["created_at"], known["id"]):
            merged[path] = run
    return merged


def settle(read_runs: Callable[[], dict], expected: list[str], limit: float,
           pause: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic) -> tuple[dict, list[str]]:
    """Poll until every expected workflow has a finished run. Returns the runs and the workflows still running."""
    start, runs, listings = clock(), {}, 0
    while True:
        listed = read_runs()
        # A listing can leave out a run that exists. One that an earlier
        # listing showed is not missing, so its last known state is kept.
        left_out = [path for path in expected if path in runs and path not in listed]
        runs, listings, waited = merge_runs(runs, listed), listings + 1, clock() - start
        missing = [path for path in expected if path not in runs]
        running = [path for path in expected if path in runs and runs[path].get("status") != "completed"]
        print(f"note listing {listings} after {waited:.0f}s: {len(expected) - len(missing)} of {len(expected)} expected "
              f"workflow(s) listed, {len(running)} running, {len(missing)} not listed", flush=True)
        if left_out:
            print(f"note listing {listings} left out {len(left_out)} run(s) that an earlier listing showed "
                  f"({', '.join(left_out)}); their last known state is kept", flush=True)
        if not running and (not missing or waited >= GRACE_SECONDS):
            return runs, []
        if waited >= limit:
            return runs, running
        pause(POLL_SECONDS)


def api(path: str, token: str) -> list:
    """Every page of a GitHub API answer, decoded."""
    url, pages = f"{API}/{path}", []
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    while url:
        for attempt in range(3):
            try:
                with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30) as response:
                    pages.append(json.load(response))
                    link = response.headers.get("Link", "")
                break
            except (urllib.error.URLError, TimeoutError) as error:
                if attempt == 2 or (isinstance(error, urllib.error.HTTPError) and error.code < 500):
                    raise RuntimeError(f"GitHub did not answer {url}: {error}") from error
                time.sleep(10)
        following = re.search(r'<([^>]+)>;\s*rel="next"', link)
        url = following.group(1) if following else ""
    return pages


def read_workflows(root: Path) -> dict[str, dict]:
    return {f"{WORKFLOW_DIR}/{file.name}": yaml.safe_load(file.read_text(encoding="utf-8")) or {}
            for file in sorted((root / WORKFLOW_DIR).glob("*.y*ml"))}


def changed_files(root: Path, base_sha: str) -> list[str]:
    done = subprocess.run(["git", "diff", "--name-only", "--no-renames", "--end-of-options", f"{base_sha}...HEAD", "--"],
                          cwd=root, capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise RuntimeError(f"git cannot compare HEAD with the base commit {base_sha} ({done.stderr.strip()}); "
                           "the checkout needs the full history (fetch-depth: 0)")
    return done.stdout.splitlines()


def expected_workflows(workflows: dict[str, dict], change: Change, changed: list[str], own: str) -> tuple[list[str], list[str]]:
    """The workflow files that must have a run, and those whose filters this cannot read."""
    expected, unread = [], []
    for path, workflow in workflows.items():
        if path == own:
            continue
        verdict = starts_on(workflow, change, changed)
        if verdict is None:
            unread.append(path)
        elif verdict:
            expected.append(path)
    return expected, unread


def check(workflows: dict[str, dict], expected: list[str], runs: dict[str, dict], jobs_of: Callable[[dict], list[dict]],
          required: list[str], change: Change) -> tuple[list[Finding], list[str], int]:
    """Every finding, the jobs skipped by their own condition, and the number of jobs that ran to an end."""
    findings, by_condition, reported = [], [], []
    for path in expected:
        run = runs.get(path)
        problems = workflow_findings(path, run, change.event)
        findings += problems
        if problems:
            continue
        jobs = jobs_of(run)
        reported += jobs
        found, skipped = job_findings(path, workflows[path], jobs)
        findings += found
        by_condition += skipped
    findings += required_findings(required, reported, change.branch)
    return findings, by_condition, sum(1 for job in reported if job.get("conclusion") in RAN_TO_AN_END)


def judge(workflows: dict[str, dict], expected: list[str], runs: dict[str, dict], running: list[str],
          jobs_of: Callable[[dict], list[dict]], required: list[str], change: Change) -> tuple[list[Finding], list[str], list[str], int]:
    """The findings, the workflows that wait for approval, the jobs skipped by their own condition, and the
    number of jobs that ran to an end.

    A workflow that is still running has reported only some of its jobs, and
    one that waits for a maintainer's approval has reported none. Neither its
    jobs nor the required checks can be judged then, so they are not.
    """
    waiting = [path for path in expected if (runs.get(path) or {}).get("conclusion") == WAITS_FOR_APPROVAL]
    settled = [path for path in expected if path not in running and path not in waiting]
    findings, by_condition, ran = check(workflows, settled, runs, jobs_of, [] if running or waiting else required, change)
    return findings, waiting, by_condition, ran


def waiting_message(path: str) -> str:
    return (f"workflow {path} waits for a maintainer's approval, so none of its jobs has run and its checks have no "
            "result yet. Nothing in the change causes this: GitHub holds the runs of a pull request from a fork "
            "until a maintainer lets them start. If you opened the pull request, there is nothing for you to do. "
            "A maintainer: approve the waiting runs on the pull request (\"Approve and run\"), then run this "
            "check again. It gives its verdict then.")


def outcome(findings: list[Finding], waiting: list[str]) -> tuple[int, str]:
    """The exit status and the last line. The check passes only when every check ran; a run that waits is no pass."""
    if findings and waiting:
        return 1, (f"checks ran: {len(findings)} check(s) did not run, and {len(waiting)} workflow(s) wait for a "
                   "maintainer's approval")
    if waiting:
        return 1, f"checks ran: no verdict yet, {len(waiting)} workflow(s) wait for a maintainer's approval"
    return int(bool(findings)), (f"checks ran: {len(findings)} check(s) did not run" if findings
                                 else "checks ran: every check ran")


def report(findings: list[Finding], notes: list[str], github: bool, waiting: tuple | list = ()) -> int:
    for note in notes:
        print(f"note {note}")
    for path in waiting:
        print(f"WAIT {waiting_message(path)}")
        if github:
            print(f"::warning file={path},title=waiting for a maintainer's approval::{waiting_message(path)}")
    for finding in findings:
        print(f"FAIL {finding.message}")
        if github:
            where = f"file={finding.path}," if finding.path else ""
            print(f"::error {where}title=a check did not run::{finding.message}")
    status, line = outcome(findings, list(waiting))
    print(line)
    return status


def gather(root: Path, own_workflow: str, limit: float):
    """Read the change, wait for its runs, and return everything `check` needs."""
    token, repo = os.environ.get("GITHUB_TOKEN", ""), os.environ["GITHUB_REPOSITORY"]
    payload = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
    change = change_from_event(os.environ["GITHUB_EVENT_NAME"], payload)
    workflows = read_workflows(root)
    expected, unread = expected_workflows(workflows, change, changed_files(root, change.base_sha), own_workflow)
    own_id = int(os.environ.get("GITHUB_RUN_ID", "0"))

    def read_runs() -> dict:
        pages = api(f"repos/{repo}/actions/runs?head_sha={change.sha}&per_page=100", token)
        return latest_runs([run for page in pages for run in page["workflow_runs"]], change.event, own_id)

    def jobs_of(run: dict) -> list[dict]:
        pages = api(f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100", token)
        return [job for page in pages for job in page["jobs"]]

    runs, running = settle(read_runs, expected, limit)
    rules = [rule for page in api(f"repos/{repo}/rules/branches/{change.branch}?per_page=100", token) for rule in page]
    required = required_names(rules, workflows.get(own_workflow) or {})
    return change, workflows, expected, unread, runs, running, jobs_of, required


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--root", type=Path, default=Path.cwd(), help="checkout of the change")
    parser.add_argument("--own-workflow", default="", help="the workflow file that runs this check; it is not waited for")
    parser.add_argument("--wait-minutes", type=float, default=60, help="how long to wait for the other runs to end")
    parser.add_argument("--github", action="store_true", help="also print the findings as annotations")
    args = parser.parse_args()
    try:
        change, workflows, expected, unread, runs, running, jobs_of, required = gather(
            args.root, args.own_workflow, args.wait_minutes * 60)
        findings, waiting, by_condition, ran = judge(workflows, expected, runs, running, jobs_of, required, change)
    except (KeyError, ValueError, OSError, RuntimeError, yaml.YAMLError) as error:
        print(f"checks ran: cannot read the change, its workflows or its runs: {error!r}\n"
              "  fix: this check runs in a pull_request or merge_group job, in a checkout with the full history, "
              "with GITHUB_TOKEN set and `actions: read`.", file=sys.stderr)
        return 2
    findings += [Finding(path, f"workflow {path} did not finish in {args.wait_minutes:g} minutes, so its jobs and the "
                               "required checks are not judged. Fix: run this check again after that run ends.")
                 for path in running]
    notes = [(f"{len(expected)} workflow(s) expected for {change.event} on `{change.branch}`; {ran} job(s) ran to an "
              f"end; {len(required)} required check(s)")]
    if by_condition:
        notes.append(f"skipped by their own `if:` condition (not judged): {', '.join(sorted(set(by_condition)))}")
    notes += [f"{path} is not judged: its `on:` filters use a form this check does not read" for path in unread]
    return report(findings, notes, args.github, waiting)


if __name__ == "__main__":
    raise SystemExit(main())
