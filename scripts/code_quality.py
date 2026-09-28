#!/usr/bin/env python3
"""Objective code-quality probes for code an agent wrote.

Correctness is table stakes: any model can produce something that runs. The
question this answers is whether what it produced is code you would keep —
best practices followed, complexity kept down, a multi-file program actually
split into modules rather than one file pretending to be several.

Everything here is measured, not judged. No model scores another model's
output: each probe is a parse, a count, or a linter's verdict, so the number
means the same thing on every run and can be compared across builds.

Used by scripts/e2e-reliability.py, and runnable on its own:

    python scripts/code_quality.py <dir> [--baseline f1.py,f2.py]

`--baseline` names files that were fixtures rather than agent output, so the
report can separate what the agent wrote from what it was given.
"""
from __future__ import annotations

import argparse
import ast
import json
import shutil
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

# Thresholds. Deliberately loose — these flag code that is hard to maintain,
# not code that departs from a style preference. A function at 11 branches is
# fine; one at 25 is a rewrite waiting to happen.
MAX_COMPLEXITY = 15
MAX_FUNCTION_LINES = 80
MAX_FILE_LINES = 400


@dataclass
class QualityReport:
    files: int = 0
    total_lines: int = 0
    lint_defects: int = 0        # pyflakes F-codes + E9: real bugs
    lint_style: int = 0          # whitespace/length: cosmetic
    lint_available: bool = False
    unused_imports: int = 0
    defect_codes: list[str] = field(default_factory=list)
    max_complexity: int = 0
    max_complexity_where: str = ""
    max_function_lines: int = 0
    max_function_where: str = ""
    max_file_lines: int = 0
    max_file_where: str = ""
    syntax_errors: list[str] = field(default_factory=list)
    # Files that parse under the target interpreter but not under this
    # script's own, so no AST metrics could be taken for them.
    unmeasured_files: list[str] = field(default_factory=list)
    parser: str = ""
    findings: list[str] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "files": self.files,
            "total_lines": self.total_lines,
            "lint_defects": self.lint_defects,
            "lint_style": self.lint_style,
            "lint_available": self.lint_available,
            "unused_imports": self.unused_imports,
            "defect_codes": self.defect_codes,
            "max_complexity": self.max_complexity,
            "max_complexity_where": self.max_complexity_where,
            "max_function_lines": self.max_function_lines,
            "max_function_where": self.max_function_where,
            "max_file_lines": self.max_file_lines,
            "max_file_where": self.max_file_where,
            "syntax_errors": self.syntax_errors,
            "unmeasured_files": self.unmeasured_files,
            "parser": self.parser,
            "findings": self.findings,
        }


def cyclomatic_complexity(fn: ast.AST) -> int:
    """Branch count + 1 — the standard McCabe measure.

    Counts the constructs that actually create a path: conditionals, loops,
    each `except`, each boolean operand beyond the first, comprehension
    filters, `with`/`assert` guards are not paths and are excluded. Matching a
    tool exactly matters less than being consistent, since the number is only
    ever compared against itself across runs.
    """
    score = 1
    for node in ast.walk(fn):
        if isinstance(node, (ast.If, ast.For, ast.AsyncFor, ast.While,
                             ast.ExceptHandler, ast.IfExp)):
            score += 1
        elif isinstance(node, ast.BoolOp):
            score += len(node.values) - 1
        elif isinstance(node, ast.comprehension):
            score += 1 + len(node.ifs)
        elif isinstance(node, ast.Match) if hasattr(ast, "Match") else False:
            score += 1
    return score


def _iter_python(root: Path, skip: set[str]) -> list[Path]:
    out = []
    for p in sorted(root.rglob("*.py")):
        if any(part in ("__pycache__", ".git", ".venv", "node_modules")
               for part in p.parts):
            continue
        if p.name in skip:
            continue
        out.append(p)
    return out


def host_parser_label() -> str:
    return f"host python {sys.version_info.major}.{sys.version_info.minor}"


