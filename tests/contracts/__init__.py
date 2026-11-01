"""Contract-test helpers.

Go functions and constants move between files of a package during
reorganizations, so contract tests locate sources by a content marker,
never by filename.
"""

from pathlib import Path

REPO = Path(__file__).resolve().parents[2]


def go_source(component: str, marker: str) -> str:
    """Source of the non-test .go file under REPO/component containing marker."""
    for go in sorted((REPO / component).rglob("*.go")):
        if go.name.endswith("_test.go"):
            continue
        src = go.read_text()
        if marker in src:
            return src
    raise AssertionError(f"{marker!r} not found in any {component}/*.go")


# ---------------------------------------------------------------------------
# Registry: modules that are duplicated by design
# ---------------------------------------------------------------------------
#
# geometric-lens and sandbox images are each built from their own Docker
# context (`context: ./geometric-lens`, `context: ./sandbox`), and atlas is
# a pip package no service image installs. None of them can import one
# shared path, so byte-identity across copies is the parity mechanism and
# the contract tests are what enforce it.
#
# Registering a copy here is what puts it under contract. Every copy also
# carries COPY_NOTICE_MARKER in its module docstring, and
# test_no_unregistered_copies asserts the two sets agree — so a service
# that copies one of these files without registering it fails the suite.
COPY_NOTICE_MARKER = "CANONICAL COPY NOTICE"

STRUCTURED_LOG_COPIES = [
    REPO / "geometric-lens" / "geometric_lens" / "structured_log.py",
    REPO / "sandbox" / "structured_log.py",
    REPO / "v3-service" / "structured_log.py",
]

# atlas/redact.py is the same module under a different name, which is why
# discovery below keys on the notice marker rather than the filename.
PRIVATE_VALUE_COPIES = [
    REPO / "geometric-lens" / "geometric_lens" / "private_values.py",
    REPO / "sandbox" / "private_values.py",
    REPO / "v3-service" / "private_values.py",
    REPO / "atlas" / "redact.py",
]

_SKIP_DIRS = {
    ".git", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache",
    "node_modules", ".venv", "venv", "build", "dist", "atlas.egg-info",
}


def files_with_copy_notice() -> set:
    """Every .py file in the repo whose docstring carries the copy notice.

    Located by content marker, not by filename, for the same reason
    go_source() is: a copy can land anywhere under any name.
    """
    self_path = Path(__file__).resolve()
    found = set()
    for path in REPO.rglob("*.py"):
        # This module spells the marker out in order to search for it.
        if path.resolve() == self_path:
            continue
        if _SKIP_DIRS.intersection(path.parts):
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            continue
        if COPY_NOTICE_MARKER in text:
            found.add(path)
    return found
