#!/usr/bin/env python3
"""Plant the canary's violations, and check that each one still turns its check red.

A check that silently stops checking looks the same as a check that passes.
The canary is one draft pull request that is never merged. Its branch is a
copy of `dev` plus one harmless violation for each check: a failing test, a
lint error, a function that is too long, and so on (.github/canary.json).
Every listed check must be red on it. One that is green has stopped checking.

  scripts/canary.py plant            write the violations into this checkout
  scripts/canary.py check --pr N     compare the checks of pull request N with the list

`plant` runs only on the canary branch. `check` reads GitHub and changes
nothing. Its exit status: 0 when every listed check is as the list says, 1
when one is not, 2 when the pull request or its checks cannot be read.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import NamedTuple

ROOT = Path(__file__).resolve().parent.parent
MANIFEST = Path(".github") / "canary.json"
RENEW = "Renew the canary (docs/quality/gates.md, section \"The canary\")."


class Finding(NamedTuple):
    check: str
    message: str


class PlantError(Exception):
    """A violation could not be written the way the list describes it."""


def load_manifest(root: Path) -> dict:
    return json.loads((root / MANIFEST).read_text(encoding="utf-8"))


def load_checks_ran():
    """scripts/checks_ran.py, for its GitHub reader and its reading of the branch rules."""
    spec = importlib.util.spec_from_file_location("atlas_checks_ran", Path(__file__).with_name("checks_ran.py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


# --- plant ---------------------------------------------------------------------

def target(root: Path, relative: str) -> Path:
    path = (root / relative).resolve()
    if not path.is_relative_to(root.resolve()):
        raise PlantError(f"{relative} is outside the checkout. Fix: use a path inside the repository in {MANIFEST}.")
    return path


LONG_FUNCTION_LINES = (101, 1000)


def long_function(length: int, language: str = "python") -> list[str]:
    low, high = LONG_FUNCTION_LINES
    if not isinstance(length, int) or not low <= length <= high:
        raise PlantError(f"the planted function must have {low} to {high} lines, and the list asks for {length!r}. "
                         f"Fix: set `length` in {MANIFEST} to a number in that range.")
    if language == "typescript":
        body = [f"    total += {number};" for number in range(length)]
        return ["// Planted for the canary pull request. Never merge it.", "",
                "export function canaryLongFunction(): number {", "    let total = 0;", *body, "    return total;", "}"]
    if language != "python":
        raise PlantError(f"the planted function can be written in python or typescript, and the list asks for "
                         f"{language!r}. Fix: set `language` in {MANIFEST} to one of the two.")
    body = [f"    total += {number}" for number in range(length)]
    return ['"""Planted for the canary pull request. Never merge it."""', "", "",
            "def canary_long_function():", "    total = 0", *body, "    return total"]


def existing_text(path: Path, plant: dict) -> str:
    if not path.is_file():
        raise PlantError(f"{plant['path']} does not exist, so `{plant['id']}` cannot be planted. "
                         f"Fix: point the entry in {MANIFEST} at the file that took its place.")
    return path.read_text(encoding="utf-8")


def planted_text(path: Path, plant: dict, planted: str | None = None) -> str:
    """The whole new content of the file this plant writes.

    `planted` is what an earlier plant of the list made of the same file. An
    edit goes on from there, so that two plants can share a file.
    """
    action, block = plant["action"], "\n".join(plant.get("lines", [])) + "\n"
    if action in ("write", "long_function"):
        if path.exists() or planted is not None:
            raise PlantError(f"{plant['path']} exists already. Fix: start the canary branch from a clean copy of "
                             "`dev`; the violations are written once.")
        if action == "long_function":
            return "\n".join(long_function(plant["length"], plant.get("language", "python"))) + "\n"
        return block
    text = planted if planted is not None else existing_text(path, plant)
    if action == "append":
        return text + ("" if text.endswith("\n") else "\n") + block
    if action == "insert_after_first_line":
        first, _, rest = text.partition("\n")
        return first + "\n" + block + rest
    if action == "replace_line":
        lines = text.split("\n")
        found = [number for number, line in enumerate(lines) if line.startswith(plant["starts_with"])]
        if len(found) != 1:
            raise PlantError(f"{len(found)} line(s) of {plant['path']} start with `{plant['starts_with']}`, and "
                             f"`{plant['id']}` needs exactly one. Fix: change `starts_with` in {MANIFEST} to fit the "
                             "file as it is now.")
        lines[found[0]] = block.rstrip("\n")
        return "\n".join(lines)
    raise PlantError(f"`{plant['id']}` has the unknown action `{action}`. Fix: use write, append, "
                     f"insert_after_first_line, replace_line, long_function or title in {MANIFEST}.")


def apply_plants(root: Path, manifest: dict) -> list[str]:
    """Write every violation into the checkout at root. Returns the paths written."""
    contents = {}
    for plant in manifest["plants"]:
        if plant["action"] == "title":
            continue
        path = target(root, plant["path"])
        contents[path] = planted_text(path, plant, contents.get(path))
    for path, text in contents.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    return [str(path.relative_to(root.resolve())) for path in contents]


def current_branch(root: Path) -> str:
    done = subprocess.run(["git", "-C", str(root), "rev-parse", "--abbrev-ref", "HEAD"],
                          capture_output=True, text=True, check=False)
    return done.stdout.strip()


def plant(root: Path) -> int:
    manifest = load_manifest(root)
    branch = current_branch(root)
    if branch != manifest["branch"]:
        print(f"canary: this checkout is on `{branch}`, and the violations belong only on `{manifest['branch']}`.\n"
              f"  fix: git checkout -B {manifest['branch']} origin/dev, then run this again.", file=sys.stderr)
        return 2
    try:
        written = apply_plants(root, manifest)
    except (PlantError, OSError, KeyError) as error:
        print(f"canary: nothing was planted: {error}", file=sys.stderr)
        return 2
    subprocess.run(["git", "-C", str(root), "add", "--", *written], check=False)
    for path in written:
        print(f"planted {path}")
    print(f"canary: {len(written)} file(s) written and staged. Commit them, push the branch, and give the pull "
          f"request the title `{manifest['title']}`.")
    return 0


# --- check ---------------------------------------------------------------------

def latest_checks(check_runs: list[dict]) -> dict[str, dict]:
    """The newest check run of each name."""
    latest = {}
    for run in sorted(check_runs, key=lambda r: (r.get("started_at") or "", r["id"])):
        latest[run["name"]] = run
    return latest


def state_finding(name: str, run: dict | None, what: str) -> Finding | None:
    """Why a check that must have run to an end on the canary did not, or None."""
    if run is None:
        return Finding(name, f"check `{name}` did not run on the canary, so nothing shows that it still catches {what}. "
                             f"Fix: open the checks of the canary pull request. If the job was renamed, change the "
                             f"name in {MANIFEST}. If its workflow did not start, fix the workflow.")
    if run.get("status") != "completed":
        return Finding(name, f"check `{name}` has not finished on the canary. Fix: run this again when the checks "
                             "of the canary pull request have ended.")
    return None


def shown_text(plant_entry: dict, name: str) -> str:
    """The text that the log of check `name` holds when the check is red for this plant."""
    shows = plant_entry["shows"]
    return shows[name] if isinstance(shows, dict) else shows


def red_finding(name: str, run: dict | None, plant_entry: dict) -> Finding | None:
    what = plant_entry["what"]
    problem = state_finding(name, run, what)
    if problem:
        return problem
    if run.get("conclusion") == "failure":
        text = shown_text(plant_entry, name)
        if text in run.get("log", ""):
            return None
        return Finding(name, f"check `{name}` is red on the canary, but not for its plant: its log does not hold "
                             f"`{text}`, which {what} gives. A check that is red for another cause shows nothing. "
                             f"Fix: open the job `{name}` on the canary pull request and read why it failed. For a "
                             "fault of the network or the runner, run the job again. If the check words its message "
                             f"in another way now, change `shows` of this entry in {MANIFEST}.")
    if run.get("conclusion") == "success":
        return Finding(name, f"check `{name}` passed on the canary, where {what} is planted for it. The check no "
                             f"longer catches that. Fix: find what changed in the job `{name}` (its workflow, its "
                             "script or its settings) and restore the check. Then renew the canary and run this again.")
    return Finding(name, f"check `{name}` ended as `{run.get('conclusion')}` on the canary, so it judged nothing. "
                         f"Fix: open the job `{name}` on the canary pull request, remove the cause, and run it again.")


def report_finding(name: str, run: dict | None, plant_entry: dict) -> Finding | None:
    """A check that reports and does not fail must have a note on the planted file. Any other note does not count."""
    problem = state_finding(name, run, plant_entry["what"])
    if problem or plant_entry["path"] in run.get("annotation_paths", ()):
        return problem
    return Finding(name, f"check `{name}` reported nothing for {plant_entry['path']} on the canary, where "
                         f"{plant_entry['what']} is planted for it. The check no longer reports that. Fix: find what "
                         f"changed in the job `{name}` and restore the report.")


def pull_findings(manifest: dict, pull: dict) -> list[Finding]:
    """The canary pull request itself: the right branch, open, a draft, with the title the list names."""
    out, name = [], "the canary pull request"
    if pull["head"]["ref"] != manifest["branch"]:
        out.append(Finding(name, f"pull request #{pull['number']} is on branch `{pull['head']['ref']}`, and the canary "
                                 f"is `{manifest['branch']}`. Fix: pass the number of the canary pull request."))
    if pull["state"] != "open":
        out.append(Finding(name, f"pull request #{pull['number']} is {pull['state']}, so its checks no longer run. "
                                 f"Fix: open a new draft pull request from `{manifest['branch']}`. {RENEW}"))
    elif not pull.get("draft"):
        out.append(Finding(name, f"pull request #{pull['number']} is not a draft. It must never be merged. "
                                 "Fix: set it back to draft."))
    if pull["title"] != manifest["title"]:
        out.append(Finding(name, f"its title is `{pull['title']}`, and the list expects `{manifest['title']}`, the "
                                 "title that turns `pr title` red. Fix: set the title back."))
    return out


def coverage_findings(manifest: dict, required: list[str], ran: list[str]) -> list[Finding]:
    """Every required check, and every check that ran on the canary, has a plant or is listed with the reason it has none."""
    red = {name for plant in manifest["plants"] for name in plant.get("red", [])}
    reports = {name for plant in manifest["plants"] for name in plant.get("reports", [])}
    out = [Finding(name, f"required check `{name}` is not in the canary list, so nothing shows that it can turn red. "
                         f"Fix: add a violation for it to {MANIFEST}, or add it under `not_covered` with the reason.")
           for name in required if name not in red | set(manifest["not_covered"])]
    known = (red | reports | set(manifest["not_covered"]) | set(manifest["other_checks"])
             | set(manifest["side_effects"]) | set(required))
    return out + [Finding(name, f"check `{name}` ran on the canary and the list does not know it, so nothing says "
                                f"whether it can turn red. Fix: add a violation for it to {MANIFEST}, or add it under "
                                "`not_covered` with the reason why it has none.")
                  for name in sorted(set(ran)) if name not in known]


def is_red(run: dict) -> bool:
    return run.get("status") == "completed" and run.get("conclusion") == "failure"


# How a check ends when it ran and did not pass, or could not start.
NOT_PASSED = ("failure", "timed_out", "cancelled", "startup_failure")


def did_not_pass(run: dict) -> bool:
    return run.get("status") == "completed" and run.get("conclusion") in NOT_PASSED


def side_effect_findings(manifest: dict, runs: dict[str, dict]) -> list[Finding]:
    """A check with no violation of its own is red only where the list says so, and the list says so only where it is."""
    out = []
    for name, run in sorted(runs.items()):
        if name in manifest["side_effects"]:
            if run.get("status") == "completed" and not is_red(run):
                out.append(Finding(name, f"check `{name}` is listed as red on the canary through another check's "
                                         f"violation, and it ended as `{run.get('conclusion')}`. Fix: if that is how "
                                         f"it is now, move it in {MANIFEST} from `side_effects` to `not_covered`, "
                                         "with its reason."))
        elif did_not_pass(run) and (name in manifest["not_covered"] or name in manifest["other_checks"]):
            out.append(Finding(name, f"check `{name}` ended as `{run.get('conclusion')}` on the canary, and the list "
                                     "gives it no violation and no side effect. A check that does not pass with no "
                                     f"cause on the list can hide a fault. Fix: open the job `{name}` on the canary "
                                     "pull request and read why it did not pass. If the violation of another check "
                                     f"makes it red, add it to `side_effects` in {MANIFEST} and say by which path. "
                                     "Else repair the job."))
    return out


def notes(manifest: dict, runs: dict[str, dict]) -> list[str]:
    """What is as the list says and still worth a line: a red through another check's violation, and a listed check that is not there."""
    out = [f"red through another check's violation, as listed: `{name}`. {reason}"
           for name, reason in sorted(manifest["side_effects"].items()) if name in runs and is_red(runs[name])]
    listed = {**manifest["not_covered"], **manifest["other_checks"], **manifest["side_effects"]}
    return out + [f"listed, and not there in this run: `{name}`. {reason}"
                  for name, reason in sorted(listed.items()) if name not in runs]


