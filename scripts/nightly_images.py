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
    top of it only while Docker has not an image under each of the run's own names;
  - reads the free room again before each pull. The registry says which layers the image has and how large each
    is, with no pull. When the layers that are not on this machine yet, and the reserve after them, do not fit,
    the run ends as not run.

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
# What a layer takes on disk, as a multiple of its compressed size: the image store of containerd keeps the layer
# compressed and unpacked, and the registry gives the compressed size only. The five images took 17.1 GB on disk
# for 4.18 GB compressed; one image alone took between 3.3 and 6.3 times its compressed size.
ON_DISK = 4.1


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


def take(folder: Path, services, must, ended, failed, hold_room) -> tuple:
    """Bring the image of each service to the one that the registry has under the tag, under this run's own name.

    Gives two things. For each service: the registry's name, its digest, the run's own name and the commit that
    the image was built from. And for each name that was to be removed: what happened.

    The four last arguments come from scripts/nightly_run.py: `must` runs a command that has to pass and gives
    its output, `ended` runs one whose status is read here, `failed` is the error of a step that failed, and
    `hold_room` ends the run as not run when Docker's disk has not the room of a step.
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
        # The room is read here, after the last image went: the removal frees room, and it can take away layers
        # that the new image has too.
        layers = layers_in_the_registry(service, pulled, must, failed)
        hold_room(f"the pull of {pulled}", room_on_disk(layers, layers_here(must, ended)))
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


# --- the room ----------------------------------------------------------------------------------------------------

def room_wanted(first_set_gb: float, reserve_gb: float, ended) -> float:
    """The free room that a look wants before it starts a run: the reserve, and the room of a first set on top of
    it while Docker has not an image under each of the run's own names. That is one question to Docker: its
    status is 0 only when every name is there."""
    own = [own_name(service) for service in IMAGES]
    asked = ended("the look for the run's own images", ["docker", "image", "inspect", "--format", "{{.Id}}", *own], 60)
    return reserve_gb if asked.returncode == 0 else first_set_gb + reserve_gb


def free_room(folders) -> tuple:
    """The free room on Docker's disk in GB, and nothing; or None, and why it could not be read.

    The room is the smallest free room of the folders. With no folder named, the folders of FOLDERS are looked at,
    and one that is not there is passed over. A folder that the settings name has to be there.
    """
    free = []
    for folder in folders or FOLDERS:
        try:
            about = os.statvfs(folder)
        except OSError:
            if folders:
                return None, (f"the free room of Docker's disk could not be read at `{folder}`. Fix: name the folder "
                              "of Docker's images with --docker-folder")
            continue
        free.append(about.f_bavail * about.f_frsize / 2**30)
    if not free:
        return None, ("the free room of Docker's disk could not be read: none of "
                      f"{', '.join(FOLDERS)} is there. Fix: name the folder of Docker's images with --docker-folder")
    return round(min(free), 1), ""


def room_fault(folders, needed_gb: float, what_for: str = "on Docker's disk") -> str:
    """Why the free room on Docker's disk is not enough, or nothing."""
    free, unread = free_room(folders)
    if unread:
        return unread
    if free >= needed_gb:
        return ""
    return f"too little room {what_for} ({free:.1f} GB free, {needed_gb:.1f} GB needed)"


def layers_in_the_registry(service: str, name: str, must, failed) -> list:
    """The layers of an image as the registry has it, lowest first: for each the id of the unpacked layer and its
    compressed size in bytes. Nothing is pulled. A name can hold images for several kinds of machine; the one for
    the kind of this Docker is read."""
    what = f"the reading of the layers of {name}"
    kind = must("the reading of Docker's kind of machine", ["docker", "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}"], 60).strip()
    try:
        index = json.loads(must(what, ["docker", "buildx", "imagetools", "inspect", "--raw", name], 120))
        of_this_kind = []
        for one in index["manifests"]:
            platform = one.get("platform") or {}
            if f"{platform.get('os')}/{platform.get('architecture')}" == kind:
                of_this_kind.append(one["digest"])
        if len(of_this_kind) != 1:
            raise failed(f"{what}: the registry has {len(of_this_kind)} images for {kind} under that name, not 1")
        image = by_digest(service, of_this_kind[0])
        sizes = [layer["size"] for layer in json.loads(must(what, ["docker", "buildx", "imagetools", "inspect", "--raw", image], 120))["layers"]]
        ids = json.loads(must(what, ["docker", "buildx", "imagetools", "inspect", image, "--format", "{{json .Image}}"], 120))["rootfs"]["diff_ids"]
    except (KeyError, TypeError, ValueError):
        raise failed(f"{what}: the registry's answer could not be read") from None
    if len(ids) != len(sizes):
        raise failed(f"{what}: the registry names {len(ids)} unpacked layers and {len(sizes)} compressed ones")
    return list(zip(ids, sizes))


def layers_here(must, ended) -> list:
    """For each image on this machine, the ids of its unpacked layers, lowest first."""
    images = sorted(set(must("the list of the images here", ["docker", "image", "ls", "--quiet", "--no-trunc"], 120).split()))
    if not images:
        return []
    # An image can go between the two commands. Then the status is not 0, and the lines of the others still count.
    said = ended("the reading of the layers here", ["docker", "image", "inspect", "--format", "{{json .RootFS.Layers}}", *images], 120)
    return [json.loads(line) for line in said.stdout.splitlines() if line.startswith("[")]


def room_on_disk(layers: list, stacks: list) -> float:
    """The room in GB that an image takes on disk when it comes: of its layers that are not here yet.

    A layer counts as here only together with every layer under it: Docker shares a layer between two images
    when the whole stack up to it is the same. `stacks` is what `layers_here` gives.
    """
    ids = [layer_id for layer_id, _size in layers]
    here = 0
    for stack in stacks:
        same = 0
        while same < len(ids) and same < len(stack) and ids[same] == stack[same]:
            same += 1
        here = max(here, same)
    return sum(size for _id, size in layers[here:]) * ON_DISK / 2**30


def room_of_a_build(service: str, must, failed) -> float:
    """The room in GB that the build of a service's image is given: that of the whole image that the registry has
    for the service, with no layer counted as here. What a build writes cannot be known before it; this is the
    careful side."""
    return room_on_disk(layers_in_the_registry(service, f"{REGISTRY}/{IMAGES[service]}:{TAG}", must, failed), [])
