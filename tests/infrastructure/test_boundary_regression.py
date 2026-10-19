"""Local boundary regression matrix for the sandbox executor's
path-confinement and limit logic.

These exercise the REAL confinement functions (`_safe_overlay_path`,
`_write_overlay_files` with its O_NOFOLLOW dir-fd walk, and
`_resolve_shell_cwd`) against temporary local directories — no Docker,
no network, no privilege operations. They lock in the properties that
keep model-authored file writes and shell cwds inside the workspace.
"""

import importlib.util
import os
import sys
from pathlib import Path

import pytest

SANDBOX_DIR = Path(__file__).resolve().parents[2] / "sandbox"


@pytest.fixture(scope="module")
def executor(tmp_path_factory):
    """Import executor_server with WORKSPACE_ROOT pointed at a temp dir."""
    root = tmp_path_factory.mktemp("ws-root")
    os.environ["ATLAS_SANDBOX_WORKSPACE_ROOT"] = str(root)
    sys.path.insert(0, str(SANDBOX_DIR))
    spec = importlib.util.spec_from_file_location(
        "executor_server_bt", SANDBOX_DIR / "executor_server.py")
    mod = importlib.util.module_from_spec(spec)
    # executor_server defers annotation evaluation, so pydantic resolves
    # model field types through sys.modules. The checks in this file only
    # touch plain functions today and pass either way, but a test that
    # reaches a request model would fail with "is not fully defined"
    # rather than anything describing the real problem.
    sys.modules[spec.name] = mod
    try:
        spec.loader.exec_module(mod)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise
    mod._WS_ROOT_FIXTURE = root
    return mod


def _http_exc(executor):
    from fastapi import HTTPException
    return HTTPException


# --- Path normalization / traversal rejection -----------------------------

@pytest.mark.parametrize("bad", [
    "../escape.txt",
    "../../etc/passwd",
    "/etc/passwd",
    "/abs/path.py",
    "a/../../b.txt",
    "sub/../../../out.txt",
    "\\windows\\style",
])
def test_overlay_rejects_traversal_and_absolute(executor, bad):
    from fastapi import HTTPException
    with pytest.raises(HTTPException):
        executor._safe_overlay_path(bad)


@pytest.mark.parametrize("ok", [
    "main.py",
    "pkg/mod.py",
    "a/b/c/deep.txt",
    "with.dots.in.name.js",
])
def test_overlay_accepts_safe_relative(executor, ok):
    rel = executor._safe_overlay_path(ok)
    assert not rel.is_absolute()
    assert ".." not in rel.parts


def test_overlay_rejects_empty_and_nonstring(executor):
    from fastapi import HTTPException
    for bad in ("", None, 123):
        with pytest.raises(HTTPException):
            executor._safe_overlay_path(bad)


# --- Write confinement: symlink components / TOCTOU (O_NOFOLLOW) ----------

def test_write_lands_inside_root(executor, tmp_path):
    root = tmp_path / "snap"
    root.mkdir()
    executor._write_overlay_files(root, {"pkg/app.py": "print('hi')\n"})
    written = root / "pkg" / "app.py"
    assert written.is_file()
    assert written.read_text() == "print('hi')\n"
    # confined under root
    assert str(written.resolve()).startswith(str(root.resolve()))


def test_write_refuses_symlinked_parent_dir(executor, tmp_path):
    """A pre-existing symlink as a path component must not let a write
    escape — O_NOFOLLOW rejects it."""
    from fastapi import HTTPException
    root = tmp_path / "snap"
    root.mkdir()
    outside = tmp_path / "outside"
    outside.mkdir()
    # attacker pre-plants a symlink 'link' -> outside inside the root
    os.symlink(outside, root / "link")
    with pytest.raises(HTTPException):
        executor._write_overlay_files(root, {"link/evil.py": "x=1\n"})
    # nothing was written outside
    assert not (outside / "evil.py").exists()


def test_write_refuses_symlinked_target_file(executor, tmp_path):
    """If the final filename is a symlink to an outside file, the
    O_NOFOLLOW open must refuse rather than overwrite the target."""
    from fastapi import HTTPException
    root = tmp_path / "snap"
    root.mkdir()
    target = tmp_path / "secret.txt"
    target.write_text("original\n")
    os.symlink(target, root / "passwd")
    with pytest.raises(HTTPException):
        executor._write_overlay_files(root, {"passwd": "overwritten\n"})
    assert target.read_text() == "original\n"


