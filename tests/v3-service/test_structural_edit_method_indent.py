"""structural_edit on a node that does not start at column 0.

The splice replaces the node's bytes from its first byte, which for a method
is after the leading whitespace of its line. A replacement sent as a column-0
block, or with its original indentation on every line, is re-based onto the
node's column before the splice.

Every shape below describes the same method, so every shape must produce the
same file. Top-level nodes (column 0) are spliced as sent.
"""
import ast
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main

pytestmark = pytest.mark.skipif(
    not getattr(main, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed in this environment",
)

structural_edit = main.structural_edit

SRC = (
    "class Account:\n"
    "    def __init__(self, balance):\n"
    "        self.balance = balance\n"
    "\n"
    "    def deposit(self, amount):\n"
    "        self.balance += amount\n"
    "        return self.balance\n"
    "\n"
    "    @property\n"
    "    def doubled(self):\n"
    "        return self.balance * 2\n"
    "\n"
    "    async def fetch(self):\n"
    "        return self.balance\n"
    "\n"
    "    class Meta:\n"
    "        def label(self):\n"
    "            return 'acct'\n"
    "\n"
    "\n"
    "def top(x):\n"
    "    return x + 1\n"
)


def _shapes(rel_lines, col):
    """The three ways a model writes one node, from its column-0 form."""
    pad = " " * col
    return {
        "col0": "\n".join(rel_lines),
        "original_indent": "\n".join(pad + line if line else line for line in rel_lines),
        "first_line_bare": rel_lines[0] + "\n"
        + "\n".join(pad + line if line else line for line in rel_lines[1:]),
    }


def _expected(old_node, rel_lines, col):
    """SRC with old_node (as it sits in the file, from its first non-blank
    character) replaced by rel_lines placed at column `col`."""
    pad = " " * col
    new_node = rel_lines[0] + "\n" + "\n".join(pad + line if line else line for line in rel_lines[1:])
    assert SRC.count(old_node) == 1
    return SRC.replace(old_node, new_node)


def _assert_applied(res, want):
    assert res["success"] is True, res.get("error")
    assert res["new_content"] == want
    compile(res["new_content"], "acct.py", "exec")


DEPOSIT_OLD = ("def deposit(self, amount):\n"
               "        self.balance += amount\n"
               "        return self.balance")
DEPOSIT_NEW = [
    "def deposit(self, amount):",
    "    if amount <= 0:",
    "        raise ValueError('amount must be positive')",
    "    self.balance += amount",
    "    return self.balance",
]


@pytest.mark.parametrize("shape", ["col0", "original_indent", "first_line_bare"])
def test_method_replacement_lands_at_the_method_column_whatever_its_indentation(shape):
    content = _shapes(DEPOSIT_NEW, 4)[shape]
    res = structural_edit("acct.py", SRC, "function:deposit", content)
    _assert_applied(res, _expected(DEPOSIT_OLD, DEPOSIT_NEW, 4))
    # still a method of Account, and nothing else moved
    cls = ast.parse(res["new_content"]).body[0]
    names = [n.name for n in cls.body]
    assert names == ["__init__", "deposit", "doubled", "fetch", "Meta"]


DOUBLED_OLD = ("@property\n"
               "    def doubled(self):\n"
               "        return self.balance * 2")
DOUBLED_NEW = ["@property", "def doubled(self):", "    return self.balance * 3"]


@pytest.mark.parametrize("shape", ["col0", "original_indent", "first_line_bare"])
def test_decorated_method_keeps_decorator_and_def_on_one_column(shape):
    content = _shapes(DOUBLED_NEW, 4)[shape]
    res = structural_edit("acct.py", SRC, "function:doubled", content)
    _assert_applied(res, _expected(DOUBLED_OLD, DOUBLED_NEW, 4))


FETCH_OLD = ("async def fetch(self):\n"
             "        return self.balance")
FETCH_NEW = ["async def fetch(self):", "    return self.balance + 0"]


@pytest.mark.parametrize("shape", ["col0", "original_indent", "first_line_bare"])
def test_async_method(shape):
    content = _shapes(FETCH_NEW, 4)[shape]
    res = structural_edit("acct.py", SRC, "function:fetch", content)
    _assert_applied(res, _expected(FETCH_OLD, FETCH_NEW, 4))


LABEL_OLD = ("def label(self):\n"
             "            return 'acct'")
LABEL_NEW = ["def label(self):", "    return 'account'"]


@pytest.mark.parametrize("shape", ["col0", "original_indent", "first_line_bare"])
def test_method_of_a_nested_class(shape):
    content = _shapes(LABEL_NEW, 8)[shape]
    res = structural_edit("acct.py", SRC, "function:label", content)
    _assert_applied(res, _expected(LABEL_OLD, LABEL_NEW, 8))


def test_multiline_string_inside_a_method_is_not_reindented():
    """Lines inside a string literal are data. Shifting them would change the
    value the method returns, so re-basing must leave them byte-for-byte."""
    rel = ["def deposit(self, amount):",
           "    return '''",
           "<b>",
           "  total",
           "</b>",
           "'''"]
    res = structural_edit("acct.py", SRC, "function:deposit", "\n".join(rel))
    assert res["success"] is True, res.get("error")
    assert ("    def deposit(self, amount):\n"
            "        return '''\n<b>\n  total\n</b>\n'''") in res["new_content"]


@pytest.mark.parametrize("lead", ["\n", "# v2\n"])
def test_a_method_never_leaves_its_class(lead):
    """A column-0 method behind a blank line or a comment. Spliced as sent,
    the node line's whitespace goes to that first row, the def lands at column
    0 and closes the class, and the file still compiles. Re-based, the method
    stays in Account."""
    content = lead + "\n".join(DEPOSIT_NEW)
    res = structural_edit("acct.py", SRC, "function:deposit", content)
    assert res["success"] is True, res.get("error")
    tree = ast.parse(res["new_content"])
    assert [type(n).__name__ for n in tree.body] == ["ClassDef", "FunctionDef"]  # Account, top
    assert [n.name for n in tree.body[0].body] == ["__init__", "deposit", "doubled", "fetch", "Meta"]


def test_top_level_function_is_unchanged():
    """A column-0 node is spliced as sent."""
    rel = ["def top(x):", "    return x + 2"]
    res = structural_edit("acct.py", SRC, "function:top", "\n".join(rel))
    _assert_applied(res, SRC.replace("def top(x):\n    return x + 1",
                                     "def top(x):\n    return x + 2"))


def test_first_line_bare_shape_is_byte_identical_to_a_plain_splice():
    """A replacement that already carries the indentation the splice needs is
    not touched."""
    content = _shapes(DEPOSIT_NEW, 4)["first_line_bare"]
    res = structural_edit("acct.py", SRC, "function:deposit", content)
    start = SRC.index("def deposit")
    end = start + len(DEPOSIT_OLD)
    assert res["new_content"] == SRC[:start] + content + SRC[end:]


def test_tab_indented_file_with_space_indented_replacement_is_refused_not_guessed():
    src = "class A:\n\tdef m(self):\n\t\treturn 1\n"
    res = structural_edit("t.py", src, "function:m", "def m(self):\n    return 2")
    assert res["success"] is False
    assert "tab" in res["error"].lower()
    assert "was NOT modified" in res["error"] or "not modified" in res["error"].lower()


def test_tab_indented_file_with_tab_indented_replacement_applies():
    src = "class A:\n\tdef m(self):\n\t\treturn 1\n"
    res = structural_edit("t.py", src, "function:m", "def m(self):\n\treturn 2")
    assert res["success"] is True, res.get("error")
    assert res["new_content"] == "class A:\n\tdef m(self):\n\t\treturn 2\n"
