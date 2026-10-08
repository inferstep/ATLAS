#!/usr/bin/env python3
"""Put the changelog section of a release together from the merged pull requests.

A pull request holds no entry of CHANGELOG.md. Its text has a part under the
heading "What users will notice", and the merge keeps that text in the commit
message on `dev`. This reads the commits of the branch since the last release
and makes the section of the new release from those parts.

Each commit that is read goes to one place, and the places add up to the
commits read. The script stops when they do not.

  - An entry: the part has plain sentences or a list. The merge breaks each
    long line at 72 columns; the lines of a paragraph and of a list item are
    put together again.
  - Left out: the part is the one sentence "Nothing."
  - Dependency updates: Dependabot's pull requests, as one line.
  - Has its entry already: the pull request has no such part and changed
    CHANGELOG.md itself.
  - To decide by hand, each with its reason: no such part; a part that is
    empty, or that starts with "Nothing" and goes on; a part with a table, a
    code block, a quoted block, HTML, a heading of its own or a list inside
    a list; the heading more than once; a part and a change of CHANGELOG.md
    in one pull request; a revert, and each pull request that it reverts.
  - Not a pull request: a commit whose title does not end with "(#number)".
    Each is listed.

Nothing is guessed: what does not fit goes to the release owner. The entries
that stand under `[Unreleased]` stay as they are, after the new ones.

Limits:
  - The commit holds the text of the pull request as it was at the merge. A
    later edit of that text on GitHub is not read.
  - A line break that the author put inside a paragraph cannot be told from
    one of the merge, and is taken out too. A broken line that happens to
    start with a list mark reads as a new list item.
  - The merge also takes the spaces at the start of a long line away. So a
    list inside a list comes out wrong, and a part with one is decided by
    hand. Where every line of the inner list was a long one, nothing shows
    it any more, and the entry is one flat list.
  - The heading counts only as a heading line of its own, outside a fenced
    code block.

It reads git only: no token and no network. The release owner runs it as one
step of a release, reads the section, and edits it by hand where needed.

Usage: release_changelog.py --version 3.2.0 [--name NAME] [--since v3.1.6] [--to COMMIT] [--date 2026-11-01] [--write]

Without --write it prints the section. With --write it puts the section into
CHANGELOG.md under a heading for the version and leaves `[Unreleased]` empty.
What the release owner has to know is printed beside it, on the error stream.

Exit status: 0 when the section was printed or written; 2 when the commits or
the changelog cannot be read, the version has a section already, or the
places do not add up.
"""
from __future__ import annotations

import argparse
import datetime as dt
import re
import subprocess
import sys
from pathlib import Path
from typing import NamedTuple

ROOT = Path(__file__).resolve().parents[1]
HEADING = "What users will notice"
NOTHING = "Nothing."
UNRELEASED = "## [Unreleased]"
# The type of a pull request's title -> the word its entry starts with.
WORDS = {"feat": "Added", "fix": "Fixed"}
OTHER = "Changed"
NUMBER = re.compile(r" \(#(\d+)\)$")
TITLE = re.compile(r"^(?P<type>[a-z]+)(?:\([^)]*\))?(?P<breaking>!)?: (?P<summary>.+?)(?: \(#\d+\))?$")
HEADING_LINE = re.compile(rf"^(#{{1,6}})[ \t]+{re.escape(HEADING)}[ \t]*$")
ANY_HEADING = re.compile(r"^(#{1,6})[ \t]+\S")
FENCE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
# A list mark with its text, or alone on its line: the merge leaves it alone when the first word is a long one.
LIST_ITEM = re.compile(r"^\s*(?:[-*+]|\d{1,9}[.)])(?:\s+\S|\s*$)")
INNER_ITEM = re.compile(r"^\s+(?:[-*+]|\d{1,9}[.)])(?:\s|$)")
COMMENT = re.compile(r"<!--.*?-->", re.DOTALL)
REVERTS = re.compile(r"^(?:Reverts|This reverts commit)\b(.*)$", re.MULTILINE)
# The places a commit can go to, in the order the release owner reads them, and what each is called.
PLACES = {"entries": "gave an entry",
          "nothing": f'left out, the part for users says "{NOTHING}"',
          "dependencies": "dependency updates, put together as one line",
          "has_entry": "have their entry already: no part for users, and the pull request changed CHANGELOG.md itself",
          "by_hand": "TO DECIDE BY HAND",
          "no_number": "not a pull request (the title ends with no number)"}


