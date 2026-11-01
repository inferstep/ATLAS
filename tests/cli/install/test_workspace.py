"""`atlas workspace` — reports and moves the bind ATLAS operates on.

The command is a thin wrapper over runtime._align_workspace, so these tests
cover the wrapper's own decisions: when it reports aligned, when it refuses,
and that it never recreates one container without the other. The recreate
itself is runtime's and is tested in test_runtime.py.
"""
import os

import pytest

from atlas.commands import workspace


@pytest.fixture
def binds(monkeypatch):
    """Control what the proxy/sandbox appear to be bound to."""
    state = {"proxy": None, "sandbox": None, "recreated": []}

    monkeypatch.setattr(workspace.runtime, "_docker_proxy_workspace",
                        lambda: state["proxy"])
    monkeypatch.setattr(workspace.runtime, "_docker_sandbox_workspace",
                        lambda: state["sandbox"])

    def _recreate(atlas_dir, project_dir):
        state["recreated"].append(project_dir)
        state["proxy"] = state["sandbox"] = project_dir  # runtime moves BOTH
        return True

    monkeypatch.setattr(workspace.runtime, "_recreate_docker_proxy", _recreate)
    return state


@pytest.mark.parametrize("under_docker, expected_text", [
    # The rule that makes one broad mount serve many folders: a directory
    # inside the bind needs no recreate. _align_workspace uses the same test.
    pytest.param(True, "aligned", id="directory-inside-the-bind"),
    # A local (non-Docker) proxy works in its own cwd: there is no bind to
    # align, and saying so beats reporting a failure.
    pytest.param(False, "not running under Docker", id="no-docker"),
])
def test_nothing_to_align_is_a_success(binds, tmp_path, capsys, under_docker,
                                       expected_text):
    project = tmp_path / "project"
    project.mkdir()
    if under_docker:
        binds["proxy"] = binds["sandbox"] = str(tmp_path)  # project's parent

    assert workspace.main(["show", "--dir", str(project)]) == 0
    assert expected_text in capsys.readouterr().out
    assert workspace.main(["align", "--dir", str(project)]) == 0
    assert binds["recreated"] == [], "nothing to align must not recreate"


@pytest.mark.parametrize("proxy, sandbox, expected_text", [
    pytest.param("elsewhere", "elsewhere", "NOT aligned", id="both-binds-elsewhere"),
    pytest.param("here", "elsewhere", "SPLIT", id="the-binds-differ"),
])
def test_show_fails_when_atlas_works_somewhere_else(binds, tmp_path, capsys,
                                                     proxy, sandbox, expected_text):
    here = tmp_path / "here"
    here.mkdir()
    binds["proxy"], binds["sandbox"] = str(tmp_path / proxy), str(tmp_path / sandbox)

    assert workspace.main(["show", "--dir", str(here)]) == 1
    assert expected_text in capsys.readouterr().out


def test_align_moves_both_binds_together(binds, tmp_path):
    """Recreating one alone is the split-brain bug: file tools read one tree
    while run_command uses the other, with every health check green."""
    binds["proxy"] = binds["sandbox"] = str(tmp_path / "old")
    new = tmp_path / "new"
    new.mkdir()

    assert workspace.main(["align", "--dir", str(new)]) == 0
    assert binds["recreated"] == [os.path.realpath(str(new))]
    assert binds["proxy"] == binds["sandbox"]


def test_a_failed_recreate_is_reported(binds, tmp_path, monkeypatch, capsys):
    binds["proxy"] = binds["sandbox"] = str(tmp_path / "old")
    monkeypatch.setattr(workspace.runtime, "_recreate_docker_proxy",
                        lambda *a: False)
    new = tmp_path / "new"
    new.mkdir()

    assert workspace.main(["align", "--dir", str(new)]) == 1
    assert "alignment failed" in capsys.readouterr().err


def test_a_missing_directory_is_rejected(binds, tmp_path):
    assert workspace.main(["align", "--dir", str(tmp_path / "nope")]) == 2


def test_it_is_registered_as_a_subcommand():
    """Unregistered means `atlas workspace` prints 'unknown subcommand'."""
    from atlas.__main__ import _SUBCOMMAND_HELP
    assert "workspace" in {name for name, _ in _SUBCOMMAND_HELP}