def age_finding(manifest: dict, base_date: str, now: datetime) -> list[Finding]:
    age = (now - datetime.fromisoformat(base_date.replace("Z", "+00:00"))).days
    if age <= manifest["max_age_days"]:
        return []
    return [Finding("the canary branch", f"the canary was last renewed from `dev` {age} days ago (limit "
                                         f"{manifest['max_age_days']}), so it shows the checks of that day and not "
                                         f"today's. Fix: {RENEW}")]


def judge(manifest: dict, pull: dict, check_runs: list[dict], required: list[str], base_date: str,
          now: datetime) -> list[Finding]:
    """Every difference between the canary pull request and the list."""
    runs, findings = latest_checks(check_runs), pull_findings(manifest, pull)
    for plant_entry in manifest["plants"]:
        for name in plant_entry.get("red", []):
            findings.append(red_finding(name, runs.get(name), plant_entry))
        for name in plant_entry.get("reports", []):
            findings.append(report_finding(name, runs.get(name), plant_entry))
    findings += (coverage_findings(manifest, required, list(runs)) + side_effect_findings(manifest, runs)
                 + age_finding(manifest, base_date, now))
    return [finding for finding in findings if finding]


def token() -> str:
    found = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if found:
        return found
    try:
        done = subprocess.run(["gh", "auth", "token"], capture_output=True, text=True, check=False)
    except OSError:
        return ""
    return done.stdout.strip() if done.returncode == 0 else ""


