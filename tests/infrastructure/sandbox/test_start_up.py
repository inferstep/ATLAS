"""What the executor checks and chooses when it starts: the address it listens on,
and the workspace base that every request's workspace is made in."""

import importlib.util
import os
import stat
from pathlib import Path

import pytest

SANDBOX_DIR = Path(__file__).resolve().parents[3] / "services" / "sandbox"


@pytest.fixture(scope="module")
def start_up():
    """The executor's start_up module, imported with ATLAS_SANDBOX_HOST unset."""
    with pytest.MonkeyPatch.context() as env:
        env.delenv("ATLAS_SANDBOX_HOST", raising=False)
        spec = importlib.util.spec_from_file_location("sandbox_start_up", SANDBOX_DIR / "start_up.py")
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        yield mod


def test_it_listens_on_loopback_on_the_host_and_on_every_interface_in_the_image(start_up):
    assert start_up.LISTEN_HOST == "127.0.0.1"
    assert "ENV ATLAS_SANDBOX_HOST=0.0.0.0" in (SANDBOX_DIR / "Dockerfile").read_text(encoding="utf-8").splitlines()


def test_a_new_workspace_base_is_made_for_its_owner_only(start_up, tmp_path):
    base = tmp_path / "sandbox"
    start_up.prepare_workspace_base(base)
    assert stat.S_IMODE(base.stat().st_mode) == 0o700


def test_an_open_workspace_base_of_its_own_is_closed_to_others(start_up, tmp_path):
    base = tmp_path / "sandbox"
    base.mkdir()
    base.chmod(0o777)
    start_up.prepare_workspace_base(base)
    assert stat.S_IMODE(base.stat().st_mode) == 0o700


def test_a_workspace_base_that_is_a_link_stops_the_start(start_up, tmp_path):
    (tmp_path / "elsewhere").mkdir()
    base = tmp_path / "sandbox"
    base.symlink_to(tmp_path / "elsewhere")
    with pytest.raises(SystemExit) as stop:
        start_up.prepare_workspace_base(base)
    assert str(stop.value) == (f"the workspace base {base} is a symbolic link, so the executor does not start. "
                               "Fix: remove the link, or set WORKSPACE_BASE to a folder of your own.")


def test_a_workspace_base_of_another_user_stops_the_start(start_up, tmp_path, monkeypatch):
    base = tmp_path / "sandbox"
    base.mkdir()
    # The folder is this test's; the executor is made to run as the next uid.
    owner = os.stat(base).st_uid
    monkeypatch.setattr(start_up.os, "getuid", lambda: owner + 1)
    with pytest.raises(SystemExit) as stop:
        start_up.prepare_workspace_base(base)
    # The text names the folder, its owner, and what to do.
    assert str(stop.value) == (f"the workspace base {base} belongs to uid {owner}, not to this process "
                               f"(uid {owner + 1}), so the executor does not start; an earlier run as another "
                               f"user may have left it. Fix: remove {base}, or set WORKSPACE_BASE to a folder of your own.")
