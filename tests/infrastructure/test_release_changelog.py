"""The script that makes the changelog section of a release from the merged pull requests.

Each case is a small repository made for the test: a release tag, then commits
as the merge writes them. The merge breaks each long line of a pull request's
text at 72 columns, and takes the spaces at the start of such a line away.
"""
import importlib.util
import os
import subprocess
import sys
import textwrap
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
SCRIPT = REPO / "scripts" / "release_changelog.py"
CHANGELOG = ("# Changelog\n\n> A note that stays.\n\n## [Unreleased]\n\n### Fixed: an entry from before\n\nIts text.\n\n"
             "## [1.0.0] - 2026-01-01\n\n### Added: the first release\n")
SENTENCE = ("The installer keeps its downloads in a private folder now, and a download that failed is tried again "
            "three times before the installer stops with the address that did not answer.")
ITEM = ("`atlas doctor` names the port that is taken and the program that holds it, and it says which setting "
        "moves the service to another port.")


@pytest.fixture(scope="module")
def release():
    spec = importlib.util.spec_from_file_location("atlas_release_changelog", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


PERSON = ("A Contributor", "t@example.invalid")
DEPENDABOT = ("dependabot[bot]", "49699333+dependabot[bot]@users.noreply.github.com")


def git(root, *args, author=PERSON):
    # The author goes through the environment: git takes a name that stands there before one of its settings.
    who = {"GIT_AUTHOR_NAME": author[0], "GIT_AUTHOR_EMAIL": author[1],
           "GIT_COMMITTER_NAME": author[0], "GIT_COMMITTER_EMAIL": author[1]}
    return subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True, check=True,
                          env={**os.environ, **who}).stdout.strip()


def as_the_merge_writes(text):
    """The text of a pull request as its commit on the branch holds it."""
    lines = []
    for line in text.split("\n"):
        long = len(line) > 72
        lines += textwrap.wrap(" ".join(line.split()), 72, break_long_words=False, break_on_hyphens=False) if long else [line]
    return "\n".join(lines)


def users(part, before="What it changes.\n\n", after="\n\n## How it was verified\n\nBy its tests."):
    """The text of a pull request with this part for users."""
    return f"{before}## What users will notice\n\n{part}{after}"


def from_pull_requests(section):
    """The headings of the entries that the script made, which end with the number of their pull request."""
    return [line for line in section.splitlines() if line.startswith("### ") and line.endswith(")") and "(#" in line]


class Made:
    """A repository with one release, and what the script says about the commits after it."""

    def __init__(self, root, capsys):
        self.root, self.capsys = root, capsys
        root.mkdir()
        git(root, "init", "-q", "-b", "dev")
        (root / "CHANGELOG.md").write_text(CHANGELOG, encoding="utf-8")
        git(root, "add", "-A")
        git(root, "commit", "-q", "-m", "chore: the first release")
        git(root, "tag", "v1.0.0")

    def merged(self, title, text="", author=PERSON, files=None):
        """One commit as the merge of a pull request writes it; its hash."""
        for name, content in (files or {}).items():
            (self.root / name).write_text(content, encoding="utf-8")
        git(self.root, "add", "-A")
        git(self.root, "commit", "-q", "--allow-empty", "-m", title, "-m", as_the_merge_writes(text), author=author)
        return git(self.root, "rev-parse", "HEAD")

    def run(self, release, *args):
        """The exit status, the section that was printed, and what was said to the release owner."""
        status = release.main(["--root", str(self.root), "--version", "1.1.0", "--date", "2026-02-02", *args])
        said = self.capsys.readouterr()
        return status, said.out, said.err


@pytest.fixture
def made(tmp_path, capsys):
    return Made(tmp_path / "repo", capsys)


def test_a_part_with_sentences_is_an_entry_and_its_broken_lines_are_put_together_again(release, made):
    made.merged("fix(install): the installer tries a failed download again (#7)", users(f"{SENTENCE}\n\nA second paragraph."))
    status, section, said = made.run(release)
    assert status == 0
    assert f"### Fixed: the installer tries a failed download again (#7)\n\n{SENTENCE}\n\nA second paragraph.\n" in section
    assert "1 gave an entry: #7" in said


