#!/usr/bin/env bash
# Make .github/requirements/ci.txt again: the hashed lock of what the CI jobs
# install (the tools, and the packages of the product that the tests need).
# Run it after a change to ci.in. Needs uv, which gets Python 3.12 and
# pip-tools for the run.
#
# The lock is made by pip-compile, under the Python version of the CI jobs,
# and its header names both. Dependabot reads that header: it compiles the
# file again with the same Python version and the same options when it moves
# a pin, so the tool and its version here are the ones Dependabot runs.
set -euo pipefail
cd "$(dirname "$0")/../.github/requirements"
# click is held at 8.2.1: with click 8.5 this pip-tools writes an option into
# the header that was not given (`--no-index`), and the header must say the
# command as it was run. `--no-build`: uv takes pip-tools and what it needs as
# built packages only, so getting the tool runs no package's build script.
uv tool run --no-build --python 3.12 --from 'pip-tools==7.6.1' --with 'click==8.2.1' \
    pip-compile --quiet --generate-hashes --output-file=ci.txt ci.in
