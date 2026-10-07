"""The rules learned from review are one section of the gates page: each names where it came from, and what holds it is there."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PAGE = ROOT / "docs" / "quality" / "gates.md"
GUIDE = ROOT / "CONTRIBUTING.md"
HEADING = "## Rules learned from review and from failures"
ANCHOR = "rules-learned-from-review-and-from-failures"


def section() -> str:
    """The section of the rules: from its heading to the next heading of the same level."""
    text = PAGE.read_text(encoding="utf-8")
    assert text.count(HEADING + "\n") == 1, f"docs/quality/gates.md must have the heading `{HEADING}` once"
    return re.split(r"^## ", text.split(HEADING + "\n", 1)[1], maxsplit=1, flags=re.M)[0]


def rows():
    """Each rule of the section: (the rule, where it came from, what holds it)."""
    found = re.findall(r"^\| (?!Rule \|)(?!---)(.+?) \| (.+?) \| (.+?) \|$", section(), re.M)
    assert len(found) >= 20, f"only {len(found)} rules were read from the section; the form of its tables has changed"
    return found


def anchors_of(text: str) -> set:
    return {re.sub(r"[^\w\- ]", "", heading.strip().lower()).replace(" ", "-") for heading in re.findall(r"^#{1,4} (.+)$", text, re.M)}


def test_the_rules_are_in_the_gates_page_and_in_no_file_of_their_own():
    assert not (ROOT / "docs" / "quality" / "rules.md").exists(), (
        "docs/quality/rules.md is back. The rules are one section of docs/quality/gates.md, so that nobody misses a "
        "rule because it stands in another file. Fix: move its rows into that section.")
    assert "rules.md" not in (ROOT / "docs" / "README.md").read_text(encoding="utf-8")
    page = PAGE.read_text(encoding="utf-8")
    assert f"(#{ANCHOR})" in page, "the gates page does not link to its section of the rules"
    assert f"(docs/quality/gates.md#{ANCHOR})" in GUIDE.read_text(encoding="utf-8"), (
        "CONTRIBUTING.md does not point to the rules on the gates page. Fix: one line in its Testing section.")


def test_every_rule_names_the_pull_request_or_issue_it_came_from():
    without = [rule for rule, source, _held in rows() if not re.fullmatch(r"#\d+(?:, #\d+)*", source)]
    assert not without, (
        "these rules of docs/quality/gates.md do not name where they came from:\n  " + "\n  ".join(without)
        + "\nFix: give the number of the pull request or issue in the second column, as `#123` or `#123, #456`.")


def test_every_file_that_the_section_names_as_holding_a_rule_is_there():
    named = sorted({path for _rule, _source, held in rows() for path in re.findall(r"`((?:tests|scripts)/[\w./-]+)`", held)})
    assert named, "the section names no test and no script; the form of its third column has changed"
    gone = [path for path in named if not (ROOT / path).is_file()]
    assert not gone, (
        f"docs/quality/gates.md says that {gone} hold a rule, and they are not in the repository. Fix: give the path "
        "that the test or the script has now, or write `review` when nothing holds the rule any more.")


def test_a_rule_is_written_once_and_a_rule_of_the_guide_is_pointed_to():
    rules = [rule for rule, _source, _held in rows()]
    assert len(rules) == len(set(rules))
    guide = GUIDE.read_text(encoding="utf-8")
    pointed = [(rule, anchor) for rule in rules for anchor in re.findall(r"\(\.\./\.\./CONTRIBUTING\.md#([^)]+)\)", rule)]
    assert pointed, "no rule of the section points to the contributor guide; the form of those rows has changed"
    lost = [(rule, anchor) for rule, anchor in pointed if anchor not in anchors_of(guide)]
    assert not lost, f"these rules point to a heading that CONTRIBUTING.md does not have: {lost}"
    # A sentence of the guide is not written again here: the row points to it.
    said = " ".join(guide.split())
    twice = [rule for rule in rules if len(rule) > 40 and rule in said]
    assert not twice, f"these rules stand word for word in CONTRIBUTING.md too: {twice}. Fix: let the row point to the guide."
    for anchor in re.findall(r"\]\(#([^)]+)\)", section()):
        assert anchor in anchors_of(PAGE.read_text(encoding="utf-8")), f"the section links to #{anchor}, which the page does not have"


def test_the_gates_page_has_the_two_rounds_line_and_the_counts_with_their_date():
    page = " ".join(PAGE.read_text(encoding="utf-8").split())
    assert "After two red CI rounds on the same pull request, stop and rethink the change before a third push." in page
    assert "## What the checks found so far" in page and "This is its state on 2026-10-07." in page
    table = re.findall(r"^\| ([^|]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \| ([^|]+) \|$",
                       PAGE.read_text(encoding="utf-8"), re.M)
    counts = [row for row in table if row[0] not in ("Check", "---") and row[1].strip() in (
        "reports", "required", "blocks: every thread must be resolved")]
    assert len(counts) == 13, [row[0] for row in counts]
    for check, _mode, _on, found, right, false, outside in counts:
        numbers = [right, false, outside]
        if "not sorted yet" in numbers:
            continue
        total = int(re.match(r"\d+", found).group(0))
        assert sum(int(number) for number in numbers) == total, (
            f"the row `{check.strip()}` gives {found.strip()}, and its three columns add up to another number")
