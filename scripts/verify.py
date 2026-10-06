#!/usr/bin/env python3
"""Run the checks a change needs and print only what failed, with the fix.

`make verify` runs this before a push. It compares HEAD and the working tree
with the merge base of the base branch, picks the quality gates that cover
the changed files, and runs them through scripts/production-readiness.py, so
a check here is the check CI runs. `--full` adds the slow suites.

Exit status: 0 when every check that ran passed, 1 when one failed, 2 when
the change could not be read.
"""

from __future__ import annotations

import argparse
import importlib.util
import re
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
COMPOSE_GATES = ("compose", "compose-rocm", "compose-vulkan", "compose-cpu", "compose-macos")
PROXY = "proxy/"
INFRASTRUCTURE_SUITE = "tests/infrastructure"

# Which gates of scripts/production-readiness.py cover which changed files.
# The two under "always" take a fraction of a second and guard the tree as a whole.
GATES_BY_PATH = (
    (lambda p: True, ("test-integrity", "dockerfile-sources")),
    (lambda p: p.startswith(PROXY), ("go-proxy-vet", "go-proxy-staticcheck")),
    (lambda p: p.startswith("tui/"), ("go-tui-vet", "go-tui-staticcheck", "go-tui-test")),
    (lambda p: p.endswith(".py"), ("python-compile", "min-python", "ruff")),
    (lambda p: p.endswith(".sh"), ("shellcheck",)),
    (lambda p: p.startswith(".github/workflows/"), ("workflow-yaml",)),
    (lambda p: "docker-compose" in p, COMPOSE_GATES),
)
SLOW_GATES_BY_PATH = ((lambda p: p.startswith(PROXY), ("go-proxy-test",)),)
# The pytest suites that cover a source folder. Slow suites run with --full.
SUITES_BY_PATH = (
    ("atlas/", ("tests/cli", "tests/contracts")),
    ("v3-service/", ("tests/v3-service", "tests/v3")),
    ("sandbox/", (INFRASTRUCTURE_SUITE,)),
    ("scripts/", (INFRASTRUCTURE_SUITE,)),
    (PROXY, ("tests/replay", "tests/e2e")),
)
SLOW_SUITES = (INFRASTRUCTURE_SUITE, "tests/e2e")
# A file that sends a request to the agent must be on this test's list of
# senders, with the task mode it declares.
AGENT_ENDPOINT = "/v1/agent"
SENDER_CONTRACT = "tests/contracts/test_api_version_contract.py"

FIXES = {
    "go-proxy-vet": "Fix the line `go vet` names (run it in proxy/).",
    "go-tui-vet": "Fix the line `go vet` names (run it in tui/).",
    "go-proxy-staticcheck": "Fix the finding at the line shown. Its code (for example SA4006) is explained by `staticcheck -explain <code>`.",
    "go-tui-staticcheck": "Fix the finding at the line shown. Its code (for example SA4006) is explained by `staticcheck -explain <code>`.",
    "go-proxy-test": "Run `go test -run '<TestName>' .` in proxy/ for a test that failed, then fix the code or the test.",
    "go-tui-test": "Run `go test -run '<TestName>' ./...` in tui/ for a test that failed, then fix the code or the test.",
    "go-proxy-changed-tests": "Run `go test -run '<TestName>' .` in proxy/ for a test that failed, then fix the code or the test.",
    "python-compile": "A file does not compile. Fix the syntax error at the line shown.",
    "min-python": "The code uses a form Python 3.9 cannot run. Write it in a form 3.9 accepts.",
    "ruff": "Run `python -m ruff check <file>` and remove the undefined or redefined name it reports.",
    "test-integrity": "Fix what tests/validate_tests.py reports about the test files.",
    "python-tests": "Run `python -m pytest <path> -x` for the first failure, then fix the code or the test.",
    "python-tests-lens": "Run `python -m pytest geometric-lens/tests -x` for the first failure, then fix the code or the test.",
    "shellcheck": "Fix the line shellcheck names. Its code (SCnnnn) is explained at https://www.shellcheck.net/wiki/.",
    "workflow-yaml": "Fix the YAML at the line shown.",
    "dockerfile-sources": "A Dockerfile copies a path that is not there. Fix the COPY line or add the file.",
    "code-health": "Split the function or file it names (docs/CODE_STYLE.md). If something got smaller, run `python scripts/code_health.py --update` and commit the baseline.",
}
FIXES.update(dict.fromkeys(COMPOSE_GATES, "Run `docker compose -f <files> config` and fix the key it rejects."))
FAILING_LINE = re.compile(r"FAIL|[Ee]rror|panic:|\.(?:go|py|sh|yml|ts):\d+|^E {2}")


