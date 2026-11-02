"""Freshness checks for Go binaries built from an editable ATLAS checkout."""

from __future__ import annotations

import os
import stat
from typing import Iterable, Optional


_GO_METADATA = frozenset({"go.mod", "go.sum"})


def newest_go_source_mtime_ns(
    source_dir: str,
    extra_names: Iterable[str] = (),
) -> Optional[int]:
    """Return the newest relevant source mtime, or ``None`` without source."""
    if not source_dir or not os.path.isdir(source_dir):
        return None
    names = _GO_METADATA | frozenset(extra_names)
    newest: Optional[int] = None
    for folder, _, files in os.walk(source_dir):
        for name in files:
            if not (name.endswith(".go") or name in names):
                continue
            try:
                info = os.lstat(os.path.join(folder, name))
            except OSError:
                continue
            if stat.S_ISREG(info.st_mode):
                newest = info.st_mtime_ns if newest is None else max(newest, info.st_mtime_ns)
    return newest


def go_binary_is_current(
    binary: Optional[str],
    source_dir: str,
    extra_names: Iterable[str] = (),
) -> bool:
    """True when an executable exists and is not older than checkout source.

    If source is unavailable, an existing release binary is accepted because
    there is no local build input against which to judge it.
    """
    if not binary or not os.path.isfile(binary) or not os.access(binary, os.X_OK):
        return False
    newest_source = newest_go_source_mtime_ns(source_dir, extra_names)
    if newest_source is None:
        return True
    try:
        return os.stat(binary, follow_symlinks=True).st_mtime_ns >= newest_source
    except OSError:
        return False
