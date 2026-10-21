"""Black-box checks for the macOS Bash 3.2 launcher."""

import os
import pathlib
import subprocess


ROOT = pathlib.Path(__file__).resolve().parents[2]
LAUNCHER = ROOT / "scripts" / "atlas-llama-macos.sh"


def test_launcher_accepts_disabled_asa_on_macos_bash(tmp_path):
    prefix = tmp_path / "native"
    bin_dir = prefix / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/bash\nprintf '%s\\n' \"$@\"\n")
    binary.chmod(0o755)
    model = tmp_path / "fixture.gguf"
    model.write_bytes(b"GGUF")

    env = os.environ.copy()
    env.update({
        "ATLAS_MACOS_PREFIX": str(prefix),
        "ATLAS_MODELS_DIR": str(tmp_path),
        "ATLAS_MODEL_FILE": model.name,
        "ATLAS_MODEL_NAME": "fixture-model",
        "ATLAS_CONTROL_VECTOR": str(tmp_path / "missing-vector.gguf"),
        "ATLAS_CTX_SIZE": "4096",
        "ATLAS_PARALLEL_SLOTS": "2",
        "PARALLEL_SLOTS": "9",  # each older name loses to its ATLAS_ name
        "ATLAS_KV_TYPE_K": "q8_0",
        "KV_CACHE_TYPE_K": "f16",
        "ATLAS_KV_TYPE_V": "q4_0",
        "KV_CACHE_TYPE_V": "f16",
        "ATLAS_BATCH": "512",
        "ATLAS_UBATCH": "256",
        # The launcher keeps its slots in $TMPDIR/atlas-slots.
        "TMPDIR": str(tmp_path),
    })
    result = subprocess.run(
        ["/bin/bash", str(LAUNCHER)],
        cwd=ROOT, env=env, capture_output=True, text=True, timeout=10,
    )

    assert result.returncode == 0, result.stderr
    assert "ASA steering:         disabled" in result.stdout
    assert "Parallel slots:       2" in result.stdout
    assert "KV cache K / V:       q8_0 / q4_0" in result.stdout
    assert "Batch / micro-batch:  256 / 256" in result.stdout  # 512 capped
    assert "--control-vector-scaled" not in result.stdout
