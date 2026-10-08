"""The CI lock (.github/requirements/ci.txt) stays complete and honest.

CI installs its tools, and the packages of the product that the tests need,
from one hashed lock, so a tampered or swapped package fails the install
(OpenSSF Scorecard Pinned-Dependencies). These checks keep the lock usable
with no network and no tool: every line of ci.in stands in the lock at a
version that fits it, every package of the lock has a hash, each pin that is
a copy of a product file's pin is the same as there, the lock is made for
the Python version of the jobs, and no workflow installs around the lock.

A lock that was not made again after a change is red here. Dependabot moves
a pin in the product file and in ci.in in one pull request and makes the
lock again. When it cannot make the lock again for a pin, it moves the pin
in the product file alone: the copy then differs, and this file says so.
"""

import os
import re

import pytest

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
    """name -> (version, number of hashes) from ci.txt. A name may carry extras in brackets."""
    text = _read(".github", "requirements", "ci.txt")
    locked = {}
    for block in re.split(r"\n(?=[a-z0-9])", text):
        m = re.match(r"([a-z0-9._-]+)(?:\[[^\]]*\])?==(\S+)", block.strip())
        if m:
            locked[m.group(1).lower()] = (m.group(2), block.count("--hash=sha256:"))
    return locked


def _lines(text):
    """The requirement lines of an input or pin file: (name, what follows the name)."""
    out = []
    for line in text.splitlines():
        line = line.split("#", 1)[0].strip()
        m = re.match(r"([A-Za-z0-9._-]+)\s*(.*)$", line)
        if m and not line.startswith("-"):
            out.append((m.group(1).lower(), m.group(2).split(";")[0].strip()))
    return out


def _fits(version, wanted):
    """Whether a locked version fits what a line of ci.in asks for. A line with a bare name asks for nothing."""
    if not wanted:
        return True
    if wanted.startswith("=="):
        return version == wanted[2:].strip()
    from packaging.specifiers import SpecifierSet
    return SpecifierSet(wanted).contains(version, prereleases=True)


# The pins of ci.in that are copies, and the product file each one is a copy of.
COPIED = (
    ("sandbox/requirements-runtime.txt", None),   # every pin of the file
    ("sandbox/requirements-verify.txt", ("jinja2", "ruff", "mypy")),
    ("v3-service/requirements.txt", None),
    ("geometric-lens/requirements.txt", ("httpx",)),
)


def test_every_tool_in_ci_in_is_locked_with_hashes():
    locked = _locked()
    assert locked, "ci.txt has no pinned packages"
    for name in _names(_read(".github", "requirements", "ci.in")):
        assert name in locked, f"{name} is in ci.in but not in ci.txt: run scripts/ci-lock.sh"
    for name, (_version, hashes) in locked.items():
        assert hashes > 0, f"{name} has no --hash in ci.txt"


def test_every_line_of_ci_in_stands_in_the_lock_at_a_version_that_fits_it():
    locked = _locked()
    for name, wanted in _lines(_read(".github", "requirements", "ci.in")):
        assert name in locked, f"{name} is in ci.in and not in ci.txt. Fix: run scripts/ci-lock.sh."
        assert _fits(locked[name][0], wanted), (
            f"ci.in asks for {name}{wanted} and ci.txt holds {name}=={locked[name][0]}, so the lock was not made again "
            "after ci.in changed. Fix: run scripts/ci-lock.sh.")


@pytest.mark.parametrize("version, wanted, fits", [
    ("0.54.0", "==0.54.0", True), ("0.54.0", "==0.53.0", False), ("0.54.0", "", True),
    ("2.4.0", ">=2.3,<3", True), ("3.0.0", ">=2.3,<3", False), ("0.54.0", "== 0.54.0", True),
])
def test_a_locked_version_fits_a_line_or_does_not(version, wanted, fits):
    assert _fits(version, wanted) is fits


