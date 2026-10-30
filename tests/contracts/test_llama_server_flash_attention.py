"""Every script that starts the model server turns flash attention on.

The running server's /props has no field for flash attention, so the nightly run cannot check it there. The scripts
that start the server are where it is set.
"""
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


@pytest.mark.parametrize("script", ["inference/entrypoint.sh", "inference/entrypoint-v3.1.sh", "scripts/atlas-llama-macos.sh"])
def test_the_model_server_starts_with_flash_attention_on(script):
    # The script as the shell runs it: comment lines and the comment at the end of a line are dropped.
    code = [line.split("#")[0] for line in (ROOT / script).read_text(encoding="utf-8").splitlines() if not line.lstrip().startswith("#")]
    assert any("--flash-attn on" in line for line in code), (
        f"{script} starts llama-server without `--flash-attn on`. Fix: add it to the command line of llama-server.")