def test_each_item_of_a_list_is_put_together_again_and_stays_an_item(release, made):
    made.merged("feat(cli): doctor names the port (#8)", users(f"- {ITEM}\n- A short one.\n- {SENTENCE}"))
    _, section, _ = made.run(release)
    assert f"### Added: doctor names the port (#8)\n\n- {ITEM}\n- A short one.\n- {SENTENCE}\n" in section


def test_a_list_mark_that_the_merge_left_alone_on_its_line_starts_an_item(release, made):
    word = "`tests/infrastructure/test_a_very_long_name.py::test_the_name_is_longer_than_one_line_of_the_merge`"
    made.merged("fix(cli): two names (#8)", users(f"- A first item.\n- {word}\n- {word} and more"))
    _, section, _ = made.run(release)
    assert f"- A first item.\n- {word}\n- {word} and more\n" in section


def test_a_line_that_starts_with_a_dash_or_a_number_and_no_space_after_it_is_not_a_list_item(release, made):
    made.merged("fix(cli): a (#8)", users("The script takes two options for a section, which are\n--write and --since. A run takes\n"
                                          "2.5 seconds.\n\n- An item with an option:\n-x is short for it."))
    _, section, _ = made.run(release)
    assert ("The script takes two options for a section, which are --write and --since. A run takes 2.5 seconds.\n\n"
            "- An item with an option: -x is short for it.\n") in section


def test_the_type_of_the_title_gives_the_word_of_the_entry(release, made):
    made.merged("feat(tui): a (#1)", users("One."))
    made.merged("fix(tui): b (#2)", users("Two."))
    made.merged("perf(proxy): c (#3)", users("Three."))
    made.merged("feat(proxy)!: d (#4)", users("Four."))
    _, section, said = made.run(release)
    assert from_pull_requests(section) == [
        "### Added, breaking: d (#4)", "### Changed: c (#3)", "### Fixed: b (#2)", "### Added: a (#1)"]
    assert "marked as breaking in the title: #4. Such a release is a MAJOR one." in said


def test_only_the_one_sentence_of_the_template_leaves_a_pull_request_out(release, made):
    made.merged("ci(tests): a (#1)", users("Nothing."))
    made.merged("ci(tests): b (#2)", users("Nothing. But the installer asks one more question."))
    made.merged("ci(tests): c (#3)", users("nothing"))
    made.merged("ci(tests): d (#4)", users("Nothing in the product. For contributors: a new job."))
    _, section, said = made.run(release)
    assert '1 left out, the part for users says "Nothing.": #1' in said
    assert "3 TO DECIDE BY HAND" in said
    assert 'starts with "Nothing" and goes on: "Nothing. But the installer asks one more question."' in said
    assert from_pull_requests(section) == []


def test_the_comment_lines_of_the_template_are_not_part_of_an_entry(release, made):
    comment = ("<!-- What a user of ATLAS sees differently after this change, in plain sentences or a list. The changelog "
               "of the release is made from this part. -->")
    made.merged("fix(tui): a (#1)", users(f"{comment}\n\nThe status line shows the model."))
    made.merged("fix(tui): b (#2)", users(comment))
    _, section, said = made.run(release)
    assert "### Fixed: a (#1)\n\nThe status line shows the model.\n" in section
    assert "ATLAS sees" not in section
    assert "#2  fix(tui): b (#2)\n          its part for users is empty" in said


def test_a_pull_request_with_no_such_heading_is_decided_by_hand(release, made):
    made.merged("fix(tui): a (#1)", "What it changes.\n\n## Why\n\nA reason.")
    made.merged("fix(tui): b (#2)", "It says in a sentence: ## What users will notice\n\nand then goes on.")
    made.merged("fix(tui): c (#3)", "## What users will notice:\n\nA colon is not the heading.")
    _, section, said = made.run(release)
    assert said.count('its text has no heading "What users will notice"') == 3
    assert "3 TO DECIDE BY HAND" in said
    assert from_pull_requests(section) == []


