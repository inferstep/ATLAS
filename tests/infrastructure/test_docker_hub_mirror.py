"""A job that builds or runs an image of Docker Hub has a second source for it, and the step that gives it changes
nothing else of the Docker service.

Docker Hub refuses a pull for its limit, or its token service does not answer, and then a job is red for a change
that is not at fault. The workflow files themselves are read here, so a new job cannot forget the step, and an
image that a job runs cannot be left out of it.
"""
import importlib.util
import itertools
import json
import os
import re
import stat
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
STEP = "./.github/actions/docker-hub-mirror"
ACTION = ROOT / ".github" / "actions" / "docker-hub-mirror" / "action.yml"
SCRIPT = ROOT / "scripts" / "docker_hub_mirror.py"
PAGE = ROOT / "docs" / "quality" / "gates.md"
SECTION = "Docker Hub and the checks"

_spec = importlib.util.spec_from_file_location("docker_hub_mirror", SCRIPT)
mirror = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(mirror)
HOST = mirror.MIRROR.removeprefix("https://")

# A docker command that can pull an image. `docker compose config`, `docker logs` and `docker rm` pull nothing.
PULLS = re.compile(r"\bdocker\s+(?:build|buildx|run|pull|create)\b|\bdocker\s+compose\b[^\n|;&]*\b(?:build|up|pull|run|create)\b")
# The events for which a job judges a change.
FOR_A_CHANGE = {"pull_request", "merge_group"}
# The jobs that use Docker and start for no change, each with the start of the condition that says so. None: the
# workflow of the job starts for no change at all.
OTHER_STARTS = {
    ("build-images.yml", "alias"): "github.event_name == 'workflow_dispatch'",
    ("build-images.yml", "build"): "github.event_name != 'pull_request'",
    ("build-images.yml", "promote"): "github.event_name == 'push'",
    ("container-scan.yml", "attestations"): None,
}
# The flags of `docker run` that the workflow files use. A flag that is not here is not guessed at.
WITH_A_VALUE = {"-v", "-w", "-e", "-p", "--name", "--env-file", "--user", "--network", "--entrypoint"}
WITH_NO_VALUE = {"--rm", "-d", "-i", "-t", "--init", "--read-only"}
BUILDER = "docker/setup-buildx-action@"
BUILDERS_MIRROR = f'[registry."docker.io"]\n  mirrors = ["{HOST}"]\n'


def all_jobs():
    """Each job of each workflow: the file, the events that start the workflow, the name of the job, the job."""
    for path in sorted(WORKFLOWS.glob("*.yml")):
        loaded = yaml.safe_load(path.read_text(encoding="utf-8"))
        # YAML reads the key `on` as the value true.
        starts = loaded.get(True)
        events = set(starts) if isinstance(starts, (dict, list)) else {starts}
        for key, job in loaded["jobs"].items():
            yield path.name, events, key, job


def pulls(step):
    """Whether a step can make the Docker service pull an image."""
    uses = step.get("uses", "")
    return uses.startswith(("docker/", "docker://")) or bool(PULLS.search(step.get("run", "")))


def the_step_of(job):
    return [step for step in job["steps"] if step.get("uses") == STEP]


JOBS = [found for found in all_jobs() if any(pulls(step) for step in found[3].get("steps", []))]
WITH_THE_STEP = [found for found in JOBS if the_step_of(found[3])]
WITHOUT = [found for found in JOBS if not the_step_of(found[3])]
NAMES = [f"{name}:{key}" for name, _events, key, _job in WITH_THE_STEP]


def copies_of(job):
    """The values of the matrix for each job that a job with a matrix makes; one empty set of values with no matrix."""
    matrix = job.get("strategy", {}).get("matrix", {})
    if "include" in matrix:
        assert set(matrix) == {"include"}
        return [{"matrix": entry} for entry in matrix["include"]]
    keys = sorted(matrix)
    return [{"matrix": dict(zip(keys, values))} for values in itertools.product(*(matrix[key] for key in keys))]


