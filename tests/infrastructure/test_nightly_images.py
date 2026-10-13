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
BY_DIGEST = re.compile(r"ghcr\.io/inferstep/atlas-[a-z0-9]+@sha256:[0-9a-f]{64}")


class Docker:
    """Stands in for the two ways the file of the images runs a command. It writes each command down. The
    registry has the digest NEW, and the images named in `there` are on the machine."""

    def __init__(self, there=()):
        self.commands, self.there = [], set(there)

    def must(self, what, argv, limit):
        self.commands.append(" ".join(argv))
        if argv[1] == "buildx":
            return json.dumps({"digest": NEW})
        if argv[1:3] == ["image", "inspect"]:
            return json.dumps([{"Config": {"Labels": {"org.opencontainers.image.revision": "c" * 40}}}])
        return ""

    def ended(self, what, argv, limit):
        self.commands.append(" ".join(argv))
        return SimpleNamespace(returncode=0 if argv[-1] in self.there else 1)


def test_an_image_is_pulled_by_its_digest_and_gets_the_runs_own_name(tmp_path):
    docker = Docker()
    found, removed = images.take(tmp_path, ["atlas-proxy"], docker.must, docker.ended, RuntimeError)
    # The tag `dev` is named once, in the question to the registry. Nothing is pulled by a tag, so no tag moves.
    assert docker.commands == [
        "docker buildx imagetools inspect ghcr.io/inferstep/atlas-proxy:dev --format {{json .Manifest}}",
        f"docker pull --quiet ghcr.io/inferstep/atlas-proxy@{NEW}",
        f"docker tag ghcr.io/inferstep/atlas-proxy@{NEW} {OWN}",
        f"docker image inspect {OWN}",
    ]
    assert found == {"atlas-proxy": {"image": "ghcr.io/inferstep/atlas-proxy:dev", "digest": NEW, "name": OWN, "commit": "c" * 40}}
    assert removed == {}
    assert json.loads((tmp_path / "night-images.json").read_text()) == {"atlas-proxy": f"ghcr.io/inferstep/atlas-proxy@{NEW}"}


@pytest.mark.parametrize("written, there, removed", [
    ({"atlas-proxy": LAST}, [OWN, LAST], {OWN: "yes", LAST: "yes"}),
    ({"atlas-proxy": LAST}, [LAST], {OWN: "there was none", LAST: "yes"}),
    ({"atlas-proxy": f"ghcr.io/inferstep/atlas-proxy@{NEW}"}, [OWN], {}),
    ({"atlas-proxy": "ghcr.io/inferstep/atlas-proxy:eval"}, [OWN, "ghcr.io/inferstep/atlas-proxy:eval"], {}),
    ({"atlas-proxy": f"ghcr.io/inferstep/atlas-llama@{OLD}"}, [OWN, f"ghcr.io/inferstep/atlas-llama@{OLD}"], {}),
    ({"atlas-proxy": f"example.org/inferstep/atlas-proxy@{OLD}"}, [OWN], {}),
], ids=["the registry has another image", "its own name is gone already", "the registry has the same image",
        "the file names a tag", "the file names the image of another service", "the file names another registry"])
def test_the_last_image_goes_before_the_new_one_comes_and_only_by_a_name_that_the_run_gave_it(tmp_path, written, there, removed):
    (tmp_path / "night-images.json").write_text(json.dumps(written))
    docker = Docker(there)
    assert images.take(tmp_path, ["atlas-proxy"], docker.must, docker.ended, RuntimeError)[1] == removed
    removing = [command for command in docker.commands if " rm " in command or "prune" in command]
    assert removing == [f"docker image rm {name}" for name, said in removed.items() if said == "yes"]
    pull = docker.commands.index(f"docker pull --quiet ghcr.io/inferstep/atlas-proxy@{NEW}")
    assert all(docker.commands.index(command) < pull for command in removing)


@pytest.mark.parametrize("folder, needed, says", [
    ("", 0, ""),
    ("", 99999, r"nothing is started: too little room on Docker's disk \(\d+ GB free, 99999 GB needed\)"),
    ("not there", 0, r"nothing is started: the free room of Docker's disk could not be read at `.*not there`\. Fix: .*"),
])
def test_a_look_wants_room_on_dockers_disk(tmp_path, folder, needed, says):
    assert re.fullmatch(says, images.room_fault([str(tmp_path / folder)], needed))


def test_with_no_folder_named_and_none_of_the_usual_ones_there_a_look_starts_nothing(tmp_path, monkeypatch):
    monkeypatch.setattr(images, "FOLDERS", (str(tmp_path / "one"), str(tmp_path / "two")))
    assert images.room_fault([], 0).startswith("nothing is started: the free room of Docker's disk could not be read: none of ")
    (tmp_path / "two").mkdir()
    assert images.room_fault([], 0) == ""


def test_with_too_little_room_the_night_is_not_due_so_the_usual_stack_is_not_stopped(tmp_path):
    night = Night(tmp_path)
    noon = dt.datetime(2026, 10, 8, 12, 30, tzinfo=dt.timezone.utc)
    settings = ["--dir", str(night.dir), "--lock", str(night.lock), "--tick", "--look", "--night-hour", "12", "--docker-folder", str(tmp_path)]
    assert nightly.what_is_due(nightly.parse([*settings, "--room-gb", "0"]), noon) == ("night", None, "the night is due")
    mode, commit, why = nightly.what_is_due(nightly.parse([*settings, "--room-gb", "99999"]), noon)
    assert (mode, commit) == (None, None)
    assert re.fullmatch(r"nothing is started: too little room on Docker's disk \(\d+ GB free, 99999 GB needed\)", why)
    # The unit of the server gives no number, so the number of the script is the one that holds there.
    assert nightly.parse(["--dir", str(night.dir)]).room_gb == 20


def test_no_command_of_a_night_names_an_image_of_the_registry_by_a_tag_or_removes_one_that_is_not_its_own(tmp_path):
    night = Night(tmp_path)
    # An earlier night pulled another image of the proxy, so this night has one to remove.
    (night.dir / "night-images.json").write_text(json.dumps({"atlas-proxy": LAST}))
    night.run()
    assert night.report["result"] == "passed", night.report["result"]
    docker = [call["args"] for call in night.calls() if call["tool"] == "docker"]
    for args in docker:
        if args[:3] == ["buildx", "imagetools", "inspect"]:
            continue  # the question to the registry: it changes nothing on the machine
        assert all(BY_DIGEST.fullmatch(word) for word in args if word.startswith("ghcr.io/")), args
        assert not {"prune", "rmi", "push", "login"} & set(args), args
        if args[0] == "tag":
            assert args[2].startswith("localhost/atlas-nightly/"), args
    assert [args[2:] for args in docker if args[:2] == ["image", "rm"]] == [[OWN], [LAST]]
    # The stack runs the five images under the run's own names.
    services = json.loads(next(night.dir.glob("reports/*/own-images.yml")).read_text())["services"]
    assert {about["image"] for about in services.values()} == {f"localhost/atlas-nightly/{image}:night" for image in nightly.IMAGES.values()}
    assert {about["pull_policy"] for about in services.values()} == {"never"}
