"""A structural_edit must not splice code that already exists elsewhere.

Found live: the model replaced `function:a` with a body containing a, b AND
c. The node was replaced, the original b and c after it survived, and the
file came back 10 -> 18 lines defining b and c twice. It compiles, so the
syntax gate passed; the replacement was only ~3x the node, so the
size-ratio guard did not fire; and at import time the later definition
silently wins.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import symbols  # noqa: E402

FILE = '''def a():
    return 1


def b():
    return 2


def c():
    return 3
'''


def test_replacement_carrying_neighbours_is_refused():
    res = symbols.structural_edit(
        "victim.py", FILE, "function:a",
        "def a():\n    return 99\n\n\ndef b():\n    return 2\n\n\ndef c():\n    return 3\n")
    assert not res.get("success"), res
    assert "twice" in res["error"]
    assert "b" in res["error"] and "c" in res["error"]


def test_ordinary_single_node_replacement_still_works():
    res = symbols.structural_edit(
        "victim.py", FILE, "function:a", "def a():\n    return 99\n")
    assert res.get("success"), res.get("error")
    new = res["new_content"]
    assert "return 99" in new
    # Neighbours survive exactly once.
    assert new.count("def b():") == 1
    assert new.count("def c():") == 1


def test_a_file_that_already_duplicates_a_name_is_not_blamed_on_the_edit():
    already = FILE + "\n\ndef b():\n    return 22\n"
    res = symbols.structural_edit(
        "victim.py", already, "function:a", "def a():\n    return 99\n")
    assert res.get("success"), res.get("error")
