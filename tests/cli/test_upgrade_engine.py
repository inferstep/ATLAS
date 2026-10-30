"""Upgrade/rollback engine — the restore-on-failure guarantee.

Uses a fake Steps that records calls and can be told to fail at a chosen
stage, plus a temp ATLAS root with a .env. No Docker, no registry, no
network — the orchestration and restore path are exercised directly.
"""

import os

import pytest

from atlas import upgrade_engine as eng


def _root(tmp_path, tag="v1.0.0"):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / ".env").write_text(
        f"ATLAS_MODEL_FILE=m.gguf\nATLAS_IMAGE_TAG={tag}\nOTHER=keep\n")
    return str(tmp_path)


class FakeSteps:
    """Records the sequence of side-effects; fails on demand."""

    def __init__(self, fail_on=None, ready=True, smoke=True):
        self.calls = []
        self.fail_on = fail_on          # "verify" | "pull" | "up" | None
        self._ready = ready
        self._smoke = smoke
        self.tag_writes = []

    def as_steps(self, atlas_root):
        def set_tag(root, tag):
            self.calls.append(("set_env_tag", tag))
            self.tag_writes.append(tag)
            # mirror the real writer so read_env_tag reflects it
            _write_env_tag(root, tag)

        def pull(root):
            self.calls.append(("pull", eng.read_env_tag(root)))
            if self.fail_on == "pull":
                raise eng.UpgradeError("pull failed (simulated)")

        def up(root):
            self.calls.append(("up", eng.read_env_tag(root)))
            if self.fail_on == "up":
                raise eng.UpgradeError("up failed (simulated)")

        def verify(root, tag):
            self.calls.append(("verify", tag))
            if self.fail_on == "verify":
                raise eng.UpgradeError("bad signature (simulated)")

        return eng.Steps(
            snapshot_digests=lambda root: {"atlas-proxy": "sha256:old"},
            set_env_tag=set_tag,
            pull=pull,
            up=up,
            readiness=lambda root: self._ready,
            smoke=lambda root: self._smoke,
            verify_signatures=verify,
            log=lambda m: self.calls.append(("log", m)),
        )


def _write_env_tag(root, tag):
    path = os.path.join(root, ".env")
    lines, found = [], False
    with open(path) as fh:
        for line in fh:
            if line.startswith("ATLAS_IMAGE_TAG="):
                lines.append(f"ATLAS_IMAGE_TAG={tag}\n")
                found = True
            else:
                lines.append(line)
    if not found:
        lines.append(f"ATLAS_IMAGE_TAG={tag}\n")
    with open(path, "w") as fh:
        fh.writelines(lines)


