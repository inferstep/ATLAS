"""Upgrade / rollback engine for the Docker Compose deployment.

The design goal is that a *failed* upgrade always leaves a working
install: every upgrade first records a restore point (the current image
tag, the resolved image digests, and a copy of .env), then runs the
staged steps, and on ANY failure automatically restores the prior
release. `atlas rollback` re-applies a restore point on demand.

The Docker/health steps are injected as callables so the orchestration
(backup → stage → start → readiness → smoke → finalize, with
restore-on-failure) is deterministically testable without Docker or a
registry. The default callables shell out to docker compose + doctor.
"""

import json
import os
import re
import shutil
import tempfile
from dataclasses import dataclass, field
from typing import Callable, Dict, Optional

from atlas import compose as compose_config

RESTORE_DIR = ".atlas-upgrade"
RESTORE_POINT = "restore-point.json"

# ATLAS moved from itigges22/ATLAS to inferstep/ATLAS. Releases after the
# move are published only under ghcr.io/inferstep, but `atlas init` used
# to write the old owner into .env, where it outranks the compose default.
GHCR_OWNER = "inferstep"
LEGACY_GHCR_OWNER = "itigges22"


class UpgradeError(Exception):
    """A step failed; the engine restores the prior release and re-raises
    a message suitable for the CLI."""


@dataclass
class Steps:
    """Injectable side-effects. Defaults are wired in cli/commands.

    Each raises UpgradeError (or returns False for readiness/smoke) to
    signal failure; the engine converts that into an automatic restore.
    """
    snapshot_digests: Callable[[str], Dict[str, str]]
    set_env_tag: Callable[[str, str], None]
    pull: Callable[[str], None]
    up: Callable[[str], None]
    readiness: Callable[[str], bool]
    smoke: Callable[[str], bool]
    log: Callable[[str], None] = field(default=lambda m: None)
    # Verify target-image signatures before applying. Default no-op so an
    # install without cosign still upgrades; the real step (cli/commands)
    # runs `cosign verify` and raises UpgradeError on a bad signature.
    verify_signatures: Callable[[str, str], None] = field(
        default=lambda root, tag: None)


def restore_dir(atlas_root: str) -> str:
    return os.path.join(atlas_root, RESTORE_DIR)


def restore_point_path(atlas_root: str) -> str:
    return os.path.join(restore_dir(atlas_root), RESTORE_POINT)


def _env_path(atlas_root: str) -> str:
    return os.path.join(atlas_root, ".env")


def read_env_tag(atlas_root: str, default: str = "latest") -> str:
    """Current ATLAS_IMAGE_TAG from .env (the deployed release marker). A
    missing/unreadable .env means the default tag is in effect."""
    values = compose_config.read_env_file(atlas_root)
    return values.get("ATLAS_IMAGE_TAG") or default


