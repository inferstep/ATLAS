"""The Dockerfile lint reports what hadolint finds and fails only when it did not lint.

hadolint itself is not needed here: a stand-in prints the findings a test
gives it, so the tests pin what the script does with them.
"""
import importlib.util
import json
import re
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "dockerfile_lint.py"
WORKFLOW = ROOT / ".github" / "workflows" / "hadolint.yml"
STAND_IN = """#!{python}
import json, pathlib, sys
told = json.loads(pathlib.Path(__file__).with_name("told.json").read_text())
print(json.dumps([f for f in told["findings"] if f["file"] in sys.argv[1:]]) if told["status"] == 0 else "it broke")
sys.exit(told["status"])
"""


@pytest.fixture(scope="module")
def lint():
    spec = importlib.util.spec_from_file_location("atlas_dockerfile_lint", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


def git(repo: Path, *args: str) -> str:
    done = subprocess.run(["git", "-C", str(repo), "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args],
                          capture_output=True, text=True, check=True)
    return done.stdout.strip()


def finding(file: str, code: str = "DL3003", level: str = "warning", message: str = "Use WORKDIR", line: int = 2):
    return {"file": file, "code": code, "level": level, "message": message, "line": line, "column": 1}


@pytest.fixture
def repo(tmp_path):
    """A repository with two Dockerfiles in its first commit and a change to one of them in its second."""
    root = tmp_path / "repo"
    for path in ("proxy/Dockerfile", "inference/Dockerfile.vulkan", "scripts/check_dockerfile_sources.py"):
        (root / path).parent.mkdir(parents=True, exist_ok=True)
        (root / path).write_text("FROM debian:12\n", encoding="utf-8")
    git(root, "init", "-q")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", "first")
    (root / "proxy/Dockerfile").write_text("FROM debian:12\nRUN cd /tmp\n", encoding="utf-8")
    git(root, "commit", "-q", "-am", "second")
    return root


@pytest.fixture
def hadolint(tmp_path):
    """A stand-in for hadolint. Call it with the findings and the exit status it must give."""
    binary = tmp_path / "bin" / "hadolint"
    binary.parent.mkdir()
    binary.write_text(STAND_IN.format(python=sys.executable), encoding="utf-8")
    binary.chmod(0o755)

    def tell(findings, status=0):
        binary.with_name("told.json").write_text(json.dumps({"findings": findings, "status": status}), encoding="utf-8")
        return str(binary)
    return tell


def test_it_takes_every_dockerfile_and_nothing_else(lint, repo):
    assert lint.dockerfiles(repo) == ["inference/Dockerfile.vulkan", "proxy/Dockerfile"]


def test_it_takes_the_dockerfiles_of_this_repository(lint):
    found = lint.dockerfiles(ROOT)
    tracked = subprocess.run(["git", "-C", str(ROOT), "ls-files", "*Dockerfile*"], capture_output=True, text=True,
                             check=True).stdout.split()
    assert found == sorted(path for path in tracked if not path.endswith(".py"))
    assert len(found) >= 8


def test_a_finding_is_reported_and_does_not_fail(lint, repo, hadolint, capsys):
    status = lint.lint(repo, hadolint([finding("proxy/Dockerfile")]), "", False)
    out = capsys.readouterr().out
    assert status == 0
    assert "proxy/Dockerfile:2: DL3003 warning: Use WORKDIR" in out
    assert "hadolint: 1 finding(s) in 2 Dockerfile(s): 0 error, 1 warning, 0 info." in out
    assert "- DL3003 (warning): 1" in out


def test_a_dockerfile_hadolint_cannot_read_fails(lint, repo, hadolint, capsys):
    told = [finding("proxy/Dockerfile", "DL1000", "error", "unexpected 'F'\nexpecting FROM", 1)]
    status = lint.lint(repo, hadolint(told), "", False)
    err = capsys.readouterr().err
    assert status == 1
    assert "FAIL proxy/Dockerfile:1: hadolint cannot read this file as a Dockerfile: unexpected 'F'." in err
    assert "Fix: correct the instruction at that line." in err


def test_no_dockerfile_fails(lint, tmp_path, hadolint):
    empty = tmp_path / "empty"
    empty.mkdir()
    git(empty, "init", "-q")
    binary = hadolint([])
    with pytest.raises(lint.LintError, match="tracks no file named `Dockerfile`"):
        lint.lint(empty, binary, "", False)


def test_hadolint_that_is_not_there_fails(lint, repo):
    with pytest.raises(lint.LintError, match="hadolint did not start .* no Dockerfile was linted"):
        lint.lint(repo, str(repo / "no-such-binary"), "", False)


def test_hadolint_that_breaks_fails(lint, repo, hadolint):
    binary = hadolint([], status=3)
    with pytest.raises(lint.LintError, match="ended with status 3 and no list of findings"):
        lint.lint(repo, binary, "", False)


def test_hadolint_is_taken_from_the_path(lint, repo, hadolint, monkeypatch, capsys):
    monkeypatch.setattr(lint, "ROOT", repo)
    monkeypatch.setenv("PATH", str(Path(hadolint([finding("proxy/Dockerfile")])).parent), prepend=":")
    assert lint.main([]) == 0
    assert "hadolint: 1 finding(s) in 2 Dockerfile(s)" in capsys.readouterr().out


def test_without_hadolint_on_the_path_it_ends_with_status_2(lint, repo, monkeypatch, capsys):
    monkeypatch.setattr(lint, "ROOT", repo)
    monkeypatch.setattr(lint.shutil, "which", lambda name: None)
    assert lint.main([]) == 2
    assert "FAIL dockerfile lint: hadolint is not on PATH, so no Dockerfile was linted." in capsys.readouterr().err


def annotations(out: str) -> list[str]:
    return [line for line in out.splitlines() if line.startswith("::")]


def test_only_the_dockerfiles_the_change_touches_are_annotated(lint, repo, hadolint, capsys, monkeypatch):
    monkeypatch.setenv("GITHUB_ACTIONS", "true")
    told = [finding("proxy/Dockerfile"), finding("inference/Dockerfile.vulkan", "DL3008")]
    lint.lint(repo, hadolint(told), git(repo, "rev-parse", "HEAD~1"), False)
    out = capsys.readouterr().out
    assert annotations(out) == ["::warning file=proxy/Dockerfile,line=2,title=hadolint (DL3003)::Use WORKDIR"]
    assert "inference/Dockerfile.vulkan:2: DL3008 warning" in out


def test_every_finding_is_annotated_when_the_base_is_not_known(lint, repo, hadolint, capsys, monkeypatch):
    monkeypatch.setenv("GITHUB_ACTIONS", "true")
    told = [finding("proxy/Dockerfile"), finding("inference/Dockerfile.vulkan", "DL3066", "info")]
    lint.lint(repo, hadolint(told), "0" * 40, False)
    out = capsys.readouterr().out
    assert len(annotations(out)) == 2
    assert "::notice file=inference/Dockerfile.vulkan,line=2,title=hadolint (DL3066)::Use WORKDIR" in out
    assert "is not in this checkout, so every finding is shown" in out


def test_nothing_is_annotated_outside_github(lint, repo, hadolint, capsys, monkeypatch):
    monkeypatch.delenv("GITHUB_ACTIONS", raising=False)
    lint.lint(repo, hadolint([finding("proxy/Dockerfile")]), "", False)
    assert annotations(capsys.readouterr().out) == []


def test_changed_only_prints_the_findings_of_the_changed_dockerfiles(lint, repo, hadolint, capsys):
    told = [finding("proxy/Dockerfile"), finding("inference/Dockerfile.vulkan", "DL3008")]
    lint.lint(repo, hadolint(told), git(repo, "rev-parse", "HEAD~1"), True)
    out = capsys.readouterr().out
    assert "proxy/Dockerfile:2: DL3003" in out
    assert "inference/Dockerfile.vulkan:2:" not in out
    assert "hadolint: 2 finding(s) in 2 Dockerfile(s)" in out


def test_an_annotation_keeps_its_text_on_one_line(lint):
    line = lint.annotation(finding("a,b/Dockerfile", message="50% of\nit"))
    assert line == "::warning file=a%2Cb/Dockerfile,line=2,title=hadolint (DL3003)::50%25 of%0Ait"


def test_the_counts_go_to_the_job_summary(lint, repo, hadolint, tmp_path, monkeypatch):
    target = tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(target))
    lint.lint(repo, hadolint([finding("proxy/Dockerfile")]), "", False)
    assert target.read_text(encoding="utf-8").startswith("hadolint: 1 finding(s) in 2 Dockerfile(s)")


def test_the_workflow_holds_the_binary_against_a_recorded_checksum():
    text = WORKFLOW.read_text(encoding="utf-8")
    assert re.search(r"HADOLINT_VERSION: v\d+\.\d+\.\d+\n", text)
    assert re.search(r"HADOLINT_SHA256: [0-9a-f]{64}\n", text)
    assert "sha256sum --check" in text
    assert re.search(r"^ +run: python scripts/dockerfile_lint\.py --changed-from ", text, re.MULTILINE)
