"""License-compatibility check for production Python dependencies.

Every dependency declared in the production requirements files must have
a KNOWN, allowlisted license (permissive or otherwise compatible with
the project's AGPL-3.0-or-later distribution). An unknown/unlisted
dependency fails the check so a new dep with an incompatible or
unvetted license is caught in CI before it ships.

Licenses are recorded in DEP_LICENSES (spot-checked against each
package's published metadata; see THIRD_PARTY_NOTICES.md). Parsing only
— installs nothing, reaches no network.
"""

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

REQUIREMENTS = [
    "geometric-lens/requirements.txt",
    "v3-service/requirements.txt",
    "sandbox/requirements-runtime.txt",
    "sandbox/requirements-verify.txt",
]

# Licenses considered compatible with AGPL-3.0-or-later distribution.
ALLOWED_LICENSES = {
    "MIT", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "PSF",
    "ISC", "MPL-2.0", "LGPL-3.0", "Python-2.0", "HPND",
}

# Declared license per production dependency (see THIRD_PARTY_NOTICES.md).
DEP_LICENSES = {
    "defusedxml": "PSF",
    "fastapi": "MIT",
    "gguf": "MIT",
    "httpx": "BSD-3-Clause",
    "jinja2": "BSD-3-Clause",
    "mypy": "MIT",
    "numpy": "BSD-3-Clause",
    "pydantic": "MIT",
    "pytest": "MIT",
    "pyyaml": "MIT",
    "requests": "Apache-2.0",
    "ruff": "MIT",
    "tiktoken": "MIT",
    "torch": "BSD-3-Clause",
    "tree-sitter": "MIT",
    "tree-sitter-go": "MIT",
    "tree-sitter-html": "MIT",
    "tree-sitter-javascript": "MIT",
    "tree-sitter-python": "MIT",
    "tree-sitter-typescript": "MIT",
    "uvicorn": "BSD-3-Clause",
    "xgboost": "Apache-2.0",
    "xgboost-cpu": "Apache-2.0",
}


def _declared_deps():
    names = set()
    for rel in REQUIREMENTS:
        for raw in (REPO / rel).read_text().splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or line.startswith("-"):
                continue
            name = re.split(r"[<>=;!\[ ]", line, 1)[0].strip().lower()
            if name:
                names.add(name)
    return names


def test_every_dep_has_a_known_allowed_license():
    problems = []
    for dep in sorted(_declared_deps()):
        lic = DEP_LICENSES.get(dep)
        if lic is None:
            problems.append(f"{dep}: license unknown — add to DEP_LICENSES "
                            f"after verifying it is compatible")
        elif lic not in ALLOWED_LICENSES:
            problems.append(f"{dep}: license {lic} not in the allowlist")
    assert not problems, "license-compatibility failures:\n  " + \
        "\n  ".join(problems)


def test_no_stale_license_entries():
    # A DEP_LICENSES entry for a dep no longer used is dead weight; keep
    # the map honest (only checks the extras we added, not transitives).
    declared = _declared_deps()
    stale = [d for d in DEP_LICENSES if d not in declared]
    assert not stale, f"stale DEP_LICENSES entries (dep removed?): {stale}"
