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

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

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


@pytest.mark.parametrize("shape", ["col0", "original_indent", "first_line_bare"])
@pytest.mark.parametrize("selector, old_node, new_lines, column", [
    pytest.param("function:deposit",
                 "def deposit(self, amount):\n"
                 "        self.balance += amount\n"
                 "        return self.balance",
                 ["def deposit(self, amount):",
                  "    if amount <= 0:",
                  "        raise ValueError('amount must be positive')",
                  "    self.balance += amount",
                  "    return self.balance"],
                 4, id="method"),
    pytest.param("function:doubled",
                 "@property\n"
                 "    def doubled(self):\n"
                 "        return self.balance * 2",
                 ["@property", "def doubled(self):", "    return self.balance * 3"],
                 4, id="decorated-method"),  # decorator and def stay on one column
    pytest.param("function:fetch",
                 "async def fetch(self):\n"
                 "        return self.balance",
                 ["async def fetch(self):", "    return self.balance + 0"],
                 4, id="async-method"),
    pytest.param("function:label",
                 "def label(self):\n"
                 "            return 'acct'",
                 ["def label(self):", "    return 'account'"],
                 8, id="method-of-a-nested-class"),
])
def test_a_method_lands_at_its_column_whatever_its_indentation(selector, old_node, new_lines, column, shape):
    pad = " " * column
    # The three ways a model writes one node. The last one is what the splice
    # needs, because the node's line keeps its own indentation.
    first_line_bare = "\n".join([new_lines[0]] + [pad + line for line in new_lines[1:]])
    content = {
        "col0": "\n".join(new_lines),
        "original_indent": "\n".join(pad + line for line in new_lines),
        "first_line_bare": first_line_bare,
    }[shape]
    res = structural_edit("acct.py", SRC, selector, content)
    assert res["success"] is True, res.get("error")
    assert res["new_content"] == SRC.replace(old_node, first_line_bare)
    compile(res["new_content"], "acct.py", "exec")
    # still methods of Account, and nothing else moved
    account = ast.parse(res["new_content"]).body[0]
    assert [n.name for n in account.body] == ["__init__", "deposit", "doubled", "fetch", "Meta"]


def test_multiline_string_inside_a_method_is_not_reindented():
    """Lines inside a string literal are data. Shifting them would change the
    value the method returns, so re-basing must leave them byte-for-byte. A tab
    there is data too, not a mix of tabs and spaces."""
    rel = ["def deposit(self, amount):",
           "    return '''",
           "<b>",
           "  total",
           "\tby tab",
           "</b>",
           "'''"]
    res = structural_edit("acct.py", SRC, "function:deposit", "\n".join(rel))
    assert res["success"] is True, res.get("error")
    assert ("    def deposit(self, amount):\n"
            "        return '''\n<b>\n  total\n\tby tab\n</b>\n'''") in res["new_content"]


@pytest.mark.parametrize("lead", ["\n", "# v2\n"], ids=["blank-line-first", "comment-first"])
def test_a_method_never_leaves_its_class(lead):
    """A column-0 method behind a blank line or a comment. Spliced as sent,
    the node line's whitespace goes to that first row, the def lands at column
    0 and closes the class, and the file still compiles. Re-based, the method
    stays in Account."""
    content = lead + "def deposit(self, amount):\n    self.balance += amount * 2\n    return self.balance"
    res = structural_edit("acct.py", SRC, "function:deposit", content)
    assert res["success"] is True, res.get("error")
    tree = ast.parse(res["new_content"])
    assert [type(n).__name__ for n in tree.body] == ["ClassDef", "FunctionDef"]  # Account, top
    assert [n.name for n in tree.body[0].body] == ["__init__", "deposit", "doubled", "fetch", "Meta"]


def test_a_method_added_after_the_last_one_stays_in_its_class():
    """label is the last method in Account. Spliced as sent, a second method at
    column 0 would land at the top level, and the file would still compile.
    Re-based, both stay in Meta."""
    content = "def label(self): return 'account'\ndef short_label(self): return 'acct'"
    res = structural_edit("acct.py", SRC, "function:label", content)
    assert res["success"] is True, res.get("error")
    tree = ast.parse(res["new_content"])
    assert [n.name for n in tree.body] == ["Account", "top"]
    meta = tree.body[0].body[-1]
    assert [n.name for n in meta.body] == ["label", "short_label"]


@pytest.mark.parametrize("content, error", [
    # Re-basing turns the indentation error into the real one.
    ("def deposit(self, amount):\n    return amount +", "invalid syntax"),
    # Re-based, the error is still about indentation: the one as sent is reported.
    ("def deposit(self, amount):\n    if amount:\n    return amount",
     "expected an indented block after function definition"),
    # As sent, the node would land off its column: the re-based error is reported.
    ("    def deposit(self, amount):\n        return amount +", "invalid syntax"),
], ids=["in-place-other-error", "in-place-indentation-error", "off-its-column"])
def test_when_neither_form_compiles_the_error_that_helps_is_reported(content, error):
    res = structural_edit("acct.py", SRC, "function:deposit", content)
    assert res["success"] is False
    assert f"SyntaxError at line 6: {error}" in res["error"]


@pytest.mark.parametrize("content", [
    "def top(x):\n    return x + 2",
    # Spliced as sent, this would make top a method of Account, and the file
    # would still compile.
    "    def top(x):\n        return x + 2",
], ids=["sent-at-column-0", "sent-indented"])
def test_top_level_function_lands_at_column_0(content):
    """A column-0 node is spliced as sent, or re-based onto column 0."""
    res = structural_edit("acct.py", SRC, "function:top", content)
    assert res["success"] is True, res.get("error")
    assert res["new_content"] == SRC.replace("return x + 1", "return x + 2")
    compile(res["new_content"], "acct.py", "exec")


def test_tab_indented_file_with_space_indented_replacement_is_refused_not_guessed():
    src = "class A:\n\tdef m(self):\n\t\treturn 1\n"
    res = structural_edit("t.py", src, "function:m", "def m(self):\n    return 2")
    assert res["success"] is False
    assert "tab" in res["error"].lower()
    assert "not modified" in res["error"].lower()


@pytest.mark.parametrize("content, new_body", [
    ("def m(self):\n\treturn 2", "\t\treturn 2\n"),
    # A blank row's whitespace is not indentation, whatever it is made of.
    ("def m(self):\n\tx = 2\n    \n\treturn x", "\t\tx = 2\n    \n\t\treturn x\n"),
], ids=["tabs", "a-blank-row-with-spaces"])
def test_tab_indented_file_with_tab_indented_replacement_applies(content, new_body):
    src = "class A:\n\tdef m(self):\n\t\treturn 1\n"
    res = structural_edit("t.py", src, "function:m", content)
    assert res["success"] is True, res.get("error")
    assert res["new_content"] == "class A:\n\tdef m(self):\n" + new_body
