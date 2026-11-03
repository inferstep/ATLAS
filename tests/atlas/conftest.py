"""Hermetic guard for the CLI unit tests.

CI checks out a clean repo with no ``.env``, but a developer's working tree
almost always has one (from ``atlas init`` / ``cp .env.example .env``). Code or
scripts that read the repo-root ``.env`` then pass locally and fail in CI. This
has bitten twice: the macOS launcher hard-failing when ``.env`` was absent, and
``docker compose config`` choking on a required ``${VAR:?}`` that ``.env`` had
been quietly supplying.

Move any repo-root ``.env`` aside for the duration of the CLI test package so
local runs match CI, then restore it. Tests that need ``.env`` values must
create their own (``tmp_path / ".env"``), which the rest of this suite already
does. The backup name matches the ``.env.*`` ``.gitignore`` rule, so it is
never committed, and an interrupted run is recovered on the next start.
"""

import pathlib

import pytest

_REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
_ENV = _REPO_ROOT / ".env"
_HIDDEN = _REPO_ROOT / ".env.pytest-hidden"  # matches .env.* in .gitignore


@pytest.fixture(scope="package", autouse=True)
def _hide_repo_env():
    # Recover a backup left behind by an interrupted earlier run.
    if _HIDDEN.exists() and not _ENV.exists():
        _HIDDEN.rename(_ENV)

    hidden = False
    if _ENV.exists():
        _ENV.rename(_HIDDEN)
        hidden = True
    try:
        yield
    finally:
        if hidden and _HIDDEN.exists():
            _HIDDEN.rename(_ENV)
