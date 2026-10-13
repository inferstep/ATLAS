"""The images of the nightly run: no tag of the server moves, the run keeps one set, and a look asks for room first.

The first tests call scripts/nightly_images.py directly and read the docker commands that it gives. The last one
starts a whole night with the stand-ins of test_nightly_run.py.
"""
import datetime as dt
import json
import re
from types import SimpleNamespace

import pytest

from tests.infrastructure.test_nightly_run import Night, nightly

images = nightly.images
OLD, NEW = "sha256:" + "a" * 64, "sha256:" + "b" * 64
OWN = "localhost/atlas-nightly/atlas-proxy:night"
LAST = f"ghcr.io/inferstep/atlas-proxy@{OLD}"
TODAY = f"ghcr.io/inferstep/atlas-proxy@{NEW}"
EVAL = "ghcr.io/inferstep/atlas-proxy:eval"
BY_DIGEST = re.compile(r"ghcr\.io/inferstep/atlas-[a-z0-9]+@sha256:[0-9a-f]{64}")


FOR_AMD64 = "sha256:" + "f" * 64
# The image that the registry has in these tests: three layers, the lowest first, 4 GB compressed together.
LAYERS = [("sha256:low", 2 * 2**30), ("sha256:mid", 2**30), ("sha256:top", 2**30)]


class Docker:
    """Stands in for the ways the file of the images runs a command and asks for room. It writes each one down.
    The registry has the digest NEW; the image under it for linux/amd64 has the layers of LAYERS. `there` gives,
    for each name that is on the machine, the names that Docker lists for its image. `docker image ls` lists
    no image, so no layer is here."""

    def __init__(self, there=None):
        self.commands, self.there = [], there or {}

    def must(self, what, argv, limit):
        self.commands.append(" ".join(argv))
        if argv[1] == "version":
            return "linux/amd64\n"
        if argv[1] == "buildx" and "--raw" in argv:
            # One answer for both lists: the images under a name, and the layers of an image.
            return json.dumps({"manifests": [{"digest": FOR_AMD64, "platform": {"os": "linux", "architecture": "amd64"}},
                                             {"digest": OLD, "platform": {"os": "unknown", "architecture": "unknown"}}],
                               "layers": [{"size": size} for _id, size in LAYERS]})
        if argv[1] == "buildx" and "{{json .Image}}" in argv:
            return json.dumps({"rootfs": {"diff_ids": [layer_id for layer_id, _size in LAYERS]}})
        if argv[1] == "buildx":
            return json.dumps({"digest": NEW})
        if argv[1:3] == ["image", "inspect"]:
            return json.dumps([{"Config": {"Labels": {"org.opencontainers.image.revision": "c" * 40}}}])
        return ""

    def ended(self, what, argv, limit):
        self.commands.append(" ".join(argv))
        if argv[-1] not in self.there:
            return SimpleNamespace(returncode=1, stdout="")
        return SimpleNamespace(returncode=0, stdout=json.dumps(self.there[argv[-1]]))

    def hold_room(self, step, room):
        self.commands.append(f"room for {step}: {room:.1f} GB")


def test_an_image_is_pulled_by_its_digest_and_gets_the_runs_own_name(tmp_path):
    docker = Docker()
    found, removed = images.take(tmp_path, ["atlas-proxy"], docker.must, docker.ended, RuntimeError, docker.hold_room)
    # The tag `dev` is named once, in the question to the registry. Nothing is pulled by a tag, so no tag moves.
    assert docker.commands == [
        "docker buildx imagetools inspect ghcr.io/inferstep/atlas-proxy:dev --format {{json .Manifest}}",
        # The room of the pull: the layers of the image for this Docker's kind of machine, against the images here.
        # 4 GB compressed are given 4.1 times that on disk.
        "docker version --format {{.Server.Os}}/{{.Server.Arch}}",
        f"docker buildx imagetools inspect --raw {TODAY}",
        f"docker buildx imagetools inspect --raw ghcr.io/inferstep/atlas-proxy@{FOR_AMD64}",
        f"docker buildx imagetools inspect ghcr.io/inferstep/atlas-proxy@{FOR_AMD64} --format " + "{{json .Image}}",
        "docker image ls --quiet --no-trunc",
        f"room for the pull of {TODAY}: 16.4 GB",
        f"docker pull --quiet ghcr.io/inferstep/atlas-proxy@{NEW}",
        f"docker tag ghcr.io/inferstep/atlas-proxy@{NEW} {OWN}",
        f"docker image inspect {OWN}",
    ]
    assert found == {"atlas-proxy": {"image": "ghcr.io/inferstep/atlas-proxy:dev", "digest": NEW, "name": OWN, "commit": "c" * 40}}
    assert removed == {}
    assert json.loads((tmp_path / "night-images.json").read_text()) == {"atlas-proxy": [TODAY]}


