#!/usr/bin/env python3
"""Whether a pull request that changes the text every request carries has its smoke result.

The replay job compares each request that the proxy sends with a recorded one,
in full. So a change to text that the model reads turns replay red until the
recordings are made again, and "a recording changed" marks such a change.

This check reads the recordings of the base and of the change as it would
land: the merge of the pull request into its base (`--texts`), so that a
branch made on an older base is judged by its own change only. In each it
takes the first request to the model, and of it the part that every request
carries: the system prompt (with the tool descriptions), the grammar, and the
schema of the reply. When such a part has other values than on the base and
none that the base had, the text of every request changed. A part that no
request carries any more, and a part that is new in every request, count
too. Then a session with a real model has to
show that sessions still run: the smoke run. The development server makes
it for a commit that a maintainer pushed to a branch `smoke/<name>`, and
writes its result on that commit as the status `server/smoke`.

The check is green when the newest such status on the head commit of the
pull request is green. It counts only a status that the server's own
account wrote (WRITER), and it compares the id of that account, not a name.
A status of that name by another account counts for nothing, and the output
names it. A red status of the server is red. A line in the text of the pull
request that says a smoke run passed is not read.

The status is for one commit. A new commit of the pull request needs a new
smoke run.

When the server for the smoke run is not available, the text of the pull
request says so in one line, with the mark of the text:

    No smoke run: the server is not available. Not tried with a real model; the first nightly run after the server is back covers this text (text <mark>).

The mark is made from the text that every request carries. Such a change is
then not tried with a real model before it merges. The replay tests are no
check of it: its recordings are written again from the new text, so they
pass. The line counts only while the server wrote no status for the commit:
a commit that the server tried has its result.

For any other change of a recording nothing is asked: the recording shows
the text in its situation, and the replay job holds it.

A status that comes later does not start this check again. The person who
waits for it starts the job again by hand ("Re-run jobs").

Usage: smoke_result.py --base <commit> --head <commit> [--texts <commit>]
                                    (the text of the pull request in PR_BODY, and a token that can read
                                    statuses in GITHUB_TOKEN; the recordings of --texts are judged, those of
                                    --head without it, and the statuses are read on --head)
       smoke_result.py --mark <commit>                   (prints the mark of that commit's text)

Exit status: 0 when nothing is asked, or the status of the server is green,
or the line for a server that is not available holds; 1 when the smoke
result is missing or red; 2 when the recordings or the statuses cannot be
read, or the head has no recording.
"""
from __future__ import annotations

import argparse
import hashlib
import http.client
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

RECORDINGS = "tests/replay/recordings/"
# The parts of a request to the model that every request of a session carries, and what each is called.
PARTS = ("the system prompt", "the grammar", "the schema of the reply")
# The status that the development server writes on a commit that it made the smoke run for, and the id of the account
# that writes it: `atlas-server-results[bot]`, the account of the server's own GitHub App. With 0 here no account is
# set, and no status is counted.
REPOSITORY, GITHUB_API = "inferstep/ATLAS", "https://api.github.com"
STATUS, WRITER = "server/smoke", 340202947
# A line that says a smoke run passed. It is not read as a result; the output says so when one is there.
TYPED = re.compile(r"^Smoke run on [0-9a-f]{7,40}: ", re.MULTILINE)
NO_SERVER_LINE = ("No smoke run: the server is not available. Not tried with a real model; the first nightly run after "
                  "the server is back covers this text (text {mark}).")
# The text of a pull request that was typed in GitHub's web form has CR LF at the end of each line. So a CR may stand
# before the end of the line.
NO_SERVER = re.compile("^" + re.escape(NO_SERVER_LINE).replace(re.escape("{mark}"), "([0-9a-f]{12})") + r"[ \t]*\r?$", re.MULTILINE)
AGAIN = ("A status that comes later does not start this check again: open this job and choose \"Re-run jobs\", or "
         "edit the text of the pull request")
HOW = ("Fix: ask a maintainer for the smoke run of the head commit of this pull request. The maintainer reads the "
       "whole change and pushes that commit to a branch `smoke/<name>`; the development server runs it and writes "
       f"the status `{STATUS}` on the commit. {AGAIN}. Do not change a text to make the smoke run pass: after two "
       "red smoke runs the change stops and is thought over. When the server is not available, a maintainer writes "
       "this line into the text of the pull request: \"" + NO_SERVER_LINE + "\"")
