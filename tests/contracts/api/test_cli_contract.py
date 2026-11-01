"""CLI surface drift check.

Every advertised subcommand must resolve to a callable implementation
and appear in the CLI docs; removed commands must stay gone from docs.
"""

import importlib
from pathlib import Path

from atlas.__main__ import _SUBCOMMAND_HELP

REPO = Path(__file__).resolve().parents[3]

# Subcommands implemented inline in atlas/__main__.py rather than as a module.
INLINE = {"compose"}


def test_every_subcommand_has_a_main():
    for name, _desc in _SUBCOMMAND_HELP:
        if name in INLINE:
            continue
        mod = importlib.import_module(f"atlas.commands.{name}")
        assert callable(getattr(mod, "main", None)), (
            f"subcommand {name!r} has no callable main() — "
            "advertised commands must be implemented")


def test_every_subcommand_is_documented():
    doc = (REPO / "docs" / "CLI.md").read_text()
    missing = [name for name, _ in _SUBCOMMAND_HELP
               if f"atlas {name}" not in doc and f"`{name}`" not in doc]
    assert not missing, (
        f"subcommands absent from docs/CLI.md: {missing}")


def test_removed_surfaces_stay_removed():
    """Names removed in the completion pass must not resurface in
    user-facing docs or help without a real implementation behind them."""
    checks = [
        ("docs/API.md", "plan_tasks"),
        ("docs/ARCHITECTURE.md", "plan_tasks"),
        ("atlas/display.py", "/ablation"),
    ]
    for rel, needle in checks:
        text = (REPO / rel).read_text()
        assert needle not in text, (
            f"{rel} references removed surface {needle!r} — either it "
            "was reintroduced without implementation or the doc is stale")
