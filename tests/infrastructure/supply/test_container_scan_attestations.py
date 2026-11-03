"""The attestation check of the weekly container scan: what makes it try cosign again."""

import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".github" / "workflows" / "container-scan.yml"

# Stand-ins: cosign answers each call with the next line of $ANSWERS ("ok" passes), docker gives a BuildKit
# SBOM and provenance, and sleep does not wait.
COSIGN = ('#!/bin/bash\nn=$(cat "$CALLS" 2>/dev/null || echo 0)\necho $((n + 1)) > "$CALLS"\n'
          'answer=$(sed -n "$((n + 1))p" "$ANSWERS")\n[ "$answer" = ok ] && exit 0\necho "Error: $answer" >&2\nexit 1\n')
DOCKER = '#!/bin/bash\necho \'{"spdx": {"name": "x"}}\'\n'
SLEEP = "#!/bin/bash\nexit 0\n"
NO_ROOT = "trusted root is required when using new bundle format"


@pytest.mark.parametrize("answers, ends, calls", [
    pytest.param(["ok"], 0, 1, id="a signature that verifies passes at once"),
    pytest.param([NO_ROOT, "ok"], 0, 2, id="a failed fetch of the trusted root is tried again"),
    pytest.param(["no matching signatures"], 1, 1, id="any other failure fails at once"),
    pytest.param([NO_ROOT] * 3, 1, 3, id="three failed fetches fail the job"),
])
def test_only_a_failed_fetch_of_the_trusted_root_is_tried_again(tmp_path, answers, ends, calls):
    steps = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))["jobs"]["attestations"]["steps"]
    (step,) = [s["run"] for s in steps if s.get("name", "").startswith("Verify signature")]
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name, text in [("cosign", COSIGN), ("docker", DOCKER), ("sleep", SLEEP)]:
        (bin_dir / name).write_text(text, encoding="utf-8")
        (bin_dir / name).chmod(0o755)
    (tmp_path / "answers").write_text("\n".join(answers) + "\n", encoding="utf-8")

    done = subprocess.run(["bash", "-e", "-c", step.replace("${{ matrix.image }}", "atlas-proxy")], capture_output=True,
                          text=True, timeout=60, env={"PATH": f"{bin_dir}:/usr/bin:/bin", "RUNNER_TEMP": str(tmp_path),
                                                      "CALLS": str(tmp_path / "calls"), "ANSWERS": str(tmp_path / "answers")})

    assert done.returncode == ends, done.stderr
    assert int((tmp_path / "calls").read_text(encoding="utf-8")) == calls
