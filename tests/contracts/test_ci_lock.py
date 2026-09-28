"""The CI tool lock (.github/requirements/ci.txt) stays complete and honest.

CI installs its tools from a hashed lock so a tampered or swapped package
fails the install (OpenSSF Scorecard Pinned-Dependencies). These checks keep
the lock usable: every tool in ci.in is pinned with hashes, ruff and mypy
match the sandbox's verify pins, and no workflow installs a locked tool
around the lock.
"""

import os
import re

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
REQ = os.path.join(ROOT, ".github", "requirements")


def _read(*parts):
    with open(os.path.join(ROOT, *parts), encoding="utf-8") as fh:
        return fh.read()


def _names(text):
    out = []
    for line in text.splitlines():
        line = line.split("#", 1)[0].strip()
        if line and not line.startswith("-"):
            out.append(re.split(r"[=<>!~ ]", line, maxsplit=1)[0].lower())
    return out


def _locked():
    """name -> (version, number of hashes) from ci.txt."""
    text = _read(".github", "requirements", "ci.txt")
    locked = {}
    for block in re.split(r"\n(?=[a-z0-9])", text):
        m = re.match(r"([a-z0-9._-]+)==(\S+)", block.strip())
        if m:
            locked[m.group(1).lower()] = (m.group(2), block.count("--hash=sha256:"))
    return locked


def test_every_tool_in_ci_in_is_locked_with_hashes():
    locked = _locked()
    assert locked, "ci.txt has no pinned packages"
    for name in _names(_read(".github", "requirements", "ci.in")):
        assert name in locked, f"{name} is in ci.in but not in ci.txt: run scripts/ci-lock.sh"
    for name, (_version, hashes) in locked.items():
        assert hashes > 0, f"{name} has no --hash in ci.txt"


def test_ruff_and_mypy_match_the_sandbox_verify_pins():
    locked = _locked()
    verify = dict(re.findall(r"^(ruff|mypy)==(\S+)", _read("sandbox", "requirements-verify.txt"), re.M))
    for tool in ("ruff", "mypy"):
        assert locked[tool][0] == verify[tool], (
            f"ci.txt has {tool}=={locked[tool][0]} but sandbox/requirements-verify.txt has "
            f"{tool}=={verify[tool]}: update ci.in and run scripts/ci-lock.sh")


def test_workflows_install_locked_tools_only_through_the_lock():
    tools = _names(_read(".github", "requirements", "ci.in"))
    loose = re.compile(r"pip install (?!--require-hashes)(?!--no-deps)(?!-r )(?!-e )[^\n]*\b(%s)\b" % "|".join(
        re.escape(t) for t in tools))
    wf_dir = os.path.join(ROOT, ".github", "workflows")
    for name in sorted(os.listdir(wf_dir)):
        if name.endswith((".yml", ".yaml")):
            for n, line in enumerate(_read(".github", "workflows", name).splitlines(), 1):
                assert not loose.search(line), f"{name}:{n} installs a locked tool without the lock: {line.strip()}"