def filled(text, values):
    """A text of a workflow with the values of the matrix in the place of `${{ matrix... }}`."""
    def value(found):
        at = values
        for part in found.group(1).split("."):
            at = at[part]
        return str(at)
    return re.sub(r"\$\{\{\s*(matrix\.[a-z0-9_.-]+)\s*\}\}", value, text)


def images_run(command_text):
    """The image of each `docker run` in the text of a step. A flag that this test does not know stops it."""
    found = []
    joined = re.sub(r"\\\n\s*", " ", command_text)
    for start in re.finditer(r"\bdocker\s+run\s", joined):
        words = iter(joined[start.end():].split())
        for word in words:
            if not word.startswith("-"):
                found.append(word)
                break
            flag = word.split("=")[0]
            assert flag in WITH_A_VALUE | WITH_NO_VALUE, (
                f"this test does not know the flag `{flag}` of docker run, so it cannot tell which word is the image. "
                "Fix: add the flag to WITH_A_VALUE or to WITH_NO_VALUE of this test.")
            if flag in WITH_A_VALUE and "=" not in word:
                next(words)
    return found


def built_by(job):
    """The names that a job gives to the images it builds itself: those are not pulled."""
    return {name for step in job["steps"] for name in re.findall(r"\bdocker\s+build\b[^\n]*?\s-t\s+(\S+)", step.get("run", ""))}


def from_docker_hub(image):
    """Whether the name of an image is one of Docker Hub: no registry before the first `/`, or Docker Hub's own."""
    first = image.split("/")[0]
    named = "/" in image and ("." in first or ":" in first or first == "localhost")
    return not named or first in ("docker.io", "index.docker.io", "registry-1.docker.io")


def test_there_are_jobs_with_the_step_so_the_tests_below_hold_something():
    assert len(WITH_THE_STEP) >= 5, NAMES


@pytest.mark.parametrize("name, key, job", [(name, key, job) for name, _events, key, job in WITH_THE_STEP], ids=NAMES)
def test_the_step_comes_after_the_checkout_and_before_the_first_docker_command(name, key, job):
    steps = job["steps"]
    (at,) = [n for n, step in enumerate(steps) if step.get("uses") == STEP]
    checkout = [n for n, step in enumerate(steps) if step.get("uses", "").startswith("actions/checkout@")]
    assert checkout, f"{name}: the job `{key}` has no checkout, and the step is a file of the commit"
    assert checkout[0] < at, f"{name}: in the job `{key}` the mirror step comes before the checkout that brings it"
    first = min(n for n, step in enumerate(steps) if pulls(step))
    assert at < first, (
        f"{name}: in the job `{key}` step {first + 1} can pull an image before the mirror step (step {at + 1}), so "
        "that pull goes to Docker Hub alone. Fix: put the mirror step right after the checkout.")


@pytest.mark.parametrize("name, events, key, job", WITHOUT, ids=[f"{name}:{key}" for name, _events, key, _job in WITHOUT])
def test_a_job_that_uses_docker_with_no_mirror_step_starts_for_no_change(name, events, key, job):
    fix = (f"{name}: the job `{key}` builds or runs an image and has no mirror step. When Docker Hub refuses a pull, "
           f"the job is red for a change that is not at fault. Fix: add `- uses: {STEP}` right after its checkout "
           f"(docs/quality/gates.md, section \"{SECTION}\"). If the job starts for no pull request and not in the merge "
           "queue, list it in OTHER_STARTS of this test with the condition that says so.")
    assert (name, key) in OTHER_STARTS, fix
    condition = OTHER_STARTS[(name, key)]
    if condition is None:
        assert not events & FOR_A_CHANGE, fix
    else:
        assert str(job.get("if", "")).startswith(condition), fix