def repository(root: Path) -> str:
    """owner/name, from GITHUB_REPOSITORY or the `origin` remote."""
    if os.environ.get("GITHUB_REPOSITORY"):
        return os.environ["GITHUB_REPOSITORY"]
    done = subprocess.run(["git", "-C", str(root), "remote", "get-url", "origin"],
                          capture_output=True, text=True, check=False)
    found = re.search(r"github\.com[:/]([^/]+/[^/]+?)(?:\.git)?$", done.stdout.strip())
    if not found:
        raise RuntimeError("the repository is not known: set GITHUB_REPOSITORY to owner/name")
    return found.group(1)


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def job_log(api_root: str, repo: str, job: int, key: str) -> str:
    """The log of one job, as text.

    GitHub answers with the address of the file, on another host. That
    address is read without the token.
    """
    request = urllib.request.Request(f"{api_root}/repos/{repo}/actions/jobs/{job}/logs",
                                     headers={"Authorization": f"Bearer {key}", "Accept": "application/vnd.github+json"})
    try:
        with urllib.request.build_opener(_NoRedirect).open(request, timeout=30) as response:
            return response.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as error:
        address = error.headers.get("Location", "") if error.code in (301, 302, 303, 307, 308) else ""
        if not address.startswith("https://"):
            raise RuntimeError(f"GitHub did not give the log of job {job}: {error}") from error
    try:
        with urllib.request.urlopen(address, timeout=60) as response:
            return response.read().decode("utf-8", "replace")
    except (urllib.error.URLError, TimeoutError) as error:
        raise RuntimeError(f"the log of job {job} could not be read: {error}") from error


