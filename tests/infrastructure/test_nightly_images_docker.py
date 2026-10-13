"""The night's removal of an image, against a real Docker engine.

With a name by digest, Docker removes every tag of the same repository that names the same image (seen with
Docker 29 and the image store of containerd). On the server of the nightly run such a tag can be `<image>:dev` or
`<image>:eval`, and it is not the run's own. A stand-in for Docker cannot show what Docker does there. So this
one test asks Docker itself.

It pulls one small public image by its digest and gives it names that only this test makes. It removes each of
them by its own name at its end. It needs a Docker engine and the network, so the plain test jobs leave it out:
it is the group `docker` of scripts/tests_counted.py, and the job `sandbox tests (containerized)` runs it.
"""
import subprocess

from tests.infrastructure.test_nightly_run import nightly

images = nightly.images
REPOSITORY = "mirror.gcr.io/library/hello-world"
OWN = "localhost/atlas-test-of-the-removal/hello:night"
A_TAG_OF_THE_REPOSITORY = f"{REPOSITORY}:a-tag-of-this-test"
OF_ANOTHER_REPOSITORY = "localhost/atlas-test-of-the-removal/hello:eval"


def docker(*args):
    return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=300)


def is_there(name):
    return docker("image", "inspect", "--format", "{{.Id}}", name).returncode == 0


def test_the_removal_takes_no_tag_away_that_is_not_the_runs_own():
    # For the log of the job: Docker's behaviour here differs between its image stores.
    print(docker("info", "--format", "Docker {{.ServerVersion}}, storage driver {{.Driver}}").stdout.strip())
    listed = docker("image", "ls", "--format", "{{.Repository}}").stdout.split()
    assert REPOSITORY not in listed, f"{REPOSITORY} is on this machine already, so its names are not this test's own."
    asked = docker("buildx", "imagetools", "inspect", f"{REPOSITORY}:latest", "--format", "{{json .Manifest}}")
    by_digest = f"{REPOSITORY}@{images.digest_in(asked.stdout)}"
    try:
        # The image as a night has it: pulled by its digest, under a name of the run's own. And one name more,
        # as a server can have it: a tag of another repository.
        assert docker("pull", "--quiet", by_digest).returncode == 0, asked.stdout + asked.stderr
        for name in (OWN, OF_ANOTHER_REPOSITORY):
            assert docker("tag", by_digest, name).returncode == 0, name

        # The night's removal of its last image: the two names that the run gave it go. The other name stays.
        assert images.remove_own_name(OWN, nightly.must, nightly.ended) == "yes"
        assert images.remove_pull(by_digest, nightly.must, nightly.ended) == "yes"
        assert not is_there(OWN) and not is_there(by_digest)
        assert is_there(OF_ANOTHER_REPOSITORY)

        # The same once more, and now a tag of the same repository names the image too. The run's own name goes.
        # The name by digest stays: Docker would take the tag of the same repository away with it.
        assert docker("pull", "--quiet", by_digest).returncode == 0
        for name in (OWN, A_TAG_OF_THE_REPOSITORY):
            assert docker("tag", by_digest, name).returncode == 0, name
        assert images.remove_own_name(OWN, nightly.must, nightly.ended) == "yes"
        said = images.remove_pull(by_digest, nightly.must, nightly.ended)
        assert said == f"kept: {A_TAG_OF_THE_REPOSITORY} names the same image, and Docker would remove that tag too"
        assert not is_there(OWN)
        assert is_there(A_TAG_OF_THE_REPOSITORY) and is_there(OF_ANOTHER_REPOSITORY) and is_there(by_digest)
    finally:
        # Each name of this test goes by its own name. With the last tag of a repository on an image, Docker
        # removes that repository's name by digest too.
        for name in (OWN, A_TAG_OF_THE_REPOSITORY, OF_ANOTHER_REPOSITORY):
            docker("image", "rm", name)
        images.remove_pull(by_digest, nightly.must, nightly.ended)
