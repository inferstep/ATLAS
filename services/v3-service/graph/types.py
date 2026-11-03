"""Call-graph data model.

Faithful port of the CodeGraph fact model from chiasmus `src/graph/types.ts`.
Flat fact tuples (defines / calls / imports / exports / contains) so the graph
is language-agnostic and translates directly to Prolog facts (see facts.py).

TS/JS-only machinery from the original (qualified-name resolution via receiver
chains, hyperedges) is intentionally omitted from this Python-first port; it can
be added with the relevant languages later (issue #39, Phase 6).
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import List, Optional

# function | method | class | interface | variable
SymbolKind = str


@dataclass
class DefinesFact:
    file: str
    name: str
    kind: SymbolKind
    line: int
    signature: Optional[str] = None


@dataclass
class CallsFact:
    caller: str
    callee: str
    callee_qn: Optional[str] = None  # reserved for languages with QN resolution


@dataclass
class ImportsFact:
    file: str
    name: str
    source: str  # raw specifier as written (module path / dotted name)
    resolved: Optional[str] = None  # canonical in-batch file path when resolved


@dataclass
class ExportsFact:
    file: str
    name: str


@dataclass
class ContainsFact:
    parent: str
    child: str


@dataclass
class FileNode:
    path: str
    language: str
    line_count: Optional[int] = None


@dataclass
class CodeGraph:
    defines: List[DefinesFact] = field(default_factory=list)
    calls: List[CallsFact] = field(default_factory=list)
    imports: List[ImportsFact] = field(default_factory=list)
    exports: List[ExportsFact] = field(default_factory=list)
    contains: List[ContainsFact] = field(default_factory=list)
    files: List[FileNode] = field(default_factory=list)

    def merge(self, other: "CodeGraph") -> None:
        self.defines.extend(other.defines)
        self.calls.extend(other.calls)
        self.imports.extend(other.imports)
        self.exports.extend(other.exports)
        self.contains.extend(other.contains)
        self.files.extend(other.files)

    def to_dict(self) -> dict:
        return {
            "defines": [
                {"file": d.file, "name": d.name, "kind": d.kind, "line": d.line,
                 "signature": d.signature}
                for d in self.defines
            ],
            "calls": [
                {"caller": c.caller, "callee": c.callee, "calleeQN": c.callee_qn}
                for c in self.calls
            ],
            "imports": [
                {"file": i.file, "name": i.name, "source": i.source, "resolved": i.resolved}
                for i in self.imports
            ],
            "exports": [{"file": e.file, "name": e.name} for e in self.exports],
            "contains": [{"parent": c.parent, "child": c.child} for c in self.contains],
            "files": [{"path": f.path, "language": f.language, "lineCount": f.line_count}
                      for f in self.files],
        }