def write_restore_point(atlas_root: str, previous_tag: str, target_tag: str,
                        digests: Dict[str, str], stamp: str) -> str:
    """Record how to get back to the current release, and back up .env.

    stamp is passed in (the CLI provides it) so the engine stays free of
    wall-clock calls.
    """
    rdir = restore_dir(atlas_root)
    os.makedirs(rdir, exist_ok=True)
    env_backup = os.path.join(rdir, f"env-{stamp}.bak")
    src = _env_path(atlas_root)
    if os.path.isfile(src):
        shutil.copy2(src, env_backup)
    point = {
        "stamp": stamp,
        "previous_tag": previous_tag,
        "target_tag": target_tag,
        "previous_digests": digests,
        "env_backup": os.path.relpath(env_backup, atlas_root)
        if os.path.isfile(env_backup) else None,
    }
    # Unique temp in the same dir so concurrent writers don't race on a
    # shared .tmp name; os.replace is atomic on the final path.
    dest = restore_point_path(atlas_root)
    fd, tmp = tempfile.mkstemp(dir=rdir, prefix=".rp-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as fh:
            json.dump(point, fh, indent=2)
        os.replace(tmp, dest)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    return env_backup


def read_restore_point(atlas_root: str) -> Optional[dict]:
    try:
        with open(restore_point_path(atlas_root)) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return None


def _restore(atlas_root: str, point: dict, steps: Steps) -> None:
    """Put the prior release back: restore .env (incl. its
    ATLAS_IMAGE_TAG) and bring the stack up on the previous tag."""
    steps.log(f"restoring previous release ({point['previous_tag']})…")
    env_backup = point.get("env_backup")
    if env_backup:
        backup_abs = os.path.join(atlas_root, env_backup)
        if os.path.isfile(backup_abs):
            shutil.copy2(backup_abs, _env_path(atlas_root))
    # Belt-and-suspenders: force the tag back even if the .env restore
    # was a no-op (e.g. .env absent when the restore point was written).
    steps.set_env_tag(atlas_root, point["previous_tag"])
    # No pull here: the previous images are already present locally (they
    # were running before the upgrade), and for a mutable previous tag
    # (latest, dev) a pull could replace the known-good cached images
    # with whatever the tag points at NOW. `up` starts from the cache;
    # compose fetches on its own only if an image is truly absent.
    if point["previous_tag"] == point.get("target_tag"):
        steps.log("note: upgrade re-pulled the same tag — if the tag moved, "
                  "the cached previous images may already be gone")
    steps.up(atlas_root)


def _restore_or_flag(atlas_root: str, steps: Steps, cause: str,
                     previous_tag: str) -> "UpgradeError":
    """Attempt the automatic restore; return the UpgradeError to raise.

    A restore that itself fails must not swallow the original upgrade
    failure — the caller needs both: why the upgrade failed AND that the
    install is now in a half-restored state.
    """
    point = read_restore_point(atlas_root)
    if not point:
        return UpgradeError(
            f"{cause}. No restore point found — the install was left on "
            f"the target tag; run `atlas rollback --to {previous_tag}` "
            "to return manually.")
    try:
        _restore(atlas_root, point, steps)
    except Exception as restore_err:
        return UpgradeError(
            f"{cause}. Automatic restore of the previous release "
            f"({previous_tag}) ALSO failed: {restore_err}. The stack may "
            "be partially down — once Docker is healthy, run "
            "`atlas rollback` to finish restoring.")
    return UpgradeError(
        f"{cause}. Automatically restored the previous release "
        f"({previous_tag}).")


def normalize_image_tag(tag: str) -> str:
    """Registry semver tags carry no leading v (git tag v3.1.3 publishes
    atlas-*:3.1.3), so a v-prefixed semver from the user is mapped to
    the tag that actually exists."""
    if re.fullmatch(r"v\d+(\.\d+)*([.-].*)?", tag):
        return tag[1:]
    return tag


def tag_is_mutable(tag: str) -> bool:
    """Release tags (X.Y.Z…, with or without a v prefix) are treated as
    immutable; anything else (latest, dev, branch tags) can move
    between pulls."""
    return re.fullmatch(r"v?\d+(\.\d+)*([.-].*)?", tag) is None


def legacy_ghcr_owner_applies(env: Dict[str, str],
                              keep_release_pin: bool) -> bool:
    """Whether a parsed .env still names the pre-move image owner and
    should move to the current one.

    An owner set in the process environment is an explicit choice (and
    wins in compose too), so it is left alone. With keep_release_pin, an
    ATLAS_IMAGE_TAG that pins a release also keeps the old owner:
    releases from before the move exist only there."""
    if os.environ.get("ATLAS_GHCR_OWNER"):
        return False
    if env.get("ATLAS_GHCR_OWNER") != LEGACY_GHCR_OWNER:
        return False
    tag = env.get("ATLAS_IMAGE_TAG") or "latest"
    return not (keep_release_pin and not tag_is_mutable(tag))


def migrate_legacy_ghcr_owner(env_path: str,
                              keep_release_pin: bool) -> Optional[str]:
    """Rewrite ATLAS_GHCR_OWNER from the pre-move owner to the current one
    in place, keeping every other line. Returns a note when it rewrote
    the file, None when nothing applied."""
    env = compose_config.read_env_path(env_path)
    if not legacy_ghcr_owner_applies(env, keep_release_pin):
        return None
    lines = []
    with open(env_path, encoding="utf-8") as fh:
        for line in fh:
            body = line.strip()
            prefix = "export " if body.startswith("export ") else ""
            key = body[len(prefix):].split("=", 1)[0].strip()
            if "=" in body and key == "ATLAS_GHCR_OWNER":
                line = f"{prefix}ATLAS_GHCR_OWNER={GHCR_OWNER}\n"
            lines.append(line)
    d = os.path.dirname(env_path) or "."
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".env-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.writelines(lines)
        shutil.copymode(env_path, tmp)
        os.replace(tmp, env_path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    return (f"ATLAS_GHCR_OWNER {LEGACY_GHCR_OWNER} → {GHCR_OWNER} in .env "
            f"(ATLAS moved to {GHCR_OWNER}/ATLAS)")


def run_upgrade(atlas_root: str, target_tag: str, steps: Steps,
                stamp: str, run_smoke: bool = True) -> dict:
    """Staged upgrade with automatic restore on failure.

    Returns a result dict {status, previous_tag, target_tag, ...}.
    Raises UpgradeError (after a completed restore) if any step fails.
    """
    previous_tag = read_env_tag(atlas_root)
    same_tag = previous_tag == target_tag
    if same_tag and not tag_is_mutable(target_tag):
        # Release tags don't move; re-applying the same one is a no-op.
        return {"status": "noop", "previous_tag": previous_tag,
                "target_tag": target_tag,
                "detail": f"already on {target_tag}"}
    if same_tag:
        # Mutable tag (latest, dev): "already on latest" says nothing
        # about whether the tag moved in the registry, so run the full
        # staged flow — the pull is cheap when nothing changed.
        steps.log(f"already on {target_tag} — refreshing "
                  "(mutable tags can move)")

    steps.log(f"upgrade {previous_tag} → {target_tag}")
    digests = steps.snapshot_digests(atlas_root)
    write_restore_point(atlas_root, previous_tag, target_tag, digests, stamp)
    steps.log("restore point recorded")

    try:
        # After the restore point, so a failed upgrade puts the old owner
        # back along with the rest of .env. The target is a new release,
        # so a release pin in the current .env does not hold the owner.
        moved = migrate_legacy_ghcr_owner(_env_path(atlas_root),
                                          keep_release_pin=False)
        if moved:
            steps.log(moved)
        steps.log("verifying target image signatures…")
        steps.verify_signatures(atlas_root, target_tag)
        steps.set_env_tag(atlas_root, target_tag)
        steps.log("staging images (pull)…")
        steps.pull(atlas_root)
        steps.log("starting target release…")
        steps.up(atlas_root)
        steps.log("waiting for readiness…")
        if not steps.readiness(atlas_root):
            raise UpgradeError("services did not become ready")
        if run_smoke:
            steps.log("running smoke check…")
            if not steps.smoke(atlas_root):
                raise UpgradeError("post-upgrade smoke check failed")
    except UpgradeError as e:
        raise _restore_or_flag(atlas_root, steps, str(e), previous_tag)
    except Exception as e:  # unexpected: still attempt restore
        raise _restore_or_flag(atlas_root, steps,
                               f"unexpected error: {e}", previous_tag)

    steps.log("upgrade succeeded")
    return {"status": "refreshed" if same_tag else "upgraded",
            "previous_tag": previous_tag,
            "target_tag": target_tag,
            "rollback_hint": f"atlas rollback  # returns to {previous_tag}"}


def run_rollback(atlas_root: str, steps: Steps,
                 target_tag: Optional[str] = None) -> dict:
    """Roll back to the recorded restore point, or to an explicit tag.

    With no argument, restores the last upgrade's previous release. With
    --to TAG, points the deployment at that immutable tag directly.
    """
    if target_tag:
        previous = read_env_tag(atlas_root)
        steps.log(f"rolling back to {target_tag}…")
        steps.set_env_tag(atlas_root, target_tag)
        try:
            steps.pull(atlas_root)
            steps.up(atlas_root)
        except UpgradeError as e:
            # Don't leave .env pointing at a tag that never came up
            # (typo'd / nonexistent tag) — every later `compose up`
            # would fail on it.
            steps.set_env_tag(atlas_root, previous)
            raise UpgradeError(
                f"{e}. The tag may not exist; .env was restored to the "
                f"previous tag ({previous}).")
        if not steps.readiness(atlas_root):
            raise UpgradeError(
                f"services did not become ready on {target_tag}; the tag "
                f"may not exist. Previous deployment tag was {previous}.")
        return {"status": "rolled-back", "target_tag": target_tag}

    point = read_restore_point(atlas_root)
    if not point:
        raise UpgradeError(
            "no restore point found — nothing to roll back to. Use "
            "`atlas rollback --to <tag>` to target a specific release.")
    _restore(atlas_root, point, steps)
    if not steps.readiness(atlas_root):
        raise UpgradeError(
            "restored the previous release but it did not become ready.")
    return {"status": "rolled-back", "target_tag": point["previous_tag"]}
