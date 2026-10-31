"""Configuration drift check.

Every key shipped in .env.example / atlas.conf.example must have a
reader; every ATLAS_* variable interpolated by the compose files must be
documented in docs/CONFIGURATION.md. Exceptions require a documented
allowlist entry.
"""

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

# Directories whose text is searched for readers.
READER_GLOBS = [
    "proxy/*.go", "tui/*.go",
    "atlas/**/*.py", "v3-service/**/*.py", "geometric-lens/**/*.py",
    "sandbox/*.py",
    "scripts/*.sh", "scripts/**/*.sh", "scripts/**/*.py",
    "inference/*.sh", "inference/Dockerfile*",
    "templates/*.tmpl", "docker-compose*.yml", "compose/*.yml",
]


def _reader_corpus() -> str:
    parts = []
    for pattern in READER_GLOBS:
        for p in REPO.glob(pattern):
            if p.is_file():
                parts.append(p.read_text(errors="replace"))
    return "\n".join(parts)


def _example_keys(path: Path) -> set:
    keys = set()
    for line in path.read_text().splitlines():
        m = re.match(r"^([A-Z][A-Z0-9_]+)=", line.strip())
        if m:
            keys.add(m.group(1))
    return keys


def test_env_example_keys_all_have_readers():
    corpus = _reader_corpus()
    dead = {k for k in _example_keys(REPO / ".env.example")
            if k not in corpus}
    assert not dead, (
        f".env.example ships keys nothing reads: {sorted(dead)}. "
        "Remove the key or wire its reader.")


def test_atlas_conf_example_keys_all_have_readers():
    corpus = _reader_corpus()
    dead = {k for k in _example_keys(REPO / "atlas.conf.example")
            if k not in corpus}
    assert not dead, (
        f"atlas.conf.example ships keys nothing reads: {sorted(dead)}. "
        "Remove the key (recording it in CONFIGURATION.md § removed "
        "variables) or wire its reader.")


def test_compose_interpolations_are_documented():
    doc = (REPO / "docs" / "CONFIGURATION.md").read_text()
    example = (REPO / ".env.example").read_text()
    undocumented = set()
    for compose in [*REPO.glob("docker-compose*.yml"), *REPO.glob("compose/*.yml")]:
        for var in re.findall(r"\$\{(ATLAS_[A-Z0-9_]+)", compose.read_text()):
            if f"`{var}`" not in doc and var not in example:
                undocumented.add(var)
    assert not undocumented, (
        f"compose interpolates undocumented vars: {sorted(undocumented)}. "
        "Add a row to docs/CONFIGURATION.md (or the .env.example entry).")


def test_documented_atlas_vars_have_readers():
    """Every `ATLAS_*` table row in CONFIGURATION.md must be read
    somewhere in the repo — a documented knob with no reader is a
    silently-ignored setting."""
    doc = (REPO / "docs" / "CONFIGURATION.md").read_text()
    corpus = _reader_corpus()
    documented = set(re.findall(r"^\| `(ATLAS_[A-Z0-9_]+)`", doc,
                                flags=re.M))
    dead = {v for v in documented if v not in corpus}
    assert not dead, (
        f"CONFIGURATION.md documents vars nothing reads: {sorted(dead)}. "
        "Remove the row or wire the reader.")
