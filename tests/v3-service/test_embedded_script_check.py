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

import main
import symbols

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


# --- the bug --------------------------------------------------------------

def test_stray_paren_in_python_template_string_is_rejected():
    source = flask_app(BROKEN_HANDLER)
    res = main.embedded_script_check("app.py", source)
    assert res["ok"], res
    # Exactly one finding: the template's CSS, canvas and event wiring add no noise.
    assert len(res["findings"]) == 1, res
    f = res["findings"][0]
    # The line number must be the line of the .py FILE, not of the string.
    file_lines = source.split("\n")
    assert file_lines[f["line"] - 1].strip() == f["text"]
    assert "ArrowDown" in f["text"]
    assert f["text"].endswith("'DOWN');")
    assert f["kind"] == "javascript"
    assert f["message"] == "unexpected `)`"
    # The model has to know WHICH string to go fix.
    assert "HTML_TEMPLATE" in f["where"]
    assert "<script>" in f["where"]
    assert "delete the stray `)`" in f["hint"]


# --- a broken block in an HTML carrier --------------------------------------

# A quoted CSS string, then a rule: the string must end at its closing quote, even when it is empty.
STRING_THEN_UNCLOSED_RULE = ("<html><head><style>\n"
                             "  .a::after { content: %s; }\n"
                             "  body { margin: 0;\n"
                             "</style></head></html>\n")
STRING_THEN_RULE = STRING_THEN_UNCLOSED_RULE.replace("body { margin: 0;", ".c { color: red; }")


@pytest.mark.parametrize("path, source, kind, line, message, where", [
    pytest.param("templates/index.html", BROKEN_HTML,
                 "javascript", 7, "unexpected `)`", "the <script> block",
                 id="a script in an .html file"),
    pytest.param("templates/page.jinja2", BROKEN_HTML,
                 "javascript", 7, "unexpected `)`", "the <script> block",
                 id="a script in a .jinja2 template"),
    pytest.param("index.html",
                 '<html><body><script type="module">\n'
                 "  import { a } from './a.js'));\n"
                 "</script></body></html>\n",
                 "javascript", 2, "unexpected `))`", "the <script> block",
                 id="a module script is still JavaScript"),
    pytest.param("index.html",
                 "<html><body><script>\n"
                 "function boot() {\n"
                 "  const x = 1;\n"
                 "</script></body></html>\n",
                 "javascript", 3, "a `}` is missing", "the <script> block",
                 id="a missing brace is reported as missing"),
    pytest.param("index.html",
                 "<html><head><style>\n"
                 "  body { margin: 0;\n"
                 "  .score { font-size: 32px; }\n"
                 "</style></head></html>\n",
                 "css", 2, "a `{` that is never closed", "the <style> block",
                 id="an unclosed CSS rule"),
    pytest.param("index.html",
                 "<html><head><style>\n"
                 "  body { margin: 0; }}\n"
                 "</style></head></html>\n",
                 "css", 2, "an extra `}`", "the <style> block",
                 id="an extra CSS brace"),
    pytest.param("index.html", STRING_THEN_UNCLOSED_RULE % '""', "css", 3, "a `{` that is never closed",
                 "the <style> block", id='an unclosed rule after an empty "" string'),
    pytest.param("index.html", STRING_THEN_UNCLOSED_RULE % "''", "css", 3, "a `{` that is never closed",
                 "the <style> block", id="an unclosed rule after an empty '' string"),
    pytest.param("index.html", STRING_THEN_UNCLOSED_RULE % '"x"', "css", 3, "a `{` that is never closed",
                 "the <style> block", id='an unclosed rule after a one-letter "x" string'),
    pytest.param("index.html", STRING_THEN_UNCLOSED_RULE % "'x'", "css", 3, "a `{` that is never closed",
                 "the <style> block", id="an unclosed rule after a one-letter 'x' string"),
])
def test_a_broken_block_is_reported_once(path, source, kind, line, message, where):
    res = main.embedded_script_check(path, source)
    assert res["ok"], res
    assert [(f["kind"], f["line"], f["message"], f["where"]) for f in res["findings"]] == [
        (kind, line, message, where)]


