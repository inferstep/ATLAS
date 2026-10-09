"""structural_edit HTML selector tests — regression for <script>/<style> matching.

tree-sitter-html parses <script> and <style> as dedicated script_element /
style_element nodes (raw JS/CSS bodies), NOT generic `element` nodes, so the
generic element query matched them 0 times. These confirm the dedicated-node
queries match (and that bare tags still work)."""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main

pytestmark = pytest.mark.skipif(
    not getattr(main, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed in this environment",
)

HTML = (
    "<!DOCTYPE html>\n<html>\n<head>\n  <style>body { margin: 0; }</style>\n"
    "</head>\n<body>\n  <canvas id=\"gameCanvas\"></canvas>\n"
    "  <script src=\"/static/game.js\"></script>\n</body>\n</html>\n"
)


def test_script_selector_matches_script_element():
    res = main.structural_edit("templates/index.html", HTML, "<script>",
                         "<script>\n  const c = 1; // inline\n</script>")
    assert res.get("success"), res
    # the src-based script was replaced
    assert "/static/game.js" not in res["new_content"]
    assert "inline" in res["new_content"]


def test_style_selector_matches_style_element():
    res = main.structural_edit("templates/index.html", HTML, "<style>",
                         "<style>body { margin: 8px; }</style>")
    assert res.get("success"), res
    assert "margin: 8px" in res["new_content"]


def test_bare_element_selector_still_works():
    res = main.structural_edit("templates/index.html", HTML, "<canvas>",
                         "<canvas id=\"gameCanvas\" width=\"400\"></canvas>")
    assert res.get("success"), res
    assert "width=\"400\"" in res["new_content"]


def test_attribute_selector_rejected_with_guidance():
    q, _, err = main._ast_selector_to_query('<script src="x">', "html")
    assert q is None and err and "bare tag" in err


def test_html_tag_on_python_file_names_the_escape_hatch():
    """An HTML tag selector on a .py file is the Flask-template case.

    A live session editing a Flask app reached for `<script>` on the .py file
    whose script lives inside a template string, and got a selector list that
    did not address what it was trying to do. The Python grammar sees one
    string literal, so no selector can reach inside: the message has to name
    what does work instead.
    """
    q, _, err = main._ast_selector_to_query("<script>", "python")
    assert q is None and err
    assert "HTML-only" in err
    assert "edit_file" in err
    # Must NOT send the model to function:NAME. A live session followed that
    # advice, rewrote index() -- which only renders the module-level
    # HTML_TEMPLATE -- and the pause it "added" never reached the template.
    assert "Do NOT reach for function:NAME" in err


def test_non_tag_unknown_python_selector_still_lists_selectors():
    q, _, err = main._ast_selector_to_query("def:index", "python")
    assert q is None and err
    assert "function:NAME, class:NAME" in err


def test_entity_encoded_replacement_names_the_encoding_first():
    """A SyntaxError from `&lt;head&gt;` points several lines from the cause.

    The generic checklist already mentions entities, but a model handed a line
    number plus five things to check re-emits the same encoding — observed
    live, twice in one session. When the content really is entity-encoded, say
    so before the checklist.
    """
    src = ("from flask import Flask\napp = Flask(__name__)\n\n"
           "@app.route('/')\ndef index():\n    return 'ok'\n")
    bad = ('@app.route(\'/\')\ndef index():\n'
           '    return render_template_string("""\n&lt;html&gt;\n<p>x</p>\n')
    res = main.structural_edit("app.py", src, "function:index", bad)
    assert not res.get("success")
    err = res["error"]
    assert "HTML-escaped characters" in err
    assert "&lt;" in err and "&gt;" in err


def test_plain_syntax_error_does_not_claim_entity_encoding():
    src = ("from flask import Flask\napp = Flask(__name__)\n\n"
           "@app.route('/')\ndef index():\n    return 'ok'\n")
    res = main.structural_edit("app.py", src, "function:index",
                               "@app.route('/')\ndef index():\n    return 'oops\n")
    assert not res.get("success")
    assert "HTML-escaped characters" not in res["error"]


# --- semantic no-op gate (O4) -------------------------------------------

_APP = ("from flask import Flask, render_template_string\napp = Flask(__name__)\n\n"
        'HTML_TEMPLATE = """<script>let x=1;</script>"""\n\n'
        "@app.route('/')\ndef index():\n    return render_template_string(HTML_TEMPLATE)\n")


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


def test_real_code_change_still_applies():
    replacement = ("@app.route('/')\ndef index():\n"
                   "    return render_template_string(HTML_TEMPLATE, title='x')\n")
    res = main.structural_edit("app.py", _APP, "function:index", replacement)
    assert res.get("success"), res


def test_no_op_gate_fails_open_when_the_original_is_broken():
    """A file mid-repair must not be blocked by a gate that cannot parse it."""
    res = main.structural_edit("app.py", "def index(:\n    pass\n",
                               "function:index", "def index():\n    pass\n")
    assert "changes no code" not in res.get("error", "")


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


def test_plain_syntax_error_gets_no_template_advice():
    res = main.structural_edit("app.py", _APP, "function:index",
                               "@app.route('/')\ndef index():\n    return 'oops\n")
    assert not res.get("success")
    assert "template literal" not in res["error"]


# --- large-node steer ----------------------------------------------------

def test_large_node_failure_steers_to_edit_file():
    """Re-emitting a big node is where compact models fall over.

    Observed on a 1,702-line file: the model navigated correctly to the right
    ~200-line function, then failed three times trying to re-emit it in order
    to add six lines — a different syntax error each attempt.
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


def test_small_node_failure_does_not_steer_away():
    """structural_edit is still the right tool for a node the model can emit."""
    res = main.structural_edit("m.py", "def tiny():\n    return 1\n",
                               "function:tiny", "def tiny():\n    return 'oops\n")
    assert not res.get("success")
    assert "use edit_file instead" not in res["error"]


def test_plain_unterminated_string_is_not_read_as_a_truncated_template():
    """Python 3.12 words both as "unterminated ... string literal".

    Matching the shorter phrase fired template advice on every quoting slip
    AND suppressed the large-node message, because that message only renders
    when no other lead is set. The host runs 3.9 ("EOL while scanning") so it
    passed locally and failed in CI — hence the explicit both-messages check.
    """
    big = "def big():\n" + "".join(f"    x{i} = {i}\n" for i in range(60))
    src = big + "\ndef other():\n    return 1\n"
    res = main.structural_edit("m.py", src, "function:big",
                               "def big():\n    return 'oops\n")
    assert not res.get("success")
    err = res["error"]
    assert "template literal" not in err, "a plain quoting error is not a template truncation"
    assert "61 lines" in err, "the large-node steer must still render"


class TestJinjaCommentBlanking:
    """`_blank_jinja_comments` masks {# #} before the JS parse runs.

    It replaced a regex, `\\{#[^\\n]*#\\}`, for two reasons kept here as
    tests: the regex was quadratic on input the model chooses, and its
    greedy `[^\\n]*` masked live template text between two comments.
    """

    @staticmethod
    def _f():
        import symbols
        return symbols._blank_jinja_comments

    def test_masking_preserves_length_and_newlines(self):
        """The caller documents that every byte offset in the masked block
        still points at the same byte of the real file. Any length change
        silently misplaces every finding after it."""
        src = b"a{# c #}b\n{# d #}e\r\n{# no close\n{##}"
        out = self._f()(src)
        assert len(out) == len(src)
        nl = lambda b: [i for i, c in enumerate(b) if c in (0x0A, 0x0D)]
        assert nl(out) == nl(src)

    def test_a_comment_ends_at_the_first_terminator(self):
        """Jinja renders '{# a #} tail {# b #}' as ' tail '. The greedy regex
        matched it as one comment and blanked ' tail ' with it."""
        out = self._f()(b"{# ONE #} tail {# TWO #}")
        assert b"tail" in out, "text between two comments must survive masking"
        assert b"ONE" not in out and b"TWO" not in out, "comment bodies must be masked"

    def test_an_unterminated_comment_is_left_alone(self):
        assert self._f()(b"{# no close") == b"{# no close"
        # ...and does not leak across the newline into the next line's text
        assert self._f()(b"{# open\nkeep me") == b"{# open\nkeep me"

    def test_many_openers_without_a_terminator_stay_linear(self):
        """The ReDoS guard. The old regex rescanned to end-of-line from every
        opener: 15.9s at 25k openers, minutes at 100k. A generous bound still
        fails loudly if a regex ever comes back."""
        import time
        src = b"{#" * 100_000 + b"a" * 100_000
        t0 = time.perf_counter()
        out = self._f()(src)
        elapsed = time.perf_counter() - t0
        assert out == src, "no terminator anywhere means nothing is masked"
        assert elapsed < 5.0, f"masking went superlinear: {elapsed:.1f}s"


class TestWrongNodeSteer:
    """A SMALL node with a HUGE replacement means the model picked a node that
    cannot hold the change — usually markup/JS living in a module-level string.

    Observed live three sessions running: `function:index` in a Flask app is
    two lines (`return render_template_string(HTML_TEMPLATE)`) and the model
    kept inlining the whole ~190-line template into it. Every rejection talked
    about quoting, because the content really was malformed, so nothing ever
    said the selector could not work.
    """

    FIXTURE = (
        'from flask import Flask, render_template_string\n\n'
        'app = Flask(__name__)\n\n'
        'HTML_TEMPLATE = """\n' + "\n".join(f"<div>{i}</div>" for i in range(60)) + '\n"""\n\n'
        "@app.route('/')\n"
        'def index():\n'
        '    return render_template_string(HTML_TEMPLATE)\n'
    )

    def _huge_replacement(self, terminated: bool) -> str:
        body = "\n".join(f"  <div>line {i}</div>" for i in range(120))
        tail = '\n""")' if terminated else "\n}} {\n"
        return ("@app.route('/')\ndef index():\n"
                '    return render_template_string("""\n' + body + tail)

    def test_it_names_the_node_size_and_the_real_location(self):
        res = main.structural_edit("app.py", self.FIXTURE, "function:index",
                                   self._huge_replacement(terminated=False))
        assert not res.get("success")
        err = res["error"]
        assert "only 3 line(s)" in err or "only 2 line(s)" in err
        assert "HTML_TEMPLATE" in err, "must name where the code actually lives"
        assert "edit_file" in err

    def test_it_outranks_the_quoting_advice(self):
        """Quoting broke BECAUSE the wrong node was re-emitted. Leading with
        'check your quotes' sends the model back to reproduce the same lines."""
        err = main.structural_edit("app.py", self.FIXTURE, "function:index",
                                   self._huge_replacement(terminated=False))["error"]
        assert "does not live here" in err
        # ...and must not then contradict itself by demanding a re-emit.
        assert "re-emit the full node" not in err

    def test_the_offending_line_stays_one_line(self):
        """e.text for a multi-line literal is the WHOLE literal — this used to
        paste ~190 lines into the error as the 'offending line'."""
        err = main.structural_edit("app.py", self.FIXTURE, "function:index",
                                   self._huge_replacement(terminated=False))["error"]
        marker = "offending line: "
        if marker in err:
            shown = err.split(marker, 1)[1].split(")", 1)[0]
            assert "\n" not in shown
            assert len(shown) < 200

    def test_a_proportionate_replacement_is_left_alone(self):
        """The steer must not fire on an ordinary whole-node rewrite."""
        ok = ("@app.route('/')\ndef index():\n"
              "    x = 1\n    return render_template_string(HTML_TEMPLATE)\n")
        res = main.structural_edit("app.py", self.FIXTURE, "function:index", ok)
        assert res.get("success"), res.get("error")
