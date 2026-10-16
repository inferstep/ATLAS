#!/usr/bin/env python3
"""The images of the nightly run: names of the run's own for them, and the room that they need.

The file beside this one, scripts/nightly_run.py, loads it. It starts nothing by itself.

The server of the nightly run has a usual stack too, and other images that have to stay as they are. So the run:

  - moves no tag of the server. It reads the digest of `<image>:dev` from the registry with no pull, pulls the
    image by that digest, and gives it a name of its own, `localhost/atlas-nightly/<image>:night`. A name that
    starts with `localhost/` is in no registry. The compose file of the run names these images, with
    `pull_policy: never`;
  - keeps one set. When the registry has another image for a service, the run first removes its last one, by the
    two names that the run itself gave it: the name of its own, and the name by digest that its pull left. It
    removes by no other name and never prunes. Docker deletes a layer only when no other image uses it, so an
    image of another name keeps every layer;
  - leaves a name by digest alone while a tag of the same repository names the same image. With such a name
    Docker removes those tags too (tried with Docker 29 and the image store of containerd), and a tag like
    `<image>:dev` or `<image>:eval` is not the run's own. The name stays written down, and a later run removes
    it when no such tag is on the image any more;
  - looks at the free room first. A look of the timer that finds too little room on Docker's disk starts nothing,
    and the usual stack is not stopped. The look wants a reserve before every run, and the room of a first set on
    top of it only while the run has not written down a pull for each of its images.

What the run pulled and has not removed is written down in <dir>/night-images.json: for each service, the names
by digest, the newest last.
"""
from __future__ import annotations

import json
import os
import re
from pathlib import Path

REGISTRY, TAG = "ghcr.io/inferstep", "dev"
# The service of the compose file -> the image it runs.
IMAGES = {"llama-server": "atlas-llama", "geometric-lens": "atlas-lens", "v3-service": "atlas-v3",
          "sandbox": "atlas-sandbox", "atlas-proxy": "atlas-proxy"}
# The names of this run's own: of this machine only, so Docker cannot pull another image under one of them.
OWN = "localhost/atlas-nightly"
REVISION = "org.opencontainers.image.revision"
A_DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
RECORD = "night-images.json"
# Where Docker keeps its images: its own folder, and the folder of containerd, which holds them where Docker uses
# containerd's image store. A folder of this list that is not there is passed over.
FOLDERS = ("/var/lib/docker", "/var/lib/containerd")
# The room, in GB as `df -h` counts them, that a look wants free before it starts a run.
# The first set: the five images are about 4 GB compressed in the registry and 13 GB unpacked, and the image store
# of containerd keeps both. A machine that has some of their layers already needs less.
FIRST_SET_GB = 17.0
# The reserve, before every run: for what the containers of the stack write, and for an image that grew. It is a
# choice, not a measure.
RESERVE_GB = 5.0


def own_name(service: str) -> str:
    """The name of this run's own for the registry's image of a service."""
    return f"{OWN}/{IMAGES[service]}:night"


def built_name(service: str, stamp: str, commit: str) -> str:
    """The name of an image that a run builds: with the start time of the run and the commit."""
    return f"{OWN}/{IMAGES[service]}:{stamp}-{commit[:12]}"


def by_digest(service: str, digest: str) -> str:
    """The name that a pull by digest leaves on the image of a service. It has no tag."""
    return f"{REGISTRY}/{IMAGES[service]}@{digest}"


def is_a_pull_of(service: str, name: str) -> bool:
    """Whether a name is the whole name by digest that a pull of this run leaves on the image of a service."""
    digest = name.rpartition("@")[2]
    return bool(A_DIGEST.fullmatch(digest)) and name == by_digest(service, digest)


def digest_in(printed: str) -> str:
    """The digest in what `docker buildx imagetools inspect --format '{{json .Manifest}}'` printed, or nothing."""
    try:
        digest = json.loads(printed)["digest"]
    except (ValueError, KeyError, TypeError):
        return ""
    return digest if isinstance(digest, str) and A_DIGEST.fullmatch(digest) else ""