def test_the_list_of_jobs_that_start_for_no_change_names_only_jobs_that_are_there():
    assert set(OTHER_STARTS) == {(name, key) for name, _events, key, _job in WITHOUT}


@pytest.mark.parametrize("text, images", [
    ("docker run --rm ubuntu:24.04 true", ["ubuntu:24.04"]),
    ("docker run --rm \\\n  -v \"$PWD\":/atlas:ro \\\n  -w /work \\\n  -e A=1 \\\n  debian:12 \\\n  bash -c 'x'", ["debian:12"]),
    ("docker run -d --name one -p 8020:8020 built-here\nsleep 5\ndocker run --rm --network=host alpine:3 ls", ["built-here", "alpine:3"]),
    ("docker build -t x .\ndocker compose up -d", []),
])
def test_the_image_of_a_docker_run_is_read_from_the_text_of_a_step(text, images):
    assert images_run(text) == images


def test_a_flag_of_docker_run_that_this_test_does_not_know_is_not_passed_over():
    with pytest.raises(AssertionError, match="does not know the flag `--gpus`"):
        images_run("docker run --gpus all ubuntu:24.04 true")


@pytest.mark.parametrize("name, key, job", [(name, key, job) for name, _events, key, job in WITH_THE_STEP], ids=NAMES)
def test_the_step_is_given_the_very_images_of_docker_hub_that_the_job_runs(name, key, job):
    # An image that the job runs and the step was not given has Docker Hub as its one source again, with no sign.
    (step,) = the_step_of(job)
    for values in copies_of(job):
        given = filled(str(step.get("with", {}).get("images", "")), values).split()
        run = [image for one in job["steps"] for image in images_run(filled(one.get("run", ""), values))]
        of_the_hub = sorted({image for image in run if image not in built_by(job) and from_docker_hub(image)})
        assert sorted(given) == of_the_hub, (
            f"{name}: the job `{key}` runs {of_the_hub} of Docker Hub, and its mirror step is given {sorted(given)}. "
            "An image that is run and not given is pulled from Docker Hub alone. Fix: give the step, under "
            "`with: images:`, the names that the job's `docker run` commands use, and no other.")
        for image in given:
            assert mirror.at_the_mirror(image).startswith(f"{HOST}/")


@pytest.mark.parametrize("image, hub", [
    ("postgres:16", True), ("ubuntu", True), ("library/redis:7", True), ("rockylinux/rockylinux:9", True),
    ("docker.io/library/redis", True), ("index.docker.io/library/redis", True),
    ("ghcr.io/inferstep/atlas-sandbox:dev", False), ("mcr.microsoft.com/playwright:v1", False),
    ("localhost:5000/thing", False), ("mirror.gcr.io/library/ubuntu:24.04", False),
])
def test_an_image_name_is_read_as_docker_hubs_or_not(image, hub):
    assert from_docker_hub(image) is hub


def test_no_job_has_a_container_or_a_service_image_of_docker_hub():
    # GitHub pulls the image of `container:` and of `services:` before the first step of the job, so no step can
    # give it a second source in time.
    for name, _events, key, job in all_jobs():
        container = job.get("container")
        images = [container if isinstance(container, str) else container["image"]] if container else []
        images += [service["image"] for service in (job.get("services") or {}).values()]
        for image in images:
            assert not from_docker_hub(image), (
                f"{name}: the job `{key}` gets the image `{image}` from Docker Hub before its first step. No step can "
                "come before that pull, so the job is red when Docker Hub refuses it. Fix: start the image with "
                f"`docker run` in a step after `- uses: {STEP}` and give the step its name, or use an image of "
                "another registry.")