# --- false-positive budget ------------------------------------------------

NOT_JAVASCRIPT = ('<html><body><script type="%s">\n'
                  '  { "not": "javascript", }\n'
                  "</script></body></html>\n")


@pytest.mark.parametrize("path, source", [
    pytest.param("app.py", flask_app(FIXED_HANDLER), id="the corrected template string"),
    pytest.param("templates/index.html", BROKEN_HTML.replace("('app'));", "('app');"),
                 id="the corrected html file"),
    # The browser ignores the inline body of a script that has a src.
    pytest.param("index.html",
                 "<html><body>\n"
                 '  <script src="/static/game.js"></script>\n'
                 '  <script src="/static/other.js">\n'
                 "    this is not JavaScript (\n"
                 "  </script>\n"
                 "</body></html>\n",
                 id="a src script, empty or with an inline body"),
    pytest.param("index.html",
                 "<html><body><script>\n"
                 "  const user = '{{ current_user.name }}';\n"
                 "  fetch('{{ url_for(\"api\") }}').then(r => r.json());\n"
                 "  const cfg = {{ config_json }};\n"
                 "</script></body></html>\n",
                 id="Jinja placeholders in JavaScript"),
    # {% %} wraps arbitrary control flow: undecidable, so never a finding.
    pytest.param("index.html",
                 "<html><body><script>\n"
                 "  {% if debug %}\n"
                 "  console.log('debug');\n"
                 "  {% endif %}\n"
                 "  let n = 1;\n"
                 "</script></body></html>\n",
                 id="Jinja statement tags in JavaScript"),
    # A <script type=...> body that is not JavaScript must never be JS-parsed.
    pytest.param("index.html", NOT_JAVASCRIPT % "text/template", id="a text/template script"),
    pytest.param("index.html", NOT_JAVASCRIPT % "application/json", id="an application/json script"),
    pytest.param("index.html", NOT_JAVASCRIPT % "importmap", id="an importmap script"),
    pytest.param("index.html", NOT_JAVASCRIPT % "text/x-handlebars", id="a text/x-handlebars script"),
    # A literal `</script>` in a JS string truncates tree-sitter-html's raw
    # text; the tag-count imbalance must suppress the phantom error.
    pytest.param("index.html",
                 "<html><body><script>\n"
                 "  document.write('</script>');\n"
                 "</script></body></html>\n",
                 id="a closing script tag inside a JS string"),
    # `{{` is legal JavaScript (a block inside a block). The raw parse runs
    # first, so placeholder masking can only ever remove findings.
    pytest.param("index.html",
                 "<html><body><script>\n"
                 "  function f() {{ return 1; }}\n"
                 "</script></body></html>\n",
                 id="a JS block inside a block"),
    pytest.param("index.html",
                 "<html><head><style>\n"
                 '  .a::after { content: "}"; }\n'
                 "  /* .b { commented out */\n"
                 "  .c { color: red; }\n"
                 "</style></head></html>\n",
                 id="CSS braces in strings and comments"),
    pytest.param("index.html",
                 "<html><head><style>\n"
                 "  body { color: {{ theme.fg }};\n"
                 "</style></head></html>\n",
                 id="templated CSS"),
    pytest.param("index.html", STRING_THEN_RULE % '""', id='a CSS rule after an empty "" string'),
    pytest.param("index.html", STRING_THEN_RULE % "''", id="a CSS rule after an empty '' string"),
    pytest.param("calc.py",
                 "import math\n"
                 "\n"
                 "def area(r):\n"
                 "    return math.pi * r ** 2\n"
                 "\n"
                 "MESSAGE = 'no markup here at all'\n",
                 id="a Python file without HTML"),
    # In an f-string the braces are Python's, so the source bytes are not
    # what the browser receives.
    pytest.param("app.py",
                 'NAME = "x"\n'
                 'HTML = f"""\n'
                 "<html><body><script>\n"
                 "  const n = '{NAME}'));\n"
                 "</script></body></html>\n"
                 '"""\n',
                 id="a Python f-string"),
    # Python renders `\n` before the browser sees it, so parsing the source
    # bytes would be parsing something the page never receives.
    pytest.param("app.py",
                 'HTML = """\n'
                 "<html><body><script>\n"
                 "  const s = 'a\\nb'));\n"
                 "</script></body></html>\n"
                 '"""\n',
                 id="a Python string with escapes"),
    # Each literal of a concatenation holds only part of the document, so none
    # is checked, not even one that holds a whole broken block.
    pytest.param("app.py",
                 'HTML = ("<html><body><script>  const x = 1));</script>"\n'
                 '        "</body></html>")\n',
                 id="concatenated Python strings"),
    pytest.param("game.js", "function f( {", id="a .js file"),
    pytest.param("style.css", "function f( {", id="a .css file"),
    pytest.param("notes.md", BROKEN_HTML, id="HTML in a .md file"),
    pytest.param("data.json", "function f( {", id="a .json file"),
    # The gate blocks on positive findings only, so input it cannot read is clean.
    pytest.param("app.py", "", id="an empty Python file"),
    pytest.param("index.html", "", id="an empty html file"),
    pytest.param("app.py", "def f(", id="Python that does not parse"),
    pytest.param("index.html", "<script>", id="an unterminated script tag"),
    pytest.param("app.py", "HTML = '<script>'", id="an unterminated script tag in a Python string"),
    pytest.param("app.py", flask_app(BROKEN_HANDLER) + "\n# " + "x" * (symbols._EMBEDDED_MAX_BYTES + 1),
                 id="a source over the size cap is not parsed"),
])
def test_reports_nothing(path, source):
    res = main.embedded_script_check(path, source)
    assert res["ok"] is True, res
    assert res["findings"] == [], res


