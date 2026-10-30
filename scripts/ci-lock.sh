#!/usr/bin/env bash
# Make .github/requirements/ci.txt again: the hashed lock of what the CI jobs
# install (the tools, and the packages of the product that the tests need),
# lens.txt (the lock of the lens tests), and the service image locks
# (geometric-lens/requirements-lock.txt and v3-service/requirements-lock.txt).
# Run it after a change to ci.in, lens.in, geometric-lens/requirements.txt or
# v3-service/requirements.txt. Needs uv, which gets Python 3.12 and pip-tools
# for the run.
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
# The lens lock is made by uv for the Linux runners of CI, whatever machine runs
# this: lens.in takes a package by platform (xgboost-cpu on Linux), and
# pip-compile would choose for this machine. It takes torch from PyTorch's
# index, so it reads two indexes.
uv pip compile --quiet --python-version 3.12 --python-platform x86_64-unknown-linux-gnu --generate-hashes \
    --emit-index-url --index-strategy unsafe-best-match --output-file=lens.txt lens.in

# Lock v3-service (Python 3.13) and geometric-lens (Python 3.11) Dockerfile requirements
uv pip compile --quiet --python-version 3.13 --python-platform x86_64-unknown-linux-gnu --generate-hashes \
    --output-file=../../v3-service/requirements-lock.txt ../../v3-service/requirements.txt
uv pip compile --quiet --python-version 3.11 --python-platform x86_64-unknown-linux-gnu --generate-hashes \
    --emit-index-url --index-url https://download.pytorch.org/whl/cpu --extra-index-url https://pypi.org/simple \
    --index-strategy unsafe-best-match --output-file=../../geometric-lens/requirements-lock.txt ../../geometric-lens/requirements.txt


