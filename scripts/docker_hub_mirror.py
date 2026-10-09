#!/usr/bin/env python3
"""Give the Docker service of a runner a mirror of Docker Hub, and change nothing else of its settings.

A job that builds or runs an image of Docker Hub is red when Docker Hub refuses the pull for its limit, or when its
token service does not answer. The change under test is not at fault then. With a mirror in its settings the Docker
service asks the mirror first and Docker Hub second (docs/quality/gates.md, section "Docker Hub and the checks").

usage:
  docker_hub_mirror.py <the settings file of the Docker service>
      Sets the one key `registry-mirrors` in the file. Every other key stays as it is. A file that is not there, or
      that is empty, becomes a file with that one key.
  docker_hub_mirror.py --asked-first '<the mirrors that `docker info` names, as JSON>'
      Says whether the running service has the mirror.

Exit status: 0 done, or the service has the mirror; 1 not so, and for the settings file nothing was written.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

# Google's public mirror of Docker Hub. The builder jobs name the same host in the settings of their builder.
MIRROR = "https://mirror.gcr.io"
KEY = "registry-mirrors"


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


def main(argv: list | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) == 2 and args[0] == "--asked-first":
        return 0 if asked_first(args[1]) else 1
    if len(args) != 1 or args[0].startswith("-"):
        print(__doc__, file=sys.stderr)
        return 1
    path = Path(args[0])
    try:
        text = with_the_mirror(path.read_text(encoding="utf-8") if path.exists() else "")
    except (ValueError, TypeError, OSError) as error:
        print(f"docker hub mirror: the settings file `{path}` was not changed: {error}. So the Docker service has no "
              "mirror, and a pull is refused when Docker Hub refuses it. Fix: look at the file on the runner (the "
              "step prints it); this script expects one JSON object in it.", file=sys.stderr)
        return 1
    path.write_text(text, encoding="utf-8")
    print(f"docker hub mirror: `{path}` names the mirror `{MIRROR}`; every other setting is as it was.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