def test_each_builder_job_sets_its_builder_up_from_the_mirror_first_and_from_docker_hub_second():
    # The setup step of a builder always pulls the builder's own image through the Docker service, with the
    # runner's login, and does not take an image that is on the machine. So the first of two setup steps names the
    # image at the mirror and may fail; the second is the usual one and runs only then.
    builders = [(name, key, [step for step in job["steps"] if step.get("uses", "").startswith(BUILDER)])
                for name, _events, key, job in WITH_THE_STEP if any(step.get("uses", "").startswith(BUILDER) for step in job["steps"])]
    assert builders, "no job with the mirror step sets up a builder any more: this test holds nothing"
    for name, key, steps in builders:
        fix = (f"{name}: the job `{key}` does not set its builder up in the two steps of the section \"{SECTION}\" of the "
               "gates page. Fix: a first setup step with an `id`, `continue-on-error: true` and `driver-opts: "
               f"image={HOST}/moby/buildkit:buildx-stable-1`; a second with `if: steps.<id>.outcome == 'failure'` and no "
               "`driver-opts`; both with the mirror in `buildkitd-config-inline`.")
        assert len(steps) == 2, fix
        first, second = steps
        assert first.get("continue-on-error") is True, fix
        assert first["with"]["driver-opts"] == f"image={HOST}/moby/buildkit:buildx-stable-1", fix
        assert second.get("if") == f"steps.{first['id']}.outcome == 'failure'", fix
        assert "continue-on-error" not in second, fix
        assert "driver-opts" not in second["with"], fix
        assert first["uses"] == second["uses"], fix
        for step in steps:
            assert step["with"]["buildkitd-config-inline"] == BUILDERS_MIRROR, fix


def test_the_mirror_is_one_address_with_https_and_no_path():
    assert re.fullmatch(r"https://[a-z0-9.-]+", mirror.MIRROR)


# --- the script -----------------------------------------------------------------------------------------------------

@pytest.mark.parametrize("before", ["", "   \n", "{}", '{"exec-opts": ["native.cgroupdriver=cgroupfs"], "cgroup-parent": "/actions_job"}',
                                    '{"registry-mirrors": ["https://another.example"], "debug": true}',
                                    '{"insecure-registries": ["10.0.0.1:5000"], "features": {"containerd-snapshotter": true}}'])
def test_the_settings_get_the_one_mirror_and_every_other_key_stays(before):
    old = json.loads(before) if before.strip() else {}
    new = json.loads(mirror.with_the_mirror(before))
    assert new["registry-mirrors"] == ["https://mirror.gcr.io"]
    assert {key: value for key, value in new.items() if key != "registry-mirrors"} == \
        {key: value for key, value in old.items() if key != "registry-mirrors"}


def filtered(text):
    """Run the script as the step runs it: the settings go in as text, the new settings come out as text."""
    return subprocess.run([sys.executable, str(SCRIPT)], input=text, capture_output=True, text=True, timeout=60, check=False)


def test_with_no_settings_as_on_a_runner_the_script_prints_the_one_key():
    done = filtered("")
    assert done.returncode == 0, done.stderr
    assert json.loads(done.stdout) == {"registry-mirrors": ["https://mirror.gcr.io"]}


def test_settings_that_are_there_keep_their_keys():
    done = filtered('{"debug": true, "dns": ["10.0.0.2"]}')
    assert done.returncode == 0, done.stderr
    assert json.loads(done.stdout) == {"debug": True, "dns": ["10.0.0.2"], "registry-mirrors": ["https://mirror.gcr.io"]}


@pytest.mark.parametrize("content", ["[1]", '"text"', "7", "null", "{not json", '{"a": 1} trailing'])
def test_settings_that_are_not_one_object_print_nothing_and_the_message_says_the_fix(content):
    done = filtered(content)
    assert done.returncode == 1
    assert done.stdout == ""
    assert "were not changed" in done.stderr
    assert "Fix:" in done.stderr


def test_the_script_opens_no_file():
    # The step reads the settings file and puts the new one in its place. A script that took the path of a file
    # from its caller would write wherever the caller says.
    source = SCRIPT.read_text(encoding="utf-8")
    for word in ("open(", "pathlib", "read_text", "write_text", "os.path"):
        assert word not in source, f"scripts/docker_hub_mirror.py uses `{word}`: it is a filter from its input to its output"


