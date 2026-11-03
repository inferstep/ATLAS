"""Verify lens/ASA bundle manifests.

Builds on the per-bundle provenance.json (geometric_lens.provenance):
a bundle can carry an SSH signature over its manifest, and verification
checks BOTH the signature (against .github/allowed_signers) AND that
every file's on-disk SHA-256 still matches the manifest. So a tampered
artifact fails even if the manifest is intact, and a swapped manifest
fails the signature — trust needs both integrity and a known signer.

Signatures are produced out of band with the release signing key
(`ssh-keygen -Y sign -n atlas-artifact provenance.json`, see
docs/project/RELEASE.md); verification needs only allowed_signers.
"""

import contextlib
import hashlib
import os
import subprocess
from typing import List, Optional, Tuple

from atlas.env import atlas_root

MANIFEST = "provenance.json"
SIGNATURE = "provenance.json.sig"
NAMESPACE = "atlas-artifact"


def _allowed_signers() -> str:
    return os.path.join(atlas_root(), ".github", "allowed_signers")


def _signature_principals(sig_path: str) -> List[str]:
    """Principals to verify the signature against. Verification must not
    depend on the *verifier's* identity (any user can verify a bundle
    signed by the maintainer), so ask ssh-keygen which allowed_signers
    principals match the signature, falling back to every principal
    listed in the file."""
    with contextlib.suppress(subprocess.SubprocessError, OSError):
        out = subprocess.check_output(
            ["ssh-keygen", "-Y", "find-principals",
             "-f", _allowed_signers(), "-s", sig_path],
            text=True, stderr=subprocess.DEVNULL)
        principals = [ln.strip() for ln in out.splitlines() if ln.strip()]
        if principals:
            return principals
    principals = []
    with contextlib.suppress(OSError):
        with open(_allowed_signers()) as fh:
            for line in fh:
                line = line.strip()
                if line and not line.startswith("#"):
                    # The principal field may hold a comma-separated list
                    # (sshsig allowed_signers format).
                    principals.extend(line.split()[0].split(","))
    return principals


def _sha256(path: str) -> Optional[str]:
    try:
        h = hashlib.sha256()
        with open(path, "rb") as fh:
            for chunk in iter(lambda: fh.read(65536), b""):
                h.update(chunk)
        return h.hexdigest()
    except OSError:
        return None


def verify_manifest(bundle_dir: str, principal: Optional[str] = None
                    ) -> Tuple[bool, List[str]]:
    """Verify the manifest signature (if present) and that every file's
    on-disk hash matches the manifest. Returns (ok, problems)."""
    import json
    problems: List[str] = []
    manifest_path = os.path.join(bundle_dir, MANIFEST)
    sig_path = os.path.join(bundle_dir, SIGNATURE)
    if not os.path.isfile(manifest_path):
        return False, [f"no {MANIFEST}"]

    # 1. Signature (when present — a bundle may be hash-only Preview).
    if os.path.isfile(sig_path):
        candidates = [principal] if principal else \
            _signature_principals(sig_path)
        if not candidates:
            problems.append("cannot determine signer principal to verify "
                            "(no matching entry in allowed_signers)")
        else:
            def _verifies(who: str) -> bool:
                try:
                    with open(manifest_path, "rb") as mf:
                        subprocess.run(
                            ["ssh-keygen", "-Y", "verify", "-f",
                             _allowed_signers(), "-I", who, "-n", NAMESPACE,
                             "-s", sig_path],
                            stdin=mf, check=True,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL)
                    return True
                except (subprocess.SubprocessError, OSError):
                    return False

            if not any(_verifies(who) for who in candidates):
                problems.append("manifest signature did not verify against "
                                "allowed_signers")
    else:
        problems.append("unsigned (no signature; Preview-level trust only)")

    # 2. File hashes vs manifest — catches tampering even if signed.
    try:
        with open(manifest_path) as fh:
            manifest = json.load(fh)
    except (OSError, ValueError):
        return False, problems + ["manifest unreadable"]
    for name, expected in manifest.get("artifact_sha256", {}).items():
        actual = _sha256(os.path.join(bundle_dir, name))
        if actual is None:
            problems.append(f"{name}: listed in manifest but missing on disk")
        elif actual != expected:
            problems.append(f"{name}: hash mismatch (tampered or stale)")

    # Signed + all hashes good = fully verified.
    ok = not [p for p in problems if "Preview-level" not in p]
    return ok, problems