def test_write_enforces_per_file_byte_cap(executor, tmp_path):
    from fastapi import HTTPException
    root = tmp_path / "snap"
    root.mkdir()
    huge = "x" * (executor.SHELL_SNAPSHOT_MAX_FILE_BYTES + 1)
    with pytest.raises(HTTPException):
        executor._write_overlay_files(root, {"big.txt": huge})


# --- Shell cwd confinement ------------------------------------------------

def test_cwd_defaults_to_root(executor):
    root = executor._WS_ROOT_FIXTURE
    assert executor._resolve_shell_cwd(None, root) == root
    assert executor._resolve_shell_cwd("", root) == root


def test_cwd_rejects_traversal(executor, tmp_path):
    from fastapi import HTTPException
    root = executor._WS_ROOT_FIXTURE
    for bad in ("../..", "sub/../../etc", "../outside"):
        with pytest.raises(HTTPException):
            executor._resolve_shell_cwd(bad, root)


def test_cwd_rejects_absolute_outside_workspace(executor):
    from fastapi import HTTPException
    root = executor._WS_ROOT_FIXTURE
    with pytest.raises(HTTPException):
        executor._resolve_shell_cwd("/etc", root)


def test_cwd_accepts_existing_subdir(executor):
    root = executor._WS_ROOT_FIXTURE
    (root / "proj").mkdir(exist_ok=True)
    resolved = executor._resolve_shell_cwd("proj", root)
    assert resolved == (root / "proj").resolve()


def test_cwd_rejects_nonexistent(executor):
    from fastapi import HTTPException
    root = executor._WS_ROOT_FIXTURE
    with pytest.raises(HTTPException):
        executor._resolve_shell_cwd("does-not-exist", root)


# --- Workspace snapshot and contained paths --------------------------------

def test_snapshot_keeps_a_link_inside_the_workspace_and_drops_one_out_of_it(executor, tmp_path):
    workspace, snapshot = tmp_path / "workspace", tmp_path / "snapshot"
    workspace.mkdir()
    (workspace / "inside.txt").write_text("inside")
    (tmp_path / "outside.txt").write_text("outside")
    (workspace / "link_in").symlink_to("inside.txt")
    (workspace / "link_out").symlink_to("../outside.txt")

    executor._copy_workspace_snapshot(workspace, snapshot)

    assert (snapshot / "link_in").is_symlink()
    assert not (snapshot / "link_out").is_symlink()


@pytest.mark.parametrize("parts, refused", [
    (("src", "app.py"), False),
    (("..", "escape.py"), True),
    # A sibling folder whose name starts with the base's name.
    (("..", "ws-sibling", "app.py"), True),
])
def test_contained_path_refuses_a_path_out_of_its_base(executor, tmp_path, parts, refused):
    base = tmp_path / "ws"
    base.mkdir()
    if refused:
        with pytest.raises(_http_exc(executor)):
            executor._contained_path(base, *parts)
    else:
        assert executor._contained_path(base, *parts) == base.joinpath(*parts)


# --- Limit configuration --------------------------------------------------

def test_execution_time_cap_is_bounded(executor):
    # MAX_EXECUTION_TIME is a positive int ceiling read from env.
    assert isinstance(executor.MAX_EXECUTION_TIME, int)
    assert executor.MAX_EXECUTION_TIME > 0


def test_snapshot_caps_are_positive(executor):
    assert executor.SHELL_SNAPSHOT_MAX_FILES > 0
    assert executor.SHELL_SNAPSHOT_MAX_BYTES > 0
    assert executor.SHELL_SNAPSHOT_MAX_FILE_BYTES > 0


def test_snapshot_stops_above_its_file_count_cap(executor, tmp_path, monkeypatch):
    monkeypatch.setattr(executor, "SHELL_SNAPSHOT_MAX_FILES", 2)
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    (workspace / "a.txt").write_text("a")
    (workspace / "b.txt").write_text("b")
    executor._copy_workspace_snapshot(workspace, tmp_path / "snapshot-at-the-cap")

    (workspace / "c.txt").write_text("c")
    with pytest.raises(_http_exc(executor)) as refused:
        executor._copy_workspace_snapshot(workspace, tmp_path / "snapshot-over-the-cap")
    assert refused.value.status_code == 413
