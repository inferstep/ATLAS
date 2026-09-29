"""Dependency-boundary checks for CPU-only supporting services."""

import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


def test_lens_image_installs_torch_from_cpu_only_index():
    dockerfile = (ROOT / "geometric-lens" / "Dockerfile").read_text()
    requirements = (ROOT / "geometric-lens" / "requirements.txt").read_text()
    assert "--index-url https://download.pytorch.org/whl/cpu" in dockerfile
    # The pre-install must pin the SAME version requirements pins — a
    # hardcoded version here previously let the two drift (Dockerfile
    # 2.12.1 vs requirements 2.13.0), which made the requirements install
    # replace the CPU wheel with PyPI's CUDA build (~8 GB of nvidia/cu*
    # deps in a CPU-only image). tests/contracts/test_torch_cpu_pin.py
    # guards both services; this keeps the lens file self-consistent.
    req_torch = re.search(r"^torch==(\S+)\s*$", requirements, re.MULTILINE)
    assert req_torch, "no torch pin in geometric-lens/requirements.txt"
    assert f"torch=={req_torch.group(1)}" in dockerfile
    assert "--extra-index-url" not in requirements
    assert 'xgboost-cpu==3.2.0; sys_platform == "linux"' in requirements


# --- declared Python floor -------------------------------------------------

def test_min_python_gate_passes_on_current_tree():
    """The shipped tree must not use syntax newer than requires-python.

    Guards the drift that produced 29 fixture errors: sandbox/executor_server.py
    carried a PEP 604 annotation while pyproject declared >=3.9, and the CI
    matrix (3.11/3.12 only) could not see it.
    """
    import subprocess
    import sys
    from pathlib import Path

    root = Path(__file__).resolve().parents[2]
    proc = subprocess.run(
        [sys.executable, str(root / "scripts" / "check_min_python.py")],
        capture_output=True, text=True, cwd=str(root))
    assert proc.returncode == 0, (
        f"min-python gate failed:\n{proc.stdout}\n{proc.stderr}")


def test_min_python_gate_detects_a_violation(tmp_path):
    """The gate must actually fail on offending code, not just always pass."""
    import subprocess
    import sys
    from pathlib import Path

    root = Path(__file__).resolve().parents[2]
    bad = tmp_path / "offender.py"
    bad.write_text("def f(x: int) -> str | None:\n    return None\n")

    proc = subprocess.run(
        [sys.executable, str(root / "scripts" / "check_min_python.py"), str(bad)],
        capture_output=True, text=True, cwd=str(root))
    assert proc.returncode == 1, (
        f"gate passed on a PEP 604 annotation:\n{proc.stdout}\n{proc.stderr}")
    assert "PEP 604" in proc.stderr


def test_min_python_gate_accepts_future_annotations(tmp_path):
    """`from __future__ import annotations` is the documented remedy."""
    import subprocess
    import sys
    from pathlib import Path

    root = Path(__file__).resolve().parents[2]
    ok = tmp_path / "guarded.py"
    ok.write_text("from __future__ import annotations\n\n"
                  "def f(x: int) -> str | None:\n    return None\n")

    proc = subprocess.run(
        [sys.executable, str(root / "scripts" / "check_min_python.py"), str(ok)],
        capture_output=True, text=True, cwd=str(root))
    assert proc.returncode == 0, (
        f"gate rejected a correctly-guarded file:\n{proc.stdout}\n{proc.stderr}")


# --- Dockerfile COPY sources -----------------------------------------------

def _run_gate(script, *args):
    import subprocess
    import sys
    from pathlib import Path
    root = Path(__file__).resolve().parents[2]
    return subprocess.run([sys.executable, str(root / "scripts" / script), *args],
                          capture_output=True, text=True, cwd=str(root))


def test_dockerfile_copy_sources_all_exist():
    """Every Dockerfile COPY source must exist in its build context.

    A deleted source is invisible to imports, tests, and lint — it fails only
    when the image is built. Both the lens (indexer/, retriever/) and
    v3-service (wavelet/, rpg.py) hit this during the retrieval/RPG removals;
    the second reached CI green on tests with only the image job failing.
    """
    proc = _run_gate("check_dockerfile_sources.py")
    assert proc.returncode == 0, (
        f"dockerfile-sources gate failed:\n{proc.stdout}\n{proc.stderr}")


def test_dockerfile_gate_keeps_the_dot_of_a_hidden_context(tmp_path, monkeypatch):
    """#277: the Dockerfile path was cleaned with `lstrip("./")`, which also
    ate the dot of a hidden directory, so `.ci/Dockerfile` was keyed as
    `ci/Dockerfile` and its declared context was never found."""
    import importlib.util
    spec = importlib.util.spec_from_file_location(
        "check_dockerfile_sources", ROOT / "scripts" / "check_dockerfile_sources.py")
    gate = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(gate)
    compose = tmp_path / "docker-compose.yml"
    compose.write_text(
        "services:\n"
        "  ci:\n"
        "    build:\n"
        "      context: ./.ci\n"
        "  app:\n"
        "    build:\n"
        "      context: .\n"
        "      dockerfile: ./app/Dockerfile\n")
    monkeypatch.setattr(gate, "COMPOSE", compose)
    assert gate.compose_contexts() == {".ci/Dockerfile": "./.ci", "app/Dockerfile": "."}


def test_dockerfile_gate_detects_a_missing_source(tmp_path, monkeypatch):
    """The gate must fail on a missing source, not merely always pass."""
    import subprocess
    import sys
    from pathlib import Path
    root = Path(__file__).resolve().parents[2]
    df = root / "v3-service" / "Dockerfile"
    original = df.read_text()
    df.write_text(original + "\nCOPY v3-service/definitely_not_here.py /app/x.py\n")
    try:
        proc = subprocess.run(
            [sys.executable, str(root / "scripts" / "check_dockerfile_sources.py")],
            capture_output=True, text=True, cwd=str(root))
        assert proc.returncode == 1, (
            f"gate passed despite a missing COPY source:\n{proc.stdout}\n{proc.stderr}")
        assert "definitely_not_here.py" in proc.stderr
    finally:
        df.write_text(original)
