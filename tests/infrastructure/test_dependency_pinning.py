"""Reproducible-build check: production dependencies must record an
exact version.

Scans the production requirements files and the inline `pip install`
lines in the production Dockerfiles; every installed Python package must
carry an `==` pin. The proxy's Alpine `apk` packages are pinned too.
OS-level `apt` packages on the debian-slim images are not individually
pinned (that fights distro security patching); the digest-pinned base is
their reproducibility control, enforced here and explained in
docs/CONTAINER_PACKAGING.md.

The check is configuration parsing only — it installs nothing and
reaches no network.
"""

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]

# Production Python requirements — every entry must be == pinned.
REQUIREMENTS_FILES = [
    "geometric-lens/requirements.txt",
    "sandbox/requirements-runtime.txt",
    "sandbox/requirements-verify.txt",
    "v3-service/requirements.txt",
]

# Production Dockerfiles scanned for inline `pip install <pkg>`.
DOCKERFILES = [
    "proxy/Dockerfile",
    "v3-service/Dockerfile",
    "geometric-lens/Dockerfile",
    "sandbox/Dockerfile",
]

def _requirement_lines(path: Path):
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if line.startswith(("-r", "-e", "--")):
            continue
        # strip an inline comment
        line = line.split("#", 1)[0].strip()
        # strip an environment marker (pinned form: pkg==x ; marker)
        pkg_spec = line.split(";", 1)[0].strip()
        yield line, pkg_spec


def test_requirements_files_are_fully_pinned():
    offenders = []
    for rel in REQUIREMENTS_FILES:
        path = REPO / rel
        assert path.exists(), f"missing requirements file: {rel}"
        for line, pkg_spec in _requirement_lines(path):
            if "==" not in pkg_spec:
                offenders.append(f"{rel}: {line!r}")
    assert not offenders, (
        "production requirements without an exact (==) version:\n  "
        + "\n  ".join(offenders))


# `pip install` package tokens: skip flags, flag values, requirement
# files, and URLs. What remains is a package spec that must be == pinned.
def _strip_and_join(dockerfile_text: str):
    """Return logical lines with per-line inline comments removed before
    backslash-continuations are joined (so an inline `# comment` inside a
    multi-line RUN block can't hide the tokens that follow it)."""
    out = []
    for phys in dockerfile_text.splitlines():
        # drop a trailing inline comment but keep a line-continuation
        cont = phys.rstrip().endswith("\\")
        body = phys[:-1] if cont else phys
        if "#" in body:
            body = body.split("#", 1)[0]
        out.append(body + ("\\" if cont else ""))
    return "\n".join(out).replace("\\\n", " ")


def _pip_packages(dockerfile_text: str):
    """Yield bare package specs from every `pip install` invocation.

    A logical line can chain several commands with && / ; / || (and more
    than one may be `pip install`), so split into commands first, then
    extract packages from each pip-install command."""
    text = _strip_and_join(dockerfile_text)
    for command in re.split(r"&&|\|\||;|\n", text):
        if "pip install" not in command:
            continue
        rest = command.split("pip install", 1)[1]
        tokens = rest.split()
        skip_next = False
        for tok in tokens:
            if skip_next:
                skip_next = False
                continue
            if tok in ("--index-url", "--extra-index-url", "-i",
                       "-r", "--requirement", "-c", "--constraint"):
                skip_next = True  # this flag consumes the next token
                continue
            if tok.startswith("-"):
                continue
            if tok.startswith(("http://", "https://", "/", "$")):
                continue
            if tok in ("pip", "install"):
                continue
            yield tok


def test_dockerfile_inline_pip_is_pinned():
    offenders = []
    for rel in DOCKERFILES:
        path = REPO / rel
        assert path.exists(), f"missing Dockerfile: {rel}"
        for spec in _pip_packages(path.read_text()):
            if "==" not in spec:
                offenders.append(f"{rel}: pip install {spec!r}")
    assert not offenders, (
        "inline `pip install` without an exact (==) version "
        "(move to a pinned requirements file):\n  " + "\n  ".join(offenders))


def test_production_bases_are_digest_pinned():
    """OS-layer reproducibility control: every production Dockerfile's
    FROM must be digest-pinned (@sha256:...). Debian/Alpine apt/apk
    package versions are not individually pinned across the board — that
    fights distro security patching — so the digest-pinned base plus the
    pinned Python/apk layers above are the reproducibility mechanism.
    See docs/CONTAINER_PACKAGING.md."""
    offenders = []
    for rel in DOCKERFILES:
        for line in (REPO / rel).read_text().splitlines():
            st = line.strip()
            if st.startswith("FROM ") and "@sha256:" not in st:
                offenders.append(f"{rel}: {st}")
    assert not offenders, (
        "production base images must be digest-pinned:\n  "
        + "\n  ".join(offenders))


def test_proxy_apk_packages_are_pinned():
    """The proxy's alpine packages support `=version` pinning; enforce it
    (the two runtime packages, curl + bash)."""
    text = (REPO / "proxy" / "Dockerfile").read_text().replace("\\\n", " ")
    m = re.search(r"apk add[^\n]*", text)
    assert m, "proxy Dockerfile no longer uses apk add — update this test"
    for tok in m.group(0).split():
        if tok in ("RUN", "apk", "add", "--no-cache"):
            continue
        if tok.startswith("-"):
            continue
        assert "=" in tok, f"proxy apk package not version-pinned: {tok!r}"


def test_v3_judges_python_with_the_interpreter_the_sandbox_runs():
    """Every compile() and ast.parse verdict v3-service gives is about code the
    sandbox then executes. On different interpreters they disagreed: V3 on
    3.11 rejected f"{d["k"]}" and PEP 695 type parameters as invalid Python
    while the 3.13 sandbox ran them (audit V-service/INTEGRITY#1)."""
    def base(rel):
        for line in (REPO / rel).read_text().splitlines():
            if line.startswith("FROM ") and "python:" in line:
                return line.split()[1]
        raise AssertionError(f"{rel} has no python base image")

    assert base("v3-service/Dockerfile") == base("sandbox/Dockerfile")
