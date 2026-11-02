#!/usr/bin/env python3
"""Fail when a Dockerfile COPY names a source path that does not exist.

A COPY whose source was deleted is invisible to every other check: the tree
imports cleanly, tests pass, lint passes, and the failure only appears when
someone builds the image. It bit twice in one cleanup — geometric-lens
COPY'd `indexer/` and `retriever/` after the retrieval stack was removed, and
v3-service COPY'd `wavelet/` and `rpg.py` after the planner was removed. The
second one reached CI, where `tests` was green and only the image job failed.

Each Dockerfile is checked against its build context as declared in
docker-compose.yml, because the context determines what a relative source
resolves against: v3-service builds from the repo root, so its sources read
`v3-service/main.py`, while geometric-lens builds from its own directory and
its sources read `main.py`. Getting that wrong in either direction produces
false results, so contexts are read from compose rather than guessed.

Sources containing a glob or a build-arg substitution are skipped — they
cannot be resolved statically. `--from=` COPYs are skipped too: those name a
path inside an earlier build stage, not the context.

Usage: check_dockerfile_sources.py
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
COMPOSE = ROOT / "docker-compose.yml"

# Fallback when a Dockerfile is not referenced by docker-compose.yml: assume
# it builds from its own directory, the Docker default.
DEFAULT_CONTEXT = None


def _build_block(text: str, start: int, indent: int) -> str:
    """One `build:` block, up to the next key at the same or lower indent."""
    block = []
    for line in text[start:].splitlines():
        if line.strip() and (len(line) - len(line.lstrip())) <= indent:
            break
        block.append(line)
    return "\n".join(block)


def _dockerfile_path(context: str, blob: str) -> str:
    """The repo-relative Dockerfile of the `build:` block `blob`."""
    dockerfile = re.search(r'^\s*dockerfile:\s*(\S+)', blob, re.MULTILINE)
    if dockerfile:
        df = dockerfile.group(1).strip().strip('"\'')
        # `dockerfile:` is relative to the context.
        df_path = (Path(context) / df) if not df.startswith("/") else Path(df)
    else:
        df_path = Path(context) / "Dockerfile"
    # Only a leading `./`: lstrip("./") also ate a hidden directory's dot (#277).
    return re.sub(r"^(?:\./)+", "", Path(df_path).as_posix())


def compose_contexts() -> dict[str, str]:
    """Map dockerfile path -> build context, both repo-relative.

    Parsed with regexes rather than PyYAML so the gate has no dependency and
    runs in the same stdlib-only step as the other static checks.
    """
    if not COMPOSE.exists():
        return {}
    text = COMPOSE.read_text(encoding="utf-8")
    out: dict[str, str] = {}
    for m in re.finditer(r'^(\s+)build:\s*$', text, re.MULTILINE):
        blob = _build_block(text, m.end(), len(m.group(1)))
        ctx = re.search(r'^\s*context:\s*(\S+)', blob, re.MULTILINE)
        if not ctx:
            continue
        context = ctx.group(1).strip().strip('"\'')
        out[_dockerfile_path(context, blob)] = context
    return out


def _statement_at(lines: list[str], i: int) -> tuple[str, int]:
    """The statement that starts at line `i`, with its continuations joined, and the index of its last line."""
    stmt = lines[i]
    while stmt.rstrip().endswith("\\") and i + 1 < len(lines):
        i += 1
        stmt = stmt.rstrip()[:-1] + " " + lines[i]
    return stmt.strip(), i


def _sources_of(stmt: str) -> list[str]:
    """The sources of a COPY or ADD that can be looked up; none for any other statement."""
    if not re.match(r'^(COPY|ADD)\s', stmt, re.IGNORECASE):
        return []
    # Skip cross-stage copies: the source is inside another stage.
    if re.search(r'--from=', stmt, re.IGNORECASE):
        return []
    parts = [p for p in stmt.split()[1:] if not p.startswith("--")]
    # Last token is the destination.
    return [src for src in parts[:-1] if not any(c in src for c in "*?[$")]


def copy_sources(dockerfile: Path) -> list[tuple[int, str]]:
    """Yield (lineno, source) for every resolvable COPY/ADD source."""
    out: list[tuple[int, str]] = []
    lines = dockerfile.read_text(encoding="utf-8").splitlines()
    i = 0
    while i < len(lines):
        stmt, i = _statement_at(lines, i)
        out += [(i + 1, src) for src in _sources_of(stmt)]
        i += 1
    return out


def _check_dockerfile(df: Path, contexts: dict[str, str]) -> tuple[int, list[str]]:
    """How many COPY sources `df` names, and a line for each that does not exist."""
    rel = df.relative_to(ROOT).as_posix()
    context = contexts.get(rel, DEFAULT_CONTEXT)
    base = (ROOT / context).resolve() if context else df.parent
    where = f"context={context}" if context else "context=<dockerfile dir>"
    sources = copy_sources(df)
    return len(sources), [f"{rel}:{lineno}: COPY source {src!r} does not exist ({where})"
                          for lineno, src in sources if not (base / src).exists()]


def main() -> int:
    contexts = compose_contexts()
    problems: list[str] = []
    checked = 0

    for df in sorted(ROOT.rglob("Dockerfile*")):
        if any(p in df.parts for p in (".git", "node_modules", ".ua")):
            continue
        count, missing = _check_dockerfile(df, contexts)
        checked += count
        problems += missing

    if problems:
        print(f"check_dockerfile_sources: {len(problems)} missing COPY source(s):",
              file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print("\nA deleted file left in a COPY breaks the image build only — "
              "imports, tests, and lint all stay green.", file=sys.stderr)
        return 1

    print(f"check_dockerfile_sources: {checked} COPY source(s) resolve")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
