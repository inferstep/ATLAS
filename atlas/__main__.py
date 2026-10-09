"""atlas — command-line entry point (console script and `python -m atlas`).

`atlas <subcommand>` dispatches to atlas.commands.<name>. Bare `atlas`
launches the Bubbletea TUI, the one interactive surface; there is no
built-in fallback chat loop. Without a TTY the launcher prints a pointer
to `atlas doctor` plus the subcommand list and exits nonzero.
"""

import os
import sys
from typing import List

_SUBCOMMAND_HELP = [
    ("init",    "first-run install wizard"),
    ("doctor",  "install health diagnostic"),
    ("tier",    "hardware tier probe + runtime fit"),
    ("model",   "model registry: list/install/verify/remove"),
    ("onboard", "guided drop-in for a new model"),
    ("lens",    "Geometric Lens check/build/publish"),
    ("asa",     "ASA control-vector check/build/publish"),
    ("publish", "publish lens + ASA artifacts in one step"),
    ("bench",   "run benchmarks with live progress"),
    ("compose", "docker compose with ATLAS's compose files"),
    ("upgrade", "staged upgrade with auto-restore on failure"),
    ("rollback","return the deployment to a previous release"),
    ("diagnostics", "collect a filtered diagnostic bundle"),
    ("artifact", "verify / snapshot / roll back artifact bundles"),
    ("config",  "validate / migrate the .env configuration"),
    ("workspace", "show or move the directory ATLAS operates on"),
    ("tui",     "launch the terminal UI"),
]

# Flags recognized by the interactive UI. They are passed through to the TUI
# binary unchanged (a leading-dash argument is not treated as a subcommand);
# listed here so `atlas --help` documents them.
_SESSION_FLAG_HELP = [
    ("--continue", "resume the most recent session in the current directory"),
    ("--resume [id]", "resume a session by id, or pick from a list"),
]


def _print_usage(stream=None) -> None:
    out = stream or sys.stdout
    out.write("usage: atlas [subcommand] [args...]\n\n")
    out.write("Run `atlas` with no arguments for the interactive UI.\n\n")
    out.write("subcommands:\n")
    for name, desc in _SUBCOMMAND_HELP:
        out.write(f"  {name:<8} {desc}\n")
    out.write("\nsession flags (interactive UI):\n")
    for name, desc in _SESSION_FLAG_HELP:
        out.write(f"  {name:<14} {desc}\n")
    out.write("\n  --version     print the CLI version and exit\n")


def _dispatch_subcommand(name: str, argv: List[str]) -> int:
    if name == "compose":
        return _run_compose(argv)
    import importlib
    module = importlib.import_module(f"atlas.commands.{name}")
    return module.main(argv)


def _run_compose(argv: List[str]) -> int:
    """`atlas compose ...` — thin passthrough to `docker compose` with the
    project's compose file set (backend overlays included)."""
    import subprocess
    from atlas import compose as compose_config
    from atlas import env as cli_env
    atlas_dir = cli_env.atlas_root()
    if not os.path.isfile(os.path.join(atlas_dir, "docker-compose.yml")):
        print("atlas compose: no docker-compose.yml found — run from an "
              "ATLAS checkout.", file=sys.stderr)
        return 1
    try:
        cmd = compose_config.command(atlas_dir, argv)
    except FileNotFoundError as e:
        print(f"atlas compose: {e}", file=sys.stderr)
        return 1
    try:
        return subprocess.call(cmd, cwd=atlas_dir)
    except FileNotFoundError:
        print("atlas compose: docker not found on PATH.", file=sys.stderr)
        return 1


def _stdio_is_tty() -> bool:
    return sys.stdin.isatty() and sys.stdout.isatty()


def _tui_pointer(stream) -> None:
    """The short pointer shown when the interactive UI can't run."""
    stream.write("Run `atlas doctor` to diagnose the install, or use a "
                 "subcommand directly:\n\n")
    _print_usage(stream)


def main():
    """Entry point.

    1. `atlas <subcommand> [...]` → subcommand dispatch (see _SUBCOMMAND_HELP)
    2. `atlas --help` / `--version` / unknown subcommand → usage
    3. Bare `atlas` (and session flags) on a terminal → the Bubbletea TUI
    4. No TTY, or the TUI can't start → pointer to `atlas doctor` + the
       subcommand list, nonzero exit
    """
    # Internal service auth: one global opener covers every urllib
    # call the CLI makes (proxy, llama, lens, v3, sandbox). No-op when
    # secrets/service-token doesn't exist.
    try:
        from atlas.token import install_urllib_opener
        install_urllib_opener()
    except Exception:  # noqa: S110
        pass  # auth is best-effort on the client side; servers enforce

    if len(sys.argv) > 1:
        first = sys.argv[1]
        known = {name for name, _ in _SUBCOMMAND_HELP}
        if first in ("--help", "-h"):
            _print_usage()
            sys.exit(0)
        if first in ("--version", "-V"):
            from atlas import __version__
            print(f"atlas {__version__}")
            sys.exit(0)
        if first in known:
            sys.exit(_dispatch_subcommand(first, sys.argv[2:]))
        if not first.startswith("-"):
            print(f"atlas: unknown subcommand {first!r}\n", file=sys.stderr)
            _print_usage(sys.stderr)
            sys.exit(2)

    # Interactive default → the TUI. It needs a real terminal; piped
    # stdin/stdout gets the pointer instead of a UI it can't drive.
    if not _stdio_is_tty():
        sys.stderr.write("atlas: the interactive UI needs a terminal "
                         "(stdin/stdout is not a TTY).\n")
        _tui_pointer(sys.stderr)
        sys.exit(1)

    from atlas.commands import tui
    code = tui.main(sys.argv[1:])
    if code != 0:
        sys.stderr.write("\natlas: the terminal UI could not start.\n")
        _tui_pointer(sys.stderr)
    sys.exit(code)


if __name__ == "__main__":
    main()