FETCH = "the checkout of this job needs the base commit and the head commit (fetch-depth 0)"


class Unreadable(Exception):
    """The recordings of a commit, or its statuses, cannot be read. `fix` says what to do about it."""

    def __init__(self, what: str, fix: str = FETCH, of: str = "the recordings"):
        super().__init__(what)
        self.fix, self.of = fix, of


def git(*args: str) -> str:
    done = subprocess.run(["git", *args], capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise Unreadable(f"git {' '.join(args[:2])} ended with status {done.returncode}: {done.stderr.strip()[-300:]}")
    return done.stdout


def carried_by_every_request(recording: dict) -> dict:
    """The parts of the first request to the model in a recording that every request of the session carries."""
    for exchange in recording.get("exchanges") or []:
        if exchange.get("service") == "model" and str(exchange.get("path", "")).endswith("/chat/completions"):
            request = json.loads(exchange["request"])
            first = (request.get("messages") or [{}])[0]
            return {"the system prompt": first.get("content") if first.get("role") == "system" else None,
                    "the grammar": request.get("grammar"),
                    "the schema of the reply": request.get("response_format")}
    return {}


def texts_at(commit: str, must_have_recordings: bool = False) -> dict:
    """For each part, the values that the recordings of a commit have for it, each once, as text."""
    names = [name for name in git("ls-tree", "-r", "--name-only", commit, "--", RECORDINGS).splitlines() if name.endswith(".json")]
    if must_have_recordings and not names:
        raise Unreadable(f"{commit[:12]} has no recording under {RECORDINGS}, so nothing shows what its requests carry",
                         "keep the recordings. A change that takes every recording away takes the replay tests away "
                         "with them, and is not one for this job to pass")
    found = {part: set() for part in PARTS}
    for name in names:
        try:
            parts = carried_by_every_request(json.loads(git("show", f"{commit}:{name}")))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            raise Unreadable(f"{name} at {commit[:12]} is not a recording that can be read: {error!r}") from None
        for part, value in parts.items():
            if value is not None:
                found[part].add(json.dumps(value, sort_keys=True, ensure_ascii=False))
    return {part: sorted(values) for part, values in found.items()}


def mark_of(texts: dict) -> str:
    """A short mark of the text that every request carries: the same text gives the same mark."""
    return hashlib.sha256(json.dumps(texts, sort_keys=True, ensure_ascii=False).encode("utf-8")).hexdigest()[:12]


def changed_for_every_request(base: dict, head: dict) -> list:
    """The parts whose text changed in every recording: the head has other values than the base, and none that the base had.

    A part that no request of the head carries any more counts, and so does a part that the base did not carry.
    """
    return [part for part in PARTS if set(head[part]) != set(base[part]) and not set(base[part]) & set(head[part])]


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """An answer that points to another address is not followed: the token would go there too."""

    def redirect_request(self, *args, **kwargs):
        return None


def statuses_of(commit: str) -> list:
    """Every status that the repository holds for a commit, newest first. GitHub's own address is the only one asked."""
    token, found = os.environ.get("GITHUB_TOKEN", ""), []
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "atlas-smoke-result",
               **({"Authorization": f"Bearer {token}"} if token else {})}
    again = "start this job again; when it stays so, GitHub's status page says whether its API works"
    # The commit goes into the path of the call. Only a full commit id does, and as letters and digits.
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise Unreadable("the head is not a full commit id, so GitHub was not asked", "give --head a commit of this repository",
                         "the statuses of the head commit")
    asked = urllib.parse.quote(commit, safe="")
    for page in range(1, 11):
        request = urllib.request.Request(f"{GITHUB_API}/repos/{REPOSITORY}/commits/{asked}/statuses?per_page=100&page={page}", headers=headers)
        try:
            with urllib.request.build_opener(NoRedirect, urllib.request.ProxyHandler({})).open(request, timeout=30) as response:
                part = json.load(response)
        except urllib.error.HTTPError as error:
            raise Unreadable(f"GitHub answered {error.code}", again, "the statuses of the head commit") from None
        except (OSError, ValueError, http.client.HTTPException):
            raise Unreadable("GitHub gave no answer that can be read", again, "the statuses of the head commit") from None
        if not isinstance(part, list) or not all(isinstance(status, dict) for status in part):
            raise Unreadable("GitHub gave an answer of another shape", again, "the statuses of the head commit")
        found += part
        if len(part) < 100:
            return found
    raise Unreadable("the commit has more than 1000 statuses", "a maintainer looks at what writes them", "the statuses of the head commit")


