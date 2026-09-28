"""Heterogeneous capability matrix: where can ATLAS produce trustworthy evidence?

Everything in this line of work was shaped by one Snake prompt, so the risk
is an architecture built around canvas games by accident. This matrix asks a
narrower question than the benchmark: across task families, which adapter is
selected, what strength can it honestly claim, and where must it say
unsupported or inconclusive instead of guessing.

Deterministic fixtures, no model, no sandbox. The invariant that matters
most is the false-behavioral_complete rate: zero.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters as A  # noqa: E402
import contract as C  # noqa: E402

# --- fixtures by family ----------------------------------------------------

ALGO_PY = "print(sum(int(x) for x in open('input.txt')))"
CLI_PY = "import sys\nprint('usage: tool <file>') if len(sys.argv) < 2 else print('ok')"
ALGO_JS = "const fs=require('fs');console.log(fs.readFileSync(0,'utf8').trim().length);"
NODE_MODULE = "const fs=require('fs');module.exports=(a,b)=>a+b;"
CANVAS_GAME = ("const c=document.getElementById('gameCanvas');const x=c.getContext('2d');"
               "document.addEventListener('keydown',e=>{});"
               "function loop(){x.fillRect(0,0,9,9);setTimeout(loop,50);}loop();")
DOM_APP = ("const btn=document.querySelector('#go');"
           "btn.addEventListener('click',()=>{document.getElementById('out').textContent='hi';});")
STATIC_HTML = "<html><body><h1>Docs</h1><p>Hello</p></body></html>"
HTML_INLINE = ("<html><body><canvas id='c'></canvas><script>"
               "const q=document.getElementById('c');q.getContext('2d');"
               "document.addEventListener('keydown',()=>{});setTimeout(function f(){},9);"
               "</script></body></html>")
HTML_EXTERNAL = "<html><body><canvas id='c'></canvas><script src='game.js'></script></body></html>"
CSS = "body { margin: 0; background: #111; }"
JSX = "export default function App(){ return <div className='x'>hi</div>; }"
FLASK_API = ("from flask import Flask\napp=Flask(__name__)\n"
             "@app.route('/health')\ndef h(): return {'ok': True}")
PYGAME = "import pygame\npygame.init()\nscreen=pygame.display.set_mode((320,240))"
TKINTER = "from tkinter import Tk\nroot=Tk()\nroot.mainloop()"
CONFIG_JSON = '{"debug": false, "retries": 3}'
UNSUPPORTED_LANG = "package main\nimport \"fmt\"\nfunc main(){ fmt.Println(1) }"

def _record(adapter, accepted=True):
    """The record the production path builds for a smoke-passing artifact."""
    return A.contract_record(
        adapter=adapter, accepted=accepted,
        contract_id="matrix", contract_version="1", artifact_scope="artifact",
        evaluation_context_hash=C.content_hash("ctx"),
        candidate_content_hash=C.content_hash("bytes"))


# task_family, fixture, path, expected_adapter
MATRIX = [
    ("algorithmic_io_python",   ALGO_PY,          "solve.py",    A.ADAPTER_PYTHON_COMPILE),
    ("cli_python",              CLI_PY,           "tool.py",     A.ADAPTER_PYTHON_COMPILE),
    ("algorithmic_io_js",       ALGO_JS,          "solve.js",    A.ADAPTER_JAVASCRIPT_COMPILE),
    ("node_module",             NODE_MODULE,      "util.js",     A.ADAPTER_JAVASCRIPT_COMPILE),
    ("canvas_game",             CANVAS_GAME,      "game.js",     A.ADAPTER_JAVASCRIPT_COMPILE),
    ("dom_app_no_canvas",       DOM_APP,          "app.js",      A.ADAPTER_JAVASCRIPT_COMPILE),
    ("static_html",             STATIC_HTML,      "index.html",  A.ADAPTER_UNSUPPORTED),
    ("html_inline_script",      HTML_INLINE,      "index.html",  A.ADAPTER_UNSUPPORTED),
    ("html_external_script",    HTML_EXTERNAL,    "index.html",  A.ADAPTER_UNSUPPORTED),
    ("css",                     CSS,              "style.css",   A.ADAPTER_CSS_SYNTAX),
    ("react_jsx",               JSX,              "App.jsx",     A.ADAPTER_UNSUPPORTED),
    ("backend_api",             FLASK_API,        "api.py",      A.ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED),
    ("pygame",                  PYGAME,           "game.py",     A.ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED),
    ("tkinter",                 TKINTER,          "ui.py",       A.ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED),
    ("config_data",             CONFIG_JSON,      "config.json", A.ADAPTER_UNSUPPORTED),
    ("unsupported_language",    UNSUPPORTED_LANG, "main.go",     A.ADAPTER_UNSUPPORTED),
]


@pytest.mark.parametrize("family,code,path,expected_adapter", MATRIX,
                         ids=[m[0] for m in MATRIX])
def test_adapter_routing(family, code, path, expected_adapter):
    assert A.select_adapter(path, code) == expected_adapter, family


@pytest.mark.parametrize("family,code,path,expected_adapter", MATRIX,
                         ids=[m[0] for m in MATRIX])
def test_no_family_claims_behaviour_it_cannot_demonstrate(
        family, code, path, expected_adapter):
    """The invariant that matters: false behavioral_complete rate is zero.

    Every family gets a smoke pass, and none may claim behaviour on that
    basis: no verifier here demonstrates more than syntax.
    """
    rec = _record(expected_adapter)
    # Nothing here may claim behavioural strength.
    assert rec["evidence_strength"] in (C.SYNTAX, C.RUNTIME), family
    # It may still close if its own contract floor is that low -- a
    # stylesheet has no behaviour to demand -- but never otherwise.
    floor = A.closure_floor(expected_adapter)
    if rec["closure_eligible"]:
        assert floor == rec["evidence_strength"], \
            f"{family} closed above its demonstrated strength"
        assert floor == C.SYNTAX, f"{family} closed without behaviour"
    else:
        assert C.STRENGTH_ORDER.index(floor) > \
            C.STRENGTH_ORDER.index(rec["evidence_strength"]) \
            or not rec["supported"] or not rec["requirements_complete"], family


@pytest.mark.parametrize("family,code,path,expected_adapter", MATRIX,
                         ids=[m[0] for m in MATRIX])
def test_unsupported_is_never_represented_as_failed(
        family, code, path, expected_adapter):
    rec = _record(expected_adapter)
    assert rec["execution_status"] != C.EXEC_ERROR, \
        f"{family}: the smoke check passed, so it is unverified, not failed"


def test_the_generic_policy_functions_carry_no_domain_vocabulary():
    """Strength ordering, coverage and ranking must be prompt-agnostic.

    The policy FUNCTIONS below are clean; this test fails the moment domain
    words spread into them.
    """
    src = Path(C.__file__).read_text()
    for fn in ("def rank_key(", "def _closure(", "def select(", "def build(",
               "def envelope("):
        start = src.index(fn)
        nxt = src.find("\n\n", start)
        body = src[start:nxt if nxt != -1 else len(src)]
        for word in ("snake", "food", "collision", "game"):
            assert word not in body.lower(), f"'{word}' leaked into {fn}"


def test_no_game_shaped_verifier_vocabulary_survives():
    """The browser probe and its snake-shaped criteria were removed outright.
    Nothing in the service may name them again: a verifier for one artifact
    class is not capability, and a criterion named for one game is not a
    contract."""
    v3 = Path(A.__file__).resolve().parent
    for word in ("collision_transition", "food_or_score_transition",
                 "temporal_progress", "input_causality", "browser_canvas_js",
                 "browser_inline_script", "run_browser_probe",
                 "js_is_instrumentable", "ATLAS_EVIDENCE_MODE"):
        owners = sorted(p.name for p in v3.rglob("*.py") if word in p.read_text())
        assert owners == [], f"{word} survives in {owners}"


def test_multi_file_behaviour_is_not_claimed_from_one_file():
    """HTML whose behaviour lives in an external file must not be graded from
    the HTML alone — that requires project-aware evaluation, which does not
    exist yet."""
    assert A.select_adapter("index.html", HTML_EXTERNAL) == A.ADAPTER_UNSUPPORTED
    rec = _record(A.ADAPTER_UNSUPPORTED)
    assert rec["supported"] is False
    assert rec["closure_eligible"] is False
