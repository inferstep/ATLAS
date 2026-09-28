"""ATLAS moved from itigges22/ATLAS to inferstep/ATLAS.

Installs made before the move carry ATLAS_GHCR_OWNER=itigges22 in .env
(`atlas init` wrote it), which outranks the compose default, so they would
keep pulling a namespace that gets no new images. These tests pin how each
path moves that owner — `atlas upgrade` (and puts it back on a failed
upgrade), `atlas config migrate`, and a bootstrap re-run — and that a
release-pinned install and an owner set in the shell are left alone.
"""

import os
import re
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

from atlas import compose as compose_config
from atlas import upgrade_engine as eng
from atlas.commands import config as cfg
from atlas.commands import upgrade as up

REPO = Path(__file__).resolve().parents[2]


@pytest.fixture(autouse=True)
def _no_shell_owner(monkeypatch):
    monkeypatch.delenv("ATLAS_GHCR_OWNER", raising=False)


def _env(tmp_path, owner="itigges22", tag="latest", line=None):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    body = "# ATLAS config\nATLAS_MODEL_FILE=m.gguf\n"
    if line is not None:
        body += line + "\n"
    elif owner is not None:
        body += f"ATLAS_GHCR_OWNER={owner}\n"
    if tag is not None:
        body += f"ATLAS_IMAGE_TAG={tag}\n"
    path = tmp_path / ".env"
    path.write_text(body)
    return path


def _owner_in(path):
    return compose_config.read_env_path(str(path)).get("ATLAS_GHCR_OWNER")


# --- the shared rule -------------------------------------------------------


@pytest.mark.parametrize("tag", ["latest", "dev", None])
def test_a_moving_tag_moves_the_legacy_owner(tmp_path, tag):
    path = _env(tmp_path, tag=tag)
    note = eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=True)
    assert note and "itigges22 → inferstep" in note
    assert _owner_in(path) == "inferstep"
    text = path.read_text()
    assert "# ATLAS config\n" in text and "ATLAS_MODEL_FILE=m.gguf\n" in text


@pytest.mark.parametrize("tag", ["3.1.3", "v3.1.3", "3.2.0-rc.1"])
def test_a_release_pin_keeps_the_legacy_owner(tmp_path, tag):
    path = _env(tmp_path, tag=tag)
    before = path.read_bytes()
    assert eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=True) is None
    assert path.read_bytes() == before


def test_an_upgrade_moves_the_owner_off_a_release_pin(tmp_path):
    path = _env(tmp_path, tag="3.1.3")
    assert eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=False)
    assert _owner_in(path) == "inferstep"


@pytest.mark.parametrize("owner", ["myfork", "inferstep", None])
def test_other_owners_are_left_alone(tmp_path, owner):
    path = _env(tmp_path, owner=owner)
    before = path.read_bytes()
    assert eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=False) is None
    assert path.read_bytes() == before


def test_an_owner_set_in_the_shell_is_left_alone(tmp_path, monkeypatch):
    monkeypatch.setenv("ATLAS_GHCR_OWNER", "itigges22")
    path = _env(tmp_path)
    before = path.read_bytes()
    assert eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=False) is None
    assert path.read_bytes() == before


def test_the_rewrite_keeps_export_prefix_and_file_mode(tmp_path):
    path = _env(tmp_path, line='export ATLAS_GHCR_OWNER="itigges22"')
    os.chmod(path, 0o600)
    assert eng.migrate_legacy_ghcr_owner(str(path), keep_release_pin=True)
    assert "export ATLAS_GHCR_OWNER=inferstep\n" in path.read_text()
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600


def test_a_missing_env_is_a_noop(tmp_path):
    missing = tmp_path / ".env"
    assert eng.migrate_legacy_ghcr_owner(str(missing), keep_release_pin=False) is None
    assert not missing.exists()


# --- atlas upgrade ------------------------------------------------------------