def test_the_heading_inside_a_fenced_block_does_not_count(release, made):
    shown = "The template gets a part:\n\n```\n## What users will notice\n\nNothing.\n```\n\n~~~md\n## What users will notice\n~~~"
    made.merged("docs(guide): a (#1)", shown)
    made.merged("docs(guide): b (#2)", shown + "\n\n## What users will notice\n\nThe guide names the part.")
    _, section, said = made.run(release)
    assert '#1  docs(guide): a (#1)\n          its text has no heading "What users will notice"' in said
    assert "### Changed: b (#2)\n\nThe guide names the part.\n" in section


def test_a_fence_is_closed_only_by_a_fence_of_its_kind_that_is_as_long(release):
    text = "````\n```\n## What users will notice\n~~~\n````\n## What users will notice\n\nOne."
    assert release.users_part(text) == (1, "One.")
    assert [outside for _, outside in release.lines_outside_fences("```py\ncode\n``` not a close\n```\ntext")] == [
        False, False, False, False, True]


def test_the_heading_more_than_once_is_decided_by_hand(release, made):
    made.merged("fix(tui): a (#1)", users("One.") + "\n\n## What users will notice\n\nTwo.")
    _, section, said = made.run(release)
    assert 'its text has the heading "What users will notice" 2 times' in said
    assert from_pull_requests(section) == []


def test_the_part_ends_at_the_next_heading_of_its_level_and_takes_any_level(release):
    assert release.users_part("# What users will notice\n\nOne.\n\n# Why\n\nTwo.") == (1, "One.")
    assert release.users_part("### What users will notice  \nOne.\n\n## Proof\n\nTwo.") == (1, "One.")
    assert release.users_part("## What users will notice\n\nOne.\n\n### More\n\nTwo.\n\n## Why") == (1, "One.\n\n### More\n\nTwo.")
    assert release.users_part("## What users will notice\n\nOne.") == (1, "One.")
    assert release.users_part("## Why\n\nOne.") == (0, "")


@pytest.mark.parametrize("part, what", [
    ("| Before | Now |\n|---|---|\n| a | b |", "a table"),
    ("Run this:\n\n```\natlas doctor\n```", "a code block"),
    ("The first.\n\n### For Windows\n\nThe second.", "a heading of its own"),
    ("> A quoted line.", "a quoted block"),
    ("<details>\nMore.\n</details>", "HTML"),
    ("- An item.\n  - An item inside it.", "a list inside a list"),
])
def test_a_part_that_is_not_sentences_or_a_list_is_decided_by_hand(release, made, part, what):
    made.merged("fix(tui): a (#1)", users(part))
    _, section, said = made.run(release)
    assert f"its part for users is not plain sentences or a list: it has {what}" in said
    assert from_pull_requests(section) == []


def test_dependabots_pull_requests_are_one_line(release, made):
    made.merged("chore(deps): bump a (#1)", "Bumps a.\n<details>\n<summary>Notes</summary>\n</details>", author=DEPENDABOT)
    made.merged("chore(deps): bump b (#2)", "Bumps b.", author=DEPENDABOT)
    _, section, said = made.run(release)
    assert "### Changed: 2 dependency updates (#2, #1)\n" in section
    assert "2 dependency updates, put together as one line: #2, #1" in said
    assert "BY HAND" not in said


def test_dependabot_is_known_by_the_address_of_the_bot_and_not_by_a_name(release, made):
    made.merged("chore(deps): bump a (#1)", "Bumps a.", author=("dependabot[bot]", "someone@example.invalid"))
    made.merged("chore(deps): bump b (#2)", "Bumps b.", author=("Another Name", DEPENDABOT[1]))
    _, section, said = made.run(release)
    assert "### Changed: 1 dependency update (#2)\n" in section
    assert "#1  chore(deps): bump a (#1)\n          its text has no heading" in said


def test_a_made_commit_has_its_author_also_where_the_environment_names_another(release, made, monkeypatch):
    for name in ("GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"):
        monkeypatch.setenv(name, "The Person Who Runs The Tests")
    for name in ("GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"):
        monkeypatch.setenv(name, "runs-the-tests@example.invalid")
    made.merged("chore(deps): bump a (#1)", "Bumps a.", author=DEPENDABOT)
    assert "1 dependency updates, put together as one line: #1" in made.run(release)[2]


