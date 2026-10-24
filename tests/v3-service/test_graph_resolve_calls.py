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

HELPER = "def helper():\n    return 1\n"


def test_identifier_calls_only():
    code = "def f():\n    g()\n    obj.method()\n    h(x)\n"
    names = _extract_python_call_targets(code.encode("utf-8"))
    assert "g" in names
    assert "h" in names
    assert "method" not in names  # attribute call skipped


@pytest.mark.parametrize("code, project, expected", [
    pytest.param("def helper():\n    return 1\n\ndef main():\n    helper()\n    print(len([]))\n",
                 {}, [], id="local-def-and-builtins"),
    pytest.param("from pkg.util import helper\n\ndef main():\n    return helper()\n",
                 {"pkg/util.py": HELPER}, [], id="from-import"),
    # os.path.join() is an attribute call: out of scope, although `join` is
    # neither local nor imported.
    pytest.param("import os\n\ndef main():\n    return os.path.join('a', 'b')\n",
                 {}, [], id="attribute-call"),
    pytest.param("def (:\n  broken", {}, [], id="unparseable-candidate"),
    # Names bound by means other than def/class are not flagged.
    pytest.param("def m():\n    x = lambda: 1\n    return x()\n", {}, [], id="assigned-lambda"),
    # `something` is read, not called, so the veto does not look at it.
    pytest.param("make = something\nhandler = make()\n\ndef m():\n    return handler()\n",
                 {}, [], id="module-level-assignment"),
    pytest.param("def run(cb):\n    return cb()\n", {}, [], id="parameter-callback"),
    pytest.param("def apply(fn, x):\n    return fn(x)\n", {}, [], id="higher-order-parameter"),
    pytest.param("def m(items):\n"
                 "    for f in items:\n"
                 "        f()\n"
                 "    with open('x') as fh:\n"
                 "        fh()\n"
                 "    if (g := items[0]):\n"
                 "        g()\n",
                 {}, [], id="for-with-and-walrus-targets"),
    pytest.param("def m(items):\n    return [f() for f in items]\n", {}, [],
                 id="comprehension-target"),
    # `import a.b.c` binds `a` only, so a bare `c()` is not bound.
    pytest.param("import xml.etree.ElementTree\n\ndef m():\n    ElementTree()\n",
                 {}, ["ElementTree"], id="dotted-import-binds-only-its-top-package"),
    # helper exists in the project but is NOT imported: a real NameError. This
    # is the deepening over the shipped veto, which accepted any project symbol.
    pytest.param("def main():\n    return helper()\n", {"other.py": HELPER}, ["helper"],
                 id="project-symbol-not-imported"),
])
def test_unresolved_calls_in_strict_mode(code, project, expected):
    result = graph.unresolved_calls("cand.py", code, project)
    assert result["ok"]
    assert result["unresolved"] == expected


def test_non_strict_mode_accepts_a_project_symbol_that_is_not_imported():
    result = graph.unresolved_calls("cand.py", "def main():\n    return helper()\n",
                                    {"other.py": HELPER}, strict=False)
    assert result["ok"]
    assert result["unresolved"] == []


@pytest.mark.parametrize("code, project, lenient, expected", [
    # The module is in the batch: its defs resolve and an unknown name is flagged.
    pytest.param("from pkg.mod import *\n\ndef main():\n    helper()\n    missing()\n",
                 {"pkg/mod.py": HELPER}, False, ["missing"], id="from-a-project-module"),
    # The module is not in the batch (stdlib): its names are unknown, so
    # nothing is flagged.
    pytest.param("from os import *\n\ndef main():\n    getcwd()\n    whatever()\n",
                 {}, True, [], id="from-an-unknown-module"),
])
def test_wildcard_import(code, project, lenient, expected):
    result = graph.unresolved_calls("cand.py", code, project)
    assert result["ok"]
    assert result["lenient"] is lenient
    assert result["unresolved"] == expected


# The resolver parses with the Python grammar, which reads any text
# tolerantly. An HTML page whose <script> called setInterval came back with
# setInterval and `function` unresolved, so the page with working JavaScript
# was vetoed and a static one kept. It answers only for Python now.
@pytest.mark.parametrize("path, code", [
    pytest.param("templates/index.html",
                 "<!DOCTYPE html>\n<html><body><canvas id='c'></canvas>\n"
                 "<script>function draw() {}\nsetInterval(draw, 100);</script>\n"
                 "</body></html>\n",
                 id="html-page"),
    pytest.param("static/app.js", "fetch('/api').then(r => r.json());\n", id="javascript"),
    pytest.param("main.go", "package main\nfunc main() { helper() }\n", id="go"),
])
def test_non_python_is_not_resolved(path, code):
    r = graph.unresolved_calls(path, code, {"app.py": "def index():\n    pass\n"})
    assert r["ok"] is False
    assert "Python only" in r["error"]
    assert "unresolved" not in r
