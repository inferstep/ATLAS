#!/usr/bin/env bash
# Regenerate .github/requirements/ci.txt, the hashed lock of the tools the
# CI jobs install (pytest, pyyaml, ruff, mypy, yamllint). Needs uv.
set -euo pipefail
cd "$(dirname "$0")/.."
uv pip compile --quiet --generate-hashes --python-version 3.12 \
    --custom-compile-command "scripts/ci-lock.sh" \
    .github/requirements/ci.in -o .github/requirements/ci.txt