def earlier_pulls(folder: Path) -> dict:
    """What the run wrote down: for each service the names by digest that it pulled and has not removed. A name of
    the file is taken only when it is such a name for that service, because the run gives it to `docker image rm`."""
    try:
        written = json.loads((folder / RECORD).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    if not isinstance(written, dict):
        return {}
    return {service: [name for name in names if isinstance(name, str) and is_a_pull_of(service, name)]
            for service, names in written.items() if service in IMAGES and isinstance(names, list)}


def remove_own_name(own: str, must, ended) -> str:
    """Remove the run's own name from the image that it stands on. Gives what happened, in words."""
    if ended(f"the look for {own}", ["docker", "image", "inspect", "--format", "{{.Id}}", own], 60).returncode != 0:
        return "there was none"
    must(f"the removal of {own}", ["docker", "image", "rm", own], 300)
    return "yes"


def remove_pull(name: str, must, ended) -> str:
    """Remove the name by digest that a pull of this run left, unless a tag of the same repository names the same
    image: Docker would take that tag away with it. Gives what happened, in words; `kept` when the name stays."""
    looked = ended(f"the look for {name}", ["docker", "image", "inspect", "--format", "{{json .RepoTags}}", name], 60)
    if looked.returncode != 0:
        return "there was none"
    try:
        names_of_the_image = [str(one) for one in json.loads(looked.stdout)]
    except (ValueError, TypeError):
        return "kept: what Docker said of the names of this image could not be read"
    repository = name.split("@")[0]
    tags = [one for one in names_of_the_image if "@" not in one and one.rpartition(":")[0] == repository]
    if tags:
        return f"kept: {', '.join(tags)} names the same image, and Docker would remove that tag too"
    must(f"the removal of {name}", ["docker", "image", "rm", name], 300)
    return "yes"


def take(folder: Path, services, must, ended, failed) -> tuple:
    """Bring the image of each service to the one that the registry has under the tag, under this run's own name.

    Gives two things. For each service: the registry's name, its digest, the run's own name and the commit that
    the image was built from. And for each name that was to be removed: what happened.

    The three last arguments come from scripts/nightly_run.py: `must` runs a command that has to pass and gives
    its output, `ended` runs one whose status is read here, and `failed` is the error of a step that failed.
    """
    pulls, found, removed = earlier_pulls(folder), {}, {}
    for service in services:
        tagged, own = f"{REGISTRY}/{IMAGES[service]}:{TAG}", own_name(service)
        digest = digest_in(must(f"the reading of the digest of {tagged}",
                                ["docker", "buildx", "imagetools", "inspect", tagged, "--format", "{{json .Manifest}}"], 120))
        if not digest:
            raise failed(f"the reading of the digest of {tagged}: the registry's answer names no digest")
        pulled, earlier = by_digest(service, digest), pulls.get(service, [])
        if earlier and pulled not in earlier:
            # The registry has another image. The last one goes before the new one comes, so the run holds one set.
            removed[own] = remove_own_name(own, must, ended)
        for name in earlier:
            if name != pulled:
                removed[name] = remove_pull(name, must, ended)
        must(f"the pull of {pulled}", ["docker", "pull", "--quiet", pulled], 1200)
        must(f"the naming of {pulled}", ["docker", "tag", pulled, own], 60)
        pulls[service] = [name for name in earlier if name != pulled and removed[name].startswith("kept")] + [pulled]
        (folder / RECORD).write_text(json.dumps(pulls, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        about = json.loads(must(f"the reading of {own}", ["docker", "image", "inspect", own], 60))[0]
        found[service] = {"image": tagged, "digest": digest, "name": own,
                          "commit": ((about.get("Config") or {}).get("Labels") or {}).get(REVISION, "")}
    return found, removed


def compose_file(built: dict) -> str:
    """The compose file that names the image of each service: the one that this run built, or else the run's own
    name for the registry's image. JSON is YAML. An image of this run is never looked for in a registry."""
    return json.dumps({"services": {service: {"image": built.get(service) or own_name(service), "pull_policy": "never"}
                                    for service in IMAGES}}, indent=2) + "\n"


def room_wanted(folder: Path, first_set_gb: float, reserve_gb: float) -> float:
    """The free room that a look wants before it starts a run: the reserve, and the room of a first set on top of
    it while the run has not written down a pull for each of its images."""
    written = earlier_pulls(folder)
    has_the_set = all(written.get(service) for service in IMAGES)
    return reserve_gb if has_the_set else first_set_gb + reserve_gb


def room_fault(folders, needed_gb: float) -> str:
    """Why a look starts nothing for the room on Docker's disk, or nothing.

    The room is the smallest free room of the folders. With no folder named, the folders of FOLDERS are looked at,
    and one that is not there is passed over. A folder that the settings name has to be there.
    """
    free = []
    for folder in folders or FOLDERS:
        try:
            about = os.statvfs(folder)
        except OSError:
            if folders:
                return (f"nothing is started: the free room of Docker's disk could not be read at `{folder}`. Fix: "
                        "name the folder of Docker's images with --docker-folder")
            continue
        free.append(about.f_bavail * about.f_frsize / 2**30)
    if not free:
        return ("nothing is started: the free room of Docker's disk could not be read: none of "
                f"{', '.join(FOLDERS)} is there. Fix: name the folder of Docker's images with --docker-folder")
    if min(free) >= needed_gb:
        return ""
    return f"nothing is started: too little room on Docker's disk ({int(min(free))} GB free, {needed_gb:g} GB needed)"