def test_one_dependency_update_is_not_called_updates(release, made):
    made.merged("chore(deps): bump a (#1)", "Bumps a.", author=DEPENDABOT)
    assert "### Changed: 1 dependency update (#1)\n" in made.run(release)[1]


def test_a_pull_request_that_wrote_its_own_entry_has_it_already(release, made):
    own = CHANGELOG.replace("## [Unreleased]\n", "## [Unreleased]\n\n### Fixed: its own entry\n\nText.\n")
    made.merged("fix(tui): a (#1)", "What it changes.", files={"CHANGELOG.md": own})
    made.merged("fix(tui): b (#2)", "What it changes.", files={"other.txt": "x"})
    _, section, said = made.run(release)
    assert "1 have their entry already: no part for users, and the pull request changed CHANGELOG.md itself: #1" in said
    assert "1 TO DECIDE BY HAND" in said
    assert "#2  fix(tui): b (#2)" in said
    assert "### Fixed: its own entry" in section


def test_a_part_for_users_and_an_own_entry_in_one_pull_request_is_decided_by_hand(release, made):
    own = CHANGELOG.replace("## [Unreleased]\n", "## [Unreleased]\n\n### Fixed: its own entry\n\nText.\n")
    made.merged("fix(tui): a (#1)", users("The status line shows the model."), files={"CHANGELOG.md": own})
    _, section, said = made.run(release)
    assert "it has a part for users and it changed CHANGELOG.md too, so its entry may stand twice" in said
    assert "The status line shows the model." not in section


def test_a_commit_that_is_not_a_pull_request_is_listed(release, made):
    first = made.merged("chore(ci): a commit with no number", "Its text.\n\n## What users will notice\n\nSomething.")
    made.merged("fix(tui): a (#1)", users("One."))
    _, section, said = made.run(release)
    assert "1 not a pull request (the title has no number of one):" in said
    assert f"{first[:7]}  chore(ci): a commit with no number" in said
    assert "Something." not in section
    assert "2 = 1 + 1" in said


def test_a_merge_commit_that_github_wrote_for_a_pull_request_is_a_pull_request(release, made):
    git(made.root, "checkout", "-q", "-b", "side")
    made.merged("feat: on the side", "Its text.", files={"new.txt": "x"})
    git(made.root, "checkout", "-q", "dev")
    git(made.root, "merge", "-q", "--no-ff", "-m", "Merge pull request #145 from someone/side", "-m", "feat: on the side", "side")
    _, _, said = made.run(release)
    assert '#145  Merge pull request #145 from someone/side\n          its text has no heading "What users will notice"' in said
    assert "not a pull request" not in said
    assert "1 commit(s) read" in said


def test_of_a_merge_only_the_commit_itself_is_read(release, made):
    git(made.root, "checkout", "-q", "-b", "side")
    made.merged("fix(tui): on the side (#5)", users("From the side."), files={"CHANGELOG.md": CHANGELOG + "\nside\n"})
    git(made.root, "checkout", "-q", "dev")
    made.merged("fix(tui): a (#1)", users("One."))
    git(made.root, "merge", "-q", "--no-ff", "-m", "chore(ci): merge the side back into dev", "side")
    merge = git(made.root, "rev-parse", "HEAD")
    _, section, said = made.run(release)
    assert "From the side." not in section
    assert "(#5)" not in said
    assert f"{merge[:7]}  chore(ci): merge the side back into dev\n          it changed CHANGELOG.md" in said
    assert "2 commit(s) read" in said
    assert "2 = 1 + 1" in said


def test_a_merge_that_brings_no_change_of_the_changelog_is_not_said_to_change_it(release, made):
    git(made.root, "checkout", "-q", "-b", "side")
    made.merged("docs: a note on the side", "A note.", files={"note.txt": "x"})
    git(made.root, "checkout", "-q", "dev")
    made.merged("fix(tui): a (#1)", "No part.", files={"CHANGELOG.md": CHANGELOG + "\nan entry\n"})
    git(made.root, "merge", "-q", "--no-ff", "-m", "chore(ci): merge the side back into dev", "side")
    _, _, said = made.run(release)
    assert "chore(ci): merge the side back into dev\n" in said + "\n"
    assert "merge the side back into dev\n          it changed" not in said
    assert "1 have their entry already" in said