@pytest.mark.parametrize("named, has", [
    ('["https://mirror.gcr.io/"]', True), ('["https://mirror.gcr.io"]', True), ('["https://a.example/", "https://mirror.gcr.io/"]', True),
    ("[]", False), ("null", False), ("", False), ("not json", False), ('"https://mirror.gcr.io/"', False),
    ('["https://mirror.gcr.io.other.example/"]', False), ('["http://mirror.gcr.io/"]', False), ('[["https://mirror.gcr.io/"]]', False),
])
def test_the_running_service_has_the_mirror_only_when_it_names_it(named, has):
    assert mirror.asked_first(named) is has
    done = subprocess.run([sys.executable, str(SCRIPT), "--asked-first", named], capture_output=True, text=True, timeout=60, check=False)
    assert done.returncode == (0 if has else 1)


@pytest.mark.parametrize("image, there", [
    ("ubuntu:22.04", "mirror.gcr.io/library/ubuntu:22.04"), ("debian:12", "mirror.gcr.io/library/debian:12"),
    ("rockylinux/rockylinux:9", "mirror.gcr.io/rockylinux/rockylinux:9"),
    ("docker.io/library/ubuntu:24.04", "mirror.gcr.io/library/ubuntu:24.04"),
    ("moby/buildkit:buildx-stable-1", "mirror.gcr.io/moby/buildkit:buildx-stable-1"),
])
def test_an_image_of_docker_hub_has_its_name_at_the_mirror(image, there):
    assert mirror.at_the_mirror(image) == there
    done = subprocess.run([sys.executable, str(SCRIPT), "--at-the-mirror", image], capture_output=True, text=True, timeout=60, check=False)
    assert (done.returncode, done.stdout) == (0, there + "\n")


@pytest.mark.parametrize("image", ["ghcr.io/inferstep/atlas-sandbox:dev", "localhost:5000/x:1", "ubuntu", "ubuntu:", ":22.04",
                                   "python:3.13-slim@sha256:" + "a" * 64, "a/b/c:1", "ubuntu:22.04; true", "ubuntu:22.04 debian:12",
                                   "UBUNTU:22.04", "../ubuntu:22.04", "$(id):1", "ubuntu:22.04\n", ""])
def test_another_name_is_not_asked_of_the_mirror_and_the_message_says_the_fix(image):
    with pytest.raises(ValueError, match="is not a name of Docker Hub with a tag"):
        mirror.at_the_mirror(image)
    done = subprocess.run([sys.executable, str(SCRIPT), "--at-the-mirror", image], capture_output=True, text=True, timeout=60, check=False)
    assert done.returncode == 1
    assert done.stdout == ""
    assert "Fix:" in done.stderr


@pytest.mark.parametrize("arguments", [["a file"], ["--other", "x"], ["a", "b"], ["--asked-first"], ["--at-the-mirror"]])
def test_another_call_of_the_script_writes_nothing_and_says_how_it_is_called(tmp_path, arguments):
    done = subprocess.run([sys.executable, str(SCRIPT), *arguments], input="{}", capture_output=True, text=True, timeout=60, check=False,
                          cwd=tmp_path)
    assert done.returncode == 1
    assert done.stdout == ""
    assert "usage:" in done.stderr
    assert list(tmp_path.iterdir()) == []


# --- the step itself, run with stand-ins for sudo, the service and docker ----------------------------------------

def the_steps():
    loaded = yaml.safe_load(ACTION.read_text(encoding="utf-8"))
    return loaded, loaded["runs"]["steps"]


