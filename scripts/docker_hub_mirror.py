#!/usr/bin/env python3
"""Give the Docker service of a runner a mirror of Docker Hub, and change nothing else of its settings.

A job that builds or runs an image of Docker Hub is red when Docker Hub refuses the pull for its limit, or when its
token service does not answer. The change under test is not at fault then. With a mirror in its settings the Docker
service asks the mirror first and Docker Hub second (docs/quality/gates.md, section "Docker Hub and the checks").

usage:
  docker_hub_mirror.py < the settings of the Docker service
      Reads the text of the settings file and prints it with the one key `registry-mirrors` set. Every other key
      stays as it is. An empty text gives settings with that one key. This script opens no file: the step that
      calls it reads the file and puts the new one in its place.
  docker_hub_mirror.py --asked-first '<the mirrors that `docker info` names, as JSON>'
      Says whether the running service has the mirror.
  docker_hub_mirror.py --at-the-mirror <an image of Docker Hub, by name and tag>
      Prints the name of that image at the mirror: `ubuntu:22.04` is `mirror.gcr.io/library/ubuntu:22.04`.

A runner of GitHub is logged in to Docker Hub. For `docker run` and `docker pull` the Docker service sends that
login to the mirror too, and the mirror refuses a login that it does not know. So the step pulls an image that a
job runs by the mirror's own name, with which no login goes, and gives it the name of Docker Hub.

Exit status: 0 done, or the service has the mirror; 1 not so, and then nothing is printed.
"""
from __future__ import annotations

import json
import re
import sys

# Google's public mirror of Docker Hub. The builder jobs name the same host in the settings of their builder.
MIRROR = "https://mirror.gcr.io"
KEY = "registry-mirrors"
# A name of Docker Hub as a job writes it: `ubuntu:22.04`, `rockylinux/rockylinux:9`, `docker.io/library/debian:12`.
PART = r"[a-z0-9]+(?:[._-][a-z0-9]+)*"
OF_THE_HUB = re.compile(rf"(?:docker\.io/)?(?P<path>{PART}(?:/{PART})?):(?P<tag>[A-Za-z0-9_][A-Za-z0-9_.-]{{0,127}})")


def with_the_mirror(text: str) -> str:
    """The settings with the one mirror, as the text of the file. Raises ValueError or TypeError when the text is
    not one JSON object: then nothing can be said about the other settings, and nothing is written."""
    settings = json.loads(text) if text.strip() else {}
    if not isinstance(settings, dict):
        raise TypeError(f"it holds a {type(settings).__name__}, and the settings of the service are one JSON object")
    settings[KEY] = [MIRROR]
    return json.dumps(settings, indent=2) + "\n"


def asked_first(named: str) -> bool:
    """Whether the mirrors that the running service names have ours. The service writes the address with a `/` at
    its end."""
    try:
        mirrors = json.loads(named)
    except ValueError:
        return False
    return isinstance(mirrors, list) and any(isinstance(one, str) and one.rstrip("/") == MIRROR for one in mirrors)


def at_the_mirror(image: str) -> str:
    """The name of a Docker Hub image at the mirror. Raises ValueError for a name that is not one of Docker Hub with
    a tag: the name of another registry, a name with a digest, or a name with no tag."""
    found = OF_THE_HUB.fullmatch(image)
    if not found:
        raise ValueError(f"`{image}` is not a name of Docker Hub with a tag, like `ubuntu:22.04` or `rockylinux/rockylinux:9`")
    path = found["path"] if "/" in found["path"] else f"library/{found['path']}"
    return f"{MIRROR.removeprefix('https://')}/{path}:{found['tag']}"


def main(argv: list | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) == 2 and args[0] == "--asked-first":
        return 0 if asked_first(args[1]) else 1
    if len(args) == 2 and args[0] == "--at-the-mirror":
        try:
            print(at_the_mirror(args[1]))
        except ValueError as error:
            print(f"docker hub mirror: {error}. So the step cannot pull it from the mirror. Fix: give the step the "
                  "name that the job runs, with its tag; an image of another registry needs no mirror.", file=sys.stderr)
            return 1
        return 0
    if args:
        print(__doc__, file=sys.stderr)
        return 1
    try:
        text = with_the_mirror(sys.stdin.read())
    except (ValueError, TypeError) as error:
        print(f"docker hub mirror: the settings of the Docker service were not changed: {error}. So the service has "
              "no mirror, and a pull is refused when Docker Hub refuses it. Fix: look at the settings file on the "
              "runner (the step prints it); this script expects one JSON object in it.", file=sys.stderr)
        return 1
    sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
