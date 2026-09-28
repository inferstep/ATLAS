"""embedded_script_check — JavaScript/CSS syntax inside HTML the gates never parsed.

Ground truth is the 2026-08-01 dogfooding failure: a Flask app whose entire UI
is one `HTML_TEMPLATE = \"\"\"...\"\"\"` string rendered by render_template_string,
carrying a stray `)` in its <script> block. The Python compiles, the server
starts, `curl /` returns 200 — every existing gate passes and the game is dead
in the browser. The snippet below is that file's keydown handler verbatim.

The other half of the suite is the false-positive budget: this check BLOCKS
writes, so `<script src>`, non-JS script types, Jinja placeholders, escaped
Python strings and a `</script>` inside a JS string must all stay silent.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main  # noqa: E402

pytestmark = pytest.mark.skipif(
    not getattr(main, "_EMBEDDED_SCRIPT_AVAILABLE", False),
    reason="tree-sitter-javascript not installed in this environment",
)


# The keydown handler from /home/isaac/demo2/app.py, verbatim. The last line
# ends `= 'DOWN');` — one paren too many.
BROKEN_HANDLER = """        document.addEventListener('keydown', function(e) {
            const key = e.key;

            if(key === 'ArrowLeft' && direction !== 'RIGHT') nextDirection = 'LEFT';
            else if(key === 'ArrowUp' && direction !== 'DOWN') nextDirection = 'UP';
            else if(key === 'ArrowRight' && direction !== 'LEFT') nextDirection = 'RIGHT';
            else if(key === 'ArrowDown' && direction !== 'UP') nextDirection = 'DOWN');
        });
"""

FIXED_HANDLER = BROKEN_HANDLER.replace("= 'DOWN');", "= 'DOWN';")


def flask_app(handler: str) -> str:
    """The render_template_string shape the failure shipped in."""
    return (
        "from flask import Flask, render_template_string\n"
        "\n"
        "app = Flask(__name__)\n"
        "\n"
        'HTML_TEMPLATE = """\n'
        "<!DOCTYPE html>\n"
        '<html lang="en">\n'
        "<head>\n"
        "    <style>\n"
        "        body { margin: 0; background-color: #1a1a2e; }\n"
        "    </style>\n"
        "</head>\n"
        "<body>\n"
        '    <canvas id="gameCanvas" width="400" height="400"></canvas>\n'
        "    <script>\n"
        "        let direction = 'RIGHT';\n"
        "        let nextDirection = 'RIGHT';\n"
        "\n"
        + handler +
        "    </script>\n"
        "</body>\n"
        "</html>\n"
        '"""\n'
        "\n"
        "@app.route('/')\n"
        "def index():\n"
        "    return render_template_string(HTML_TEMPLATE)\n"
    )


def check(path, source):
    return main.embedded_script_check(path, source)


# --- the bug --------------------------------------------------------------

def test_stray_paren_in_python_template_string_is_rejected():
    source = flask_app(BROKEN_HANDLER)
    res = check("app.py", source)
    assert res["ok"], res
    assert len(res["findings"]) == 1, res
    f = res["findings"][0]
    # The line number must be the line of the .py FILE, not of the string.
    file_lines = source.split("\n")
    assert file_lines[f["line"] - 1].strip() == f["text"]
    assert "ArrowDown" in f["text"] and f["text"].endswith("'DOWN');")
    assert f["kind"] == "javascript"
    assert f["message"] == "unexpected `)`"
    # The model has to know WHICH string to go fix.
    assert "HTML_TEMPLATE" in f["where"] and "<script>" in f["where"]
    assert "delete the stray `)`" in f["hint"]


def test_corrected_python_template_string_passes():
    res = check("app.py", flask_app(FIXED_HANDLER))
    assert res["ok"] and res["findings"] == [], res


def test_real_dogfood_file_shape_reports_one_finding_only():
    """The surrounding template (CSS, canvas, event wiring) must not add noise —
    exactly one finding, the real one."""
    res = check("app.py", flask_app(BROKEN_HANDLER))
    assert [f["message"] for f in res["findings"]] == ["unexpected `)`"]


# --- .html / .jinja carriers ----------------------------------------------

BROKEN_HTML = (
    "<!DOCTYPE html>\n<html>\n<body>\n"
    '  <div id="app"></div>\n'
    "  <script>\n"
    "    function boot() {\n"
    "      const el = document.getElementById('app'));\n"
    "    }\n"
    "  </script>\n"
    "</body>\n</html>\n"
)