def of_the_server(statuses: list) -> tuple:
    """The statuses of the smoke run, apart: those that the server's own account wrote, newest first, and the
    accounts of the others."""
    named = [status for status in statuses if status.get("context") == STATUS]
    writer = [status["creator"].get("id") if isinstance(status.get("creator"), dict) else None for status in named]
    # An id is a whole number. Anything else in its place is named as "not given", never printed.
    writer = [account if type(account) is int else None for account in writer]
    own = [status for status, account in zip(named, writer) if WRITER and account == WRITER]
    own.sort(key=lambda status: (str(status.get("created_at", "")), status.get("id") if type(status.get("id")) is int else 0), reverse=True)
    others = {"not given" if account is None else str(account) for status, account in zip(named, writer) if status not in own}
    return own, sorted(others)


def plain(text) -> str:
    """A text of a status for the output of this job: its plain letters only, as code. No mark that could make a
    mention, a link, a second line or a command of the job out of it."""
    return "`" + re.sub(r"[^A-Za-z0-9 ,.:;()'-]", "?", str(text))[:140] + "`"


def judge(base: dict, head: dict, text: str, statuses) -> tuple:
    """Whether the pull request may go on, and why, in words. `statuses` gives the statuses of the head commit, and
    is asked only when the text of every request changed."""
    changed = changed_for_every_request(base, head)
    if not changed:
        return True, ("the smoke run is not asked for: this pull request changes no text that every request carries "
                      "(the system prompt, the grammar, the schema of the reply), by its recordings.")
    what, mark = " and ".join(changed), mark_of(head)
    own, others = of_the_server(statuses())
    beside = (f" A status `{STATUS}` by another account (id {', '.join(others)}) is on the commit; it is not counted." if others else "")
    beside += (" No account is set as the writer of the server's statuses (WRITER in scripts/smoke_result.py), so no "
               "status is counted." if not WRITER else "")
    beside += (" A line in the text of the pull request that says a smoke run passed is not read: the result comes "
               "from the server." if TYPED.search(text or "") else "")
    if own and own[0].get("state") == "success":
        return True, f"the smoke run of the server passed on the head commit ({plain(own[0].get('description', ''))}).{beside}"
    if own:
        return False, (f"the smoke run of the server did not pass on the head commit (state {plain(own[0].get('state'))}, "
                       f"{plain(own[0].get('description', ''))}). The change does not go on with a red smoke run, and the "
                       "line for a server that is not available does not set it aside: the change was tried. Fix: find "
                       "the cause first. No text is changed to make the smoke pass; after two red smoke runs the change "
                       f"stops and is thought over. A new commit needs a new smoke run.{beside}")
    if mark in NO_SERVER.findall(text or ""):
        return True, (f"no smoke run was made for this change of {what}: the server was not available, as the text of "
                      "the pull request says. The change was not tried with a real model; the first nightly run after "
                      f"the server is back covers this text (text {mark}).{beside}")
    return False, (f"this pull request changes {what}, which every request to the model carries, and its head commit "
                   f"has no status `{STATUS}` of the development server. A replay shows what the proxy sends; it cannot "
                   f"show that a real model still works with the new text. {HOW.format(mark=mark)}{beside}")


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", help="the commit to compare with")
    parser.add_argument("--head", help="the head commit of the pull request, whose statuses are read")
    parser.add_argument("--texts", help="the commit whose recordings are judged: what would land (default: --head)")
    parser.add_argument("--mark", help="print the mark of this commit's text, and nothing else")
    args = parser.parse_args(argv)
    try:
        if args.mark:
            print(mark_of(texts_at(args.mark, must_have_recordings=True)))
            return 0
        if not args.base or not args.head:
            parser.error("--base and --head are needed (or --mark)")
        texts = texts_at(args.texts or args.head, must_have_recordings=True)
        commit = git("rev-parse", "--verify", f"{args.head}^{{commit}}").strip()
        passed, why = judge(texts_at(args.base), texts, os.environ.get("PR_BODY", ""), lambda: statuses_of(commit))
    except Unreadable as error:
        print(f"::error title=smoke result::{error.of} cannot be read, so nothing was judged: {error}. Fix: "
              f"{error.fix}.", file=sys.stderr)
        return 2
    print(f"smoke result: {why}" if passed else f"::error title=smoke result::{why}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
