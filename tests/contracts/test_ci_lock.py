"""The CI lock (.github/requirements/ci.txt) stays complete and honest.

CI installs its tools, and the packages of the product that the tests need,
from one hashed lock, so a tampered or swapped package fails the install
(OpenSSF Scorecard Pinned-Dependencies). These checks keep the lock usable
with no network and no tool: every line of ci.in stands in the lock at a
version that fits it, every package of the lock has a hash, each pin that is
a copy of a product file's pin is the same as there, the lock is made for
the Python version of the jobs, and no workflow installs around the lock.
The lock of the lens tests (lens.txt) is held to its product file too.

A lock that was not made again after a change is red here. Dependabot moves
a pin in the product file and in ci.in in one pull request and makes the
lock again. When it cannot make the lock again for a pin, it moves the pin
in the product file alone: the copy then differs, and this file says so.
"""

import os
import re
import subprocess

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


def _read(*parts):
    with open(os.path.join(ROOT, *parts), encoding="utf-8") as fh:
        return fh.read()


def _locked(lock="ci.txt"):
    """name -> (version, number of hashes) from a lock in .github/requirements. A name may carry extras in brackets."""
    text = _read(".github", "requirements", lock)
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


def _workflow_lines():
    """Each line of each workflow: (file name, line number, line)."""
    for name in sorted(os.listdir(os.path.join(ROOT, ".github", "workflows"))):
        if name.endswith((".yml", ".yaml")):
            for number, line in enumerate(_read(".github", "workflows", name).splitlines(), 1):
                yield name, number, line


# The pins of ci.in that are copies, and the product file each one is a copy of.
COPIED = (
    ("sandbox/requirements-runtime.txt", None),   # every pin of the file
    ("sandbox/requirements-verify.txt", ("jinja2", "ruff", "mypy")),
    ("v3-service/requirements.txt", None),
    ("geometric-lens/requirements.txt", ("httpx",)),
)


def test_the_lock_script_finds_the_requirements_from_its_folder():
    script = os.path.join(ROOT, "scripts", "ci", "ci-lock.sh")
    with open(script, encoding="utf-8") as fh:
        (found,) = [line for line in fh.read().splitlines() if line.startswith('cd "$(dirname "$0")')]

    # Only the script's own line: the rest of the script makes a lock.
    done = subprocess.run(["bash", "-c", f"{found}\npwd\n", script], capture_output=True, text=True, timeout=30)

    assert done.stdout.strip() == os.path.join(ROOT, ".github", "requirements"), done.stderr


def test_every_package_of_the_lock_has_a_hash():
    locked = _locked()
    assert locked, "ci.txt has no pinned packages"
    for name, (_version, hashes) in locked.items():
        assert hashes > 0, f"{name} has no --hash in ci.txt"


def test_every_line_of_ci_in_stands_in_the_lock_at_a_version_that_fits_it():
    locked = _locked()
    for name, wanted in _lines(_read(".github", "requirements", "ci.in")):
        assert name in locked, f"{name} is in ci.in and not in ci.txt. Fix: run scripts/ci/ci-lock.sh."
        assert _fits(locked[name][0], wanted), (
            f"ci.in asks for {name}{wanted} and ci.txt holds {name}=={locked[name][0]}, so the lock was not made again "
            "after ci.in changed. Fix: run scripts/ci/ci-lock.sh.")


@pytest.mark.parametrize("version, wanted, fits", [
    pytest.param("0.54.0", "==0.54.0", True, id="the-same-pin"),
    pytest.param("0.54.0", "== 0.54.0", True, id="the-same-pin-after-a-space"),
    pytest.param("0.54.0", "==0.53.0", False, id="another-pin"),
    pytest.param("0.54.0", "", True, id="a-bare-name"),
    pytest.param("2.4.0", ">=2.3,<3", True, id="inside-a-range"),
    pytest.param("3.0.0", ">=2.3,<3", False, id="outside-a-range"),
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
                "product file's pin and run scripts/ci/ci-lock.sh.")
            assert locked[name][0] == pins[name][2:], (
                f"{path} has {name}{pins[name]} and ci.txt holds {name}=={locked[name][0]}. Fix: run scripts/ci/ci-lock.sh.")
    assert seen >= 14, f"only {seen} copied pins were compared; the product files were read wrongly"


def test_the_lock_is_made_for_the_python_version_of_the_jobs_that_install_it():
    import yaml
    header = re.search(r"autogenerated by pip-compile with Python (\d+\.\d+)", _read(".github", "requirements", "ci.txt"))
    assert header, ("ci.txt does not say which Python version it was made with. Dependabot reads that line to make the "
                    "lock again with the same version. Fix: make the lock with scripts/ci/ci-lock.sh.")
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
            "scripts/ci/ci-lock.sh.")


def test_no_workflow_installs_a_copied_pin_from_its_product_file():
    product = "(" + "|".join(re.escape(path) for path, _names in COPIED) + ")"
    loose = re.compile(rf"pip install[^\n]*{product}|{product}[^\n]*\|\s*xargs pip install")
    for name, number, line in _workflow_lines():
        assert not loose.search(line), (
            f"{name}:{number} installs from a product pin file, with no hashes: {line.strip()}. Fix: install "
            "from the hashed lock that holds these pins, .github/requirements/ci.txt or lens.txt, with "
            "`pip install --require-hashes -r`.")


def test_the_lens_lock_holds_every_linux_pin_of_the_lens_requirements():
    """lens.txt is made from geometric-lens/requirements.txt for the Linux runners. A pin there that the lock does not
    hold at the same version means the lock was not made again after a change. The lock's torch carries the local
    version of PyTorch's CPU index (2.14.0+cpu)."""
    locked = _locked("lens.txt")
    for line in _read("geometric-lens", "requirements.txt").splitlines():
        line = line.split("#", 1)[0].strip()
        if not line or 'sys_platform != "linux"' in line:
            continue
        name, version = re.match(r"([A-Za-z0-9._-]+)==([^;\s]+)", line).groups()
        why = (f"geometric-lens/requirements.txt has {name}=={version} and lens.txt holds "
               f"{locked.get(name.lower(), ('nothing',))[0]}. Fix: run scripts/ci/ci-lock.sh.")
        assert name.lower() in locked, why
        assert locked[name.lower()][0].split("+")[0] == version, why


def test_every_install_of_the_lock_checks_the_hashes():
    installs = [(name, number, line) for name, number, line in _workflow_lines()
                if "pip install" in line and "requirements/ci.txt" in line]
    assert len(installs) >= 5, (
        f"only {len(installs)} installs of the lock were found; this test reads the workflows wrongly")
    for name, number, line in installs:
        assert "--require-hashes" in line, (
            f"{name}:{number} installs the lock and does not check its hashes: {line.strip()}. Fix: write "
            "`pip install --require-hashes -r .github/requirements/ci.txt`.")


def test_workflows_install_locked_tools_only_through_the_lock():
    tools = [name for name, _wanted in _lines(_read(".github", "requirements", "ci.in"))]
    loose = re.compile(r"pip install (?!--require-hashes)(?!--no-deps)(?!-r )(?!-e )[^\n]*\b(%s)\b" % "|".join(
        re.escape(tool) for tool in tools))
    for name, number, line in _workflow_lines():
        assert not loose.search(line), f"{name}:{number} installs a locked tool without the lock: {line.strip()}"