def stand_in(folder, name, body):
    path = folder / name
    path.write_text("#!/bin/sh\n" + body + "\n", encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


INFO = 'echo \'["https://mirror.gcr.io/"]\''


RUNS_NOTHING = 'echo "sudo $*" >> "$CALLS"\nexit 97'


def run_a_step(tmp_path, number, restart="exit 0", docker=INFO, content=None, images="", settings=None, sudo='exec "$@"'):
    """Run the shell of one step of the action. `sudo` runs its command as it is, `systemctl` and `docker` answer as
    the test says, and the settings file is one of the test's own, in the folder for temporary files.

    A test that names a file outside that folder gives `sudo` as RUNS_NOTHING: when the step does not refuse the
    name, the test is red and still nothing was done to that file."""
    tools = tmp_path / "tools"
    tools.mkdir()
    (tmp_path / "temporary").mkdir()
    settings = tmp_path / "temporary" / "docker" / "daemon.json" if settings is None else settings
    if content is not None:
        settings.parent.mkdir(parents=True, exist_ok=True)
        settings.write_text(content, encoding="utf-8")
    stand_in(tools, "sudo", sudo)
    stand_in(tools, "systemctl", f'echo "systemctl $*" >> "{tmp_path}/calls"\n{restart}')
    stand_in(tools, "docker", f'echo "docker $*" >> "{tmp_path}/calls"\n{docker}')
    stand_in(tools, "journalctl", 'echo "a line of the service"')
    stand_in(tools, "sleep", "exit 0")
    stand_in(tools, "python3", f'exec "{sys.executable}" "$@"')
    _loaded, steps = the_steps()
    script = tmp_path / "step.sh"
    script.write_text(steps[number]["run"], encoding="utf-8")
    environment = {"PATH": f"{tools}{os.pathsep}/usr/bin{os.pathsep}/bin", "GITHUB_ACTION_PATH": str(ACTION.parent),
                   "DOCKER_SETTINGS_FILE": str(settings), "HOME": str(tmp_path), "IMAGES": images,
                   "TMPDIR": str(tmp_path / "temporary"), "CALLS": str(tmp_path / "calls")}
    done = subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=120, check=False, env=environment, cwd=tmp_path)
    calls = (tmp_path / "calls").read_text(encoding="utf-8").splitlines() if (tmp_path / "calls").exists() else []
    return done, calls, settings


def test_the_first_step_sets_the_mirror_starts_the_service_again_and_asks_it(tmp_path):
    done, calls, settings = run_a_step(tmp_path, 0)
    assert done.returncode == 0, done.stdout + done.stderr
    assert json.loads(settings.read_text(encoding="utf-8")) == {"registry-mirrors": ["https://mirror.gcr.io"]}
    assert calls == ["systemctl restart docker", "docker info --format {{json .RegistryConfig.Mirrors}}"]
    assert "The Docker service asks the mirror first and Docker Hub second." in done.stdout
    assert "::error::" not in done.stdout
    # The file that the step made for the new settings is gone again.
    assert sorted(path.name for path in (tmp_path / "temporary").iterdir()) == ["docker"]
    assert sorted(path.name for path in settings.parent.iterdir()) == ["daemon.json"]


def test_the_first_step_keeps_the_other_settings_of_a_file_that_is_there(tmp_path):
    done, _calls, settings = run_a_step(tmp_path, 0, content='{"debug": true, "dns": ["10.0.0.2"]}\n')
    assert done.returncode == 0, done.stdout + done.stderr
    assert json.loads(settings.read_text(encoding="utf-8")) == {"debug": True, "dns": ["10.0.0.2"], "registry-mirrors": ["https://mirror.gcr.io"]}
    assert stat.S_IMODE(settings.stat().st_mode) == 0o644


def test_when_the_service_does_not_start_again_the_step_fails_and_nothing_asks_docker(tmp_path):
    done, calls, _settings = run_a_step(tmp_path, 0, restart="exit 1")
    assert done.returncode == 1
    assert calls == ["systemctl restart docker"]
    assert "::error::The Docker service did not start again" in done.stdout
    assert "a line of the service" in done.stdout


