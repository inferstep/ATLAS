"""structural_edit HTML selector tests — regression for <script>/<style> matching.

tree-sitter-html parses <script> and <style> as dedicated script_element /
style_element nodes (raw JS/CSS bodies), NOT generic `element` nodes, so the
generic element query matched them 0 times. These confirm the dedicated-node
queries match (and that bare tags still work)."""
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main
import embedded_script

pytestmark = pytest.mark.skipif(
    not getattr(main, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed in this environment",
)

HTML = (
    "<!DOCTYPE html>\n<html>\n<head>\n  <style>body { margin: 0; }</style>\n"
    "</head>\n<body>\n  <canvas id=\"gameCanvas\"></canvas>\n"
    "  <script src=\"/static/game.js\"></script>\n</body>\n</html>\n"
)

_APP = ("from flask import Flask, render_template_string\napp = Flask(__name__)\n\n"
        'HTML_TEMPLATE = """<script>let x=1;</script>"""\n\n'
        "@app.route('/')\ndef index():\n    return render_template_string(HTML_TEMPLATE)\n")


@pytest.mark.parametrize("selector, old_element, new_element", [
    pytest.param("<script>", '<script src="/static/game.js"></script>',
                 "<script>\n  const c = 1; // inline\n</script>", id="script-element"),
    pytest.param("<style>", "<style>body { margin: 0; }</style>",
                 "<style>body { margin: 8px; }</style>", id="style-element"),
    pytest.param("<canvas>", '<canvas id="gameCanvas"></canvas>',
                 '<canvas id="gameCanvas" width="400"></canvas>', id="bare-tag"),
])
def test_a_tag_selector_replaces_that_element(selector, old_element, new_element):
    res = main.structural_edit("templates/index.html", HTML, selector, new_element)
    assert res.get("success"), res
    assert res["new_content"] == HTML.replace(old_element, new_element)


@pytest.mark.parametrize("selector, language, error_mentions", [
    pytest.param('<script src="x">', "html", ["bare tag"], id="attribute-selector-on-html"),
    # The Flask-template case: the script lives inside a template string, which
    # the Python grammar sees as one literal, so the message names what does
    # work. It must NOT send the model to function:NAME: a live session did
    # that, rewrote index() -- which only renders HTML_TEMPLATE -- and the
    # pause it "added" never reached the template.
    pytest.param("<script>", "python", ["HTML-only", "edit_file", "Do NOT reach for function:NAME"],
                 id="html-tag-on-python"),
    pytest.param("def:index", "python", ["function:NAME, class:NAME"],
                 id="unknown-selector-on-python-lists-the-selectors"),
])
def test_a_selector_the_language_cannot_take_is_refused_with_advice(selector, language, error_mentions):
    query, _, err = main._ast_selector_to_query(selector, language)
    assert query is None
    for text in error_mentions:
        assert text in err


def test_entity_encoded_replacement_names_the_encoding_first():
    """A SyntaxError from `&lt;head&gt;` points several lines from the cause.

    The generic checklist already mentions entities, but a model handed a line
    number plus five things to check re-emits the same encoding — observed
    live, twice in one session. When the content really is entity-encoded, say
    so before the checklist.
    """
    bad = ('@app.route(\'/\')\ndef index():\n'
           '    return render_template_string("""\n&lt;html&gt;\n<p>x</p>\n')
    res = main.structural_edit("app.py", _APP, "function:index", bad)
    assert not res.get("success")
    err = res["error"]
    assert "HTML-escaped characters" in err
    assert "&lt;" in err and "&gt;" in err


def test_a_plain_quoting_slip_in_a_small_node_gets_only_the_quoting_checklist():
    res = main.structural_edit("app.py", _APP, "function:index",
                               "@app.route('/')\ndef index():\n    return 'oops\n")
    assert not res.get("success")
    err = res["error"]
    assert "re-emit the full node" in err
    assert "HTML-escaped characters" not in err
    assert "template literal" not in err
    # structural_edit is still the right tool for a node the model can emit
    assert "use edit_file instead" not in err


def test_the_offending_line_stays_one_line():
    """e.text for a multi-line literal is the WHOLE literal — this used to
    paste ~190 lines into the error as the 'offending line'. Now the error
    quotes at most the first 160 characters of the first line, which one long
    line shows."""
    long_line = "return '" + "<div>row</div>" * 20  # 288 characters, the quote never closes
    res = main.structural_edit("app.py", _APP, "function:index",
                               "@app.route('/')\ndef index():\n    " + long_line + "\n")
    err = res["error"]
    assert "(offending line: " in err
    assert f"(offending line: {long_line[:160]} ...)" in err


# --- semantic no-op gate (O4) -------------------------------------------

def test_comment_only_replacement_is_rejected():
    """The observed false-done: deliberation written into the node as comments.

    The splice succeeded, the file parsed, the app answered curl, and the model
    reported adding a pause that was never in the template. Comments are absent
    from the AST, so comparing parsed trees answers "did anything executable
    change" exactly.
    """
    replacement = ("@app.route('/')\ndef index():\n"
                   "    return render_template_string(HTML_TEMPLATE) # JS is in the string.\n\n"
                   "# Wait, the instruction is to update the JS inside HTML_TEMPLATE.\n"
                   "# I should use edit_file to target the string literal.\n")
    res = main.structural_edit("app.py", _APP, "function:index", replacement)
    assert not res.get("success")
    assert "changes no code" in res["error"]
    # It must also say where the code actually lives, or the model retries the
    # same node with different comments.
    assert "edit_file" in res["error"]


def test_no_op_gate_fails_open_when_the_original_is_broken():
    """A file mid-repair must not be blocked by a gate that cannot parse it."""
    res = main.structural_edit("app.py", "def index(:\n    pass\n",
                               "function:index", "def index():\n    pass\n")
    assert res["success"] is True, res.get("error")


def test_truncated_template_names_edit_file_as_the_route():
    """Seven consecutive structural_edits failed this way in one session.

    The model re-emitted a large template inside function:index and truncated
    it every time. function:index is a VALID selector, so it never took the
    tag-selector path that explains where the markup actually lives.
    """
    truncated = ('@app.route(\'/\')\ndef index():\n'
                 '    return render_template_string("""\n<html>\n<script>\n'
                 'let paused = false;\n')
    res = main.structural_edit("app.py", _APP, "function:index", truncated)
    assert not res.get("success")
    err = res["error"]
    assert "template literal" in err
    assert "edit_file" in err and "one unique line" in err


# --- large-node steer ----------------------------------------------------

def test_large_node_failure_steers_to_edit_file():
    """Re-emitting a big node is where compact models fall over.

    Observed on a 1,702-line file: the model navigated correctly to the right
    ~200-line function, then failed three times trying to re-emit it in order
    to add six lines — a different syntax error each attempt.

    The slip here is a plain unterminated string, and Python 3.12 words both
    it and a cut-off template as "unterminated ... string literal". Matching
    the shorter phrase fired template advice on every quoting slip AND
    suppressed this message, because it only renders when no other lead is set.
    The host runs 3.9 ("EOL while scanning"), so it passed locally and failed
    in CI.
    """
    big = "def big():\n" + "".join(f"    x{i} = {i}\n" for i in range(60))
    src = big + "\ndef other():\n    return 1\n"
    res = main.structural_edit("m.py", src, "function:big",
                               "def big():\n    return 'oops\n")
    assert not res.get("success")
    err = res["error"]
    assert "61 lines" in err
    assert "use edit_file instead" in err
    assert "one unique line" in err
    assert "template literal" not in err, "a plain quoting error is not a template truncation"


# --- Jinja comment blanking ------------------------------------------------
#
# `_blank_jinja_comments` masks {# #} before the JS parse runs. It replaced a
# regex, `\{#[^\n]*#\}`, for two reasons kept here as tests: the regex was
# quadratic on input the model chooses, and its greedy `[^\n]*` masked live
# template text between two comments.

@pytest.mark.parametrize("block, masked", [
    # Every byte offset in the masked block must still point at the same byte
    # of the real file, so the length and every CR and LF stay where they are.
    pytest.param(b"a{# c #}b\n{# d #}e\r\n{# no close\n{##}",
                 b"a" + b" " * 7 + b"b\n" + b" " * 7 + b"e\r\n{# no close\n" + b" " * 4,
                 id="same-length-and-line-ends"),
    # Jinja renders '{# ONE #} tail {# TWO #}' as ' tail '. The greedy regex
    # matched it as one comment and blanked ' tail ' with it.
    pytest.param(b"{# ONE #} tail {# TWO #}", b" " * 9 + b" tail " + b" " * 9,
                 id="a-comment-ends-at-the-first-terminator"),
    pytest.param(b"{# no close", b"{# no close", id="an-unterminated-comment-is-left-alone"),
    pytest.param(b"{# open\nkeep me", b"{# open\nkeep me",
                 id="an-unterminated-comment-does-not-reach-the-next-line"),
])
def test_jinja_comments_are_blanked(block, masked):
    assert embedded_script._blank_jinja_comments(block) == masked


def test_many_openers_without_a_terminator_stay_linear():
    """The ReDoS guard. The old regex rescanned to end-of-line from every
    opener: 15.9s at 25k openers, minutes at 100k. A generous bound still
    fails loudly if a regex ever comes back."""
    src = b"{#" * 100_000 + b"a" * 100_000
    t0 = time.perf_counter()
    out = embedded_script._blank_jinja_comments(src)
    elapsed = time.perf_counter() - t0
    assert out == src, "no terminator anywhere means nothing is masked"
    assert elapsed < 5.0, f"masking went superlinear: {elapsed:.1f}s"
