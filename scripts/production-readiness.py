#!/usr/bin/env python3
"""Run ATLAS quality gates with consistent local and CI reporting."""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Callable, Dict, Optional, Sequence


ROOT = Path(__file__).resolve().parent.parent
PYTHON_TARGETS = (
    "atlas",
    "tests",
    "geometric-lens",
    "v3-service",
    "sandbox",
)
# Mirrors the CI python-tests matrix (.github/workflows/test.yml) plus the
# e2e-acceptance suite, so the documented local gate runs what CI gates on.
# tests/e2e skips cleanly when the proxy binary isn't built;
# tests/infrastructure's integration-marked tests are excluded by the
# repo-wide `-m 'not integration'` addopts.
PYTEST_PATHS = ("tests/v3", "tests/v3-service", "tests/cli",
                "tests/infrastructure", "tests/concurrency", "tests/perf",
                "tests/contracts", "tests/e2e")
# geometric-lens/tests runs as its own gate (python-tests-lens), matching
# its dedicated CI matrix leg: geometric-lens/ and v3-service/ both define
# top-level modules named `pipeline`/`main`, so the two trees cannot share
# one pytest process.
LENS_PYTEST_PATH = "geometric-lens/tests"


@dataclass(frozen=True)
class Gate:
    name: str
    command: tuple[str, ...]
    cwd: Path = ROOT
    required: bool = True
    available: Callable[[], bool] = lambda: True
    unavailable_reason: str = "required tool is not installed"
    env: Optional[Dict[str, str]] = None


@dataclass
class Result:
    name: str
    status: str
    required: bool
    duration_seconds: float
    command: list[str]
    output: str = ""
    reason: str = ""


def _module_available(name: str) -> bool:
    return importlib.util.find_spec(name) is not None


def _command_available(name: str) -> bool:
    return shutil.which(name) is not None


def _docker_compose_available() -> bool:
    if not _command_available("docker"):
        return False
    completed = subprocess.run(
        ["docker", "compose", "version"],
        cwd=ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )
    return completed.returncode == 0


def _compose_gates() -> dict[str, Gate]:
    """One validation gate per compose file combination we ship.

    The overlays (`-f base -f overlay`) are what real installs run —
    validating only the base file lets an overlay-only regression (bad
    `!reset`, dangling service key) through. Combinations whose files
    don't exist in this checkout are skipped.
    """
    combos: dict[str, tuple[str, ...]] = {
        "compose": ("docker-compose.yml",),
        "compose-rocm": ("docker-compose.yml", "docker-compose.rocm.yml"),
        "compose-vulkan": ("docker-compose.yml", "docker-compose.vulkan.yml"),
        "compose-cpu": (
            "docker-compose.yml",
            "docker-compose.vulkan.yml",
            "docker-compose.cpu.yml",
        ),
        "compose-macos": ("docker-compose.yml", "docker-compose.macos.yml"),
    }
    gates: dict[str, Gate] = {}
    for name, files in combos.items():
        if not all((ROOT / f).exists() for f in files):
            continue
        command: list[str] = ["docker", "compose"]
        for f in files:
            command += ["-f", f]
        command += ["config", "-q"]
        gates[name] = Gate(
            name,
            tuple(command),
            required=False,
            available=_docker_compose_available,
            unavailable_reason="Docker Compose v2 is not available",
            # These gates only validate YAML structure, not runtime config,
            # so they run with no .env present. Supply placeholders for the
            # required (:?) interpolation vars so parsing succeeds — real
            # users still get the :? error from `atlas init` / compose up
            # if unset.
            env={
                "ATLAS_MODEL_FILE": "placeholder.gguf",
                "ATLAS_MODEL_NAME": "placeholder",
            },
        )
    return gates


