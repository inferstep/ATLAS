"""The lens bundle's file lists agree (#230).

A bundle is named in several places: the provenance manifest hashes
geometric_lens.provenance.BUNDLE_FILES, `atlas artifact` snapshots and rolls
back its own copy (kept local so the stdlib-only CLI need not import the lens
package), and `atlas lens build` activates _MANAGED_LENS_ARTIFACTS. A file
missing from one of them is hashed but not moved, or moved but not rolled
back: a rollback that kept a newer drift fingerprint would read the restored
weights as drift.
"""
import ast
from pathlib import Path

from atlas.commands import artifact, lens

REPO = Path(__file__).resolve().parents[3]


def _provenance_bundle_files():
    tree = ast.parse((REPO / "geometric-lens" / "geometric_lens" / "provenance.py").read_text())
    for node in tree.body:
        if isinstance(node, ast.Assign) and any(
                getattr(t, "id", "") == "BUNDLE_FILES" for t in node.targets):
            return ast.literal_eval(node.value)
    raise AssertionError("geometric_lens.provenance.BUNDLE_FILES not found")


def test_the_cli_bundle_list_mirrors_the_manifest_one():
    assert artifact.BUNDLE_FILES == _provenance_bundle_files()


def test_activation_moves_every_file_the_manifest_hashes():
    assert set(_provenance_bundle_files()) <= set(lens._MANAGED_LENS_ARTIFACTS)


def test_a_bundle_carries_its_drift_fingerprint():
    assert "drift_fingerprint.json" in artifact.BUNDLE_FILES