class _Steps:
    """Records the owner each stage sees; fails on demand."""

    def __init__(self, root, fail_on=None):
        self.root = root
        self.fail_on = fail_on
        self.seen = []
        self.logs = []

    def _see(self, stage):
        self.seen.append((stage, _owner_in(Path(self.root) / ".env")))
        if self.fail_on == stage:
            raise eng.UpgradeError(f"{stage} failed (simulated)")

    def build(self):
        return eng.Steps(
            snapshot_digests=lambda root: {},
            set_env_tag=up._set_env_tag,
            pull=lambda root: self._see("pull"),
            up=lambda root: self._see("up"),
            readiness=lambda root: True,
            smoke=lambda root: True,
            verify_signatures=lambda root, tag: self._see("verify"),
            log=self.logs.append,
        )


def test_upgrade_moves_the_owner_before_verifying(tmp_path):
    _env(tmp_path, tag="3.1.3")
    steps = _Steps(str(tmp_path))
    res = eng.run_upgrade(str(tmp_path), "3.2.0", steps.build(), "s1")
    assert res["status"] == "upgraded"
    assert steps.seen[0] == ("verify", "inferstep")
    assert _owner_in(tmp_path / ".env") == "inferstep"
    assert any("itigges22 → inferstep" in m for m in steps.logs)


def test_a_failed_upgrade_puts_the_legacy_owner_back(tmp_path):
    _env(tmp_path, tag="3.1.3")
    steps = _Steps(str(tmp_path), fail_on="pull")
    with pytest.raises(eng.UpgradeError):
        eng.run_upgrade(str(tmp_path), "3.2.0", steps.build(), "s2")
    assert _owner_in(tmp_path / ".env") == "itigges22"
    assert eng.read_env_tag(str(tmp_path)) == "3.1.3"


def test_rollback_puts_the_legacy_owner_back(tmp_path):
    _env(tmp_path, tag="3.1.3")
    steps = _Steps(str(tmp_path))
    eng.run_upgrade(str(tmp_path), "3.2.0", steps.build(), "s3")
    eng.run_rollback(str(tmp_path), steps.build())
    assert _owner_in(tmp_path / ".env") == "itigges22"
    assert eng.read_env_tag(str(tmp_path)) == "3.1.3"


def test_owner_resolution_is_shell_then_env_then_default(tmp_path, monkeypatch):
    _env(tmp_path, owner="myfork")
    assert up._owner(str(tmp_path)) == "myfork"
    monkeypatch.setenv("ATLAS_GHCR_OWNER", "fromshell")
    assert up._owner(str(tmp_path)) == "fromshell"
    monkeypatch.delenv("ATLAS_GHCR_OWNER")
    _env(tmp_path, owner=None)
    assert up._owner(str(tmp_path)) == "inferstep"


def test_the_signature_identity_follows_the_repo_that_built_the_image():
    assert "github.com/itigges22/ATLAS/" in up._cosign_identity("itigges22")
    assert "github.com/inferstep/ATLAS/" in up._cosign_identity("inferstep")
    # Other owners (forks) are still checked against upstream's identity.
    assert "github.com/inferstep/ATLAS/" in up._cosign_identity("myfork")


def test_target_images_use_the_env_owner(tmp_path, monkeypatch):
    _env(tmp_path, owner="myfork")

    def no_compose(*args, **kwargs):
        raise OSError("compose unavailable (simulated)")

    monkeypatch.setattr(up.compose_config, "command", no_compose)
    refs = up._target_images(str(tmp_path), "3.2.0")
    assert refs and all(r.startswith("ghcr.io/myfork/") for r in refs)


# --- atlas config migrate ---------------------------------------------------


def test_config_migrate_moves_the_owner_on_a_moving_tag(tmp_path, capsys):
    path = _env(tmp_path, tag="latest")
    assert cfg._migrate(str(path)) == 0
    assert _owner_in(path) == "inferstep"
    assert _owner_in(str(path) + ".bak") == "itigges22"
    assert "itigges22 → inferstep" in capsys.readouterr().out


