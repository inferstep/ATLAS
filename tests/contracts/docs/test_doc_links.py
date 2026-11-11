"""Relative-link and anchor integrity for every Markdown file.

Every `[text](target)` link with a relative-file target must point at a
file that exists, and every `#fragment` (same-file or cross-file) must
match a heading anchor — computed with GitHub's slugger (lowercase,
punctuation stripped, spaces to hyphens, `-1`/`-2` suffixes for
duplicate headings) — or an explicit `<a id="..."></a>` anchor.
External http(s)/mailto links are never checked; this stays hermetic.
"""

import functools
import re
import subprocess
from pathlib import Path
from urllib.parse import unquote, urlsplit

REPO = Path(__file__).resolve().parents[3]

SKIP_DIRS = {".git", "node_modules", ".venv", "venv", "__pycache__",
             ".pytest_cache"}

# Known-unfixable links, as "relative/path.md:target". Each entry needs
# a comment explaining why the link cannot be fixed.
SKIP: set = set()

LINK_RE = re.compile(r"\[[^\]]*\]\(\s*(<[^>]*>|[^()\s]+(?:\([^()\s]*\))?)\s*(?:\"[^\"]*\")?\)")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.*)$")
EXPLICIT_ANCHOR_RE = re.compile(r'<a\s+(?:id|name)="([^"]+)"')


def _lines_without_code_blocks(text: str) -> list:
    """The lines of a Markdown text, each line of a fenced code block blanked, so line numbers stay right."""
    out, in_fence, fence = [], False, ""
    for line in text.splitlines():
        stripped = line.lstrip()
        if not in_fence and (stripped.startswith("```") or stripped.startswith("~~~")):
            in_fence, fence = True, stripped[:3]
            out.append("")
        elif in_fence:
            if stripped.startswith(fence):
                in_fence = False
            out.append("")
        else:
            out.append(line)
    return out


def _links(path: Path):
    """Yield (line number, target) for every inline link of a Markdown file that renders as a link."""
    for lineno, line in enumerate(_lines_without_code_blocks(path.read_text(errors="replace")), 1):
        line = re.sub(r"`[^`]*`", "", line)  # a link inside inline code does not render
        for link in LINK_RE.finditer(line):
            yield lineno, link.group(1).strip("<>").strip()


@functools.cache
def _anchors(path: Path) -> set:
    """The anchors of a Markdown file: its heading slugs as GitHub makes them, and its explicit <a> anchors."""
    raw = path.read_text(errors="replace")
    anchors, seen = set(), {}
    for line in _lines_without_code_blocks(raw):
        heading = HEADING_RE.match(line)
        if not heading:
            continue
        # GitHub's slugger: the rendered text only (a link keeps its text), lowercase, punctuation dropped
        # (word characters incl. CJK, "-", "_" and spaces stay), each space one hyphen, no collapsing.
        text = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", heading.group(2).strip()).lower()
        slug = re.sub(r"[^\w\- ]", "", text).replace(" ", "-")
        # The second heading with the same slug gets "-1", the third "-2", and so on.
        repeats = seen.get(slug, 0)
        seen[slug] = repeats + 1
        anchors.add(slug if repeats == 0 else f"{slug}-{repeats}")
    anchors.update(EXPLICIT_ANCHOR_RE.findall(raw))
    return anchors


def _markdown_files() -> list:
    """The git-tracked Markdown files of the repository, sorted, without the files in SKIP_DIRS."""
    # Git-tracked Markdown only: local-only files (git-ignored notes) do not exist in CI checkouts
    # and are not part of the published docs.
    tracked = subprocess.run(["git", "ls-files", "*.md"], cwd=REPO, capture_output=True, text=True, check=True)
    paths = sorted({REPO / name for name in tracked.stdout.splitlines() if name})
    return [path for path in paths if not SKIP_DIRS.intersection(path.parts) and path.is_file()]


def _relative_links():
    """Yield ("file:line", target, destination path, fragment) for each relative link of the Markdown files."""
    for path in _markdown_files():
        rel = path.relative_to(REPO)
        for lineno, target in _links(path):
            if target.startswith(("http://", "https://", "mailto:", "ftp://")) or f"{rel}:{target}" in SKIP:
                continue
            split = urlsplit(target)
            if split.scheme or split.netloc:  # any other scheme or a host is external too
                continue
            file_part = unquote(split.path)
            dest = (path.parent / file_part).resolve() if file_part else path
            yield f"{rel}:{lineno}", target, dest, unquote(split.fragment)


def test_every_relative_link_names_a_file_that_exists():
    failures = []
    for where, target, dest, _fragment in _relative_links():
        if not dest.exists():
            failures.append(f"{where} → {target} (file not found)")
    assert not failures, "broken Markdown links:\n  " + "\n  ".join(failures)


def test_every_fragment_names_an_anchor_of_its_file():
    failures = []
    for where, target, dest, fragment in _relative_links():
        # A link to a missing file fails in the test above; a fragment of a non-Markdown file is not checked.
        if fragment and dest.suffix == ".md" and dest.exists():
            anchors = _anchors(dest)
            if fragment not in anchors and fragment.lower() not in anchors:
                failures.append(f"{where} → {target} (no such anchor)")
    assert not failures, "broken Markdown anchors:\n  " + "\n  ".join(failures)