def test_when_the_service_gives_no_answer_after_its_start_the_step_fails(tmp_path):
    done, calls, _settings = run_a_step(tmp_path, 0, docker="exit 1")
    assert done.returncode == 1
    assert calls.count("docker info --format {{json .RegistryConfig.Mirrors}}") == 30
    assert "::error::The Docker service gave no answer in 30 seconds" in done.stdout


@pytest.mark.parametrize("docker", ["echo '[]'", "echo null", "echo '[\"https://another.example/\"]'"])
def test_when_the_running_service_does_not_name_the_mirror_the_step_fails(tmp_path, docker):
    done, _calls, _settings = run_a_step(tmp_path, 0, docker=docker)
    assert done.returncode == 1
    assert "::error::The Docker service runs again and does not name the mirror" in done.stdout


def test_when_the_settings_file_is_not_one_object_the_step_fails_before_it_touches_the_service(tmp_path):
    done, calls, settings = run_a_step(tmp_path, 0, content="[1]")
    assert done.returncode == 1
    assert calls == []
    assert settings.read_text(encoding="utf-8") == "[1]"
    assert "::error::The settings file of the Docker service could not get the mirror" in done.stdout


def test_the_second_step_pulls_each_image_by_the_mirrors_name_and_gives_it_the_name_of_docker_hub(tmp_path):
    done, calls, _settings = run_a_step(tmp_path, 1, docker='case "$1" in image) echo "[\\"a digest\\"]";; esac', images="ubuntu:22.04\nrockylinux/rockylinux:9")
    assert done.returncode == 0, done.stdout + done.stderr
    assert calls == [
        "docker pull --quiet mirror.gcr.io/library/ubuntu:22.04", "docker tag mirror.gcr.io/library/ubuntu:22.04 ubuntu:22.04",
        "docker image inspect ubuntu:22.04 --format {{json .RepoDigests}}",
        "docker pull --quiet mirror.gcr.io/rockylinux/rockylinux:9", "docker tag mirror.gcr.io/rockylinux/rockylinux:9 rockylinux/rockylinux:9",
        "docker image inspect rockylinux/rockylinux:9 --format {{json .RepoDigests}}"]
    assert 'From the mirror: `ubuntu:22.04` is `["a digest"]`' in done.stdout
    assert "::notice" not in done.stdout


def test_when_the_mirror_does_not_give_an_image_the_step_says_so_and_the_job_goes_on(tmp_path):
    done, calls, _settings = run_a_step(tmp_path, 1, docker='[ "$1" != pull ]', images="ubuntu:22.04 debian:12")
    assert done.returncode == 0, done.stdout + done.stderr
    assert calls == ["docker pull --quiet mirror.gcr.io/library/ubuntu:22.04", "docker pull --quiet mirror.gcr.io/library/debian:12"]
    assert done.stdout.count("::notice title=not from the mirror::The mirror did not give `") == 2
    assert "The job's own command pulls it from Docker Hub." in done.stdout
    assert "::error::" not in done.stdout


def test_with_no_image_given_the_second_step_asks_docker_for_nothing(tmp_path):
    done, calls, _settings = run_a_step(tmp_path, 1)
    assert done.returncode == 0, done.stdout + done.stderr
    assert calls == []


@pytest.mark.parametrize("images", ["ghcr.io/inferstep/atlas-sandbox:dev", "ubuntu", "ubuntu:22.04 python@sha256:" + "a" * 64])
def test_an_image_that_the_mirror_cannot_be_asked_for_fails_the_step(tmp_path, images):
    done, _calls, _settings = run_a_step(tmp_path, 1, images=images)
    assert done.returncode == 1
    assert "::error::The step was given an image that it cannot ask the mirror for" in done.stdout


@pytest.mark.parametrize("named", ["{here}/outside/daemon.json", "{here}/temporary/../outside/daemon.json",
                                   "{here}/temporary/docker/../../outside.json", "/etc/passwd", "/etc/docker/other.json",
                                   "/etc/docker/daemon.json/../../passwd", "daemon.json", "./daemon.json"])