def test_broken_script_in_html_file_is_rejected():
    res = check("templates/index.html", BROKEN_HTML)
    assert res["ok"] and len(res["findings"]) == 1, res
    f = res["findings"][0]
    assert f["line"] == 7 and f["message"] == "unexpected `)`"
    assert f["where"] == "the <script> block"


def test_jinja_extension_is_carried_too():
    res = check("templates/page.jinja2", BROKEN_HTML)
    assert res["ok"] and len(res["findings"]) == 1, res


def test_clean_html_file_passes():
    res = check("templates/index.html", BROKEN_HTML.replace("('app'));", "('app');"))
    assert res["ok"] and res["findings"] == [], res


def test_missing_brace_is_reported_as_missing():
    html = ("<html><body><script>\n"
            "function boot() {\n"
            "  const x = 1;\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and len(res["findings"]) == 1, res
    assert "missing" in res["findings"][0]["message"]


# --- false-positive budget ------------------------------------------------

def test_script_src_without_body_passes():
    html = ('<html><body>\n'
            '  <script src="/static/game.js"></script>\n'
            '  <script src="/static/other.js">\n  </script>\n'
            '</body></html>\n')
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


def test_jinja_placeholders_in_js_pass():
    html = ("<html><body><script>\n"
            "  const user = '{{ current_user.name }}';\n"
            "  fetch('{{ url_for(\"api\") }}').then(r => r.json());\n"
            "  const cfg = {{ config_json }};\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


def test_jinja_statement_tags_in_js_pass():
    """{% %} wraps arbitrary control flow — undecidable, so never a finding."""
    html = ("<html><body><script>\n"
            "  {% if debug %}\n"
            "  console.log('debug');\n"
            "  {% endif %}\n"
            "  let n = 1;\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


def test_python_file_without_html_passes_untouched():
    source = (
        "import math\n"
        "\n"
        "def area(r):\n"
        "    return math.pi * r ** 2\n"
        "\n"
        "MESSAGE = 'no markup here at all'\n"
    )
    res = check("calc.py", source)
    assert res["ok"] and res["findings"] == [], res


def test_unsupported_extension_passes():
    for path in ("game.js", "style.css", "notes.md", "data.json"):
        res = check(path, "function f( {")
        assert res["ok"] and res["findings"] == [], path


def test_non_js_script_type_is_skipped():
    """A <script type=...> body that isn't JavaScript must never be JS-parsed."""
    for stype in ("text/template", "application/json", "importmap", "text/x-handlebars"):
        html = ('<html><body><script type="%s">\n'
                '  { "not": "javascript", }\n'
                "</script></body></html>\n" % stype)
        res = check("index.html", html)
        assert res["ok"] and res["findings"] == [], stype


def test_module_type_is_still_checked():
    html = ('<html><body><script type="module">\n'
            "  import { a } from './a.js'));\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and len(res["findings"]) == 1, res


def test_closing_script_tag_inside_js_string_reports_nothing():
    """A literal `</script>` in a JS string truncates tree-sitter-html's raw
    text; the tag-count imbalance must suppress the phantom error."""
    html = ("<html><body><script>\n"
            "  document.write('</script>');\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


def test_python_fstring_template_is_skipped():
    """In an f-string the braces are Python's, so the source bytes are not
    what the browser receives."""
    source = (
        'NAME = "x"\n'
        'HTML = f"""\n'
        "<html><body><script>\n"
        "  const n = '{NAME}'));\n"
        "</script></body></html>\n"
        '"""\n'
    )
    res = check("app.py", source)
    assert res["ok"] and res["findings"] == [], res


def test_python_string_with_escapes_is_skipped():
    """Python renders `\\n` before the browser sees it, so parsing the source
    bytes would be parsing something the page never receives."""
    source = (
        'HTML = """\n'
        "<html><body><script>\n"
        "  const s = 'a\\nb'));\n"
        "</script></body></html>\n"
        '"""\n'
    )
    res = check("app.py", source)
    assert res["ok"] and res["findings"] == [], res


def test_concatenated_python_strings_are_skipped():
    source = (
        'HTML = ("<html><body><script>"\n'
        '        "  const x = 1));"\n'
        '        "</script></body></html>")\n'
    )
    res = check("app.py", source)
    assert res["ok"] and res["findings"] == [], res


def test_js_block_inside_block_is_not_masked_into_an_error():
    """`{{` is legal JavaScript (a block inside a block). The raw parse runs
    first, so placeholder masking can only ever remove findings."""
    html = ("<html><body><script>\n"
            "  function f() {{ return 1; }}\n"
            "</script></body></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


# --- embedded CSS ---------------------------------------------------------

def test_unclosed_css_rule_is_reported():
    html = ("<html><head><style>\n"
            "  body { margin: 0;\n"
            "  .score { font-size: 32px; }\n"
            "</style></head></html>\n")
    res = check("index.html", html)
    assert res["ok"] and len(res["findings"]) == 1, res
    f = res["findings"][0]
    assert f["kind"] == "css" and f["line"] == 2
    assert "<style>" in f["where"]


def test_extra_css_brace_is_reported():
    html = ("<html><head><style>\n"
            "  body { margin: 0; }}\n"
            "</style></head></html>\n")
    res = check("index.html", html)
    assert res["ok"] and len(res["findings"]) == 1, res
    assert res["findings"][0]["message"] == "an extra `}`"


def test_css_braces_in_strings_and_comments_pass():
    html = ("<html><head><style>\n"
            '  .a::after { content: "}"; }\n'
            "  /* .b { commented out */\n"
            "  .c { color: red; }\n"
            "</style></head></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


def test_templated_css_is_skipped():
    html = ("<html><head><style>\n"
            "  body { color: {{ theme.fg }};\n"
            "</style></head></html>\n")
    res = check("index.html", html)
    assert res["ok"] and res["findings"] == [], res


# --- fail-soft ------------------------------------------------------------

def test_missing_grammar_reports_not_ok_never_raises(monkeypatch):
    """A build without tree-sitter-javascript must degrade to ok:false (the
    caller fails open), not crash the endpoint."""
    import symbols
    monkeypatch.setattr(symbols, "_EMBEDDED_SCRIPT_AVAILABLE", False)
    res = symbols.embedded_script_check("app.py", flask_app(BROKEN_HANDLER))
    assert res["ok"] is False and "error" in res


def test_oversized_source_is_skipped_not_parsed():
    import symbols
    padding = "\n# " + "x" * (symbols._EMBEDDED_MAX_BYTES + 1)
    res = check("app.py", flask_app(BROKEN_HANDLER) + padding)
    assert res["ok"] and res["findings"] == [], res


def test_empty_and_malformed_inputs_do_not_raise():
    """Empty content, unparseable Python and an unterminated <script> tag all
    have to come back clean — the gate blocks on positive findings only."""
    for path, source in (("app.py", ""), ("index.html", ""),
                         ("app.py", "def f("), ("index.html", "<script>"),
                         ("app.py", "HTML = '<script>'")):
        res = check(path, source)
        assert res["ok"] is True, (path, res)
        assert res["findings"] == [], (path, res)


# --- stopped render loop -----------------------------------------------------
#
# 2026-08-02 dogfooding: asked to make the snake speed up with the score, the
# model replaced `setInterval(draw, 100)` with `setTimeout(draw, delay)` at the
# same top-level spot and never re-armed it inside draw(). The JavaScript
# parses, `python app.py` starts the server, and the agent reported the change
# verified — while the game drew exactly one frame.

def _flask_page(script: str) -> str:
    return (
        'from flask import Flask, render_template_string\n'
        'app = Flask(__name__)\n'
        'HTML_TEMPLATE = """\n'
        '<html><body><canvas id="c"></canvas>\n'
        '<script>\n'
        '        let score = 0;\n'
        '        function draw() {\n'
        '            score += 1;\n'
        '        }\n'
        f'{script}\n'
        '</script>\n'
        '</body></html>\n'
        '"""\n'
    )


RECURRING = _flask_page('        setInterval(draw, 100);')
ONE_SHOT = _flask_page('        let delay = Math.max(50, 100 - score * 3);\n'
                       '        setTimeout(draw, delay);')


def test_a_loop_downgraded_to_a_single_shot_is_reported():
    findings = main.embedded_script_check("app.py", ONE_SHOT, RECURRING)["findings"]
    assert len(findings) == 1, findings
    f = findings[0]
    assert f["defect"] == "stopped_loop"
    assert "`draw`" in f["message"]
    assert "setTimeout(draw, delay)" in f["text"]


def test_the_same_content_without_the_pre_edit_file_reports_nothing():
    """The comparison IS the check — one version alone cannot tell a dead loop
    from a deliberate one-shot."""
    assert main.embedded_script_check("app.py", ONE_SHOT)["findings"] == []


def test_a_loop_that_rearms_itself_is_not_reported():
    rearmed = _flask_page('        setTimeout(draw, 100);').replace(
        "            score += 1;",
        "            score += 1;\n            setTimeout(draw, 100 - score);")
    assert main.embedded_script_check("app.py", rearmed, RECURRING)["findings"] == []


def test_a_new_delayed_one_shot_is_not_reported():
    added = _flask_page('        setInterval(draw, 100);\n'
                        '        function banner() { score = 0; }\n'
                        '        setTimeout(banner, 3000);')
    assert main.embedded_script_check("app.py", added, RECURRING)["findings"] == []


def test_removing_the_loop_outright_is_not_reported():
    """Deleting a loop is a decision; leaving one scheduled once is a slip."""
    removed = _flask_page('        // no loop here')
    assert main.embedded_script_check("app.py", removed, RECURRING)["findings"] == []


def test_a_file_that_never_looped_reports_nothing():
    plain = _flask_page('        setTimeout(draw, 100);')
    assert main.embedded_script_check("app.py", plain, _flask_page('        draw();'))["findings"] == []


# --- duplicate lexical binding ------------------------------------------------
#
# 2026-08-02 dogfooding: a replace_lines edit appended a second `let score = 0`
# to a <script> that already had one. tree-sitter parses it, so the syntax
# check passed and the edit landed — but a repeated let/const is an early
# SyntaxError, so the browser threw out the whole script and every handler on
# the page died. node --check on the extracted block: "SyntaxError: Identifier
# 'score' has already been declared".

def test_a_repeated_let_in_one_scope_is_reported():
    dup = _flask_page('        let score = 0;\n        setInterval(draw, 100);')
    findings = main.embedded_script_check("app.py", dup)["findings"]
    assert len(findings) == 1, findings
    assert findings[0]["defect"] == "redeclaration"
    assert "`score`" in findings[0]["message"]


def test_shadowing_in_an_inner_scope_is_legal():
    shadowed = _flask_page(
        '        function reset() {\n'
        '            let score = 0;\n'
        '            return score;\n'
        '        }\n'
        '        setInterval(draw, 100);')
    assert main.embedded_script_check("app.py", shadowed)["findings"] == []


def test_var_and_function_redeclaration_are_left_alone():
    """Legal JavaScript, however untidy — the check only reports what the
    engine actually refuses."""
    loose = _flask_page(
        '        var score = 1;\n'
        '        var score = 2;\n'
        '        function draw() {}\n'
        '        setInterval(draw, 100);')
    assert main.embedded_script_check("app.py", loose)["findings"] == []


def test_the_same_name_in_two_sibling_blocks_is_legal():
    siblings = _flask_page(
        '        if (true) { let n = 1; }\n'
        '        if (true) { let n = 2; }\n'
        '        setInterval(draw, 100);')
    assert main.embedded_script_check("app.py", siblings)["findings"] == []


def test_a_missing_brace_names_the_block_that_was_left_open():
    """tree-sitter reports the absence where the parser gave up — past the end
    of the construct. Observed live: "line 202: a `}` is missing" against an
    untouched `setInterval(draw, 100);`, which the model then tried to fix
    twice before the breaker stopped the run."""
    src = _flask_page(
        '        function draw() {\n'
        '            if (x) {\n'
        '                go();\n'
        '        }\n'
        '\n'
        '        setInterval(draw, 100);')
    findings = main.embedded_script_check("app.py", src)["findings"]
    assert len(findings) == 1, findings
    f = findings[0]
    assert f["message"] == "a `}` is missing"
    assert "function draw() {" in f["opened_text"]
    # The parser's stopping point is a line the edit never touched.
    assert "setInterval" in f["text"]
    assert f["opened_line"] < f["line"], f


def test_a_stray_closer_carries_no_opener():
    """It is reported exactly where it sits, so there is no second line to
    point at."""
    src = _flask_page("        if (a && b) go());")
    findings = main.embedded_script_check("app.py", src)["findings"]
    assert len(findings) == 1, findings
    assert "opened_line" not in findings[0], findings[0]
