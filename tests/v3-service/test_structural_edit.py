"""structural_edit node-size precondition.

A replacement many times the size of the node it replaces is not an edit of
that node — it is the whole file wearing a selector. Size alone is not the
test, because writing a real body over a `pass` stub is also many times the
node; what marks the blob is that its content already exists elsewhere in the
file.
"""
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

# --- node-size precondition ---------------------------------------------------
#
# 2026-08-02 dogfooding: `function:index` (3 lines, `return
# render_template_string(HTML_TEMPLATE)`) was replaced with a 258-line
# `HTML_TEMPLATE = """..."""` assignment. That is valid Python, so the
# post-splice compile() passed and the splice landed — deleting the app's only
# @app.route. The file still parsed and the agent reported success.
#
# The check for this existed, but inside the `except SyntaxError` handler, so
# it only ever fired when the blob ALSO failed to compile.

_FLASK = (
    'from flask import Flask, render_template_string\n'
    'app = Flask(__name__)\n'
    'HTML_TEMPLATE = """\n'
    + "".join(f'<div class="row-{i}">some markup content here</div>\n' for i in range(60))
    + '"""\n\n\n'
    "@app.route('/')\n"
    'def index():\n'
    '    return render_template_string(HTML_TEMPLATE)\n'
)


def test_moving_the_template_into_the_route_is_refused_although_it_is_valid_python():
    """The whole point: compile() succeeds on the blob, so a check that runs
    only on SyntaxError never sees it."""
    template = _FLASK[_FLASK.index('HTML_TEMPLATE'):_FLASK.index("@app.route")]
    compile(template, "app.py", "exec")  # valid on its own
    res = structural_edit(path="app.py", source_text=_FLASK,
                          selector="function:index", content=template)
    assert res["success"] is False
    err = res["error"]
    assert "is only" in err
    assert "your replacement is" in err
    assert "HTML_TEMPLATE" in err          # names where the bulk actually lives
    assert "replace_lines" in err          # and a tool that can reach into it
    assert "was NOT modified" in err


def test_a_freshly_written_template_inlined_into_the_route_is_also_refused():
    """Not every blob is a copy. A model that writes the markup out fresh
    inside an inline render_template_string literal has made the same mistake,
    and the reuse signal cannot see it — the file having a template-sized
    string constant is what says no selector reaches the real target.

    This replacement is also cut off, as a long one often is. Its quoting broke
    BECAUSE the wrong node was re-emitted, so the refusal must not end in
    'check your quotes and re-emit the full node'.
    """
    fresh = ("@app.route('/')\ndef index():\n"
             '    return render_template_string("""\n'
             + "".join(f"  <div>brand new line {i}</div>\n" for i in range(120)))
    res = structural_edit(path="app.py", source_text=_FLASK,
                          selector="function:index", content=fresh)
    assert res["success"] is False, "a fresh 120-line inline template must be refused"
    err = res["error"]
    assert "`function:index` is only 3 line(s)" in err
    assert "does not live here" in err
    assert "HTML_TEMPLATE" in err
    assert "edit_file" in err
    assert "re-emit the full node" not in err


def test_a_fresh_implementation_over_a_stub_is_allowed():
    """Size alone must not refuse: writing a real body over `pass` is many
    times the node and is ordinary work. A file with no template-sized string
    constant and no reused content has neither signal."""
    stub = "def helper():\n    pass\n\n\ndef other():\n    return 1\n"
    for n in (25, 60, 150):
        body = "def helper():\n" + "".join(
            f"    value_number_{i} = {i} * 3\n" for i in range(n)) + "    return 0\n"
        res = structural_edit(path="a.py", source_text=stub,
                              selector="function:helper", content=body)
        assert res["success"] is True, (n, res.get("error"))


def test_a_node_sized_replacement_is_untouched():
    res = structural_edit(path="app.py", source_text=_FLASK, selector="function:index",
                          content="@app.route('/')\ndef index():\n    return 'hi'\n")
    assert res["success"] is True, res.get("error")


# --- embedded-region outline --------------------------------------------------
#
# outline_file reports only what the host grammar sees. For a Flask app whose
# whole UI is one module-level string, that is `function:index` and nothing
# else — so a model asked to change the game loop reaches for
# `structural_edit selector="function:draw"`, a symbol the outline never
# mentioned and no selector can reach. Runs 7 and 9 both opened with exactly
# that call.

_FLASK_WITH_SCRIPT = (
    'from flask import Flask, render_template_string\n'
    'app = Flask(__name__)\n'
    'HTML_TEMPLATE = """\n'
    '<html><body><canvas id="c"></canvas>\n'
    '<script>\n'
    '        let score = 0;\n'
    '        function draw() {\n'
    '            score += 1;\n'
    '        }\n'
    '        function gameOver() {\n'
    '            score = 0;\n'
    '        }\n'
    '        setInterval(draw, 100);\n'
    '</script>\n'
    '</body></html>\n'
    '"""\n\n\n'
    "@app.route('/')\n"
    'def index():\n'
    '    return render_template_string(HTML_TEMPLATE)\n'
)


