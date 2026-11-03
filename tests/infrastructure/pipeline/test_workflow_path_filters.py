"""Every path filter of a workflow matches a file of the tree.

A workflow with a `paths:` filter starts only for a change to a file the
filter matches, and scripts/checks_ran.py expects it on the same terms. A
filter left on a folder that moved matches nothing, so the workflow never
starts for the code it checks, and no check notices. The filters are read
here the way checks_ran.py reads them.
"""
import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]

_spec = importlib.util.spec_from_file_location("checks_ran", ROOT / "scripts" / "checks_ran.py")
ran = importlib.util.module_from_spec(_spec)
sys.modules[_spec.name] = ran
_spec.loader.exec_module(ran)

TRACKED = subprocess.run(["git", "ls-files"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.splitlines()


def path_filters():
    for path, workflow in ran.read_workflows(ROOT).items():
        for event, settings in ran.triggers(workflow).items():
            # A `schedule:` is a list of times, with no filters.
            if not isinstance(settings, dict):
                continue
            for key in ("paths", "paths-ignore"):
                for pattern in ran.as_list(settings.get(key)):
                    yield pytest.param(pattern, id=f"{path} {event} {key} {pattern}")


@pytest.mark.parametrize("pattern", list(path_filters()))
def test_the_path_filter_matches_a_file_of_the_tree(pattern):
    regex = ran.glob_regex(pattern)
    assert regex is not None, f"checks_ran.py cannot read the filter {pattern!r}"
    assert any(regex.fullmatch(path) for path in TRACKED), f"the filter {pattern!r} matches no tracked file"