def gather(root: Path, number: int, manifest: dict):
    """Read the pull request, its check runs, the required checks of its base and the date of its base commit."""
    checks, key, repo = load_checks_ran(), token(), repository(root)
    if not key:
        raise RuntimeError("no GitHub token: set GITHUB_TOKEN, or sign in with `gh auth login`")
    pull = checks.api(f"repos/{repo}/pulls/{number}", key)[0]
    sha, base = pull["head"]["sha"], pull["base"]["ref"]
    check_runs = [run for page in checks.api(f"repos/{repo}/commits/{sha}/check-runs?per_page=100", key)
                  for run in page["check_runs"]]
    reporting = {name for entry in manifest["plants"] for name in entry.get("reports", [])}
    red = {name for entry in manifest["plants"] for name in entry.get("red", [])}
    for run in check_runs:
        if run["name"] in reporting:
            notes = checks.api(f"repos/{repo}/check-runs/{run['id']}/annotations?per_page=100", key)
            run["annotation_paths"] = [note["path"] for page in notes for note in page]
        if run["name"] in red and run.get("conclusion") == "failure":
            # The log says why the check is red. A check run of a workflow job has the job's number.
            run["log"] = job_log(checks.API, repo, run["id"], key)
    rules = [rule for page in checks.api(f"repos/{repo}/rules/branches/{base}?per_page=100", key) for rule in page]
    compared = checks.api(f"repos/{repo}/compare/{base}...{sha}", key)[0]
    return pull, check_runs, checks.required_names(rules, {}), compared["merge_base_commit"]["commit"]["committer"]["date"]