class Unusable(Exception):
    """The commits or the changelog cannot be read as this script needs them."""


class Commit(NamedTuple):
    hash: str
    author: str
    title: str
    body: str

    @property
    def name(self) -> str:
        """What the release owner finds it by: the number of its pull request, or the start of its hash."""
        number = NUMBER.search(self.title)
        return f"#{number.group(1)}" if number else self.hash[:7]


def git(root: Path, *args: str) -> str:
    done = subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise Unusable(f"git {' '.join(args[:2])} ended with status {done.returncode}: {done.stderr.strip()[-300:]}")
    return done.stdout


def last_release(root: Path, to: str) -> str:
    """The newest release tag that the commit has behind it."""
    return git(root, "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", "--end-of-options", to).strip()


def commits(root: Path, since: str, to: str) -> list:
    """The commits of the branch between the two, newest first. Of a merge only the commit itself is read."""
    out = git(root, "log", "--first-parent", "--format=%H%x00%an%x00%s%x00%b%x1e", "--end-of-options", f"{since}..{to}")
    found = []
    for record in out.split("\x1e"):
        if record.strip():
            commit, author, title, body = record.strip("\n").split("\x00")
            found.append(Commit(commit, author, title, body.replace("\r\n", "\n")))
    return found


def changed_the_changelog(root: Path, since: str, to: str) -> set:
    """The commits of the branch between the two that changed CHANGELOG.md, each compared with its first parent."""
    out = git(root, "log", "--first-parent", "--format=%H", "--end-of-options", f"{since}..{to}", "--", "CHANGELOG.md")
    return set(out.split())


def lines_outside_fences(text: str):
    """Each line of a text, with whether it stands outside a fenced code block. A line that is a fence stands inside."""
    opened = ""
    for line in text.splitlines():
        fence = FENCE.match(line)
        if not opened:
            opened = fence.group(1) if fence else ""
            yield line, not fence
            continue
        closes = fence and fence.group(1)[0] == opened[0] and len(fence.group(1)) >= len(opened) and not fence.group(2).strip()
        opened = "" if closes else opened
        yield line, False


def users_part(message: str) -> tuple:
    """How often the heading stands in a pull request's text, and the part under the first one, with no comment lines.

    The part ends at the next heading of the same level or of a higher one.
    """
    lines = list(lines_outside_fences(message))
    headings = [(at, len(found.group(1))) for at, (line, outside) in enumerate(lines)
                if outside and (found := HEADING_LINE.match(line))]
    if not headings:
        return 0, ""
    start, level = headings[0]
    end = len(lines)
    for at in range(start + 1, len(lines)):
        line, outside = lines[at]
        following = ANY_HEADING.match(line) if outside else None
        if following and len(following.group(1)) <= level:
            end = at
            break
    part = "\n".join(line for line, _ in lines[start + 1:end])
    return len(headings), COMMENT.sub("", part).strip()


def not_plain(part: str) -> str:
    """Why this script does not make an entry of the part by itself, or nothing when the part is sentences or a list."""
    for line, outside in lines_outside_fences(part):
        start = line.lstrip()
        if not outside:
            return "a code block"
        if start.startswith("|"):
            return "a table"
        if ANY_HEADING.match(line):
            return "a heading of its own"
        if start.startswith(">"):
            return "a quoted block"
        if start.startswith("<"):
            return "HTML"
        if INNER_ITEM.match(line):
            return "a list inside a list"
    return ""


def joined_again(part: str) -> str:
    """The part with the lines of each paragraph and of each list item put together again."""
    blocks: list = []
    inside = False
    for line in part.splitlines():
        if not line.strip():
            inside = False
            if blocks and blocks[-1]:
                blocks.append("")
        elif not inside or LIST_ITEM.match(line):
            blocks.append(line.rstrip())
            inside = True
        else:
            blocks[-1] += " " + line.strip()
    return "\n".join(blocks).strip("\n")


