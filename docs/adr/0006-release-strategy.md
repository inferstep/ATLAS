# ADR 0006: Two-phase, test-gated, signed image releases

Status: accepted 2026-07

## Context
Single-phase tag pushes could publish moving tags from commits that
failed tests, or mixed-commit tags on partial matrix failures.

## Decision
Phase 1 pushes only immutable sha-* tags per service; phase 2 repoints
dev/latest/semver via imagetools only after every service built AND the
tests workflow passed on that exact commit. Bases are digest-pinned
[Ed.: the ROCm/Vulkan bases are pinned by digest in their defaults; a tag the user sets for them is not.];
images carry SLSA provenance + SPDX SBOM attestations and a keyless
cosign signature over the digest (covers all tags pointing at it).
Releases flow dev → staging → main by fast-forward; tags on main.

## Consequences
A red commit can never move a public tag; rollback = repoint to a prior
sha-*/semver digest. Verification command documented in
build-images.yml.
