"""Tests for graph-backed call resolution (issue #39, Phase 1).

This is the deepened structural-veto core: resolve a candidate's direct calls
against the import graph rather than accepting any project symbol.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import graph
from graph.extract import available as extraction_available
from symbols import _extract_python_call_targets

pytestmark = pytest.mark.skipif(not extraction_available(),
                                reason="tree-sitter Python grammar not installed")


def _unresolved(cand_code, project=None, strict=True):
    r = graph.unresolved_calls("cand.py", cand_code, project or {}, strict=strict)
    assert r["ok"]
    return r["unresolved"], r


class TestDirectCallNames:
    def test_identifier_calls_only(self):
        code = "def f():\n    g()\n    obj.method()\n    h(x)\n"
        names = _extract_python_call_targets(code.encode("utf-8"))
        assert "g" in names and "h" in names
        assert "method" not in names  # attribute call skipped


class TestResolution:
    def test_local_and_builtin_resolve(self):
        code = "def helper():\n    return 1\n\ndef main():\n    helper()\n    print(len([]))\n"
        unresolved, _ = _unresolved(code)
        assert unresolved == []

    def test_from_import_resolves(self):
        code = "from pkg.util import helper\n\ndef main():\n    return helper()\n"
        unresolved, _ = _unresolved(code, {"pkg/util.py": "def helper():\n    return 1\n"})
        assert unresolved == []

    def test_bare_unimported_project_symbol_flagged_in_strict(self):
        # helper exists in the project but is NOT imported by the candidate.
        # Strict mode flags it (a real NameError); this is the deepening over
        # the shipped veto, which accepted it via the project-symbol set.
        code = "def main():\n    return helper()\n"
        project = {"other.py": "def helper():\n    return 1\n"}
        strict_unres, _ = _unresolved(code, project, strict=True)
        assert "helper" in strict_unres
        # Non-strict restores the old lenient behavior.
        lax_unres, _ = _unresolved(code, project, strict=False)
        assert lax_unres == []

    def test_wildcard_resolved_to_module_exports(self):
        # `from mod import *` where mod defines `helper` -> helper resolves;
        # an unknown name still flags (we know mod's actual exports).
        code = "from pkg.mod import *\n\ndef main():\n    helper()\n    missing()\n"
        project = {"pkg/mod.py": "def helper():\n    return 1\n"}
        unresolved, r = _unresolved(code, project, strict=True)
        assert r["lenient"] is False
        assert "helper" not in unresolved
        assert "missing" in unresolved

    def test_wildcard_unresolved_module_is_lenient(self):
        # `from os import *` (stdlib, no in-batch file) -> can't know exports,
        # so nothing is flagged.
        code = "from os import *\n\ndef main():\n    getcwd()\n    whatever()\n"
        unresolved, r = _unresolved(code, {}, strict=True)
        assert r["lenient"] is True
        assert unresolved == []

    def test_attribute_calls_never_flagged(self):
        # os.path.join() -> attribute call, out of scope, must not be flagged
        # even though `join` is neither local nor imported.
        code = "import os\n\ndef main():\n    return os.path.join('a', 'b')\n"
        unresolved, _ = _unresolved(code, {})
        assert unresolved == []

    def test_unparseable_candidate(self):
        # extraction yields no calls; nothing to flag.
        unresolved, r = _unresolved("def (:\n  broken")
        assert unresolved == []


class TestNoFalsePositives:
    """Names bound by means other than def/class must not be flagged."""

    def test_assigned_callable(self):
        # x = lambda ...; x()  — x is an assignment target, not a def.
        unresolved, _ = _unresolved("def m():\n    x = lambda: 1\n    return x()\n")
        assert unresolved == []

    def test_module_level_assigned_callable(self):
        code = "make = something\nhandler = make()\n\ndef m():\n    return handler()\n"
        # `something` is a bare name read (not bound) — but it's not a *call*
        # here, so it isn't in scope for the veto; handler/make are assigned.
        unresolved, _ = _unresolved(code)
        assert "handler" not in unresolved and "make" not in unresolved

    def test_function_parameter_callback(self):
        # def run(cb): return cb()  — cb is a parameter.
        unresolved, _ = _unresolved("def run(cb):\n    return cb()\n")
        assert unresolved == []

    def test_higher_order(self):
        unresolved, _ = _unresolved("def apply(fn, x):\n    return fn(x)\n")
        assert unresolved == []

    def test_loop_and_with_and_walrus_targets(self):
        code = (
            "def m(items):\n"
            "    for f in items:\n"
            "        f()\n"
            "    with open('x') as fh:\n"
            "        fh()\n"
            "    if (g := items[0]):\n"
            "        g()\n"
        )
        unresolved, _ = _unresolved(code)
        assert unresolved == []

    def test_comprehension_target(self):
        unresolved, _ = _unresolved("def m(items):\n    return [f() for f in items]\n")
        assert unresolved == []

    def test_dotted_import_binds_top_package_only(self):
        # `import a.b.c` binds `a`; a bare `c()` is NOT bound and should flag,
        # while `a` (used as a.b.c.x attribute) is fine. Confirms first-segment
        # binding, not last.
        unresolved, _ = _unresolved("import xml.etree.ElementTree\n\ndef m():\n    ElementTree()\n")
        assert "ElementTree" in unresolved

    def test_still_flags_genuinely_unbound(self):
        # Regression guard: the deepening still works after the FP fix.
        unresolved, _ = _unresolved("def m():\n    return helper()\n",
                                    {"other.py": "def helper():\n    return 1\n"})
        assert "helper" in unresolved


class TestPythonOnly:
    """The resolver parses with the Python grammar, which reads any text
    tolerantly. An HTML page whose <script> called setInterval came back with
    setInterval and `function` unresolved, so the page with working JavaScript
    was vetoed and a static one kept. It answers only for Python now."""

    PAGE = ("<!DOCTYPE html>\n<html><body><canvas id='c'></canvas>\n"
            "<script>function draw() {}\nsetInterval(draw, 100);</script>\n"
            "</body></html>\n")

    @pytest.mark.parametrize("path, code", [
        ("templates/index.html", PAGE),
        ("static/app.js", "fetch('/api').then(r => r.json());\n"),
        ("main.go", "package main\nfunc main() { helper() }\n"),
    ])
    def test_non_python_is_not_resolved(self, path, code):
        r = graph.unresolved_calls(path, code, {"app.py": "def index():\n    pass\n"})
        assert r["ok"] is False
        assert "Python only" in r["error"]
        assert "unresolved" not in r
