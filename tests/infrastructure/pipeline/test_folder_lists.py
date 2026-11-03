"""Every folder that a script or setting of the repository names by path is in the tree.

Each list below is read by something that skips a folder that is not there
without a word: the size check measures less, the floor check and the lint
read fewer files, a change stops choosing its test suites or counting as
product code, a coverage path or a review owner stops applying. A folder move
that missed one of them would still pass every other check. So each list is
read here the way its reader reads it, and every folder it names must exist.

Not read here: the `src_dir` cases of scripts/install/verify-deployed.sh,
which would need a parser of shell `case` lines.
"""
import importlib.util
import sys
import tomllib
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]


def load(relative):
    spec = importlib.util.spec_from_file_location(Path(relative).stem.replace("-", "_"), ROOT / relative)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # dataclasses look their module up there
    spec.loader.exec_module(module)
    return module


def codeowners():
    lines = (ROOT / ".github" / "CODEOWNERS").read_text(encoding="utf-8").splitlines()
    owned = [line.split()[0] for line in lines if line.startswith("/")]
    # benchmark/ is made at run time and never tracked (docs/README.md).
    return [path for path in owned if path != "/benchmark/"]


def dockerignore_source_trees():
    text = (ROOT / ".dockerignore").read_text(encoding="utf-8")
    block = text.split("# Other services' source trees (v3 doesn't need them)\n", 1)[1]
    return block.split("\n\n", 1)[0].splitlines()


def codecov_paths():
    flags = yaml.safe_load((ROOT / ".github" / "codecov.yml").read_text(encoding="utf-8"))["flag_management"]
    return [path for flag in flags["individual_flags"] for path in flag["paths"]]


def dependabot_directories():
    updates = yaml.safe_load((ROOT / ".github" / "dependabot.yml").read_text(encoding="utf-8"))["updates"]
    named = [d for update in updates for d in update.get("directories", [update.get("directory")])]
    # "/.github/actions/*" is every folder in .github/actions/.
    return [d.removesuffix("/*") for d in named]


LISTS = [
    pytest.param(lambda: load("scripts/ci/code_health.py").PY_DIRS, id="code_health.py PY_DIRS"),
    pytest.param(lambda: load("scripts/ci/code_health.py").GO_DIRS, id="code_health.py GO_DIRS"),
    pytest.param(lambda: load("scripts/ci/check_min_python.py").DEFAULT_TARGETS, id="check_min_python.py DEFAULT_TARGETS"),
    pytest.param(lambda: load("scripts/ci/production-readiness.py").PYTHON_TARGETS,
                 id="production-readiness.py PYTHON_TARGETS"),
    pytest.param(lambda: [path for path, _ in load("scripts/ci/verify.py").SUITES_BY_PATH],
                 id="verify.py SUITES_BY_PATH"),
    pytest.param(lambda: load("scripts/integrity_check.py").PRODUCT_DIRS, id="integrity_check.py PRODUCT_DIRS"),
    pytest.param(lambda: tomllib.loads((ROOT / "pyproject.toml").read_text(encoding="utf-8"))["tool"]["coverage"]["run"]["source"],
                 id="pyproject.toml coverage source"),
    pytest.param(codecov_paths, id="codecov.yml flag paths"),
    pytest.param(codeowners, id="CODEOWNERS"),
    pytest.param(dockerignore_source_trees, id=".dockerignore source trees"),
    pytest.param(dependabot_directories, id="dependabot.yml directories"),
]


@pytest.mark.parametrize("read", LISTS)
def test_every_folder_the_list_names_is_in_the_tree(read):
    names = list(read())
    assert names, "the list is empty, so this test reads it wrongly"
    missing = [name for name in names if not (ROOT / name.strip("/")).exists()]
    assert missing == [], f"named but not in the tree: {missing}"
