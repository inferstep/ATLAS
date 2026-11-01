"""Version-string parity across every surface that states a release.

pyproject.toml is the single source of truth. Each documentation or
code surface that repeats the version (package __version__, README
badges, ARCHITECTURE header, CHANGELOG top entry, SUPPORT_MATRIX
applies-to line, translated README badges) must agree, so a release
bump can't leave a stale number behind. Parsing only.
"""

import re
from pathlib import Path

import pytest

from .. import go_source

REPO = Path(__file__).resolve().parents[3]


def _truth() -> str:
    text = (REPO / "pyproject.toml").read_text()
    m = re.search(r'^version\s*=\s*"(\d+\.\d+\.\d+)"', text, re.M)
    assert m, "pyproject.toml must declare version = \"X.Y.Z\""
    return m.group(1)


@pytest.mark.parametrize("path, expected", [
    pytest.param("README.md", "version-V{}-", id="readme-badge"),
    pytest.param("docs/lang/zh-CN/README.md", "version-V{}-", id="zh-CN-readme-badge"),
    pytest.param("docs/lang/ja/README.md", "version-V{}-", id="ja-readme-badge"),
    pytest.param("docs/lang/ko/README.md", "version-V{}-", id="ko-readme-badge"),
    pytest.param("docs/ARCHITECTURE.md", "ATLAS V{}", id="architecture-header"),
    # The latest-release line of each README links its release page.
    pytest.param("README.md", "releases/tag/v{})", id="readme-latest-release"),
    pytest.param("docs/lang/zh-CN/README.md", "releases/tag/v{})", id="zh-CN-readme-latest-release"),
    pytest.param("docs/lang/ja/README.md", "releases/tag/v{})", id="ja-readme-latest-release"),
    pytest.param("docs/lang/ko/README.md", "releases/tag/v{})", id="ko-readme-latest-release"),
])
def test_doc_states_the_version(path, expected):
    version = _truth()
    stated = expected.format(version)
    assert stated in (REPO / path).read_text(), f"{path} must contain {stated!r} (pyproject.toml says {version})"


@pytest.mark.parametrize("path, version_line", [
    pytest.param("atlas/__init__.py", r'^__version__\s*=\s*"([^"]+)"', id="package-dunder-version"),
    pytest.param("docs/SUPPORT_MATRIX.md", r"Applies to:\s*\*\*V(\d+\.\d+\.\d+)", id="support-matrix-applies-to"),
])
def test_declared_version_matches(path, version_line):
    version = _truth()
    m = re.search(version_line, (REPO / path).read_text(), re.M)
    assert m, f"{path} must have a line that matches the pattern {version_line}"
    assert m.group(1) == version, f"{path} says {m.group(1)!r}, pyproject.toml says {version!r}"


def test_changelog_top_entry_matches():
    version = _truth()
    text = (REPO / "CHANGELOG.md").read_text()
    # An optional leading [Unreleased] section (Keep-a-Changelog
    # convention for merged-but-untagged work) sits above the release
    # entries; the version sync-check applies to the first RELEASE
    # heading below it.
    headings = re.findall(r"^## \[([^\]]+)\]", text, re.M)
    assert headings, "CHANGELOG.md must contain a '## [X.Y.Z]' release heading"
    releases = [h for h in headings if h.lower() != "unreleased"]
    assert releases, "CHANGELOG.md must contain a release heading besides [Unreleased]"
    assert releases[0] == version, (
        f"CHANGELOG.md top release entry is [{releases[0]}], "
        f"pyproject.toml says {version} — add the release entry")


def test_proxy_startup_banner_matches():
    """The proxy prints its version at startup from a hardcoded literal.

    It is the only version surface that lives in code rather than a doc, so
    without this a release bump leaves the running service announcing the
    previous version while every documented surface says the new one.
    Located by content marker, not filename, so consolidating proxy files
    does not silently drop the check.
    """
    version = _truth()
    src = go_source("proxy", "ATLAS Proxy v")
    m = re.search(r'"ATLAS Proxy v(\d+\.\d+\.\d+) starting', src)
    assert m, "proxy startup banner must say 'ATLAS Proxy vX.Y.Z starting'"
    assert m.group(1) == version, (
        f"proxy startup banner says {m.group(1)!r}, "
        f"pyproject.toml says {version!r}")