def test_embedded_regions_name_the_javascript_the_outline_hides():
    regions = main.embedded_region_outline("app.py", _FLASK_WITH_SCRIPT)
    js = [r for r in regions if r["kind"] == "javascript"]
    assert len(js) == 1, regions
    assert "draw" in js[0]["symbols"]
    assert "gameOver" in js[0]["symbols"]
    assert js[0]["start_line"] < js[0]["end_line"]
    assert "HTML_TEMPLATE" in js[0]["where"]


@pytest.mark.parametrize("path, source", [
    pytest.param("a.py", "def f():\n    return 1\n", id="no-embedded-code"),
    pytest.param("a.txt", _FLASK_WITH_SCRIPT, id="unsupported-carrier-file"),
])
def test_no_embedded_regions_are_reported(path, source):
    assert main.embedded_region_outline(path, source) == []


def test_a_selector_naming_embedded_code_is_told_where_it_lives():
    """The selector-not-found error is where the model actually looks — it
    does not call outline_file first. Three runs opened with `function:draw`
    against a template-held game loop, were told the symbol did not exist,
    and re-sent it: from the file they had just read, that was plainly false.
    """
    err = structural_edit(path="app.py", source_text=_FLASK_WITH_SCRIPT,
                          selector="function:draw", content="x")["error"]
    assert "does not exist" not in err, "must not contradict the file the model just read"
    assert "`draw` exists" in err
    assert "NOT as a node any selector can reach" in err
    assert "replace_lines" in err


def test_a_selector_naming_nothing_still_lists_the_embedded_regions():
    err = structural_edit(path="app.py", source_text=_FLASK_WITH_SCRIPT,
                          selector="function:nope", content="x")["error"]
    assert "does not exist in this file" in err     # it genuinely does not
    assert "function:index" in err                  # what IS selectable
    assert "no selector reaches" in err             # and what is not
    assert "draw" in err


def test_a_selector_for_a_node_being_added_says_to_use_insert_after():
    """structural_edit replaces an existing node; it cannot create one. A model
    adding a feature reaches for the name it is about to write — observed on
    "add a done command that marks a task complete": turn 1 was
    `function:done_task` against a file with no such function. Listing the
    existing selectors is the right information and the wrong advice, because
    the model does not want any of them."""
    src = "def a():\n    return 1\n\n\ndef b():\n    return 2\n"
    err = structural_edit(path="t.py", source_text=src,
                          selector="function:c", content="def c():\n    return 3\n")["error"]
    assert "does not exist in this file" in err
    assert "ADDING" in err
    assert "insert_after" in err
    assert "function:a" in err   # still says what IS selectable
    assert "embedded" not in err  # a file without embedded code gets the plain message


# --- orphaned additions -------------------------------------------------------
#
# The mirror of the unresolved-call check: that one catches a call with no
# definition, this catches a definition with no callers. Observed on "add a
# done command that marks a task complete": `done_task` was written correctly
# and the argv dispatcher was never touched, so the feature was unreachable
# and `python todo.py done 1` still exited 0 — which the agent read as proof
# it worked.

_CLI_BEFORE = (
    "import sys\n\n"
    "def add_task(t):\n    print('added', t)\n\n"
    "def list_tasks():\n    print('listing')\n\n"
    'if __name__ == "__main__":\n'
    '    if sys.argv[1] == "add":\n        add_task(sys.argv[2])\n'
    '    elif sys.argv[1] == "list":\n        list_tasks()\n'
)
_CLI_WITH_DONE_TASK = _CLI_BEFORE.replace(
    "def list_tasks():",
    "def done_task(n):\n    print('done', n)\n\n\ndef list_tasks():")
_HELPER = "def exported_helper():\n    return 1\n"


@pytest.mark.parametrize("before, after, reported", [
    pytest.param(_CLI_BEFORE, _CLI_WITH_DONE_TASK, [("done_task", 6)],
                 id="added-and-never-wired-up"),
    pytest.param(_CLI_BEFORE, _CLI_WITH_DONE_TASK.replace(
        '    elif sys.argv[1] == "list":',
        '    elif sys.argv[1] == "done":\n        done_task(int(sys.argv[2]))\n'
        '    elif sys.argv[1] == "list":'), [],
                 id="added-and-wired-into-the-dispatcher"),
    # Errs toward silence: a dispatch table entry, an __all__ export or a
    # decorator all count, so a working wiring pattern is never flagged.
    pytest.param(_CLI_BEFORE, _CLI_WITH_DONE_TASK + "\nHANDLERS = {'done': done_task}\n", [],
                 id="a-mention-anywhere-counts-as-a-reference"),
    pytest.param(_CLI_BEFORE,
                 _CLI_BEFORE + "\n\ndef _helper():\n    pass\n\n\ndef test_thing():\n    pass\n", [],
                 id="private-and-test-helpers-are-skipped"),
    # Only what this edit ADDED: a codebase full of externally-called helpers
    # must stay silent.
    pytest.param(_HELPER, _HELPER + "\n\ndef another():\n    return 2\n", [("another", 5)],
                 id="a-function-already-there-is-not-reported"),
    pytest.param(_HELPER, _HELPER, [], id="nothing-added"),
])
def test_a_function_this_edit_added_with_no_reference_is_reported(before, after, reported):
    orphans = main.orphaned_new_symbols(before, after)
    assert [(o["name"], o["line"]) for o in orphans] == reported
