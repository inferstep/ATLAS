#!/usr/bin/env bash
# Create a signed, annotated release tag for ATLAS.
#
# Release tags are cut from `main` (dev -> staging -> main fast-forward
# flow) and are SSH-signed so their provenance is verifiable
# (.github/allowed_signers). This script never pushes — it prints the
# push command so the release is a deliberate, separate step.
#
# Usage:  scripts/release/release-tag.sh v1.2.0 ["release notes"]
#
# One-time signing setup (per maintainer machine):
#   ssh-keygen -t ed25519 -f ~/.ssh/atlas_release_signing -C "you@example"
#   git config gpg.format ssh
#   git config user.signingkey ~/.ssh/atlas_release_signing.pub
#   git config gpg.ssh.allowedSignersFile .github/allowed_signers
#   # add "<git-email> <contents of the .pub>" to .github/allowed_signers
#   gh ssh-key add ~/.ssh/atlas_release_signing.pub --type signing \
#       --title "atlas release signing"   # for GitHub "Verified" badge
set -euo pipefail

TAG="${1:-}"
NOTES="${2:-ATLAS release $TAG}"

if [ -z "$TAG" ]; then
    echo "usage: $0 <vX.Y.Z> [notes]" >&2
    exit 2
fi
if ! printf '%s' "$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$'; then
    echo "error: tag '$TAG' is not a vX.Y.Z semver tag" >&2
    exit 2
fi

# Signing must be configured or the tag would be unsigned.
if [ "$(git config --get gpg.format || true)" != "ssh" ] && \
   [ -z "$(git config --get user.signingkey || true)" ]; then
    echo "error: tag signing is not configured on this machine." >&2
    echo "See the one-time setup block at the top of this script." >&2
    exit 1
fi

BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if [ "$BRANCH" != "main" ]; then
    echo "warning: releases are cut from 'main', you are on '$BRANCH'." >&2
    read -r -p "Tag '$BRANCH' anyway? [y/N] " ans
    [ "$ans" = "y" ] || exit 1
fi

if git rev-parse "$TAG" >/dev/null 2>&1; then
    echo "error: tag '$TAG' already exists (tags are immutable)." >&2
    exit 1
fi

echo "Creating signed tag $TAG at $(git rev-parse --short HEAD)…"
git tag -s "$TAG" -m "$NOTES"

echo "Verifying signature…"
if git verify-tag "$TAG"; then
    echo
    echo "Signed tag $TAG created and verified locally."
    echo "Push it when ready (this is the release step):"
    echo "    git push origin $TAG"
else
    echo "error: signature verification failed; removing tag." >&2
    git tag -d "$TAG"
    exit 1
fi
