"""atlas publish with --lens-only or --asa-only hands the run to that component's own publish."""

import pytest

from atlas.commands import publish

ALL_FLAGS = ["mymodel", "--lens-repo", "me/atlas-lens-mymodel", "--asa-repo", "me/atlas-asa-mymodel",
             "--license", "mit", "--dry-run", "--skip-pr", "--no-color"]


@pytest.mark.parametrize("argv, component, sub", [
    pytest.param(["--lens-only"], "lens", ["publish"], id="lens-alone"),
    pytest.param(["--lens-only", *ALL_FLAGS], "lens",
                 ["publish", "mymodel", "--repo", "me/atlas-lens-mymodel", "--license", "mit",
                  "--dry-run", "--skip-pr", "--no-color"], id="lens-with-every-flag"),
    pytest.param(["--asa-only"], "asa", ["publish"], id="asa-alone"),
    pytest.param(["--asa-only", *ALL_FLAGS], "asa",
                 ["publish", "mymodel", "--repo", "me/atlas-asa-mymodel", "--license", "mit",
                  "--dry-run", "--skip-pr", "--no-color"], id="asa-with-every-flag"),
])
def test_one_component_is_published_by_its_own_command(monkeypatch, argv, component, sub):
    calls = []
    monkeypatch.setattr(publish.lens_module, "main", lambda args: calls.append(("lens", args)) or 7)
    monkeypatch.setattr(publish.asa_module, "main", lambda args: calls.append(("asa", args)) or 7)

    assert publish.main(argv) == 7

    assert calls == [(component, sub)]
