"""JavaScript extraction (issue #39, Phase 6) with golden parity against
chiasmus's own extractor, captured via npx tsx on the same fixture."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

import graph
from graph.analyses import callees
from graph.extract import extract_file, is_js, is_supported, js_available

pytestmark = pytest.mark.skipif(not js_available(),
                                reason="tree-sitter JavaScript grammar not installed")

JS_FIXTURE = """import { clean } from "./helpers.js";
import defaultExport from "./def.js";

export function main() {
  const p = new Pipeline();
  return p.run(read());
}

class Pipeline {
  run(data) {
    return this.load(data);
  }
  load(data) {
    return clean(data);
  }
}

const read = () => {
  return [];
};

function helper() {
  return transform();
}
"""

GOLDEN = {
    "defines": [["Pipeline", "class"], ["helper", "function"], ["load", "method"],
                ["main", "function"], ["read", "function"], ["run", "method"]],
    "calls": [["helper", "transform"], ["load", "clean"], ["main", "read"],
              ["main", "run"], ["run", "load"]],
    "imports": [["clean", "./helpers.js"], ["defaultExport", "./def.js"]],
}


class TestJsGoldenParity:
    def test_matches_chiasmus(self):
        # The exact call list also shows that `new Pipeline()` is not a call.
        g = extract_file("pipe.js", JS_FIXTURE)
        got = {
            "defines": sorted([[d.name, d.kind] for d in g.defines]),
            "calls": sorted([[c.caller, c.callee] for c in g.calls]),
            "imports": sorted([[i.name, i.source] for i in g.imports]),
        }
        for key, expected in GOLDEN.items():
            assert got[key] == sorted(expected), f"{key} diverged from chiasmus"

    def test_method_contains(self):
        g = extract_file("pipe.js", JS_FIXTURE)
        pairs = {(c.parent, c.child) for c in g.contains}
        assert ("Pipeline", "run") in pairs
        assert ("Pipeline", "load") in pairs


def test_what_the_reference_fixture_leaves_out():
    src = (
        'import * as fs from "fs";\n'
        'import { readFile as rf } from "./io.js";\n'
        "\n"
        "const parse = function (text) {\n"
        "  return split(text);\n"
        "};\n"
        "class Store {\n"
        "  cache = makeCache();\n"
        "  save() {\n"
        "    const write = () => flush();\n"
        "    write();\n"
        "    write();\n"
        "  }\n"
        "}\n"
        "function start() {}\n"
        "start();\n"
    )
    g = extract_file("store.js", src)
    # A function value inside a class counts as a method, like the class's own.
    assert sorted([d.name, d.kind, d.line] for d in g.defines) == [
        ["Store", "class", 7], ["parse", "function", 4], ["save", "method", 9],
        ["start", "function", 15], ["write", "method", 10]]
    # A call in the class body belongs to the class, a call in a function value
    # to that value, a pair made twice is recorded once, and the call outside
    # any scope is not recorded.
    assert sorted((c.caller, c.callee) for c in g.calls) == [
        ("Store", "makeCache"), ("parse", "split"), ("save", "write"), ("write", "flush")]
    assert sorted((i.name, i.source) for i in g.imports) == [("fs", "fs"), ("rf", "./io.js")]


class TestMixedProject:
    def test_python_and_js_in_one_graph(self):
        files = {
            "a.py": "def py_fn():\n    return 1\n",
            "b.js": "function js_fn() {\n  return helper();\n}\n",
        }
        g = graph.build_graph(files)
        names = {d.name for d in g.defines}
        assert {"py_fn", "js_fn"} <= names
        # analyses work across the merged multi-language graph
        assert callees(g, "js_fn") == ["helper"]

    def test_is_supported(self):
        assert is_js("x.js")
        assert is_js("x.mjs")
        assert is_supported("x.py")
        assert is_supported("x.jsx")
        assert not is_supported("x.md")


@pytest.mark.parametrize("path", ["lines.py", "lines.js"])
def test_a_file_without_a_final_newline_counts_its_last_line(path):
    assert [f.line_count for f in extract_file(path, "a\nb\nc").files] == [3]