def test_every_pin_that_is_a_copy_is_the_same_as_in_its_product_file():
    asked = dict(_lines(_read(".github", "requirements", "ci.in")))
    locked = _locked()
    seen = 0
    for path, names in COPIED:
        pins = {name: wanted for name, wanted in _lines(_read(*path.split("/"))) if wanted.startswith("==")}
        for name in (names or sorted(pins)):
            seen += 1
            assert name in pins, f"{path} has no pin for {name}, which ci.in copies from it. Fix: correct COPIED in this file."
            assert asked.get(name) == pins[name], (
                f"{path} has {name}{pins[name]} and .github/requirements/ci.in has {name}{asked.get(name, ' (no line)')}. "
                "The tests would run with another version than the image ships. Fix: set the line in ci.in to the "
                "product file's pin and run scripts/ci-lock.sh.")
            assert locked[name][0] == pins[name][2:], (
                f"{path} has {name}{pins[name]} and ci.txt holds {name}=={locked[name][0]}. Fix: run scripts/ci-lock.sh.")
    assert seen >= 14, f"only {seen} copied pins were compared; the product files were read wrongly"


def test_the_lock_is_made_for_the_python_version_of_the_jobs_that_install_it():
    import yaml
    header = re.search(r"autogenerated by pip-compile with Python (\d+\.\d+)", _read(".github", "requirements", "ci.txt"))
    assert header, ("ci.txt does not say which Python version it was made with. Dependabot reads that line to make the "
                    "lock again with the same version. Fix: make the lock with scripts/ci-lock.sh.")
    wf_dir = os.path.join(ROOT, ".github", "workflows")
    found = {}
    for name in sorted(os.listdir(wf_dir)):
        if not name.endswith((".yml", ".yaml")):
            continue
        for job_id, job in (yaml.safe_load(_read(".github", "workflows", name)).get("jobs") or {}).items():
            steps = job.get("steps") or []
            if not any("requirements/ci.txt" in str(step.get("run", "")) for step in steps):
                continue
            versions = [str((step.get("with") or {}).get("python-version")) for step in steps
                        if "setup-python" in str(step.get("uses", ""))]
            found[f"{name}:{job_id}"] = versions
    assert found, "no job installs ci.txt; this test reads the workflows wrongly"
    for job, versions in found.items():
        assert all(version == header.group(1) for version in versions), (
            f"{job} installs ci.txt under Python {versions}, and the lock is made for Python {header.group(1)}. A lock "
            "can miss a package that another Python version needs. Fix: use the same version in the job and in "
            "scripts/ci-lock.sh.")


def test_no_workflow_installs_a_copied_pin_from_its_product_file():
    loose = re.compile(r"pip install[^\n]*(sandbox/requirements-(?:runtime|verify)\.txt|v3-service/requirements\.txt)"
                       r"|(sandbox/requirements-(?:runtime|verify)\.txt|v3-service/requirements\.txt)[^\n]*\|\s*xargs pip install")
    wf_dir = os.path.join(ROOT, ".github", "workflows")
    for name in sorted(os.listdir(wf_dir)):
        if name.endswith((".yml", ".yaml")):
            for n, line in enumerate(_read(".github", "workflows", name).splitlines(), 1):
                assert not loose.search(line), (
                    f"{name}:{n} installs from a product pin file, with no hashes: {line.strip()}. Fix: install "
                    "`pip install --require-hashes -r .github/requirements/ci.txt`, which holds these pins.")


def test_every_install_of_the_lock_checks_the_hashes():
    wf_dir = os.path.join(ROOT, ".github", "workflows")
    seen = 0
    for name in sorted(os.listdir(wf_dir)):
        if name.endswith((".yml", ".yaml")):
            for n, line in enumerate(_read(".github", "workflows", name).splitlines(), 1):
                if "pip install" in line and "requirements/ci.txt" in line:
                    seen += 1
                    assert "--require-hashes" in line, (
                        f"{name}:{n} installs the lock and does not check its hashes: {line.strip()}. Fix: write "
                        "`pip install --require-hashes -r .github/requirements/ci.txt`.")
    assert seen >= 5, f"only {seen} installs of the lock were found; this test reads the workflows wrongly"


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
