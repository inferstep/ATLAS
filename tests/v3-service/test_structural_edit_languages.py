"""structural_edit across Go, TypeScript and JavaScript.

v1 shipped Python and HTML. The engine is language-agnostic — tree-sitter
plus a selector mapping is all a language needs — and the gap was measured:
across 168 sessions, `unsupported file type for structural_edit` was the
only ATLAS-side wall a session hit, 12 times, when the model reached for it
on a .go file. The JavaScript grammar was already installed for
embedded_script_check and simply not wired into the dispatch.

The cases here are the ones that break a naive mapping: a Go method is a
different AST node than a func, and a TypeScript arrow function assigned to
a const is not a function_declaration at all. A model does not know which
spelling a file used, so one selector has to cover them.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import symbols

pytestmark = pytest.mark.skipif(
    not getattr(symbols, "_STRUCTURAL_EDIT_AVAILABLE", False),
    reason="tree-sitter not installed in this environment",
)

GO = """package main

import "fmt"

func Add(a, b int) int {
\treturn a + b
}

func (s *Server) Handle(w int) error {
\treturn nil
}

type Server struct {
\tport int
}
"""

TS = """export function render(x: number): string {
  return String(x);
}

const helper = (a: number) => a * 2;

const legacy = function (b: number) { return b; };

class Board {
  reset(): void {}
}
"""

JS = """function draw(ctx) {
  ctx.clear();
}

const tick = () => { step(); };

class Game {
  start() {}
}
"""


@pytest.mark.parametrize("path, src, selector", [
    pytest.param("main.go", GO, "function:Add", id="go-func"),
    pytest.param("main.go", GO, "function:Handle", id="go-method-is-a-different-node"),
    pytest.param("main.go", GO, "type:Server", id="go-type"),
    pytest.param("app.ts", TS, "function:render", id="ts-function-declaration"),
    pytest.param("app.ts", TS, "function:helper", id="ts-arrow-assigned-to-a-const"),
    pytest.param("app.ts", TS, "function:legacy", id="ts-function-expression"),
    pytest.param("app.ts", TS, "class:Board", id="ts-class"),
    pytest.param("app.tsx", TS, "function:render", id="tsx-function-declaration"),
    pytest.param("game.js", JS, "function:draw", id="js-function-declaration"),
    pytest.param("game.js", JS, "function:tick", id="js-arrow-assigned-to-a-const"),
    pytest.param("game.js", JS, "class:Game", id="js-class"),
])
def test_selector_resolves(path, src, selector):
    out = symbols.structural_edit(path, src, selector, "REPLACED\n")
    assert out.get("success"), out.get("error")
    assert "REPLACED" in out["new_content"]


@pytest.mark.parametrize("path, src, selector, error_mentions", [
    # The advertised list is built from the grammars that imported, so a
    # build missing one never offers it.
    pytest.param("main.rs", "fn main() {}\n", "function:main", [".go", ".py", "edit_file"],
                 id="a-language-without-a-grammar-says-what-is-supported"),
    pytest.param("main.go", GO, "<div>", ["function:NAME"],
                 id="an-unknown-selector-names-what-the-language-takes"),
    pytest.param("main.go", GO, "function:Missing", ["matched 0 nodes"],
                 id="a-missing-symbol"),
    # Single-match enforcement still applies: two nodes matching one selector
    # must fail rather than pick one.
    pytest.param("main.go", GO + "\nfunc Handle(x int) {}\n", "function:Handle", ["matched 2 nodes"],
                 id="a-go-method-and-func-of-the-same-name"),
])
def test_a_selector_that_cannot_apply_fails_and_says_why(path, src, selector, error_mentions):
    out = symbols.structural_edit(path, src, selector, "REPLACED\n")
    assert not out["success"]
    for text in error_mentions:
        assert text in out["error"]
