"""Evidence strength comes from the verifier that ran, not the extension.

The first cut keyed off file extension and mapped every .py to
behavioral_complete. That is wrong for Pygame, Tkinter, curses and Flask:
they receive a compile smoke and nothing more, and would have closed the
pipeline claiming behaviour nobody demonstrated. It also routed .css through
a JavaScript probe and treated every .js as a canvas game.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters as A  # noqa: E402
import contract as C  # noqa: E402


CANVAS_GAME = """
const c = document.getElementById('gameCanvas');
const ctx = c.getContext('2d');
document.addEventListener('keydown', e => {});
function loop(){ ctx.fillRect(0,0,10,10); setTimeout(loop, 50); } loop();
"""
NODE_SCRIPT = "const fs = require('fs');\nmodule.exports = function add(a,b){return a+b;};"
PLAIN_JS_HELPERS = "export function clamp(v, lo, hi) { return Math.min(hi, Math.max(lo, v)); }"
PYGAME = "import pygame\npygame.init()\nscreen = pygame.display.set_mode((640,480))"
TKINTER = "from tkinter import Tk\nroot = Tk()\nroot.mainloop()"
FLASK = "from flask import Flask\napp = Flask(__name__)\n@app.route('/')\ndef home(): return 'hi'"
ALGO_PY = "import sys\nprint(sum(int(x) for x in open('input.txt')))"
HTML_INLINE = "<html><body><canvas id='c'></canvas><script>const x=document.getElementById('c');x.getContext('2d');document.addEventListener('keydown',()=>{});setTimeout(function f(){},10);</script></body></html>"
HTML_STATIC = "<html><body><h1>Hello</h1></body></html>"

_SCOPE = "static/game.js"
_CTX = None  # filled below, after contract import


def _record(adapter, accepted=True):
    """The production path: raw observation inputs in, contract record out."""
    return A.contract_record(adapter=adapter, accepted=accepted,
                             contract_id="generate:js", contract_version="1",
                             artifact_scope=_SCOPE,
                             evaluation_context_hash=C.content_hash("ctx"),
                             candidate_content_hash=C.content_hash("bytes"))


def test_every_js_file_gets_the_javascript_compile_adapter():
    """No verifier here runs browser code, so a canvas script is a compile
    like any other script: it may parse, it may never claim behaviour."""
    assert A.select_adapter("util.js", PLAIN_JS_HELPERS) == A.ADAPTER_JAVASCRIPT_COMPILE
    assert A.select_adapter("build.js", NODE_SCRIPT) == A.ADAPTER_JAVASCRIPT_COMPILE
    assert A.select_adapter("game.js", CANVAS_GAME) == A.ADAPTER_JAVASCRIPT_COMPILE


def test_interactive_python_never_gets_complete_evidence_from_compile():
    for src in (PYGAME, TKINTER, FLASK):
        adapter = A.select_adapter("app.py", src)
        assert adapter == A.ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED, src[:24]
        rec = _record(adapter, True)
        assert rec["evidence_strength"] == C.SYNTAX
        assert rec["supported"] is False
        assert rec["closure_eligible"] is False, \
            "compile smoke cannot close a Pygame artifact"


def test_algorithmic_python_is_only_syntax():
    adapter = A.select_adapter("solve.py", ALGO_PY)
    assert adapter == A.ADAPTER_PYTHON_COMPILE
    rec = _record(adapter, True)
    assert rec["evidence_strength"] == C.SYNTAX
    assert rec["closure_eligible"] is False


def test_css_is_never_sent_through_the_javascript_probe():
    adapter = A.select_adapter("style.css", "body { color: red; }")
    assert adapter == A.ADAPTER_CSS_SYNTAX
    rec = _record(adapter, True)
    assert rec["evidence_strength"] == C.SYNTAX
    # A stylesheet's contract closes on syntax: there is no behaviour to demand.
    assert A.closure_floor(adapter) == C.SYNTAX
    assert rec["closure_eligible"] is True


def test_jsx_and_tsx_are_unsupported_until_transpiled():
    for name in ("App.jsx", "App.tsx", "app.ts"):
        assert A.select_adapter(name, "const A = () => <div/>;") == A.ADAPTER_UNSUPPORTED


def test_html_is_unsupported_whatever_it_contains():
    """Nothing here runs a page: a page with an inline script is exactly as
    unverifiable as a static one, never vacuously verified."""
    assert A.select_adapter("index.html", HTML_INLINE) == A.ADAPTER_UNSUPPORTED
    assert A.select_adapter("index.html", HTML_STATIC) == A.ADAPTER_UNSUPPORTED


# ---------------------------------------------------------------------------
# Direct contract-record production (evidence.py retirement, step 1)
# ---------------------------------------------------------------------------
#
# adapters.py no longer imports evidence.py: it declares its own capabilities
# and derives strength from the OBSERVATIONS rather than from that module's
# graded string. These characterize the swap over the heterogeneous adapter
# matrix -- every adapter, supported and unsupported, accepted and rejected --
# against the retiring implementation.
#
# The comparison itself is test-only and disappears with evidence.py; the
# production path never calls it, which the import sentinel proves.



def _matrix():
    """Every observation shape the pipeline can hand this layer: (name,
    adapter, smoke verdict)."""
    cases = []
    for adapter in A.ALL_ADAPTERS:
        for smoke in (True, False):
            cases.append((f"{adapter}:smoke={smoke}", adapter, smoke))
    return cases


# The characterization values below were captured from the retiring
# implementation before it was deleted, and are asserted as literals now that
# there is nothing left to compare against. Adapter routing, supported vs
# unsupported, execution status and evidence strength for every observation
# shape the pipeline can produce.
CHARACTERIZED = {
    "python_compile:smoke=True": ("syntax", "ok", True),
    "python_compile:smoke=False": ("syntax", "error", False),
    "javascript_compile:smoke=True": ("syntax", "ok", True),
    "javascript_compile:smoke=False": ("syntax", "error", False),
    "css_syntax:smoke=True": ("syntax", "ok", True),
    "css_syntax:smoke=False": ("syntax", "error", False),
    "interactive_python_unsupported:smoke=True": ("syntax", "skipped", False),
    "interactive_python_unsupported:smoke=False": ("syntax", "error", False),
    "unsupported:smoke=True": ("syntax", "skipped", False),
    "unsupported:smoke=False": ("syntax", "error", False),
}


def test_direct_production_matches_the_characterized_behaviour():
    """Adapter routing, supported/unsupported, execution status and evidence
    strength for every shape the pipeline can produce."""
    for name, adapter, smoke in _matrix():
        rec = _record(adapter, smoke)
        assert rec["adapter_id"] == adapter, name
        assert name in CHARACTERIZED, f"{name}: an adapter shape no one characterized"
        want = CHARACTERIZED[name]
        got = (rec["evidence_strength"], rec["execution_status"], rec["supported"])
        assert got == want, f"{name}: {got} != {want}"


def test_direct_production_preserves_coverage_and_closure():
    """Criterion observations, required/missing/unmeasurable coverage, quality
    and closure eligibility follow from what the adapter reported."""
    for name, adapter, smoke in _matrix():
        rec = _record(adapter, smoke)
        caps = set(A._capabilities(adapter))
        obs = rec["observations"]

        # An adapter may only report on what it can measure.
        for cid, o in obs.items():
            if o["status"] in (C.DEMONSTRATED, C.REFUTED):
                assert cid in caps, f"{name}: {cid} reported outside capabilities"
        # Everything it cannot measure is unmeasurable, never silently missing.
        for r in rec["requirements"]:
            if r["required"] and r["id"] not in caps:
                assert obs[r["id"]]["status"] == C.NOT_APPLICABLE, name
                assert r["id"] in rec["missing_required"], name

        # Closure follows contract policy, not the adapter's opinion.
        assert rec["closure_eligible"] == (
            rec["requirements_complete"] and rec["supported"]
            and rec["execution_status"] == C.EXEC_OK
            and C.STRENGTH_ORDER.index(rec["evidence_strength"])
            >= C.STRENGTH_ORDER.index(A.closure_floor(adapter))
            and rec["overall_quality_score"] >= 1.0), name
        assert 0.0 <= rec["overall_quality_score"] <= 1.0, name


def test_direct_production_carries_identity_and_hashes():
    for name, adapter, smoke in _matrix():
        rec = _record(adapter, smoke)
        C.require_identity(rec, name)
        assert rec["contract_id"] == "generate:js"
        assert rec["contract_version"] == "1"
        assert rec["artifact_scope"] == _SCOPE
        assert rec["evaluation_context_hash"] == C.content_hash("ctx")
        assert rec["candidate_content_hash"] == C.content_hash("bytes")
        assert rec["adapter_version"] == A.LIVE_ADAPTER_VERSION


def test_adapter_ids_are_the_ones_records_carry():
    """The wire values are part of the contract identity, so they are pinned
    as literals rather than compared against another copy."""
    assert A.ADAPTER_JAVASCRIPT_COMPILE == "javascript_compile"
    assert A.ADAPTER_CSS_SYNTAX == "css_syntax"
    assert A.ADAPTER_PYTHON_COMPILE == "python_compile"
    assert A.ADAPTER_INTERACTIVE_PYTHON_UNSUPPORTED == "interactive_python_unsupported"
    assert A.ADAPTER_UNSUPPORTED == "unsupported"


def test_adapter_routing_lives_only_here():
    """Adapter routing has exactly one home."""
    v3 = Path(__file__).resolve().parents[2] / "v3-service"
    owners = [p.name for p in v3.glob("*.py") if "def select_adapter(" in p.read_text()]
    assert owners == ["adapters.py"], f"select_adapter owners: {owners}"
