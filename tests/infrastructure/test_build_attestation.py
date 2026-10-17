"""Each published image gets a signed statement of where it was built, and a tag is moved by hand only by digest.

The first tests read the workflow file. The others run the script of the job
`alias image tag` as it is, in bash, with a stand-in for `docker` that answers
from a file that the test writes, and writes down each call. No registry is reached.
"""
import re
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = yaml.safe_load((ROOT / ".github" / "workflows" / "build-images.yml").read_text(encoding="utf-8"))
JOBS = WORKFLOW["jobs"]
SERVICES = ("proxy", "v3", "lens", "sandbox", "llama", "llama-vulkan")
DIGEST_OF_THE_PUSH = ("${{ steps.build-llama.outputs.digest || steps.build-vulkan.outputs.digest || "
                      "steps.build-small.outputs.digest }}")


def steps_of(job):
    return JOBS[job]["steps"]


def test_the_build_job_attests_the_digest_of_its_own_push_after_every_way_it_builds():
    steps = steps_of("build")
    ids = [step.get("id") for step in steps]
    (at,) = [n for n, step in enumerate(steps) if str(step.get("uses") or "").startswith("actions/attest@")]
    for build in ("build-llama", "build-vulkan", "build-small"):
        assert ids.index(build) < at, f"the attestation comes before the step {build}, whose digest it names"
        assert steps[ids.index(build)]["with"]["push"] is True
    attest = steps[at]
    assert re.fullmatch(r"actions/attest@[0-9a-f]{40}", attest["uses"])
    assert attest["with"] == {
        "subject-name": "${{ env.REGISTRY }}/${{ env.OWNER }}/atlas-${{ matrix.service.name }}",
        "subject-digest": DIGEST_OF_THE_PUSH, "push-to-registry": True, "create-storage-record": False}
    (sign,) = [step for step in steps if step.get("name") == "Sign image (keyless)"]
    assert sign["env"]["DIGEST"] == DIGEST_OF_THE_PUSH, "the signature and the attestation name different digests"


def test_only_the_build_job_may_write_attestations_and_it_has_no_permission_it_does_not_use():
    assert WORKFLOW["permissions"] == {"contents": "read"}
    assert JOBS["build"]["permissions"] == {"contents": "read", "packages": "write", "id-token": "write", "attestations": "write"}
    others = {name: job.get("permissions") for name, job in JOBS.items() if name != "build"}
    assert not [name for name, permissions in others.items() if "attestations" in (permissions or {})], others
    assert "artifact-metadata" not in str(WORKFLOW), "a storage record is not made, so this permission is not needed"


def test_a_pull_request_makes_no_attestation():
    assert "github.event_name != 'pull_request'" in JOBS["build"]["if"]
    assert not [step for step in steps_of("pr-build") if "attest" in str(step.get("uses") or "")]


# --- the move of a tag by hand ---------------------------------------------------------------------------------

def digest(service, tag):
    return "sha256:" + (f"{service}-{tag}".encode().hex() * 8)[:64]


def lines_for(tag, **other):
    return " ".join(f"ghcr.io/inferstep/atlas-{svc}@{other.get(svc.replace('-', '_'), digest(svc, tag))}" for svc in SERVICES)


def alias(tmp_path, alias_tag, digests="", missing=()):
    """Run the script of the alias job. Gives (the run, the tags that were moved as {image:tag -> source})."""
    (script,) = [step["run"] for step in steps_of("alias") if step.get("name") == "Create alias"]
    source_tag = alias_tag.split("=")[0]
    # What the registry answers: one line "<image>:<tag> <digest>" for each image that has the source tag.
    answers = tmp_path / "answers"
    answers.write_text("".join(f"ghcr.io/inferstep/atlas-{svc}:{source_tag} {digest(svc, source_tag)}\n"
                               for svc in SERVICES if svc not in missing))
    calls = tmp_path / "calls"
    stand_in = f"""docker() {{
  echo "$*" >> "{calls}"
  if [ "$3" = inspect ]; then
    while read -r name answer; do
      if [ "$name" = "$4" ]; then echo "$answer"; fi
    done < "{answers}"
  fi
}}
"""
    done = subprocess.run(["bash", "-c", stand_in + script], cwd=tmp_path, capture_output=True, text=True, timeout=30,
                          env={"PATH": "/usr/bin:/bin", "ALIAS": alias_tag, "EXPECTED": digests, "REGISTRY": "ghcr.io",
                               "OWNER": "inferstep"})
    made = {}
    for line in calls.read_text().splitlines() if calls.exists() else []:
        words = line.split()
        if words[2] == "create":
            made[words[words.index("-t") + 1]] = words[-1]
    return done, made


EVERY_TAG_TO_ITS_DIGEST = {f"ghcr.io/inferstep/atlas-{svc}:latest": f"ghcr.io/inferstep/atlas-{svc}@{digest(svc, '3.1.6')}"
                           for svc in SERVICES}


@pytest.mark.parametrize("given, missing, ends, says, moved", [
    pytest.param("", (), 0, "", EVERY_TAG_TO_ITS_DIGEST,
                 id="with no digests given, each tag moves to the digest that its source tag points at"),
    pytest.param(lines_for("3.1.6"), (), 0, "", EVERY_TAG_TO_ITS_DIGEST,
                 id="all six digests are as given"),
    pytest.param(lines_for("3.1.6", llama_vulkan="sha256:" + "0" * 64), (), 1, "atlas-llama-vulkan:3.1.6 points at", {},
                 id="one digest is another"),
    pytest.param(lines_for("3.1.5"), (), 1, "atlas-proxy:3.1.6 points at", {},
                 id="the digests of another release"),
    pytest.param(" ".join(lines_for("3.1.6").split()[:5]), (), 1, "gives 'nothing for this image'", {},
                 id="one image has no line"),
    pytest.param(lines_for("3.1.6") + " ghcr.io/inferstep/atlas-proxy@sha256:" + "1" * 64, (), 1, "atlas-proxy:3.1.6 points at", {},
                 id="one image has two lines"),
    pytest.param("", ("sandbox",), 1, "atlas-sandbox:3.1.6 has no digest that can be read", {},
                 id="one image does not have the source tag"),
])
def test_a_tag_moves_by_hand_only_to_a_digest_and_only_when_all_six_are_as_given(tmp_path, given, missing, ends, says, moved):
    done, made = alias(tmp_path, "3.1.6=latest", digests=given, missing=missing)
    assert done.returncode == ends, done.stdout + done.stderr
    assert says in done.stdout, done.stdout + done.stderr
    assert made == moved


def test_the_job_that_moves_a_tag_by_hand_waits_for_the_release_owners_approval_and_takes_the_digests():
    assert JOBS["alias"]["environment"] == "production"
    (step,) = [step for step in steps_of("alias") if step.get("name") == "Create alias"]
    assert step["env"] == {"ALIAS": "${{ inputs.alias_tag }}", "EXPECTED": "${{ inputs.alias_digests }}"}
    assert set(WORKFLOW[True]["workflow_dispatch"]["inputs"]) == {"alias_tag", "alias_digests"}
