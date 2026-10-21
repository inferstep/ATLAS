import os

import pytest

from atlas import runtime_artifacts
from atlas.commands import tui


@pytest.mark.parametrize(
    "main_go_mtime, prompts_mtime, expected",
    [
        (100, 100, True),
        (300, 100, False),
        (100, 300, False),  # the TUI embeds the demo prompts file
    ],
    ids=["binary-newer-than-source", "main-go-changed-after-build",
         "demo-prompts-changed-after-build"],
)
def test_go_binary_is_current_only_when_not_older_than_its_source(
        tmp_path, main_go_mtime, prompts_mtime, expected):
    source = tmp_path / "tui"
    source.mkdir()
    (source / "main.go").write_text("package main\n")
    (source / "demo_prompts_fallback.json").write_text("[]\n")
    binary = tmp_path / "atlas-tui"
    binary.write_text("binary")
    binary.chmod(0o755)
    os.utime(binary, ns=(200, 200))
    os.utime(source / "main.go", ns=(main_go_mtime, main_go_mtime))
    os.utime(source / "demo_prompts_fallback.json",
             ns=(prompts_mtime, prompts_mtime))

    is_current = runtime_artifacts.go_binary_is_current(
        str(binary), str(source), ("demo_prompts_fallback.json",))

    assert is_current is expected


@pytest.mark.parametrize(
    "binary_mtime, expect_rebuild",
    [(100, True), (300, False)],
    ids=["stale-binary-is-rebuilt", "current-binary-is-reused"],
)
def test_select_tui_binary_rebuilds_only_a_stale_binary(
        monkeypatch, tmp_path, binary_mtime, expect_rebuild):
    atlas_dir = tmp_path / "atlas"
    (atlas_dir / "tui").mkdir(parents=True)
    main_go = atlas_dir / "tui" / "main.go"
    main_go.write_text("package main\n")
    os.utime(main_go, ns=(200, 200))
    binary = tmp_path / "atlas-tui"
    binary.write_text("binary")
    binary.chmod(0o755)
    os.utime(binary, ns=(binary_mtime, binary_mtime))
    monkeypatch.setattr(tui, "_find_tui_binary", lambda _: str(binary))
    monkeypatch.setattr(tui, "_build_tui", lambda _: "/rebuilt/atlas-tui")

    selected = tui._select_tui_binary(str(atlas_dir))

    assert selected == ("/rebuilt/atlas-tui" if expect_rebuild else str(binary))