def test_a_title_that_has_no_type_still_gives_an_entry(release, made):
    made.merged("Update the README (#9)", users("The README names the new port."))
    assert from_pull_requests(made.run(release)[1]) == ["### Changed: Update the README (#9)"]


def test_no_commit_since_the_release_gives_only_the_entries_from_before(release, made):
    status, section, said = made.run(release)
    assert status == 0
    assert section == "## [1.1.0] - 2026-02-02\n\n### Fixed: an entry from before\n\nIts text.\n"
    assert said.strip() == "release changelog: 0 commit(s) read, the first parents of HEAD since v1.0.0"


def test_a_revert_and_the_pull_request_it_reverts_are_both_decided_by_hand(release, made):
    made.merged("feat(tui): a new view (#1)", users("A new view."))
    made.merged("fix(tui): b (#2)", users("Two."))
    made.merged("revert(tui): take the new view out again (#3)", users("The new view is gone.") + "\n\nReverts inferstep/ATLAS#1")
    _, section, said = made.run(release)
    assert "#3  revert(tui): take the new view out again (#3)\n          it is a revert of #1" in said
    assert "#1  feat(tui): a new view (#1)\n          #3 reverts it" in said
    assert "A new view." not in section
    assert "The new view is gone." not in section
    assert "### Fixed: b (#2)" in section
    assert "3 = 1 + 2" in said


def test_a_revert_is_known_by_its_title_or_by_its_text(release, made):
    first = made.merged("feat(tui): a (#1)", users("One."))
    made.merged('Revert "feat(tui): a" (#2)', f"This reverts commit {first}.")
    made.merged("fix(tui): c (#3)", users("Three.") + "\n\nReverts 0123abc, which was released before.")
    made.merged("revert: d (#4)", users("Four."))
    _, section, said = made.run(release)
    assert "#2  Revert \"feat(tui): a\" (#2)\n          it is a revert of #1" in said
    assert "#2 reverts it" in said
    assert "it is a revert of a commit that is not in this range, so of a change that was released before" in said
    assert "it is a revert, and its text does not name what it reverts" in said
    assert "4 TO DECIDE BY HAND" in said
    assert from_pull_requests(section) == []


def test_the_places_add_up_to_the_commits_that_were_read(release, made):
    made.merged("fix(tui): a (#1)", users("One."))
    made.merged("ci(tests): b (#2)", users("Nothing."))
    made.merged("chore(deps): c (#3)", "Bumps c.", author=DEPENDABOT)
    made.merged("fix(tui): d (#4)", "No part.")
    made.merged("chore: e", "No number.")
    status, _, said = made.run(release)
    assert status == 0
    assert "5 commit(s) read, the first parents of HEAD since v1.0.0" in said
    assert "  5 = 1 + 1 + 1 + 1 + 1" in said


@pytest.mark.parametrize("fault", ["twice", "lost"])
def test_the_script_stops_when_a_commit_stands_in_two_places_or_in_none(release, made, monkeypatch, fault):
    made.merged("fix(tui): a (#1)", users("One."))
    made.merged("fix(tui): b (#2)", users("Two."))
    sort_out = release.sort_out

    def faulty(found, changed):
        places = sort_out(found, changed)
        if fault == "twice":
            places["nothing"].append(places["entries"][0])
        else:
            places["entries"].pop()
        return places
    monkeypatch.setattr(release, "sort_out", faulty)
    status, section, said = made.run(release, "--write")
    assert status == 2
    assert section == ""
    assert f"the places do not add up: 2 commit(s) were read, and {3 if fault == 'twice' else 1} stand in a place" in said
    assert (made.root / "CHANGELOG.md").read_text(encoding="utf-8") == CHANGELOG


def test_without_write_the_section_is_printed_and_the_file_stays(release, made):
    made.merged("fix(tui): a (#1)", users("One."))
    status, section, _ = made.run(release, "--name", "Maia")
    assert status == 0
    assert section == "## [1.1.0] - 2026-02-02 — Maia\n\n### Fixed: a (#1)\n\nOne.\n\n### Fixed: an entry from before\n\nIts text.\n"
    assert (made.root / "CHANGELOG.md").read_text(encoding="utf-8") == CHANGELOG