def analyze(root: Path, baseline: set[str] | None = None,
            target_parse=None, target_label: str = "") -> QualityReport:
    """`target_parse(src) -> (ok, message)` asks the interpreter the code will
    run under; ok is None when it cannot be reached. Without it, or when it is
    unreachable, the verdict is this script's own interpreter and says so.

    Evaluator v2 (2026-09-17). v1 parsed with the host interpreter only, which
    is Python 3.9 on the dev server while the sandbox runs 3.13: PEP 701
    f-strings such as f"{d["k"]}" were counted unparseable although they run.
    """
    rep = QualityReport()
    rep.parser = target_label if target_parse else host_parser_label()
    skip = baseline or set()
    files = _iter_python(root, skip)
    rep.files = len(files)

    for path in files:
        try:
            src = path.read_text()
        except (OSError, UnicodeDecodeError):
            continue
        lines = src.count("\n") + 1
        rep.total_lines += lines
        if lines > rep.max_file_lines:
            rep.max_file_lines, rep.max_file_where = lines, path.name
        verdict = None
        if target_parse is not None:
            verdict, why = target_parse(src)
            if verdict is False:
                rep.syntax_errors.append(f"{path.name}: {why}")
                continue
            if verdict is None:
                rep.parser = f"{host_parser_label()} (target unreachable)"
        try:
            tree = ast.parse(src)
        except SyntaxError as e:
            if verdict is True:
                rep.unmeasured_files.append(path.name)
            else:
                rep.syntax_errors.append(f"{path.name}: {e.msg} (line {e.lineno})")
            continue
        for node in ast.walk(tree):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            cx = cyclomatic_complexity(node)
            if cx > rep.max_complexity:
                rep.max_complexity = cx
                rep.max_complexity_where = f"{path.name}:{node.name}"
            end = getattr(node, "end_lineno", node.lineno) or node.lineno
            length = end - node.lineno + 1
            if length > rep.max_function_lines:
                rep.max_function_lines = length
                rep.max_function_where = f"{path.name}:{node.name}"

    ruff = shutil.which("ruff")
    if ruff and files:
        rep.lint_available = True
        proc = subprocess.run(
            [ruff, "check", "--output-format", "json", "--isolated",
             "--select", "E,F,W", *[str(p) for p in files]],
            capture_output=True, text=True, timeout=120)
        try:
            issues = json.loads(proc.stdout or "[]")
        except json.JSONDecodeError:
            issues = []
        # Split real defects from cosmetics. An earlier version reported one
        # number, and on a sample run 31 of 34 "violations" were trailing
        # whitespace while the single genuine finding — an undefined name —
        # was lost in it. A score dominated by whitespace drives the wrong
        # work, so F-codes (pyflakes: undefined names, unused names,
        # redefinitions) and E9 (syntax) are counted apart from W/E501.
        for i in issues:
            code = str(i.get("code") or "")
            if code.startswith("F") or code.startswith("E9"):
                rep.lint_defects += 1
                if code not in rep.defect_codes:
                    rep.defect_codes.append(code)
            else:
                rep.lint_style += 1
        rep.unused_imports = sum(1 for i in issues
                                 if str(i.get("code", "")).startswith("F401"))

    if rep.syntax_errors:
        rep.findings.append(f"{len(rep.syntax_errors)} file(s) do not parse")
    if rep.max_complexity > MAX_COMPLEXITY:
        rep.findings.append(
            f"complexity {rep.max_complexity} in {rep.max_complexity_where} "
            f"(over {MAX_COMPLEXITY})")
    if rep.max_function_lines > MAX_FUNCTION_LINES:
        rep.findings.append(
            f"{rep.max_function_where} is {rep.max_function_lines} lines "
            f"(over {MAX_FUNCTION_LINES})")
    if rep.max_file_lines > MAX_FILE_LINES:
        rep.findings.append(
            f"{rep.max_file_where} is {rep.max_file_lines} lines "
            f"(over {MAX_FILE_LINES})")
    if rep.unused_imports:
        rep.findings.append(f"{rep.unused_imports} unused import(s)")
    if rep.lint_defects:
        rep.findings.append(
            f"{rep.lint_defects} real lint defect(s) "
            f"[{', '.join(sorted(rep.defect_codes))}]")
    # Style nits are reported but are NOT a finding: trailing whitespace says
    # nothing about whether the code is any good.
    return rep


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("directory")
    ap.add_argument("--baseline", default="",
                    help="comma-separated filenames that were fixtures, not "
                         "agent output")
    ap.add_argument("--json", dest="json_out", default="")
    args = ap.parse_args()

    root = Path(args.directory)
    if not root.is_dir():
        print(f"error: {root} is not a directory", file=sys.stderr)
        return 2
    baseline = {b.strip() for b in args.baseline.split(",") if b.strip()}
    rep = analyze(root, baseline)

    print(f"files {rep.files}   lines {rep.total_lines}   "
          f"max complexity {rep.max_complexity}"
          + (f" ({rep.max_complexity_where})" if rep.max_complexity_where else ""))
    if rep.lint_available:
        print(f"real defects {rep.lint_defects}"
              + (f" {sorted(rep.defect_codes)}" if rep.defect_codes else "")
              + f"   unused imports {rep.unused_imports}"
              f"   style nits {rep.lint_style}")
    else:
        print("lint: ruff unavailable — skipped")
    if rep.findings:
        print("\nfindings:")
        for f in rep.findings:
            print(f"  - {f}")
    else:
        print("\nno quality findings")
    if args.json_out:
        Path(args.json_out).write_text(json.dumps(rep.as_dict(), indent=2) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