def test_the_step_writes_no_other_file_than_the_settings_of_the_service_or_one_in_the_temporary_folder(tmp_path, named):
    done, calls, _settings = run_a_step(tmp_path, 0, settings=Path(named.format(here=tmp_path)), sudo=RUNS_NOTHING)
    assert done.returncode == 1
    assert "::error::This step writes the settings file of the Docker service and no other file" in done.stdout
    assert calls == []
    assert sorted(path.name for path in tmp_path.iterdir()) == ["step.sh", "temporary", "tools"]


def test_with_no_file_named_the_step_takes_the_settings_file_of_the_service():
    _loaded, steps = the_steps()
    assert 'settings="${DOCKER_SETTINGS_FILE:-/etc/docker/daemon.json}"\n' in steps[0]["run"]
    assert steps[0]["run"].count("/etc/docker/daemon.json") == 2


def test_each_error_of_the_action_says_the_fix_and_ends_the_step():
    loaded, steps = the_steps()
    assert loaded["runs"]["using"] == "composite"
    assert [set(step) for step in steps] == [{"name", "shell", "run"}, {"name", "shell", "env", "run"}]
    lines = [line for step in steps for line in step["run"].splitlines()]
    errors = [n for n, line in enumerate(lines) if "::error::" in line]
    assert len(errors) == 6
    for n in errors:
        assert "Fix:" in lines[n], lines[n]
        assert "exit 1" in [line.strip() for line in lines[n + 1:n + 4]], lines[n]
    # A value of the run reaches the shell through the environment only, and the action asks for no secret.
    assert steps[1]["env"] == {"IMAGES": "${{ inputs.images }}"}
    assert all("${{" not in step["run"] for step in steps)
    assert set(loaded["inputs"]) == {"images"}
    assert "secrets" not in ACTION.read_text(encoding="utf-8")


# --- the gates page -----------------------------------------------------------------------------------------------

def the_section():
    page = PAGE.read_text(encoding="utf-8")
    start = page.index(f"## {SECTION}\n")
    end = page.find("\n## ", start + 1)
    return " ".join(page[start:end if end != -1 else len(page)].split())


def test_the_page_has_the_section_that_the_script_and_the_workflow_files_name():
    section = the_section()
    assert HOST in section
    for path in [SCRIPT, *(WORKFLOWS / name for name in sorted({found[0] for found in WITH_THE_STEP}))]:
        assert f'"{SECTION}"' in " ".join(path.read_text(encoding="utf-8").replace("#", " ").split()), path.name


def test_the_page_gives_the_number_of_jobs_that_the_workflow_files_have():
    number = sum(len(copies_of(job)) for _name, _events, _key, job in WITH_THE_STEP)
    assert f"{number} jobs build or run an image of Docker Hub" in the_section(), (
        f"the workflow files have {number} jobs with the mirror step, and the section \"{SECTION}\" of the gates page "
        "gives another number. Fix: give the page the number and the names of the jobs.")


def test_the_page_names_the_images_that_run_by_tag_and_says_that_they_are_the_mirrors_copy():
    section = the_section()
    by_tag = set()
    for _name, _events, _key, job in WITH_THE_STEP:
        (step,) = the_step_of(job)
        for values in copies_of(job):
            by_tag.update(filled(str(step.get("with", {}).get("images", "")), values).split())
    assert by_tag == {"ubuntu:22.04", "ubuntu:24.04", "debian:12", "rockylinux/rockylinux:9"}
    for image in sorted(by_tag):
        assert f"`{image}`" in section, f"the section \"{SECTION}\" does not name `{image}`, which a job runs by its tag"
    assert "run the mirror's copy of each tag, which can be older than the one of Docker Hub" in section
    assert "prints the digest" in section
