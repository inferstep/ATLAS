"""atlas artifact — verify and roll back lens/ASA artifact bundles.

    atlas artifact verify [DIR]      verify signature + file hashes
    atlas artifact snapshot [DIR]    keep the current bundle for rollback
    atlas artifact rollback [DIR]    restore the last kept snapshot

Default DIR is the lens models dir. Snapshots keep one previous bundle
so a bad activation can be undone locally.
"""

import argparse
import os
import shutil
import sys
from typing import List, Optional

from atlas import artifact_manifest as am
from atlas import env as cli_env

SNAPSHOT_DIR = ".previous-bundle"
# Files that constitute a bundle snapshot (mirrors
# geometric_lens.provenance.BUNDLE_FILES; kept local so the stdlib-only
# CLI doesn't import the lens package).
BUNDLE_FILES = [
    "cost_field.pt", "cost_field.safetensors", "cx_normalization.json",
    "gx_xgboost.json", "gx_weights.json", "gx_thresholds.json",
    "drift_fingerprint.json", "model_identity.json",
]


def _default_dir() -> str:
    root = cli_env.atlas_root()
    return os.path.join(root, "services", "geometric-lens", "geometric_lens", "models")


def _verify(bundle_dir: str) -> int:
    ok, problems = am.verify_manifest(bundle_dir)
    if ok and not problems:
        print(f"artifact verify: OK (signed + all hashes match) — {bundle_dir}")
        return 0
    status = "OK" if ok else "FAILED"
    print(f"artifact verify: {status} — {bundle_dir}")
    for p in problems:
        print(f"  - {p}")
    return 0 if ok else 1


def _snapshot(bundle_dir: str) -> int:
    dest = os.path.join(bundle_dir, SNAPSHOT_DIR)
    # Start clean: files left over from an earlier snapshot generation
    # would otherwise merge into this one and roll back as a chimera.
    if os.path.isdir(dest):
        shutil.rmtree(dest)
    os.makedirs(dest, exist_ok=True)
    kept = 0
    for name in BUNDLE_FILES + [am.MANIFEST, am.SIGNATURE]:
        src = os.path.join(bundle_dir, name)
        if os.path.isfile(src):
            shutil.copy2(src, os.path.join(dest, name))
            kept += 1
    print(f"artifact snapshot: kept {kept} files in {dest}")
    return 0


def _rollback(bundle_dir: str) -> int:
    src = os.path.join(bundle_dir, SNAPSHOT_DIR)
    if not os.path.isdir(src):
        print(f"artifact rollback: no snapshot in {bundle_dir} "
              "(run `atlas artifact snapshot` before activating a new bundle)",
              file=sys.stderr)
        return 1
    # Remove current-bundle files the snapshot doesn't have, so the
    # restored bundle is exactly the snapshot (a leftover new-format
    # weight file or stale .sig would otherwise shadow the restored one).
    snap_files = set(os.listdir(src))
    for name in BUNDLE_FILES + [am.MANIFEST, am.SIGNATURE]:
        if name not in snap_files:
            stale = os.path.join(bundle_dir, name)
            if os.path.isfile(stale):
                os.unlink(stale)
    restored = 0
    for name in snap_files:
        shutil.copy2(os.path.join(src, name), os.path.join(bundle_dir, name))
        restored += 1
    print(f"artifact rollback: restored {restored} files. "
          "Restart the lens: docker compose restart geometric-lens")
    return 0


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(prog="atlas artifact")
    sub = parser.add_subparsers(dest="cmd")
    for name in ("verify", "snapshot", "rollback"):
        p = sub.add_parser(name)
        p.add_argument("dir", nargs="?", default=None)
    args = parser.parse_args(argv)

    if args.cmd not in ("verify", "snapshot", "rollback"):
        parser.print_help()
        return 1
    bundle_dir = args.dir or _default_dir()
    if not os.path.isdir(bundle_dir):
        print(f"atlas artifact: no such dir: {bundle_dir}", file=sys.stderr)
        return 1
    return {"verify": _verify, "snapshot": _snapshot,
            "rollback": _rollback}[args.cmd](bundle_dir)