KEPT = f"kept: {EVAL} names the same image, and Docker would remove that tag too"


@pytest.mark.parametrize("written, there, removed, written_after", [
    pytest.param([LAST], {OWN: [OWN, LAST], LAST: [OWN, LAST]}, {OWN: "yes", LAST: "yes"}, [TODAY],
                 id="the registry has another image"),
    pytest.param([LAST], {LAST: [LAST]}, {OWN: "there was none", LAST: "yes"}, [TODAY],
                 id="its own name is gone already"),
    pytest.param([TODAY], {OWN: [OWN, TODAY]}, {}, [TODAY],
                 id="the registry has the same image"),
    # Docker removes a tag of the same repository together with the name by digest, so the name stays.
    pytest.param([LAST], {OWN: [OWN, LAST, EVAL], LAST: [OWN, LAST, EVAL]}, {OWN: "yes", LAST: KEPT}, [LAST, TODAY],
                 id="a tag of the same repository names the last image"),
    pytest.param([LAST, TODAY], {OWN: [OWN, TODAY], LAST: [LAST, EVAL]}, {LAST: KEPT}, [LAST, TODAY],
                 id="that tag is still there a night later"),
    pytest.param([LAST, TODAY], {OWN: [OWN, TODAY], LAST: [LAST]}, {LAST: "yes"}, [TODAY],
                 id="that tag is gone a night later"),
    # A name of another repository on the same image is not in the way.
    pytest.param([LAST], {OWN: [OWN, LAST], LAST: [OWN, LAST, "localhost/another/atlas-proxy:eval"]}, {OWN: "yes", LAST: "yes"}, [TODAY],
                 id="a tag of another repository names the last image"),
    # A line of the file that is no pull of this run for this service is not given to Docker.
    pytest.param([EVAL], {OWN: [OWN], EVAL: [EVAL]}, {}, [TODAY],
                 id="the file names a tag"),
    pytest.param([f"ghcr.io/inferstep/atlas-llama@{OLD}"], {OWN: [OWN]}, {}, [TODAY],
                 id="the file names the image of another service"),
    pytest.param([f"example.org/inferstep/atlas-proxy@{OLD}"], {OWN: [OWN]}, {}, [TODAY],
                 id="the file names another registry"),
])
def test_the_last_image_goes_before_the_new_one_comes_and_only_by_a_name_that_the_run_gave_it(tmp_path, written, there, removed, written_after):
    (tmp_path / "night-images.json").write_text(json.dumps({"atlas-proxy": written}))
    docker = Docker(there)
    assert images.take(tmp_path, ["atlas-proxy"], docker.must, docker.ended, RuntimeError, docker.hold_room)[1] == removed
    removing = [command for command in docker.commands if " rm " in command or "prune" in command]
    assert removing == [f"docker image rm {name}" for name, said in removed.items() if said == "yes"]
    # The room is read after the last image went, and before the new one comes.
    room, pull = docker.commands.index(f"room for the pull of {TODAY}: 16.4 GB"), docker.commands.index(f"docker pull --quiet {TODAY}")
    assert all(docker.commands.index(command) < room < pull for command in removing)
    assert json.loads((tmp_path / "night-images.json").read_text()) == {"atlas-proxy": written_after}


@pytest.mark.parametrize("stacks, compressed_gb", [
    pytest.param([], 4, id="no image here has a layer of it"),
    pytest.param([["sha256:low", "sha256:another"]], 2, id="an image here stands on the same lowest layer"),
    pytest.param([["sha256:low"], ["sha256:low", "sha256:mid", "sha256:top", "sha256:more"]], 0, id="an image here has every layer"),
    pytest.param([["sha256:another", "sha256:low", "sha256:mid", "sha256:top"]], 4, id="the same layers on another lowest layer do not count"),
])
def test_the_room_of_an_image_is_that_of_its_layers_that_are_not_here_yet(stacks, compressed_gb):
    assert images.room_on_disk(LAYERS, stacks) == pytest.approx(compressed_gb * 4.1)