def check(root: Path, number: int) -> int:
    manifest = load_manifest(root)
    try:
        pull, check_runs, required, base_date = gather(root, number, manifest)
        findings = judge(manifest, pull, check_runs, required, base_date, datetime.now(timezone.utc))
    except (KeyError, ValueError, OSError, RuntimeError) as error:
        print(f"canary: cannot read pull request #{number} or its checks: {error!r}\n"
              "  fix: pass the number of the canary pull request, with a token that can read the repository "
              "(GITHUB_TOKEN, or `gh auth login`).", file=sys.stderr)
        return 2
    red = sum(len(entry.get("red", [])) for entry in manifest["plants"])
    reports = sum(len(entry.get("reports", [])) for entry in manifest["plants"])
    no_plant = len(manifest["not_covered"]) + len(manifest["other_checks"])
    print(f"note pull request #{number} at {pull['head']['sha'][:7]}: {red} check(s) must be red for their plant, "
          f"{reports} must report the planted file, {no_plant} check(s) have no plant, each with its reason")
    for line in notes(manifest, latest_checks(check_runs)):
        print(f"note {line}")
    for finding in findings:
        print(f"FAIL {finding.message}")
    print(f"canary: {len(findings)} thing(s) are not as the list says" if findings
          else "canary: every listed check is red for its plant, and every listed report is there")
    return int(bool(findings))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--root", type=Path, default=ROOT, help="the checkout to read or to plant in")
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("plant", help="write the violations into this checkout (canary branch only)")
    checker = commands.add_parser("check", help="compare the checks of the canary pull request with the list")
    checker.add_argument("--pr", type=int, required=True, help="number of the canary pull request")
    args = parser.parse_args()
    return plant(args.root) if args.command == "plant" else check(args.root, args.pr)


if __name__ == "__main__":
    raise SystemExit(main())