def named_by(commit: Commit, numbers: list, hashes: list) -> bool:
    """Whether one of these numbers is the commit's pull request, or one of these hashes starts its hash."""
    number = NUMBER.search(commit.title)
    return bool(number and number.group(1) in numbers) or any(commit.hash.startswith(start) for start in hashes)


def reverts_among(found: list) -> dict:
    """For each commit that takes part in a revert, why it is decided by hand: the revert, and each commit it names."""
    why: dict = {}
    for commit in found:
        named = TITLE.match(commit.title)
        lines = REVERTS.findall(commit.body)
        if not lines and not commit.title.startswith('Revert "') and not (named and named["type"] == "revert"):
            continue
        said = " ".join(lines)
        numbers, hashes = re.findall(r"#(\d+)\b", said), re.findall(r"\b[0-9a-f]{7,40}\b", said)
        reverted = [other for other in found if other.hash != commit.hash and named_by(other, numbers, hashes)]
        for other in reverted:
            why.setdefault(other.hash, []).append(f"{commit.name} reverts it")
        if reverted:
            reason = f"it is a revert of {', '.join(other.name for other in reverted)}"
        elif numbers or hashes:
            reason = "it is a revert of a commit that is not in this range, so of a change that was released before"
        else:
            reason = "it is a revert, and its text does not name what it reverts"
        why.setdefault(commit.hash, []).append(reason)
    return {commit: "; ".join(reasons) for commit, reasons in why.items()}


def place_of(commit: Commit, in_changelog: bool, revert: str) -> tuple:
    """The one place of a commit, and with it the text of its entry or the reason to decide by hand."""
    if not NUMBER.search(commit.title):
        return "no_number", "; ".join(filter(None, [revert, "it changed CHANGELOG.md" if in_changelog else ""]))
    if revert:
        return "by_hand", revert
    if commit.author.startswith("dependabot"):
        return "dependencies", ""
    count, part = users_part(commit.body)
    if count == 0 and in_changelog:
        return "has_entry", ""
    if count == 0:
        return "by_hand", f'its text has no heading "{HEADING}"'
    if count > 1:
        return "by_hand", f'its text has the heading "{HEADING}" {count} times'
    if part == NOTHING:
        return "nothing", ""
    if not part:
        return "by_hand", "its part for users is empty"
    if part.lower().startswith("nothing"):
        return "by_hand", f'its part for users starts with "Nothing" and goes on: "{" ".join(part.split())[:120]}"'
    if not_plain(part):
        return "by_hand", f"its part for users is not plain sentences or a list: it has {not_plain(part)}"
    if in_changelog:
        return "by_hand", "it has a part for users and it changed CHANGELOG.md too, so its entry may stand twice"
    return "entries", joined_again(part)


def sort_out(found: list, changed: set) -> dict:
    """Each commit in its one place, with its entry or its reason."""
    reverts = reverts_among(found)
    places: dict = {place: [] for place in PLACES}
    for commit in found:
        place, text = place_of(commit, commit.hash in changed, reverts.get(commit.hash, ""))
        places[place].append((commit, text))
    return places


def must_add_up(found: list, places: dict) -> None:
    """Every commit that was read stands in one place, once."""
    placed = sorted(commit.hash for rows in places.values() for commit, _ in rows)
    if placed != sorted(commit.hash for commit in found):
        raise Unusable(f"the places do not add up: {len(found)} commit(s) were read, and {len(placed)} stand in a place. "
                       "That is a fault of scripts/release_changelog.py and not of the commits; no section was made")


def entries_as_text(places: dict) -> str:
    """The entries of the pull requests in the form the changelog has, newest first, then the dependency updates."""
    parts = []
    for commit, text in places["entries"]:
        named = TITLE.match(commit.title)
        word = WORDS.get(named["type"], OTHER) if named else OTHER
        word += ", breaking" if named and named["breaking"] else ""
        summary = named["summary"] if named else NUMBER.sub("", commit.title)
        parts.append(f"### {word}: {summary} ({commit.name})\n\n{text}\n")
    updates = [commit.name for commit, _ in places["dependencies"]]
    if updates:
        parts.append(f"### {OTHER}: {len(updates)} dependency update{'s' if len(updates) > 1 else ''} ({', '.join(updates)})\n")
    return "\n".join(parts)


