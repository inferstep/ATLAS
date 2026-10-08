#!/usr/bin/env python3
"""Whether a pull request that changes the text every request carries has its smoke result.

The replay job compares each request that the proxy sends with a recorded one,
in full. So a change to text that the model reads turns replay red until the
recordings are made again, and "a recording changed" marks such a change.

This check reads the recordings of the base and of the change. In each it
takes the first request to the model, and of it the part that every request
carries: the system prompt (with the tool descriptions), the grammar, and the
schema of the reply. When such a part has other values than on the base and
none that the base had, the text of every request changed. A part that no
request carries any more, and a part that is new in every request, count
too. Then a session with a real model has to
show that sessions still run: the smoke run. A maintainer starts it by hand,
and its one result line goes into the text of the pull request:

    Smoke run on <commit>: 3 of 3 sessions with no harness defect, <seconds> s; the changed text is part of every request (text <mark>).

The mark is made from the text that every request carries. The line holds
while the head of the pull request has that same text, also after a rebase
or a later commit that leaves it as it is.

When the server for the smoke run is not available, the text of the pull
request says so in one line, with the same mark:

    No smoke run: the server is not available. Not tried with a real model; the first nightly run after the server is back covers this text (text <mark>).

Such a change is then not tried with a real model before it merges. The
replay tests are no check of it: its recordings are written again from the
new text, so they pass.

For any other change of a recording nothing is asked: the recording shows
the text in its situation, and the replay job holds it.

The check cannot know that a run took place. It holds that the line is
there, that it says 3 of 3, and that it is for the text of this head.

Usage: smoke_result.py --base <commit> --head <commit>   (the text of the pull request in PR_BODY)
       smoke_result.py --mark <commit>                   (prints the mark of that commit's text)

Exit status: 0 when nothing is asked, or the line holds; 1 when the smoke
result is missing or does not hold; 2 when the recordings cannot be read, or
the head has none.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys

RECORDINGS = "tests/replay/recordings/"
# The parts of a request to the model that every request of a session carries, and what each is called.
PARTS = ("the system prompt", "the grammar", "the schema of the reply")
LINE = re.compile(r"^Smoke run on ([0-9a-f]{7,40}): ([0-3]) of 3 sessions with no harness defect, (\d+) s; "
                  r"the changed text is part of every request \(text ([0-9a-f]{12})\)\.[ \t]*$", re.MULTILINE)
NO_SERVER_LINE = ("No smoke run: the server is not available. Not tried with a real model; the first nightly run after "
                  "the server is back covers this text (text {mark}).")
NO_SERVER = re.compile("^" + re.escape(NO_SERVER_LINE).replace(re.escape("{mark}"), "([0-9a-f]{12})") + r"[ \t]*$", re.MULTILINE)
HOW = ("Fix: ask a maintainer for the smoke run of the head commit of this pull request. The run prints one line "
       "that starts with \"Smoke run on\"; put that line, as it is, on a line of its own into the text of the pull "
       "request. Do not change a text to make the smoke run pass: after two red smoke runs the change stops and is "
       "thought over. When the server is not available, a maintainer writes this line in its place: \""
       + NO_SERVER_LINE + "\"")
FETCH = "the checkout of this job needs the base commit and the head commit (fetch-depth 0)"


class Unreadable(Exception):
    """The recordings of a commit cannot be read. `fix` says what to do about it."""

    def __init__(self, what: str, fix: str = FETCH):
        super().__init__(what)
        self.fix = fix


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


def judge(base: dict, head: dict, text: str) -> tuple:
    """Whether the pull request may go on, and why, in words."""
    changed = changed_for_every_request(base, head)
    if not changed:
        return True, ("the smoke run is not asked for: this pull request changes no text that every request carries "
                      "(the system prompt, the grammar, the schema of the reply), by its recordings.")
    what, mark = " and ".join(changed), mark_of(head)
    lines = LINE.findall(text or "")
    no_server = mark in NO_SERVER.findall(text or "")
    if not lines and no_server:
        return True, (f"no smoke run was made for this change of {what}: the server was not available, as the text of "
                      "the pull request says. The change was not tried with a real model; the first nightly run after "
                      f"the server is back covers this text (text {mark}).")
    if not lines:
        return False, (f"this pull request changes {what}, which every request to the model carries, and its text has "
                       f"no result line of a smoke run. A replay shows what the proxy sends; it cannot show that a "
                       f"real model still works with the new text. {HOW.format(mark=mark)}")
    return by_its_result_lines(lines, what, mark, no_server)


def by_its_result_lines(lines: list, what: str, mark: str, no_server: bool) -> tuple:
    """The verdict for a text that has result lines: a line of a run with the text of this head decides."""
    of_this_text = [line for line in lines if line[3] == mark]
    for commit, passed, seconds, _ in of_this_text:
        if passed == "3":
            return True, f"the smoke run on {commit} ({seconds} s, 3 of 3 sessions) was made with the text of this head (text {mark})."
    if of_this_text:
        commit, passed = of_this_text[0][:2]
        return False, (f"the smoke run on {commit} had {passed} of 3 sessions with no harness defect. The change "
                       "does not go on with a red smoke run. Fix: find the cause first. No text is changed to make "
                       "the smoke pass; after two red smoke runs the change stops and is thought over.")
    said = ", ".join(sorted({line[3] for line in lines}))
    if no_server:
        return False, (f"the text of this pull request has the line for a server that is not available for this text "
                       f"(text {mark}), and beside it a result line of a smoke run for another text (text {said}). An "
                       "older result line is not set aside by itself. Fix: delete the result line of the other text and "
                       "leave the line for the server.")
    return False, (f"the result line in the text of this pull request is for another text (text {said}); {what} of this "
                   f"head has the mark {mark}. The text that every request carries changed after that run. "
                   f"{HOW.format(mark=mark)}")


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", help="the commit to compare with")
    parser.add_argument("--head", help="the head commit of the pull request")
    parser.add_argument("--mark", help="print the mark of this commit's text, and nothing else")
    args = parser.parse_args(argv)
    try:
        if args.mark:
            print(mark_of(texts_at(args.mark, must_have_recordings=True)))
            return 0
        if not args.base or not args.head:
            parser.error("--base and --head are needed (or --mark)")
        head = texts_at(args.head, must_have_recordings=True)
        passed, why = judge(texts_at(args.base), head, os.environ.get("PR_BODY", ""))
    except Unreadable as error:
        print(f"::error title=smoke result::the recordings cannot be read, so nothing was judged: {error}. Fix: "
              f"{error.fix}.", file=sys.stderr)
        return 2
    print(f"smoke result: {why}" if passed else f"::error title=smoke result::{why}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
