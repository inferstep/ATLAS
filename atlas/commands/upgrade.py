"""atlas upgrade — staged, restore-on-failure upgrade of the Compose
deployment.

Records a restore point, stages the target images, starts them, waits
for readiness, runs a smoke check, and finalizes — automatically
restoring the prior release if any step fails. See upgrade_engine for
the orchestration; this module supplies the real Docker/health steps.
"""

import argparse
import contextlib
import os
import subprocess
import sys
import time
import urllib.request
from typing import Dict, List, Optional

from atlas import compose as compose_config
from atlas import config_schema as cs
from atlas import env as cli_env
from atlas import upgrade_engine as eng


def _compose(atlas_root: str, args: List[str], timeout: int = 600) -> None:
    cmd = compose_config.command(atlas_root, args)
    try:
        rc = subprocess.call(cmd, cwd=atlas_root, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise eng.UpgradeError(
            f"`{' '.join(args[:2])}` timed out after {timeout}s")
    if rc != 0:
        raise eng.UpgradeError(f"`{' '.join(args[:2])}` failed (exit {rc})")


# Fallback when the compose config can't be queried; must cover every
# GHCR image the base compose file deploys.
_IMAGES = ["atlas-proxy", "atlas-v3", "atlas-lens", "atlas-sandbox",
           "atlas-llama"]
_COSIGN_ISSUER = "https://token.actions.githubusercontent.com"


def _owner(atlas_root: str) -> str:
    """The GHCR owner compose resolves: shell env, then .env, then the
    default. Read at call time, after the engine's .env migration."""
    return cs.resolve("ATLAS_GHCR_OWNER",
                      compose_config.read_env_file(atlas_root),
                      eng.GHCR_OWNER) or eng.GHCR_OWNER


def _cosign_identity(owner: str) -> str:
    """The workflow identity that signed this owner's images. Images keep
    the signature of the repo that built them, so releases from before
    the move carry itigges22/ATLAS's identity. Images are built + signed
    on both branch pushes (refs/heads/…) and tag pushes (refs/tags/vX.Y.Z),
    so the identity must accept either ref type — matching only
    refs/heads would reject a validly-signed release image."""
    repo = (eng.LEGACY_GHCR_OWNER if owner == eng.LEGACY_GHCR_OWNER
            else eng.GHCR_OWNER)
    return (rf"https://github.com/{repo}/ATLAS/.github/workflows/"
            r"build-images.yml@refs/(heads|tags)/.*")


def _target_images(atlas_root: str, tag: str) -> List[str]:
    """GHCR-owned image refs from the effective compose config (backend
    overlays included — e.g. the Vulkan llama image), re-pinned to the
    target tag. Falls back to the static list if compose can't answer."""
    refs: List[str] = []
    prefix = f"ghcr.io/{_owner(atlas_root)}/"
    with contextlib.suppress(subprocess.SubprocessError, OSError):
        cmd = compose_config.command(atlas_root, ["config", "--images"])
        out = subprocess.check_output(cmd, cwd=atlas_root, text=True,
                                      timeout=60)
        for line in out.splitlines():
            line = line.strip()
            if line.startswith(prefix):
                name = line[len(prefix):].split(":", 1)[0]
                refs.append(f"{prefix}{name}:{tag}")
    return refs or [f"{prefix}{img}:{tag}" for img in _IMAGES]


def _verify_signatures(atlas_root: str, tag: str) -> None:
    """Verify each target image's keyless cosign signature. Best-effort:
    if cosign isn't installed we log and continue (an install without
    cosign still upgrades); a signature that FAILS verification raises
    UpgradeError so the upgrade aborts + restores. Skippable with
    ATLAS_UPGRADE_SKIP_VERIFY=1."""
    import shutil
    if os.environ.get("ATLAS_UPGRADE_SKIP_VERIFY") == "1":
        return
    if shutil.which("cosign") is None:
        print("  (cosign not installed — skipping signature verification)")
        return
    identity = _cosign_identity(_owner(atlas_root))
    for ref in _target_images(atlas_root, tag):
        try:
            proc = subprocess.run(
                ["cosign", "verify",
                 "--certificate-identity-regexp", identity,
                 "--certificate-oidc-issuer", _COSIGN_ISSUER, ref],
                stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
                text=True, timeout=300)
        except subprocess.TimeoutExpired:
            raise eng.UpgradeError(
                f"signature verification timed out for {ref} — refusing "
                "to upgrade (set ATLAS_UPGRADE_SKIP_VERIFY=1 to override)")
        if proc.returncode == 0:
            continue
        err = proc.stderr or ""
        # Not-published refs are skipped, not fatal: some compose
        # services build locally and never push (e.g. the ROCm llama
        # image). GHCR answers 403 DENIED at the anonymous token grant
        # for a repo that doesn't exist, so "access denied" here means
        # "no such published image", not a signature problem; a repo
        # that exists without the tag yields MANIFEST_UNKNOWN. A real
        # bad signature on a published image still falls through to the
        # UpgradeError.
        err_upper = err.upper()
        not_published = ("MANIFEST_UNKNOWN", "MANIFEST UNKNOWN",
                         "NAME_UNKNOWN", "DENIED", "UNAUTHORIZED")
        if any(code in err_upper for code in not_published):
            print(f"  (skipping {ref}: not published in the registry)")
            continue
        raise eng.UpgradeError(
            f"signature verification failed for {ref} — refusing to "
            "upgrade (set ATLAS_UPGRADE_SKIP_VERIFY=1 to override)")


def _snapshot_digests(atlas_root: str) -> Dict[str, str]:
    """Current image digests per service (best-effort; empty on failure —
    the restore point still records the tag + .env backup)."""
    try:
        cmd = compose_config.command(
            atlas_root, ["images", "--format", "json"])
        out = subprocess.check_output(cmd, cwd=atlas_root, text=True,
                                      timeout=60)
    except (subprocess.SubprocessError, OSError):
        return {}
    import json
    digests: Dict[str, str] = {}
    for line in out.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rec = json.loads(line)
        except ValueError:
            continue
        # `docker compose images --format json` may emit JSONL objects or
        # a single array; only per-image dicts carry the fields we want.
        records = rec if isinstance(rec, list) else [rec]
        for r in records:
            if not isinstance(r, dict):
                continue
            svc = r.get("Service") or r.get("Repository") or ""
            dig = r.get("ID") or r.get("Digest") or ""
            if svc and dig:
                digests[svc] = dig
    return digests


def _set_env_tag(atlas_root: str, tag: str) -> None:
    """Rewrite ATLAS_IMAGE_TAG in .env (append if missing)."""
    path = os.path.join(atlas_root, ".env")
    lines: List[str] = []
    found = False
    if os.path.isfile(path):
        with open(path) as fh:
            for line in fh:
                if line.strip().startswith("ATLAS_IMAGE_TAG="):
                    lines.append(f"ATLAS_IMAGE_TAG={tag}\n")
                    found = True
                else:
                    lines.append(line)
    if not found:
        # A .env whose last line lacks a trailing newline would otherwise
        # get the tag glued onto it, destroying both variables.
        if lines and not lines[-1].endswith("\n"):
            lines[-1] += "\n"
        lines.append(f"ATLAS_IMAGE_TAG={tag}\n")
    import tempfile
    d = os.path.dirname(path) or "."
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".env-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as fh:
            fh.writelines(lines)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


def _readiness(atlas_root: str, timeout_s: int = 180) -> bool:
    """Poll the proxy /ready until 200 or timeout."""
    url = compose_config.service_url("proxy") + "/ready"
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        # Not ready yet (connection refused, 5xx, ...) — poll again.
        with contextlib.suppress(Exception):
            with urllib.request.urlopen(url, timeout=5) as resp:
                if resp.status == 200:
                    return True
        time.sleep(3)
    return False


def _smoke(atlas_root: str) -> bool:
    """Quick doctor as the post-upgrade smoke check."""
    from atlas.commands import doctor
    try:
        rc = doctor.main(["--quick", "--json"])
    except SystemExit as e:
        rc = int(e.code or 0)
    return rc == 0


def _pull_timeout() -> int:
    """Staging ~13 GB of images can legitimately take a long while on a
    slow link; the pull gets a much longer leash than up/config."""
    try:
        return int(os.environ.get("ATLAS_UPGRADE_PULL_TIMEOUT", "3600"))
    except ValueError:
        return 3600


def _default_steps() -> eng.Steps:
    return eng.Steps(
        snapshot_digests=_snapshot_digests,
        set_env_tag=_set_env_tag,
        pull=lambda root: _compose(root, ["pull"], timeout=_pull_timeout()),
        up=lambda root: _compose(root, ["up", "-d"]),
        readiness=_readiness,
        smoke=_smoke,
        verify_signatures=_verify_signatures,
        log=lambda m: print(f"  {m}"),
    )


def _stamp() -> str:
    # Filesystem-safe timestamp for the restore-point backup name.
    return time.strftime("%Y%m%d-%H%M%S", time.localtime())


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas upgrade",
        description="Staged upgrade with automatic restore on failure.")
    parser.add_argument("--to", default="latest",
                        help="target image tag (default: latest)")
    parser.add_argument("--skip-smoke", action="store_true",
                        help="skip the post-upgrade doctor smoke check")
    parser.add_argument("--yes", action="store_true",
                        help="don't prompt before starting")
    parser.add_argument("--dry-run", action="store_true",
                        help="show the upgrade plan (current tag/digests → "
                             "target) without applying anything")
    args = parser.parse_args(argv)

    normalized = eng.normalize_image_tag(args.to)
    if normalized != args.to:
        print(f"(image tags carry no leading v — using {normalized})")
        args.to = normalized

    atlas_root = cli_env.atlas_root()
    if not os.path.isfile(os.path.join(atlas_root, "docker-compose.yml")):
        print("atlas upgrade: run from an ATLAS checkout.", file=sys.stderr)
        return 1

    previous = eng.read_env_tag(atlas_root)

    if args.dry_run:
        digests = _snapshot_digests(atlas_root)
        print(f"upgrade plan: {previous} → {args.to}")
        if digests:
            print("  current image digests:")
            for svc, dig in sorted(digests.items()):
                print(f"    {svc}: {dig}")
        else:
            print("  (no running images to diff against)")
        print("  target digests resolve on `docker compose pull` at apply.")
        print("  steps: verify signatures → record restore point → pull → "
              "up → readiness → smoke → finalize (auto-restore on failure).")
        print("  (dry run — nothing changed)")
        return 0

    same_tag = previous == args.to
    will_noop = same_tag and not eng.tag_is_mutable(args.to)
    if not args.yes and not will_noop:
        if same_tag:
            print(f"Re-pull {args.to} (a mutable tag — the registry may "
                  "point it at newer images). A restore point is recorded "
                  "first.")
        else:
            print(f"Upgrade {previous} → {args.to}. A restore point is "
                  "recorded first; a failed upgrade auto-restores the "
                  "previous release.")
        try:
            if input("Continue? [y/N] ").strip().lower() != "y":
                print("aborted.")
                return 1
        except EOFError:
            print("non-interactive; pass --yes to proceed.", file=sys.stderr)
            return 1

    try:
        result = eng.run_upgrade(atlas_root, args.to, _default_steps(),
                                 _stamp(), run_smoke=not args.skip_smoke)
    except eng.UpgradeError as e:
        print(f"\natlas upgrade: {e}", file=sys.stderr)
        return 1

    if result["status"] == "noop":
        print(result["detail"])
        return 0
    if result["status"] == "refreshed":
        # No rollback hint: a same-tag refresh replaces the cached
        # images, so `atlas rollback` can only come back up on the
        # refreshed build, not the pre-refresh one.
        print(f"\nRefreshed {result['target_tag']}. For reversible "
              "upgrades, pin release tags (--to vX.Y.Z).")
        return 0
    print(f"\nUpgraded to {result['target_tag']}. "
          f"Roll back with: atlas rollback")
    return 0