def _gates(pytest_paths: Sequence[str]) -> dict[str, Gate]:
    python = sys.executable
    go_env = {"GOCACHE": os.environ.get("GOCACHE", "/tmp/atlas-go-cache")}
    return {
        "test-integrity": Gate(
            "test-integrity",
            (python, "tests/validate_tests.py"),
        ),
        "python-compile": Gate(
            "python-compile",
            (python, "-m", "compileall", "-q", *PYTHON_TARGETS),
        ),
        # compileall proves the tree parses on the interpreter running CI.
        # It cannot see syntax that parses everywhere but is only *evaluated*
        # correctly on newer versions — a PEP 604 annotation is valid syntax
        # on 3.9 and raises TypeError at import. This compares the tree
        # against pyproject's declared requires-python instead.
        "min-python": Gate(
            "min-python",
            (python, "scripts/check_min_python.py"),
        ),
        # A COPY whose source was deleted breaks only the image build —
        # imports, tests, and lint all stay green, so nothing else sees it.
        "dockerfile-sources": Gate(
            "dockerfile-sources",
            (python, "scripts/check_dockerfile_sources.py"),
        ),
        "python-tests": Gate(
            "python-tests",
            (python, "-m", "pytest", *pytest_paths, "--no-header", "-q"),
            available=lambda: _module_available("pytest"),
            unavailable_reason="pytest is not installed",
        ),
        # Separate process on purpose — see the LENS_PYTEST_PATH comment.
        "python-tests-lens": Gate(
            "python-tests-lens",
            (python, "-m", "pytest", LENS_PYTEST_PATH, "--no-header", "-q"),
            available=lambda: (_module_available("pytest")
                               and _module_available("torch")),
            unavailable_reason="pytest/torch not installed "
                               "(pip install -r geometric-lens/requirements.txt)",
        ),
        # Candidate staging against a REAL executor process, not a stub.
        # Isolation, `finally` teardown and the before/after observation are
        # properties of the running executor, and a stub can only assert that
        # the contract was honoured, never that it is. Optional because it
        # needs uvicorn/fastapi and a free port; when those are missing it
        # reports `unavailable`, which the skip policy does not count as a
        # pass. `--only staging-executor` makes a missing dependency a failure.
        "staging-executor": Gate(
            "staging-executor",
            (python, "scripts/staging-integration.py"),
            required=False,
            available=lambda: (_command_available("go")
                               and _module_available("uvicorn")
                               and _module_available("fastapi")),
            unavailable_reason="Go, uvicorn or fastapi is not installed "
                               "(pip install -r sandbox/requirements.txt)",
            env=go_env,
        ),
        "go-proxy-test": Gate(
            "go-proxy-test",
            # An explicit budget, because Go's implicit 10m default is not one
            # this suite ever chose. proxy is a single package of ~1470 tests
            # that run serially under -race. It measured 1473s before the
            # slot-erase retry stopped waiting on a refused erase (every fake
            # llama-server here answers /slots with 404, and each agent loop
            # paid 1.5s per slot for four slots), and 279s after. 20m is over
            # four times that measurement, room for a runner slower than this
            # machine, and it still bounds a real hang inside one job. A CI
            # panic here should be read from its goroutine dump: one young
            # running test means budget, a test stuck for minutes means hang.
            ("go", "test", "-race", "-timeout", "20m", "./..."),
            cwd=ROOT / "proxy",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "go-tui-test": Gate(
            "go-tui-test",
            ("go", "test", "-race", "./..."),
            cwd=ROOT / "tui",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "go-proxy-vet": Gate(
            "go-proxy-vet",
            ("go", "vet", "./..."),
            cwd=ROOT / "proxy",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "go-tui-vet": Gate(
            "go-tui-vet",
            ("go", "vet", "./..."),
            cwd=ROOT / "tui",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "go-proxy-staticcheck": Gate(
            "go-proxy-staticcheck",
            ("go", "run", "honnef.co/go/tools/cmd/staticcheck@2026.1",
             "./..."),
            cwd=ROOT / "proxy",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "mypy-typed": Gate(
            "mypy-typed",
            (
                python, "-m", "mypy",
                "--ignore-missing-imports", "--no-error-summary",
                "--follow-imports=skip",
                "atlas/config_schema.py",
                "atlas/upgrade_engine.py",
                "atlas/artifact_manifest.py",
                "tests/perf/harness.py",
                "geometric-lens/geometric_lens/provenance.py",
            ),
            cwd=ROOT,
            available=lambda: _module_available("mypy"),
            unavailable_reason="mypy is not installed",
        ),
        "go-tui-staticcheck": Gate(
            "go-tui-staticcheck",
            ("go", "run", "honnef.co/go/tools/cmd/staticcheck@2026.1",
             "./..."),
            cwd=ROOT / "tui",
            available=lambda: _command_available("go"),
            unavailable_reason="Go is not installed",
            env=go_env,
        ),
        "ruff": Gate(
            "ruff",
            (
                python,
                "-m",
                "ruff",
                "check",
                "--select",
                "F",
                "--ignore",
                "F401,F541,F841",
                *PYTHON_TARGETS,
            ),
            required=False,
            available=lambda: _module_available("ruff"),
            unavailable_reason="ruff is not installed",
        ),
        "shellcheck": Gate(
            "shellcheck",
            (
                "shellcheck",
                "--severity=error",
                *sorted(str(p.relative_to(ROOT)) for p in (ROOT / "scripts").glob("*.sh")),
            ),
            required=False,
            available=lambda: _command_available("shellcheck"),
            unavailable_reason="shellcheck is not installed",
        ),
        **_compose_gates(),
        "workflow-yaml": Gate(
            "workflow-yaml",
            (
                python,
                "-m",
                "yamllint",
                "-d",
                "{rules: {line-length: disable, document-start: disable}}",
                ".github/workflows/",
            ),
            required=False,
            available=lambda: _module_available("yamllint"),
            unavailable_reason="yamllint is not installed",
        ),
    }


def _run_gate(gate: Gate, force_required: bool) -> Result:
    required = gate.required or force_required
    if not gate.available():
        return Result(
            name=gate.name,
            status="unavailable",
            required=required,
            duration_seconds=0.0,
            command=list(gate.command),
            reason=gate.unavailable_reason,
        )

    env = os.environ.copy()
    if gate.env:
        env.update(gate.env)
    start = time.monotonic()
    completed = subprocess.run(
        gate.command,
        cwd=gate.cwd,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    duration = time.monotonic() - start
    output = completed.stdout.rstrip()
    return Result(
        name=gate.name,
        status="passed" if completed.returncode == 0 else "failed",
        required=required,
        duration_seconds=round(duration, 3),
        command=list(gate.command),
        output=output,
        reason="" if completed.returncode == 0 else f"exit code {completed.returncode}",
    )


def _print_human(results: Sequence[Result]) -> None:
    labels = {"passed": "PASS", "failed": "FAIL", "unavailable": "UNAVAILABLE"}
    for result in results:
        requirement = "required" if result.required else "optional"
        print(
            f"[{labels[result.status]:11}] {result.name} "
            f"({requirement}, {result.duration_seconds:.3f}s)"
        )
        if result.reason:
            print(f"  {result.reason}")
        if result.status == "failed" and result.output:
            print(result.output)

    counts = {
        status: sum(result.status == status for result in results)
        for status in ("passed", "failed", "unavailable")
    }
    print(
        "\nSummary: "
        f"{counts['passed']} passed, {counts['failed']} failed, "
        f"{counts['unavailable']} unavailable"
    )


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Run ATLAS production-readiness quality gates."
    )
    parser.add_argument(
        "--only",
        action="append",
        default=[],
        metavar="GATE",
        help="run one gate; repeat to run several (selected gates are required)",
    )
    parser.add_argument(
        "--pytest-path",
        action="append",
        default=[],
        metavar="PATH",
        help="override the default hermetic pytest subsets",
    )
    parser.add_argument("--json", action="store_true", help="emit JSON")
    parser.add_argument("--list", action="store_true", help="list gate names")
    return parser.parse_args()


def main() -> int:
    args = _parse_args()
    gates = _gates(args.pytest_path or PYTEST_PATHS)
    if args.list:
        for name, gate in gates.items():
            print(f"{name}\t{'required' if gate.required else 'optional'}")
        return 0

    unknown = sorted(set(args.only) - set(gates))
    if unknown:
        print(f"unknown gate(s): {', '.join(unknown)}", file=sys.stderr)
        return 2

    selected = args.only or list(gates)
    results = [_run_gate(gates[name], force_required=bool(args.only)) for name in selected]
    if args.json:
        print(json.dumps({"results": [asdict(result) for result in results]}, indent=2))
    else:
        _print_human(results)

    return int(
        any(result.status == "failed" for result in results)
        or any(result.required and result.status == "unavailable" for result in results)
    )


if __name__ == "__main__":
    raise SystemExit(main())
