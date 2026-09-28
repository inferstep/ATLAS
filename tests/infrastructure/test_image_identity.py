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
import re

import pytest

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
RUNNERS = [
    os.path.join(ROOT, "redteam", "runs", "candidate-eligibility-pilot", "run_pilot.py"),
]

MUTABLE_TAGS = (":dev", ":latest")


def _read(path):
    with open(path) as fh:
        return fh.read()


@pytest.mark.parametrize("runner", [r for r in RUNNERS if os.path.exists(r)])
def test_an_acquisition_runner_rejects_a_mutable_tag(runner):
    """A runner may not launch from a tag that can be rewritten under it."""
    body = _read(runner)
    assert "reject_mutable_image" in body or "MUTABLE_IMAGE_TAGS" in body, (
        f"{os.path.basename(runner)} does not refuse a mutable image tag")
    for tag in MUTABLE_TAGS:
        # A literal mutable tag as a default is the shape that fails silently.
        bad = re.findall(r'["\'][^"\']*' + re.escape(tag) + r'["\']', body)
        allowed = [b for b in bad if "MUTABLE" in body[:body.index(b)][-200:]]
        assert not [b for b in bad if b not in allowed], (
            f"{os.path.basename(runner)} names {tag} outside the refusal list: {bad}")


@pytest.mark.parametrize("runner", [r for r in RUNNERS if os.path.exists(r)])
def test_an_acquisition_runner_pins_an_exact_image_id(runner):
    body = _read(runner)
    for required in ("image_id", "sha256:"):
        assert required in body, (
            f"{os.path.basename(runner)} does not pin an exact image identity")


@pytest.mark.parametrize("runner", [r for r in RUNNERS if os.path.exists(r)])
def test_a_missing_historical_image_is_reported_not_reconstructed(runner):
    body = _read(runner)
    for banned in ("docker pull", "docker tag "):
        assert banned not in body, (
            f"{os.path.basename(runner)} would reconstruct an image identity "
            f"with {banned!r} rather than reporting it missing")


def test_the_runbook_names_the_unsafe_tag():
    ops = _read(os.path.join(ROOT, "docs", "OPERATIONS.md"))
    assert "Building for verification" in ops
    assert "ATLAS_IMAGE_TAG" in ops
    # And says plainly that a tag is not an identity.
    assert "image ID" in ops or "image id" in ops