@pytest.mark.parametrize("folder, needed, says", [
    ("", 0, ""),
    ("", 99999, r"too little room on Docker's disk \([\d.]+ GB free, 99999\.0 GB needed\)"),
    ("not there", 0, r"the free room of Docker's disk could not be read at `.*not there`\. Fix: .*"),
])
def test_the_free_room_of_dockers_disk_is_read_at_its_folder(tmp_path, folder, needed, says):
    assert re.fullmatch(says, images.room_fault([str(tmp_path / folder)], needed))


def test_with_no_folder_named_and_none_of_the_usual_ones_there_the_room_cannot_be_read(tmp_path, monkeypatch):
    monkeypatch.setattr(images, "FOLDERS", (str(tmp_path / "one"), str(tmp_path / "two")))
    assert images.room_fault([], 0).startswith("the free room of Docker's disk could not be read: none of ")
    (tmp_path / "two").mkdir()
    assert images.room_fault([], 0) == ""


TOO_LITTLE = r"nothing is started: too little room on Docker's disk \([\d.]+ GB free, 99999\.0 GB needed\)"
OWN_NAMES = [f"localhost/atlas-nightly/{image}:night" for image in images.IMAGES.values()]


@pytest.mark.parametrize("docker_has_the_set, first_set, reserve, due, why", [
    pytest.param(False, "0", "0", "night", "the night is due",
                 id="no set yet, and room for both"),
    # 60000 and 39999 add up to the 99999 of the message.
    pytest.param(False, "60000", "39999", None, TOO_LITTLE,
                 id="no set yet: the room of a first set and the reserve are wanted together"),
    pytest.param(True, "99999", "0", "night", "the night is due",
                 id="the set is there: the room of a first set is not wanted again"),
    pytest.param(True, "0", "99999", None, TOO_LITTLE,
                 id="the set is there: the reserve is wanted"),
])
def test_a_look_wants_the_room_of_a_first_set_while_the_run_has_none_and_a_reserve_before_every_run(
        tmp_path, monkeypatch, docker_has_the_set, first_set, reserve, due, why):
    # The stand-in for Docker answers the look's question with status 0, and with status 1 when the plan fails it.
    night = Night(tmp_path, fail=[] if docker_has_the_set else ["image inspect --format {{.Id}} localhost/atlas-nightly/"])
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    noon = dt.datetime(2026, 10, 8, 12, 30, tzinfo=dt.timezone.utc)
    settings = nightly.parse(["--dir", str(night.dir), "--lock", str(night.lock), "--tick", "--look", "--night-hour", "12",
                              "--docker-folder", str(tmp_path), "--first-set-gb", first_set, "--reserve-gb", reserve])
    # With too little room the night is not due, so the usual stack is not stopped.
    mode, commit, says = nightly.what_is_due(settings, noon)
    assert (mode, commit) == (due, None)
    assert re.fullmatch(why, says)
    # One question to Docker, with each of the run's five own names: its status is 0 only when every name is there.
    assert [call["args"] for call in night.calls() if call["tool"] == "docker"] == [["image", "inspect", "--format", "{{.Id}}", *OWN_NAMES]]


def test_the_unit_of_the_server_gives_no_number_so_the_numbers_of_the_script_hold_there(tmp_path):
    settings = nightly.parse(["--dir", str(tmp_path)])
    assert (settings.first_set_gb, settings.reserve_gb) == (17, 5)


def test_no_command_of_a_night_names_an_image_of_the_registry_by_a_tag_or_removes_one_that_is_not_its_own(tmp_path):
    night = Night(tmp_path)
    # An earlier night pulled another image of the proxy, so this night has one to remove.
    (night.dir / "night-images.json").write_text(json.dumps({"atlas-proxy": [LAST]}))
    night.run()
    assert night.report["result"] == "passed", night.report["result"]
    docker = [call["args"] for call in night.calls() if call["tool"] == "docker"]
    for args in docker:
        if args[:3] == ["buildx", "imagetools", "inspect"]:
            continue  # the question to the registry: it changes nothing on the machine
        assert all(BY_DIGEST.fullmatch(word) for word in args if word.split("/")[0] == "ghcr.io"), args
        assert not {"prune", "rmi", "push", "login"} & set(args), args
        if args[0] == "tag":
            assert args[2].startswith("localhost/atlas-nightly/"), args
    assert [args[2:] for args in docker if args[:2] == ["image", "rm"]] == [[OWN], [LAST]]
    # The stack runs the five images under the run's own names.
    services = json.loads(next(night.dir.glob("reports/*/own-images.yml")).read_text())["services"]
    assert {about["image"] for about in services.values()} == {f"localhost/atlas-nightly/{image}:night" for image in nightly.IMAGES.values()}
    assert {about["pull_policy"] for about in services.values()} == {"never"}
