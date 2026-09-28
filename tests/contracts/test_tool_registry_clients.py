"""Clients that enumerate tool names must not drift from the proxy registry.

The VS Code extension shipped listing `ast_edit` for two weeks after the
proxy renamed it to `structural_edit`, and never learned about
`insert_after`. Nothing failed: the diff preview silently did nothing on
the two tools that edit code in place, which is the feature. A rename in
Go cannot break a string in TypeScript, so only a check that reads both
catches it.

This asserts the *edit* tools specifically. A client is free to ignore
read-only tools (read_file, search_files); what it must not do is claim to
preview file edits and then miss one.
"""
import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
TOOLS_GO = REPO / "proxy" / "tools.go"
EXT_PREVIEW = REPO / "extensions" / "vscode" / "src" / "session" / "editPreview.ts"
EXT_MISMATCH = REPO / "extensions" / "vscode" / "src" / "workspace" / "mismatch.ts"

# Tools that write to a file the user can see. Anything registered in the
# proxy that mutates a path belongs here; read-only tools do not.
EDIT_TOOLS = {"write_file", "edit_file", "structural_edit", "insert_after", "replace_lines"}
# Mutating but diff-less: the extension prompts for these without a preview.
MUTATING_NO_DIFF = {"delete_file", "move_file"}


def _registered_tools() -> set[str]:
    """Every name in the proxy's tool registry."""
    src = TOOLS_GO.read_text()
    return set(re.findall(r'Name:\s*"([a-z_]+)"', src))


def _ts_string_set(path: Path, symbol: str) -> set[str]:
    """The string literals of a `export const NAME = new Set([...])`."""
    src = path.read_text()
    m = re.search(rf"{symbol}\s*=\s*new Set\(\s*\[(.*?)\]\s*\)", src, re.S)
    assert m, f"{symbol} not found in {path.name} — did it get renamed?"
    return set(re.findall(r"'([a-z_]+)'", m.group(1)))


def _ts_record_keys(path: Path, symbol: str) -> set[str]:
    """The keys of a `const NAME: Record<string, X> = { ... }`."""
    src = path.read_text()
    m = re.search(rf"{symbol}[^=]*=\s*\{{(.*?)\n\}};", src, re.S)
    assert m, f"{symbol} not found in {path.name}"
    return set(re.findall(r"^\s*([a-z_]+):", m.group(1), re.M))


def test_the_edit_tools_this_contract_names_are_all_registered():
    """Guards the guard: if the proxy renames one of these, this fails first
    and points at the rename rather than at a client."""
    registered = _registered_tools()
    missing = sorted(EDIT_TOOLS - registered)
    assert not missing, (
        f"EDIT_TOOLS names tools the proxy does not register: {missing}. "
        f"Registered: {sorted(registered)}")


@pytest.mark.skipif(not EXT_PREVIEW.exists(), reason="vscode extension not present")
def test_vscode_previews_every_edit_tool():
    got = _ts_string_set(EXT_PREVIEW, "FILE_EDIT_TOOLS")
    assert got == EDIT_TOOLS, (
        f"extension's FILE_EDIT_TOOLS is {sorted(got)}, edit tools are "
        f"{sorted(EDIT_TOOLS)}. Missing means a permission prompt offers no "
        f"diff; extra means it names a tool the proxy no longer has.")


@pytest.mark.skipif(not EXT_MISMATCH.exists(), reason="vscode extension not present")
def test_vscode_mismatch_heuristic_watches_every_mutating_tool():
    got = _ts_record_keys(EXT_MISMATCH, "FILE_OP_KINDS")
    want = EDIT_TOOLS | MUTATING_NO_DIFF
    assert got == want, (
        f"extension's FILE_OP_KINDS is {sorted(got)}, mutating tools are "
        f"{sorted(want)}. A tool missing here is invisible to the "
        f"workspace-mismatch warning.")


@pytest.mark.skipif(not EXT_PREVIEW.exists(), reason="vscode extension not present")
def test_no_client_names_a_tool_the_proxy_dropped():
    """The failure that actually happened: ast_edit lived on in TypeScript
    for two weeks after the Go rename."""
    registered = _registered_tools()
    for path in (EXT_PREVIEW, EXT_MISMATCH):
        named = set(re.findall(r"'([a-z_]+_(?:file|after|edit|command))'", path.read_text()))
        unknown = sorted(n for n in named if n not in registered)
        assert not unknown, (
            f"{path.name} names tools the proxy does not register: {unknown}")
