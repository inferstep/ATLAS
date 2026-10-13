#!/usr/bin/env python3
"""Build the tests of a Go module the way the test gate builds them, and run none.

A pull request job reads the Go build cache that a run on `dev` saved. On a
push to `dev` the tests do not run again, so a job fills the cache by
building them. The command is the test gate's own, read from
scripts/production-readiness.py, with a pattern added that fits no test. So
the build flags here cannot differ from the gate's, and what is cached is
what a pull request job looks for.

Usage: go_test_build.py proxy|tui
"""
from __future__ import annotations

import importlib.util
import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
# A pattern for `go test -run` that no test name fits.
NO_TEST = "^$"


def gate_script():
    """scripts/production-readiness.py as a module."""
    spec = importlib.util.spec_from_file_location("atlas_production_readiness", Path(__file__).with_name("production-readiness.py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def build_command(module: str, coverage_dir: str) -> tuple[list[str], Path, dict[str, str]]:
    """The gate's `go test` command for a module as CI runs it, with the pattern that fits no test; its folder
    and the variables it sets."""
    gates = gate_script()
    before = os.environ.get(gates.COVERAGE_DIR_ENV)
    os.environ[gates.COVERAGE_DIR_ENV] = coverage_dir
    try:
        gate = gates._with_coverage(gates._gates(()))[f"go-{module}-test"]  # the gate as a CI test job runs it
    finally:
        if before is None:
            os.environ.pop(gates.COVERAGE_DIR_ENV, None)
        else:
            os.environ[gates.COVERAGE_DIR_ENV] = before
    command = [part for part in gate.command if part != "-json"]
    if tuple(command[:2]) != ("go", "test") or command[-1] != "./...":
        raise ValueError(f"the gate go-{module}-test is not `go test ... ./...` any more: {command}")
    return command[:-1] + ["-run", NO_TEST, command[-1]], Path(gate.cwd), dict(gate.env or {})


def main(argv: list[str]) -> int:
    if len(argv) != 1 or argv[0] not in ("proxy", "tui"):
        print("usage: go_test_build.py proxy|tui", file=sys.stderr)
        return 2
    try:
        with tempfile.TemporaryDirectory() as coverage_dir:
            command, folder, env = build_command(argv[0], coverage_dir)
            print("go test build: " + " ".join(command), flush=True)
            done = subprocess.run(command, cwd=folder, env={**os.environ, **env}, check=False)
    except (KeyError, ValueError, OSError) as error:
        print(f"go test build: the test gate's command could not be read: {error!r}. Fix: bring this script in line "
              "with the gate go-<module>-test of scripts/production-readiness.py.", file=sys.stderr)
        return 2
    if done.returncode != 0:
        print(f"go test build: the tests of {argv[0]} did not build (status {done.returncode}), so the cache is not "
              "filled. Fix: read the compiler's lines above.", file=sys.stderr)
    return done.returncode


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