def test_write_puts_the_section_under_its_heading_and_leaves_unreleased_empty(release, made):
    made.merged("fix(tui): a (#1)", users("One."))
    status, section, said = made.run(release, "--write")
    assert status == 0
    assert section == ""
    assert "the section for 1.1.0 is in CHANGELOG.md; [Unreleased] is empty" in said
    assert (made.root / "CHANGELOG.md").read_text(encoding="utf-8") == (
        "# Changelog\n\n> A note that stays.\n\n## [Unreleased]\n\n"
        "## [1.1.0] - 2026-02-02\n\n### Fixed: a (#1)\n\nOne.\n\n### Fixed: an entry from before\n\nIts text.\n\n"
        "## [1.0.0] - 2026-01-01\n\n### Added: the first release\n")


def test_the_range_can_be_given(release, made):
    first = made.merged("fix(tui): a (#1)", users("One."))
    second = made.merged("fix(tui): b (#2)", users("Two."))
    made.merged("fix(tui): c (#3)", users("Three."))
    _, section, said = made.run(release, "--since", first, "--to", second)
    assert "(#2)" in section
    assert "(#1)" not in section
    assert "(#3)" not in section
    assert f"1 commit(s) read, the first parents of {second} since {first}" in said


@pytest.mark.parametrize("arguments, changelog, why", [
    (["--version", "1.0.0"], CHANGELOG, "CHANGELOG.md has a section for 1.0.0 already; nothing was written"),
    (["--version", "next"], CHANGELOG, "`next` is not a version as in 3.2.0"),
    ([], CHANGELOG.replace("## [Unreleased]\n", ""), "CHANGELOG.md must have the heading `## [Unreleased]` once"),
    (["--since", "v9.9.9"], CHANGELOG, "git log --first-parent ended with status 128"),
])
def test_what_cannot_be_read_stops_the_script_and_writes_nothing(release, made, arguments, changelog, why):
    made.merged("fix(tui): a (#1)", users("One."), files={"CHANGELOG.md": changelog})
    status, section, said = made.run(release, "--write", *arguments)
    assert status == 2
    assert section == ""
    assert why in said
    assert "Fix: " in said
    assert (made.root / "CHANGELOG.md").read_text(encoding="utf-8") == changelog


@pytest.mark.parametrize("argument", ["--since=--output={file}", "--to=--output={file}"])
def test_a_range_that_is_written_like_an_option_of_git_is_not_taken_as_one(release, made, tmp_path, argument):
    made.merged("fix(tui): a (#1)", users("One."))
    status, section, said = made.run(release, argument.format(file=tmp_path / "written"))
    assert status == 2
    assert section == ""
    assert "ended with status 128" in said
    assert sorted(path.name for path in tmp_path.iterdir()) == ["repo"]


def test_a_repository_with_no_release_tag_needs_since(release, made):
    git(made.root, "tag", "-d", "v1.0.0")
    made.merged("fix(tui): a (#1)", users("One."))
    status, _, said = made.run(release)
    assert status == 2
    assert "git describe --tags" in said
    assert "with --since when no release tag is behind that commit" in said


def test_the_template_of_a_pull_request_has_the_heading_and_names_the_sentence(release):
    template = (REPO / ".github" / "PULL_REQUEST_TEMPLATE.md").read_text(encoding="utf-8")
    count, part = release.users_part(template)
    # A template that nobody filled in is decided by hand: its part is empty, it does not say "Nothing." by itself.
    assert (count, part) == (1, "")
    comment = template.split(f"## {release.HEADING}\n", 1)[1].split("-->", 1)[0]
    assert f"write only: {release.NOTHING}" in " ".join(comment.split())
    assert "plain sentences or a list" in " ".join(comment.split())


def test_the_guide_for_contributors_names_the_heading_and_the_sentence(release):
    guide = " ".join((REPO / "CONTRIBUTING.md").read_text(encoding="utf-8").split())
    assert f"**{release.HEADING}**" in guide
    assert f'"{release.NOTHING}"' in guide
    assert "does not edit `CHANGELOG.md`" in guide