def unreleased_of(changelog: str) -> tuple:
    """The changelog in three parts: up to and with the `[Unreleased]` heading, what stands under it, and the rest."""
    if changelog.count(UNRELEASED + "\n") != 1:
        raise Unusable(f"CHANGELOG.md must have the heading `{UNRELEASED}` once")
    before, after = changelog.split(UNRELEASED + "\n", 1)
    following = re.search(r"^## \[", after, re.MULTILINE)
    cut = following.start() if following else len(after)
    return before + UNRELEASED + "\n", after[:cut].strip("\n"), after[cut:]


def section(version: str, name: str, date: str, new: str, kept: str) -> str:
    """The section of the release: its heading, the new entries, then the entries that were under `[Unreleased]`."""
    heading = f"## [{version}] - {date}" + (f" — {name}" if name else "")
    return "\n\n".join(part.strip("\n") for part in (heading, new, kept) if part.strip()) + "\n"


def for_the_owner(places: dict, read: int, since: str, to: str) -> str:
    """What the release owner has to know: where each commit went, and that the places add up."""
    lines = [f"release changelog: {read} commit(s) read, the first parents of {to} since {since}"]
    for place, called in PLACES.items():
        rows = places[place]
        if not rows:
            continue
        if place in ("by_hand", "no_number"):
            lines.append(f"  {len(rows)} {called}:")
            for commit, why in rows:
                lines.append(f"      {commit.name}  {commit.title}" + (f"\n          {why}" if why else ""))
        else:
            lines.append(f"  {len(rows)} {called}: {', '.join(commit.name for commit, _ in rows)}")
    if read:
        lines.append(f"  {read} = " + " + ".join(str(len(places[place])) for place in PLACES if places[place]))
    breaking = [commit.name for commit, _ in places["entries"] if (TITLE.match(commit.title) or {"breaking": ""})["breaking"]]
    if breaking:
        lines.append(f"  marked as breaking in the title: {', '.join(breaking)}. Such a release is a MAJOR one.")
    return "\n".join(lines)


def parse(argv: list | None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--version", required=True, help="the version of the release, as in 3.2.0")
    parser.add_argument("--name", default="", help="the name of the release line, when the heading carries one")
    parser.add_argument("--since", default="", help="the last release; by default the newest release tag behind --to")
    parser.add_argument("--to", default="HEAD", help="the commit that is released")
    parser.add_argument("--date", default=dt.datetime.now(dt.timezone.utc).date().isoformat(),
                        help="the day of the release; by default today, in UTC")
    parser.add_argument("--root", type=Path, default=ROOT, help="the repository")
    parser.add_argument("--write", action="store_true", help="put the section into CHANGELOG.md")
    return parser.parse_args(argv)


def main(argv: list | None = None) -> int:
    args = parse(argv)
    path = args.root / "CHANGELOG.md"
    try:
        if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?", args.version):
            raise Unusable(f"`{args.version}` is not a version as in 3.2.0")
        since = args.since or last_release(args.root, args.to)
        found = commits(args.root, since, args.to)
        places = sort_out(found, changed_the_changelog(args.root, since, args.to))
        must_add_up(found, places)
        changelog = path.read_text(encoding="utf-8")
        if re.search(rf"^## \[{re.escape(args.version)}\]", changelog, re.MULTILINE):
            raise Unusable(f"CHANGELOG.md has a section for {args.version} already; nothing was written")
        head, kept, rest = unreleased_of(changelog)
    except (Unusable, OSError) as error:
        print(f"release changelog: {error}. Fix: run it in the repository, on the commit that is released, with a version "
              "that has no section yet, and with --since when no release tag is behind that commit.", file=sys.stderr)
        return 2
    made = section(args.version, args.name, args.date, entries_as_text(places), kept)
    print(for_the_owner(places, len(found), since, args.to), file=sys.stderr)
    if args.write:
        path.write_text(head + "\n" + made + "\n" + rest, encoding="utf-8")
        print(f"release changelog: the section for {args.version} is in {path.name}; [Unreleased] is empty", file=sys.stderr)
    else:
        print(made, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())
