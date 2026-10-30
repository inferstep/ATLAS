"""Entry-point dispatch tests: subcommands, --help, `atlas compose`, and
the bare-`atlas` TUI launch paths (no fallback chat loop)."""

import subprocess
import sys

import pytest

from atlas import __main__ as main_mod
from atlas import env as cli_env


def test_unknown_subcommand_prints_usage_and_exits_2(monkeypatch, capsys):
    monkeypatch.setattr(sys, "argv", ["atlas", "bogus-subcommand"])
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 2
    err = capsys.readouterr().err
    assert "unknown subcommand" in err
    # usage list
    assert "doctor" in err
    assert "compose" in err


def test_help_flag_prints_usage_and_exits_0(monkeypatch, capsys):
    monkeypatch.setattr(sys, "argv", ["atlas", "--help"])
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 0
    out = capsys.readouterr().out
    assert "usage: atlas" in out
    for name in ("init", "doctor", "model", "onboard", "compose"):
        assert name in out


def test_compose_subcommand_passes_through_to_docker_compose(
        monkeypatch, tmp_path):
    (tmp_path / "docker-compose.yml").write_text("services: {}\n")
    (tmp_path / "compose").mkdir()
    (tmp_path / "compose/docker-compose.macos.yml").write_text("services: {}\n")
    (tmp_path / ".env").write_text("ATLAS_BACKEND=metal\n")
    calls = []

    monkeypatch.setattr(cli_env, "atlas_root", lambda: str(tmp_path))
    monkeypatch.setattr(subprocess, "call",
                        lambda cmd, **kwargs: calls.append(cmd) or 0)
    monkeypatch.setattr(sys, "argv", ["atlas", "compose", "ps"])
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 0
    assert calls[0][:2] == ["docker", "compose"]
    # The metal backend's overlay set is honored, args pass through.
    assert "compose/docker-compose.macos.yml" in calls[0]
    assert calls[0][-1] == "ps"


def test_compose_subcommand_requires_checkout(monkeypatch, tmp_path, capsys):
    monkeypatch.setattr(cli_env, "atlas_root", lambda: str(tmp_path))
    monkeypatch.setattr(sys, "argv", ["atlas", "compose", "ps"])
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 1
    assert "docker-compose.yml" in capsys.readouterr().err


def test_bare_atlas_without_tty_points_to_doctor(monkeypatch, capsys):
    """No TTY → no interactive fallback loop: a pointer to `atlas doctor`
    plus the subcommand list, nonzero exit."""
    monkeypatch.setattr(sys, "argv", ["atlas"])
    # Under pytest stdin/stdout are not TTYs, so this exercises the real check.
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert "needs a terminal" in err
    assert "atlas doctor" in err
    assert "subcommands:" in err
    assert "bench" in err


def test_bare_atlas_tui_failure_points_to_doctor(monkeypatch, capsys):
    """TUI couldn't start (no Go, build failure, proxy down) → pointer to
    `atlas doctor` and the subcommand list, TUI's exit code preserved."""
    from atlas.commands import tui

    monkeypatch.setattr(sys, "argv", ["atlas"])
    monkeypatch.setattr(main_mod, "_stdio_is_tty", lambda: True)
    monkeypatch.setattr(tui, "main", lambda argv: 1)
    with pytest.raises(SystemExit) as exc:
        main_mod.main()
    assert exc.value.code == 1
    err = capsys.readouterr().err
    assert "could not start" in err
    assert "atlas doctor" in err
    assert "subcommands:" in err