# --- fail-soft ------------------------------------------------------------

def test_missing_grammar_reports_not_ok_never_raises(monkeypatch):
    """A build without tree-sitter-javascript must degrade to ok:false (the
    caller fails open), not crash the endpoint."""
    monkeypatch.setattr(symbols, "_EMBEDDED_SCRIPT_AVAILABLE", False)
    res = symbols.embedded_script_check("app.py", flask_app(BROKEN_HANDLER))
    assert res["ok"] is False
    assert "error" in res


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


# --- what neither check reports ---------------------------------------------

@pytest.mark.parametrize("content, previous", [
    # The comparison IS the check: one version alone cannot tell a dead loop
    # from a deliberate one-shot.
    pytest.param(ONE_SHOT, "", id="a one-shot without the pre-edit file"),
    pytest.param(_flask_page('        setTimeout(draw, 100);').replace(
                     "            score += 1;",
                     "            score += 1;\n            setTimeout(draw, 100 - score);"),
                 RECURRING, id="a loop that re-arms itself"),
    pytest.param(_flask_page('        setInterval(draw, 100);\n'
                             '        function banner() { score = 0; }\n'
                             '        setTimeout(banner, 3000);'),
                 RECURRING, id="a new delayed one-shot beside the loop"),
    # Deleting a loop is a decision; leaving one scheduled once is a slip.
    pytest.param(_flask_page('        // no loop here'), RECURRING, id="the loop removed outright"),
    pytest.param(_flask_page('        setTimeout(draw, 100);'), _flask_page('        draw();'),
                 id="a file that never looped"),
    pytest.param(_flask_page('        function reset() {\n'
                             '            let score = 0;\n'
                             '            return score;\n'
                             '        }\n'
                             '        setInterval(draw, 100);'),
                 "", id="a let shadowed in an inner scope"),
    # Legal JavaScript, however untidy: the check only reports what the engine
    # actually refuses.
    pytest.param(_flask_page('        var score = 1;\n'
                             '        var score = 2;\n'
                             '        function draw() {}\n'
                             '        setInterval(draw, 100);'),
                 "", id="var and function declared twice"),
    pytest.param(_flask_page('        if (true) { let n = 1; }\n'
                             '        if (true) { let n = 2; }\n'
                             '        setInterval(draw, 100);'),
                 "", id="the same let in two sibling blocks"),
])
def test_no_stopped_loop_or_redeclaration_is_reported(content, previous):
    assert main.embedded_script_check("app.py", content, previous)["findings"] == []