def test_config_migrate_keeps_a_release_pinned_owner(tmp_path, capsys):
    path = _env(tmp_path, tag="3.1.3")
    assert cfg._migrate(str(path)) == 0
    assert _owner_in(path) == "itigges22"
    assert "kept ATLAS_GHCR_OWNER=itigges22" in capsys.readouterr().out


def test_config_migrate_dry_run_reports_without_writing(tmp_path, capsys):
    path = _env(tmp_path, tag="latest")
    before = path.read_bytes()
    assert cfg._migrate(str(path), dry_run=True) == 0
    assert path.read_bytes() == before
    assert "would set:    ATLAS_GHCR_OWNER=inferstep" in capsys.readouterr().out


# --- bootstrap re-run ---------------------------------------------------------


def _bootstrap_function(name):
    text = (REPO / "scripts" / "atlas-bootstrap.sh").read_text()
    m = re.search(rf"^{name}\(\) \{{\n.*?^\}}\n", text, re.M | re.S)
    assert m, f"{name}() not found in atlas-bootstrap.sh"
    return m.group(0)


def _bootstrap_migrate(cwd, shell_owner=None):
    script = ("set -euo pipefail\n"
              'log_info() { echo "INFO $*"; }\n'
              'log_ok() { echo "OK $*"; }\n'
              'log_warn() { echo "WARN $*"; }\n'
              + _bootstrap_function("env_file_value")
              + _bootstrap_function("migrate_legacy_ghcr_owner")
              + "migrate_legacy_ghcr_owner\n")
    env = {k: v for k, v in os.environ.items() if k != "ATLAS_GHCR_OWNER"}
    if shell_owner:
        env["ATLAS_GHCR_OWNER"] = shell_owner
    return subprocess.run(["bash", "-c", script], cwd=cwd, env=env,
                          capture_output=True, text=True, check=True)


needs_bash = pytest.mark.skipif(shutil.which("bash") is None,
                                reason="bootstrap tests need bash")


@needs_bash
def test_bootstrap_moves_the_owner_and_keeps_file_mode(tmp_path):
    path = _env(tmp_path, tag="latest")
    os.chmod(path, 0o600)
    out = _bootstrap_migrate(tmp_path).stdout
    assert "OK ATLAS_GHCR_OWNER: itigges22 → inferstep" in out
    assert _owner_in(path) == "inferstep"
    assert "ATLAS_MODEL_FILE=m.gguf\n" in path.read_text()
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600
    assert not (tmp_path / ".env.owner-tmp").exists()


@needs_bash
def test_bootstrap_moves_a_quoted_owner(tmp_path):
    path = _env(tmp_path, line='ATLAS_GHCR_OWNER="itigges22"')
    _bootstrap_migrate(tmp_path)
    assert _owner_in(path) == "inferstep"


@needs_bash
def test_bootstrap_keeps_a_release_pin_and_a_shell_owner(tmp_path):
    path = _env(tmp_path, tag="3.1.3")
    before = path.read_bytes()
    assert "Keeping ATLAS_GHCR_OWNER=itigges22" in _bootstrap_migrate(tmp_path).stdout
    assert path.read_bytes() == before
    path = _env(tmp_path, tag="latest")
    before = path.read_bytes()
    _bootstrap_migrate(tmp_path, shell_owner="itigges22")
    assert path.read_bytes() == before


@needs_bash
@pytest.mark.parametrize("tag", ["latest", "dev", "", "main", "3.1.3",
                                 "v3.1.3", "3.2.0-rc.1"])
def test_bootstrap_and_config_migrate_follow_the_same_rule(tmp_path, tag):
    path = _env(tmp_path, tag=tag)
    expect = eng.legacy_ghcr_owner_applies(
        compose_config.read_env_path(str(path)), keep_release_pin=True)
    _bootstrap_migrate(tmp_path)
    assert (_owner_in(path) == "inferstep") is expect
