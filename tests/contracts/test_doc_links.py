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

REPO = Path(__file__).resolve().parents[2]

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


def test_relative_links_and_anchors_resolve():
    # Git-tracked Markdown only: local-only files (git-ignored notes) do not exist in CI checkouts
    # and are not part of the published docs.
    tracked = subprocess.run(["git", "ls-files", "*.md"], cwd=REPO, capture_output=True, text=True, check=True)
    failures = []
    for path in sorted({REPO / name for name in tracked.stdout.splitlines() if name}):
        if SKIP_DIRS.intersection(path.parts) or not path.is_file():
            continue
        rel = path.relative_to(REPO)
        for lineno, target in _links(path):
            if target.startswith(("http://", "https://", "mailto:", "ftp://")) or f"{rel}:{target}" in SKIP:
                continue
            split = urlsplit(target)
            if split.scheme or split.netloc:  # any other scheme or a host is external too
                continue
            file_part, fragment = unquote(split.path), unquote(split.fragment)
            dest = (path.parent / file_part).resolve() if file_part else path
            if not dest.exists():
                failures.append(f"{rel}:{lineno} → {target} (file not found)")
            elif fragment and dest.suffix == ".md":
                anchors = _anchors(dest)
                if fragment not in anchors and fragment.lower() not in anchors:
                    failures.append(f"{rel}:{lineno} → {target} (no such anchor)")
    assert not failures, "broken Markdown links/anchors:\n  " + "\n  ".join(failures)