def test_successful_upgrade_finalizes_on_target(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps()
    res = eng.run_upgrade(root, "v2.0.0", fake.as_steps(root), "stamp1")
    assert res["status"] == "upgraded"
    assert eng.read_env_tag(root) == "v2.0.0"
    # a restore point was recorded pointing back to v1.0.0
    point = eng.read_restore_point(root)
    assert point["previous_tag"] == "v1.0.0"
    assert point["target_tag"] == "v2.0.0"


def test_failed_pull_restores_previous_release(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(fail_on="pull")
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_upgrade(root, "v2.0.0", steps, "stamp2")
    assert "Automatically restored" in str(ei.value)
    # .env is back on the previous tag
    assert eng.read_env_tag(root) == "v1.0.0"


def test_failed_readiness_restores(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(ready=False)
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError):
        eng.run_upgrade(root, "v2.0.0", steps, "stamp3")
    assert eng.read_env_tag(root) == "v1.0.0"
    # the last up() during restore ran on the previous tag
    up_tags = [tag for (call, tag) in fake.calls if call == "up"]
    assert up_tags[-1] == "v1.0.0"


def test_failed_smoke_restores(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(smoke=False)
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_upgrade(root, "v2.0.0", steps, "stamp4")
    assert "smoke" in str(ei.value)
    assert eng.read_env_tag(root) == "v1.0.0"


def test_env_backup_preserves_other_keys(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(fail_on="up")
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError):
        eng.run_upgrade(root, "v2.0.0", steps, "stamp5")
    # restore brought back the full .env, not just the tag
    env = (tmp_path / ".env").read_text()
    assert "OTHER=keep" in env
    assert "ATLAS_IMAGE_TAG=v1.0.0" in env


def test_noop_when_already_on_target(tmp_path):
    root = _root(tmp_path, "v2.0.0")
    fake = FakeSteps()
    res = eng.run_upgrade(root, "v2.0.0", fake.as_steps(root), "stamp6")
    assert res["status"] == "noop"
    # no pull/up attempted
    assert not [c for c in fake.calls if c[0] in ("pull", "up")]


def test_rollback_to_restore_point(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps()
    eng.run_upgrade(root, "v2.0.0", fake.as_steps(root), "stamp7")
    assert eng.read_env_tag(root) == "v2.0.0"
    # now roll back
    fake2 = FakeSteps()
    res = eng.run_rollback(root, fake2.as_steps(root))
    assert res["status"] == "rolled-back"
    assert res["target_tag"] == "v1.0.0"
    assert eng.read_env_tag(root) == "v1.0.0"


def test_rollback_to_explicit_tag(tmp_path):
    root = _root(tmp_path, "v2.0.0")
    fake = FakeSteps()
    res = eng.run_rollback(root, fake.as_steps(root), target_tag="v1.5.0")
    assert res["status"] == "rolled-back"
    assert eng.read_env_tag(root) == "v1.5.0"
    # The signatures of the target tag are verified before anything is
    # pulled or started.
    assert [c for c in fake.calls if c[0] != "log"] == [
        ("verify", "v1.5.0"), ("set_env_tag", "v1.5.0"), ("pull", "v1.5.0"), ("up", "v1.5.0")]


def test_rollback_with_a_bad_signature_changes_nothing(tmp_path):
    root = _root(tmp_path, "v2.0.0")
    fake = FakeSteps(fail_on="verify")
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_rollback(root, fake.as_steps(root), target_tag="v1.5.0")
    assert "bad signature (simulated)" in str(ei.value)
    assert [c for c in fake.calls if c[0] != "log"] == [("verify", "v1.5.0")]
    assert eng.read_env_tag(root) == "v2.0.0"


def test_the_rollback_command_hands_the_engine_the_real_signature_step(tmp_path, monkeypatch):
    from atlas.commands import rollback, upgrade
    root = _root(tmp_path, "v2.0.0")
    monkeypatch.setattr(rollback.cli_env, "atlas_root", lambda: root)
    given = {}

    def run_rollback(atlas_root, steps, target_tag=None):
        given["steps"], given["tag"] = steps, target_tag
        return {"status": "rolled-back", "target_tag": target_tag}

    monkeypatch.setattr(rollback.eng, "run_rollback", run_rollback)
    assert rollback.main(["--to", "1.5.0", "--yes"]) == 0
    assert given["tag"] == "1.5.0"
    assert given["steps"].verify_signatures is upgrade._verify_signatures


@pytest.mark.parametrize("cosign_says, words", [
    pytest.param("Error: no signatures found",
                 "cosign found no signature it can read for ghcr.io/inferstep/atlas-proxy:3.1.6. "
                 "Install cosign 3 or newer and try again", id="no signature it can read: install cosign 3"),
    pytest.param("Error: none of the expected identities matched",
                 "signature verification failed for ghcr.io/inferstep/atlas-proxy:3.1.6", id="another failed check"),
])
def test_a_failed_signature_check_refuses_with_words_that_fit_it(tmp_path, monkeypatch, cosign_says, words):
    from atlas.commands import upgrade
    root = _root(tmp_path, "v2.0.0")
    cosign = tmp_path / "bin" / "cosign"
    cosign.parent.mkdir()
    cosign.write_text(f"#!/bin/sh\necho '{cosign_says}' >&2\nexit 1\n")
    cosign.chmod(0o755)
    monkeypatch.setenv("PATH", f"{cosign.parent}{os.pathsep}{os.environ['PATH']}")
    monkeypatch.delenv("ATLAS_UPGRADE_SKIP_VERIFY", raising=False)
    monkeypatch.setattr(upgrade, "_target_images", lambda root, tag: [f"ghcr.io/inferstep/atlas-proxy:{tag}"])
    with pytest.raises(eng.UpgradeError) as ei:
        upgrade._verify_signatures(root, "3.1.6")
    assert str(ei.value).startswith(words)


def test_rollback_without_point_errors(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps()
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_rollback(root, steps)
    assert "no restore point" in str(ei.value)


def test_bad_signature_aborts_and_restores(tmp_path):
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(fail_on="verify")
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_upgrade(root, "v2.0.0", steps, "stampV")
    assert "Automatically restored" in str(ei.value)
    # never advanced past verify: no pull/up of the target
    up_target = [t for (c, t) in fake.calls if c == "up" and t == "v2.0.0"]
    assert not up_target
    assert eng.read_env_tag(root) == "v1.0.0"


def test_same_mutable_tag_refreshes_instead_of_noop(tmp_path):
    # `atlas upgrade` on the default install is latest -> latest; the
    # registry tag can move, so the staged flow must still run.
    root = _root(tmp_path, "latest")
    fake = FakeSteps()
    res = eng.run_upgrade(root, "latest", fake.as_steps(root), "stampR")
    assert res["status"] == "refreshed"
    assert ("pull", "latest") in fake.calls


def test_same_release_tag_noops(tmp_path):
    root = _root(tmp_path, "v2.0.0")
    fake = FakeSteps()
    res = eng.run_upgrade(root, "v2.0.0", fake.as_steps(root), "stampN")
    assert res["status"] == "noop"
    assert not [c for c in fake.calls if c[0] == "pull"]


def test_tag_mutability_classification():
    assert eng.tag_is_mutable("latest")
    assert eng.tag_is_mutable("dev")
    assert not eng.tag_is_mutable("v2.0.0")
    assert not eng.tag_is_mutable("v2.0.0-rc.1")


def test_restore_never_pulls_the_previous_tag(tmp_path):
    # A mutable previous tag could have moved; restore must come up on
    # the locally cached images, not re-pull.
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(ready=False)
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError):
        eng.run_upgrade(root, "v2.0.0", steps, "stampP")
    pulls = [t for (c, t) in fake.calls if c == "pull"]
    assert pulls == ["v2.0.0"]  # target staged once; no pull during restore
    assert eng.read_env_tag(root) == "v1.0.0"


def test_failed_restore_reports_both_failures(tmp_path):
    # fail_on="up" fails the upgrade's up AND the restore's up: the
    # error must carry the original cause and flag the failed restore,
    # not swallow either.
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(fail_on="up")
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_upgrade(root, "v2.0.0", steps, "stampF")
    msg = str(ei.value)
    assert "up failed (simulated)" in msg
    assert "ALSO failed" in msg


def test_rollback_to_missing_tag_restores_env(tmp_path):
    # A typo'd --to tag whose pull fails must not leave .env pointing
    # at it (every later compose up would fail).
    root = _root(tmp_path, "v1.0.0")
    fake = FakeSteps(fail_on="pull")
    steps = fake.as_steps(root)
    with pytest.raises(eng.UpgradeError) as ei:
        eng.run_rollback(root, steps, target_tag="v9.9.9")
    assert eng.read_env_tag(root) == "v1.0.0"
    assert "restored to the previous tag" in str(ei.value)


def test_real_set_env_tag_appends_newline_first(tmp_path):
    # .env whose last line lacks a trailing newline: the appended tag
    # must not be glued onto it.
    from atlas.commands.upgrade import _set_env_tag
    root = _root(tmp_path, "v1.0.0")
    env = os.path.join(root, ".env")
    with open(env, "w") as fh:
        fh.write("ATLAS_CTX_SIZE=131072")  # no trailing newline, no tag
    _set_env_tag(root, "v2.0.0")
    with open(env) as fh:
        content = fh.read()
    assert "ATLAS_CTX_SIZE=131072\n" in content
    assert "ATLAS_IMAGE_TAG=v2.0.0\n" in content
    assert eng.read_env_tag(root) == "v2.0.0"


def test_normalize_image_tag_strips_v_from_semver():
    # Registry semver tags carry no leading v (git tag v3.1.3 publishes
    # atlas-*:3.1.3).
    assert eng.normalize_image_tag("v3.1.3") == "3.1.3"
    assert eng.normalize_image_tag("v3.1.3-rc.1") == "3.1.3-rc.1"
    assert eng.normalize_image_tag("3.1.3") == "3.1.3"
    assert eng.normalize_image_tag("latest") == "latest"
    assert eng.normalize_image_tag("dev") == "dev"
    assert eng.normalize_image_tag("sha-abc1234") == "sha-abc1234"


def test_unprefixed_semver_is_immutable():
    # The form the registry actually serves must be treated as a release
    # tag, or same-tag re-runs would refresh instead of noop.
    assert not eng.tag_is_mutable("3.1.3")
    assert not eng.tag_is_mutable("v3.1.3")
    assert eng.tag_is_mutable("latest")