def load_gates_module():
    spec = importlib.util.spec_from_file_location("production_readiness", ROOT / "scripts" / "production-readiness.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # its dataclasses resolve annotations through it
    spec.loader.exec_module(module)
    return module


def git(*args: str) -> str:
    done = subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise RuntimeError(done.stderr.strip() or f"git {args[0]} failed")
    return done.stdout


def changed_files(base: str) -> tuple[str, list[str]]:
    """The merge base with `base`, and every file that differs from it or is new."""
    merge_base = git("merge-base", "--end-of-options", base, "HEAD").strip()
    names = git("diff", "--name-only", merge_base).splitlines()
    names += git("ls-files", "--others", "--exclude-standard").splitlines()
    return merge_base, sorted(set(names))


def select(changed: list[str], full: bool) -> tuple[list[str], list[str], bool]:
    """Gate names, pytest paths and whether the lens suite runs, for these files."""
    rules = GATES_BY_PATH + (SLOW_GATES_BY_PATH if full else ())
    gates = [name for covers, names in rules if any(map(covers, changed)) for name in names]
    suites = [suite for prefix, group in SUITES_BY_PATH if any(p.startswith(prefix) for p in changed)
              for suite in group if full or suite not in SLOW_SUITES]
    # A changed test file is run itself, whatever its suite.
    suites += [p for p in changed if p.startswith("tests/") and Path(p).name.startswith("test_")
               and (ROOT / p).is_file() and not p.startswith(tuple(suites))]
    if any(sends_to_the_agent(p) for p in changed) and not SENDER_CONTRACT.startswith(tuple(suites)):
        suites.append(SENDER_CONTRACT)
    lens = any(p.startswith("geometric-lens/") for p in changed)
    return list(dict.fromkeys(gates)), list(dict.fromkeys(suites)), lens


def sends_to_the_agent(path: str) -> bool:
    """Whether a changed Python file names the agent endpoint, as a sender of requests does."""
    file = ROOT / path
    return path.endswith(".py") and file.is_file() and AGENT_ENDPOINT in file.read_text(encoding="utf-8", errors="replace")


def changed_go_tests(changed: list[str]) -> str:
    """A -run pattern for the tests defined in the proxy test files that changed."""
    names = []
    for path in changed:
        if path.startswith(PROXY) and path.endswith("_test.go") and (ROOT / path).is_file():
            names += re.findall(r"^func (Test\w+)\(", (ROOT / path).read_text(encoding="utf-8"), re.M)
    return "^(" + "|".join(sorted(set(names))) + ")$" if names else ""


def failing_lines(output: str, limit: int = 40) -> list[str]:
    """A short output whole; of a long one, the lines that name a failure."""
    lines = output.splitlines()
    if len(lines) <= limit:
        return lines
    named = [line for line in lines if FAILING_LINE.search(line)]
    return (named or lines[-20:])[:limit]


def advisory(changed: list[str], base: str) -> list[str]:
    """Output of the checks that report and do not fail, as CI runs them."""
    out = []
    script = ROOT / "scripts" / "integrity_check.py"
    if script.is_file():
        done = subprocess.run([sys.executable, str(script), "--base", base], cwd=ROOT, capture_output=True, text=True, check=False)
        if "nothing found" not in done.stdout:
            out += ["note integrity check (reports, does not fail):"] + ["  " + line for line in done.stdout.splitlines()]
    modules = [m for m in ("proxy", "tui") if any(p.startswith(m + "/") for p in changed)]
    if not modules or not (ROOT / ".golangci.yml").is_file():
        return out
    if not shutil.which("golangci-lint"):
        return out + ["skip golangci-lint: it is not installed (https://golangci-lint.run/welcome/install/)"]
    for module in modules:
        done = subprocess.run(["golangci-lint", "run", "./...", f"--new-from-merge-base={base}", "--issues-exit-code=0"],
                              cwd=ROOT / module, capture_output=True, text=True, check=False)
        if "0 issues" not in done.stdout:
            out += [f"note golangci-lint in {module}/ (reports, does not fail):"]
            out += ["  " + line for line in done.stdout.splitlines()]
    return out


def gates_to_run(pr, changed: list[str], full: bool) -> list:
    """The gate objects for this change, in the order they run."""
    names, suites, lens = select(changed, full)
    gates = pr._gates(suites or ("tests/contracts",))
    run = [gates[name] for name in names]
    if suites:
        run.append(gates["python-tests"])
    if lens:
        run.append(gates["python-tests-lens"])
    pattern = changed_go_tests(changed)
    if pattern and "go-proxy-test" not in names:
        run.append(pr.Gate("go-proxy-changed-tests", ("go", "test", "-count=1", "-run", pattern, "."),
                           cwd=ROOT / "proxy", available=lambda: shutil.which("go") is not None,
                           unavailable_reason="Go is not installed", env=gates["go-proxy-vet"].env))
    if any(p.endswith((".go", ".py")) for p in changed):
        run.append(pr.Gate("code-health", (sys.executable, "scripts/code_health.py")))
    return run


def run_gates(pr, gates: list) -> int:
    """Run each gate; print what failed with its fix. Returns the number that failed."""
    passed, failed = [], 0
    for gate in gates:
        result = pr._run_gate(gate, force_required=False)
        if result.status == "passed":
            passed.append(gate.name)
        elif result.status == "unavailable":
            print(f"skip {gate.name}: {result.reason}")
        else:
            failed += 1
            print(f"FAIL {gate.name}")
            print("\n".join("  " + line for line in failing_lines(result.output)))
            print(f"  fix: {FIXES[gate.name]}")
    if passed:
        print("ok   " + ", ".join(passed))
    return failed


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="origin/dev", help="branch or commit the change is measured against")
    parser.add_argument("--full", action="store_true", help="also run the slow suites")
    args = parser.parse_args()
    try:
        merge_base, changed = changed_files(args.base)
    except RuntimeError as error:
        print(f"verify: cannot read the change against {args.base!r}: {error}\n"
              "  fix: fetch the base branch (git fetch origin) or pass --base <commit>.", file=sys.stderr)
        return 2
    if not changed:
        print(f"verify: nothing differs from {args.base}")
        return 0
    print(f"verify: {len(changed)} changed file(s) against {args.base} (merge base {merge_base[:7]})")
    pr = load_gates_module()
    failed = run_gates(pr, gates_to_run(pr, changed, args.full))
    print("\n".join(advisory(changed, args.base)) or "ok   advisory checks: nothing to report")
    if not args.full:
        print("note the slow suites (the whole proxy suite, tests/infrastructure, tests/e2e) run in CI; "
              "`make verify-full` runs them here.")
    print(f"verify: {failed} check(s) failed" if failed else "verify: all checks that ran passed")
    return int(failed > 0)


if __name__ == "__main__":
    raise SystemExit(main())
