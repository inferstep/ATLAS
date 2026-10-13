#!/usr/bin/env python3
"""Find the merge queue's run of the `tests` workflow for one commit, and wait until it has ended.

A commit reaches `dev` through the merge queue, which runs `tests` on it. The
push of that commit does not run the tests again; a job takes the coverage
and the test results that the queue's run kept, and sends them. This finds
that run.

It prints, for the job that called it (in GITHUB_OUTPUT when that is set):
  run-id        the number of the run
  conclusion    how the run ended
  tests-passed  true when every job that makes a coverage report passed
  not-passed    the names of those jobs that did not pass, or what else is wrong

Exit status: 0 when the run is found and has ended; 1 when the commit has no
such run, or the run did not end in time; 2 when GitHub could not be read.

Usage: queue_run.py --commit SHA [--wait-minutes 30] [--plain]
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from typing import Callable

API = "https://api.github.com"

WORKFLOW = "test.yml"
# The jobs of that workflow whose reports the coverage upload takes: it sends
# coverage only when each of them passed.
REPORT_JOBS = ("go test (", "pytest (")
POLL_SECONDS = 30
NOT_FROM_THE_QUEUE = "this commit did not come through the merge queue; start `tests` for it by hand"


class NoRun(Exception):
    """The commit has no run to take results from, or the run did not end in time."""


def api(path: str, token: str) -> list:
    """One page of a GitHub API answer, decoded, in a list. The questions asked here have one page: a commit has
    a few runs, and a run of this workflow has fewer than 100 jobs."""
    request = urllib.request.Request(f"{API}/{path}", headers={
        "Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "Authorization": f"Bearer {token}"})
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return [json.load(response)]
        except (urllib.error.URLError, TimeoutError) as error:
            if attempt == 2 or (isinstance(error, urllib.error.HTTPError) and error.code < 500):
                raise RuntimeError(f"GitHub did not answer {path}: {error}") from error
            time.sleep(10)
    return []


def queue_run(read: Callable[[str], list], repo: str, commit: str) -> dict:
    """The newest run of the workflow that the merge queue started for this commit."""
    pages = read(f"repos/{repo}/actions/workflows/{WORKFLOW}/runs?head_sha={commit}&event=merge_group&per_page=100")
    runs = [run for page in pages for run in page.get("workflow_runs", []) if run.get("head_sha") == commit]
    if not runs:
        raise NoRun(f"{NOT_FROM_THE_QUEUE}. Commit {commit[:12]} has no run of {WORKFLOW} that the merge queue "
                    "started, so there are no results to send for it. That is so for a commit of the release "
                    "step's merge-back. Fix: open the Actions tab, choose the workflow `tests`, and run it on this "
                    f"branch (or: gh workflow run {WORKFLOW} --ref <branch>). That run tests the commit and sends its "
                    "own results.")
    return max(runs, key=lambda run: (run["created_at"], run["id"]))


def ended(read: Callable[[str], list], repo: str, commit: str, limit: float,
          pause: Callable[[float], None] = time.sleep, clock: Callable[[], float] = time.monotonic) -> dict:
    """That run, once it has ended. The queue merges when the required checks passed, so other jobs may still run."""
    start = clock()
    while True:
        run = queue_run(read, repo, commit)
        if run.get("status") == "completed":
            return run
        if clock() - start >= limit:
            raise NoRun(f"the merge queue's run of {WORKFLOW} for commit {commit[:12]} has not ended after "
                        f"{limit / 60:g} minutes (run {run['id']}), so its results cannot be taken yet. Fix: run "
                        "this job again when that run has ended.")
        pause(POLL_SECONDS)


def not_passed(read: Callable[[str], list], repo: str, run: dict) -> list[str]:
    """The jobs of that run that make a coverage report and did not pass. Empty when every one of them passed.

    A run with no such job has nothing that shows its tests passed, and that is said in place of a name.
    """
    jobs = [job for page in read(f"repos/{repo}/actions/runs/{run['id']}/jobs?per_page=100")
            for job in page.get("jobs", []) if job["name"].startswith(REPORT_JOBS)]
    if not jobs:
        return ["no job of that run makes a coverage report"]
    return sorted(f"{job['name']} ({job.get('conclusion')})" for job in jobs if job.get("conclusion") != "success")


def tests_passed(read: Callable[[str], list], repo: str, run: dict) -> bool:
    """Whether every job that makes a coverage report passed in that run. At least one such job must be there."""
    return not not_passed(read, repo, run)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--commit", required=True, help="the commit to find the merge queue's run for")
    parser.add_argument("--wait-minutes", type=float, default=30, help="how long to wait for that run to end")
    parser.add_argument("--plain", action="store_true", help="say why there is no run as a plain line, not as an error")
    args = parser.parse_args()
    token, repo = os.environ.get("GITHUB_TOKEN", ""), os.environ.get("GITHUB_REPOSITORY", "")
    if not token or not repo:
        print("queue run: GITHUB_TOKEN or GITHUB_REPOSITORY is not set, so GitHub cannot be read. Fix: run this in "
              "a job with `actions: read` and pass the token in GITHUB_TOKEN.", file=sys.stderr)
        return 2

    def read(path: str) -> list:
        return api(path, token)
    try:
        run = ended(read, repo, args.commit, args.wait_minutes * 60)
        failed = not_passed(read, repo, run)
    except NoRun as error:
        print(str(error) if args.plain else f"::error title=no results of the merge queue for this commit::{error}")
        return 1
    except (KeyError, ValueError, OSError, RuntimeError) as error:
        print(f"queue run: GitHub could not be read: {error!r}. Fix: run the job again; if it fails again, check "
              "that the job has `actions: read`.", file=sys.stderr)
        return 2
    lines = [f"run-id={run['id']}", f"conclusion={run.get('conclusion')}",
             f"tests-passed={'false' if failed else 'true'}", f"not-passed={', '.join(failed)}"]
    print("\n".join(lines))
    output = os.environ.get("GITHUB_OUTPUT")
    if output:
        with open(output, "a", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
