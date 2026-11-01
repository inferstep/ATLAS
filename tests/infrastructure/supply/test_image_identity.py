"""What may be built, and what may be launched.

`docker compose build` writes the tag the running stack was started from. A
verification build during development retagged `atlas-proxy:dev` while a
container was still running the previous image; that image had since been
pruned, so the tag could not be put back. The deployable name now points at
something the running container is not, and no amount of inspecting the tag
will reveal that.

So the tag is not an identity. An image ID is.
"""

from __future__ import annotations

import os

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))))


def _read(path):
    with open(path) as fh:
        return fh.read()


def test_the_runbook_names_the_unsafe_tag():
    ops = _read(os.path.join(ROOT, "docs", "OPERATIONS.md"))
    assert "Building for verification" in ops
    assert "ATLAS_IMAGE_TAG" in ops
    # And says plainly that a tag is not an identity.
    assert "image ID" in ops or "image id" in ops
