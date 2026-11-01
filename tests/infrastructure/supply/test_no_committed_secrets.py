"""Accidental-secret scan over git-tracked files.

Fails if a tracked file contains a high-signal credential shape (private
key block, an ATLAS service/API token literal, an AWS access key, or a
URL with an embedded password). This is the local, dependency-free
complement to the CI secret scan — it runs in the normal pytest matrix
so a credential commit is caught before review.

Test fixtures with obviously-synthetic values are allowed via ALLOWLIST
so the private-value corpus and these tests don't trip the scan.
"""

import re
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]

# High-signal patterns. Deliberately narrow to avoid false positives on
# ordinary code (the generic "token=" assignment lives in the private-
# value FILTER, not here — this is for committed real secrets).
PATTERNS = {
    "private key block": re.compile(
        r"-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----"),
    "atlas service token": re.compile(r"atlas-st-[A-Za-z0-9_-]{16,}"),
    "atlas api key": re.compile(r"sk-atlas-[A-Za-z0-9_-]{16,}"),
    "aws access key id": re.compile(r"AKIA[0-9A-Z]{16}"),
    "url password": re.compile(r"://[^/\s:@]+:[^/\s:@]{6,}@"),
}

# Paths whose synthetic fixtures intentionally contain secret-shaped
# strings. Kept explicit and narrow.
ALLOWLIST = {
    "tests/fixtures/private_value_fixtures.json",
    "tests/contracts/test_private_value_filtering.py",
    "tests/infrastructure/supply/test_no_committed_secrets.py",
    "tests/infrastructure/release/test_release_signing.py",
    "tests/cli/test_service_token.py",
    "tests/cli/test_diagnostics.py",
    "proxy/main_test.go",
    "proxy/tools_test.go",
    ".github/allowed_signers",     # public keys only, no private material
}


def test_no_secrets_in_tracked_files():
    tracked = subprocess.check_output(["git", "ls-files"], cwd=REPO, text=True).splitlines()
    findings = []
    for rel in tracked:
        if rel in ALLOWLIST:
            continue
        path = REPO / rel
        # skip large files
        try:
            if path.stat().st_size > 2_000_000:
                continue
            text = path.read_text(encoding="utf-8", errors="ignore")
        except (OSError, ValueError):
            continue
        for label, pat in PATTERNS.items():
            if pat.search(text):
                findings.append(f"{rel}: possible {label}")
    assert not findings, (
        "possible committed secrets (add to ALLOWLIST only if synthetic):\n  "
        + "\n  ".join(findings))


def test_allowlist_entries_still_exist():
    # Keep the allowlist honest — a stale entry hides a gap.
    for rel in ALLOWLIST:
        assert (REPO / rel).exists(), f"stale secret-scan allowlist entry: {rel}"
