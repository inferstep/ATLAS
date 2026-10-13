"""The contributor guide keeps the section that other pages link to, and its own links lead somewhere."""
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GUIDE = (ROOT / "CONTRIBUTING.md").read_text(encoding="utf-8")


def anchor(heading: str) -> str:
    """The anchor that GitHub gives a heading: lower case, the marks taken out, a hyphen for each space."""
    return re.sub(r"[^\w\- ]", "", heading.strip().lower()).replace(" ", "-")


def test_the_section_on_red_checks_keeps_the_anchor_that_the_pinned_issue_links_to():
    headings = re.findall(r"^#{2,4} (.+)$", GUIDE, re.M)
    assert [anchor(heading) for heading in headings].count("when-a-check-is-red") == 1, (
        "CONTRIBUTING.md has no heading whose anchor is `#when-a-check-is-red`, or more than one. A pinned issue links "
        "to that anchor. Fix: keep one heading \"When a check is red\".")


def test_every_link_of_the_guide_to_a_place_in_the_guide_leads_to_a_heading():
    anchors = {anchor(heading) for heading in re.findall(r"^#{1,4} (.+)$", GUIDE, re.M)}
    linked = set(re.findall(r"\]\(#([^)]+)\)", GUIDE))
    assert linked, "the guide has no link to a place in itself; the form of its links has changed"
    lost = sorted(linked - anchors)
    assert not lost, f"these links of CONTRIBUTING.md lead to no heading of it: {lost}. Fix: give each the anchor of the heading it means."


def test_every_file_that_the_guide_links_to_is_there():
    linked = {target.split("#")[0] for target in re.findall(r"\]\(([^)#:]+(?:#[^)]*)?)\)", GUIDE)}
    gone = sorted(target for target in linked if target and not (ROOT / target).exists())
    assert not gone, f"CONTRIBUTING.md links to {gone}, and they are not in the repository."


def test_the_guide_says_once_what_happens_to_sonar_on_a_pull_request_from_a_fork():
    text = " ".join(GUIDE.split())
    assert text.count("On a pull request from a fork the Sonar check does not run; the change is analysed after the merge.") == 1


def test_the_guide_does_not_speak_in_the_first_person_singular():
    said = [(number, line) for number, line in enumerate(GUIDE.splitlines(), 1)
            if re.search(r"(?<![`\w/])(I|I'm|I've|I'd|me|my|mine|myself)(?![`\w/])", re.sub(r"`[^`]*`", "", line))]
    assert not said, (
        f"CONTRIBUTING.md speaks in the first person singular on {said}. A reader cannot tell who that is. Fix: write "
        "\"a maintainer\", \"maintainers\" or \"us\".")
