"""The request of `atlas model install`'s download and the checks around its
GET: where a resumed download starts, the bytes already in the .part file, the
server's answer, and the size and SHA256 checks before the .part becomes the
model.

atlas.commands.model streams the download itself (_get_into_part, with its
progress bar) and imports these back under their names.
"""

import os
import time
import urllib.error
import urllib.request
from typing import Optional, Tuple

from atlas.commands.model_registry import Model
from atlas.display import (
    RESET, RED, GREEN, YELLOW as YELL,
    safe_print as _safe_print,
)


def _human_bytes(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} PB"


def _build_request(url: str, range_start: int = 0,
                   token: Optional[str] = None) -> urllib.request.Request:
    """Construct a urllib Request with optional resume + HF auth headers."""
    headers = {"User-Agent": "atlas-cli/PC-056.1"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if range_start > 0:
        # bytes=N- = "from byte N to the end". HF supports this; the
        # response will be 206 Partial Content with Content-Range.
        headers["Range"] = f"bytes={range_start}-"
    return urllib.request.Request(url, headers=headers)


def _print_oversized_part(m: Model, tmp: str, range_start: int, color: bool) -> None:
    """Say that the .part is larger than the model, so no interrupted download made it."""
    _safe_print(f"  {RED if color else ''}Existing .part file "
                f"({_human_bytes(range_start)}) is larger than "
                f"the expected model size "
                f"({m.model_size_gb:.1f} GB).{RESET if color else ''}")
    _safe_print("  This isn't from a normal interrupted "
                "download. Likely causes: manual modification, "
                "a previous install of a renamed model, or a "
                "mismatched mirror. Recover by deleting it:")
    _safe_print(f"    rm {tmp}")
    _safe_print("  Or pass --no-resume to overwrite from byte 0.")


def _resume_point(m: Model, tmp: str, color: bool, resume: bool) -> Optional[Tuple[int, str]]:
    """Where the download starts: (the bytes already in the .part, "ab") to
    resume it, else (0, "wb"). None after printing that the .part is too large."""
    # Resume bookkeeping.
    range_start = 0
    file_mode = "wb"
    if resume and os.path.exists(tmp):
        try:
            range_start = os.stat(tmp).st_size
        except OSError:
            range_start = 0
        if range_start > 0:
            # PC-056.2 item B: oversized .part detection. If the existing
            # .part is wildly larger than the registered model size, it
            # was created by something other than this CLI (manual
            # touch / append, mismatched mirror, leftover from a model
            # that was renamed). Refuse cleanly rather than send a
            # nonsense Range request that would 416 OR hash garbage.
            expected_bytes = int(m.model_size_gb * (1024 ** 3))
            # 5% slack tolerates registry size drift. Anything bigger
            # than that is unambiguously wrong.
            if expected_bytes > 0 and range_start > int(expected_bytes * 1.05):
                _print_oversized_part(m, tmp, range_start, color)
                return None
            file_mode = "ab"
            _safe_print(f"  Resuming from byte {range_start} "
                        f"({_human_bytes(range_start)} already on disk)")
    elif not resume and os.path.exists(tmp):
        try:
            os.unlink(tmp)
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
    return range_start, file_mode


def _fold_part_into_hash(tmp: str, chunk: int, range_start: int, h, color: bool) -> bool:
    """When resuming, hash the bytes already in the .part. False after
    printing that the .part cannot be read."""
    # If we're resuming, we need to fold the existing .part contents into
    # the hash before continuing — otherwise the final hexdigest won't
    # match because we skipped those bytes.
    if range_start > 0:
        try:
            with open(tmp, "rb") as f:
                while True:
                    buf = f.read(chunk)
                    if not buf:
                        break
                    h.update(buf)
        except OSError as e:
            _safe_print(f"  {RED if color else ''}Cannot read existing "
                        f".part for hash continuation: {e}"
                        f"{RESET if color else ''}")
            return False
    return True


def _print_range_ignored(color: bool) -> None:
    """Say that the server ignored the Range header, so the download restarts."""
    _safe_print(f"  {YELL if color else ''}Server ignored "
                f"Range header — restarting from byte 0."
                f"{RESET if color else ''}")


def _print_unexpected_range_status(status: int, color: bool) -> None:
    """Say that the server answered a Range request with neither 200 nor 206."""
    _safe_print(f"  {RED if color else ''}Unexpected response "
                f"to Range request: HTTP {status}. Aborting."
                f"{RESET if color else ''}")


def _total_size(resp, status: int, range_start: int) -> int:
    """The download's whole size, for the progress bar; 0 when the server does not say."""
    # Total size — for resume, Content-Length is the REMAINING
    # bytes; we want absolute total for progress display.
    cl = resp.headers.get("Content-Length")
    if status == 206 and cl:
        total_n = int(cl) + range_start
    else:
        total_n = int(cl) if cl else 0
    return total_n


def _print_http_401(m: Model, token: Optional[str], color: bool) -> None:
    """HTTP 401: the token has no access to the repo, or the repo is gated and
    there is no token."""
    if token:
        _safe_print(f"  {RED if color else ''}HTTP 401 even with "
                    f"HF_TOKEN — your token may not have access "
                    f"to this repo.{RESET if color else ''}")
    else:
        _safe_print(f"  {RED if color else ''}HTTP 401: this repo "
                    f"is gated.{RESET if color else ''}")
        _safe_print("  Set the HF_TOKEN env var to a HuggingFace "
                    "access token with read access:")
        _safe_print("    export HF_TOKEN='hf_xxxxxxxxxxxxxxxx'")
        _safe_print(f"    atlas model install {m.name}")
        _safe_print("  Get one at "
                    "https://huggingface.co/settings/tokens")


def _print_http_error(e: urllib.error.HTTPError, m: Model, token: Optional[str],
                      color: bool) -> None:
    """Say why the server refused the download."""
    if e.code == 401:
        _print_http_401(m, token, color)
    else:
        _safe_print(f"  {RED if color else ''}Download failed: HTTP "
                    f"{e.code} {e.reason}{RESET if color else ''}")


def _downloaded_size(tmp: str, color: bool) -> Optional[int]:
    """The .part's size; None after printing that it is too small to be a
    model, and removing it."""
    # Final size sanity check.
    try:
        actual = os.stat(tmp).st_size
    except OSError:
        actual = 0
    if actual < 100 * 1024 * 1024:
        _safe_print(f"  {RED if color else ''}Downloaded file is too small "
                    f"({_human_bytes(actual)}). Aborting and removing "
                    f".part.{RESET if color else ''}")
        try:
            os.unlink(tmp)
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
        return None
    return actual


def _reject_mismatched_part(m: Model, actual_hash: str, tmp: str, color: bool) -> None:
    """Report a SHA256 mismatch and remove the .part."""
    _safe_print(f"  {RED if color else ''}SHA256 mismatch — "
                f"download may be corrupted or upstream has "
                f"changed.{RESET if color else ''}")
    _safe_print(f"    expected: {m.sha256}")
    _safe_print(f"    actual:   {actual_hash}")
    _safe_print("  Removing .part file. If this happens repeatedly, "
                "the registry may be stale or the upstream URL has "
                "been re-uploaded — check for a newer ATLAS release.")
    try:
        os.unlink(tmp)
    except OSError:
        # best-effort: swallow on failure (caller continues)
        pass


def _sha256_verified(m: Model, h, tmp: str, color: bool) -> bool:
    """Check the download against the registry's SHA256. False after the
    mismatch is reported; True when it matches, or when there is no hash to
    check (with a note)."""
    # SHA256 verification (PC-056.1).
    if m.sha256:
        actual_hash = h.hexdigest()
        if actual_hash != m.sha256:
            _reject_mismatched_part(m, actual_hash, tmp, color)
            return False
        _safe_print(f"  {GREEN if color else ''}SHA256 verified.{RESET if color else ''} "
                    f"({m.sha256[:16]}…)")
    else:
        _safe_print(f"  {YELL if color else ''}Note: registry has no "
                    f"expected SHA256 for this model — download integrity "
                    f"NOT verified end-to-end.{RESET if color else ''}")
    return True


def _move_into_place(tmp: str, target: str, started: float, bytes_seen: int,
                     range_start: int, actual: int, color: bool) -> int:
    """Rename the .part to the model file and print the download's rate."""
    # Atomic rename .part -> final.
    try:
        os.replace(tmp, target)
    except OSError as e:
        _safe_print(f"  {RED if color else ''}Failed to move into place: {e}"
                    f"{RESET if color else ''}")
        return 1

    elapsed = time.monotonic() - started
    # rate = bytes pulled in this invocation / time. For a fully-resumed
    # download with no new bytes the rate is misleading; just skip the
    # rate column when bytes_seen - range_start is tiny.
    new_bytes = max(bytes_seen - range_start, 0)
    rate = new_bytes / elapsed if elapsed > 0 else 0.0
    _safe_print(f"  {GREEN if color else ''}Done.{RESET if color else ''} "
                f"{_human_bytes(actual)} in {elapsed:.0f}s "
                f"({_human_bytes(rate)}/s of new bytes)")
    return 0


def _finish_part(m: Model, target: str, tmp: str, started: float, range_start: int,
                 bytes_seen: int, h, color: bool) -> int:
    """Check the downloaded .part, its size and then its SHA256, and move it into place."""
    actual = _downloaded_size(tmp, color)
    if actual is None:
        return 1
    if not _sha256_verified(m, h, tmp, color):
        return 1
    return _move_into_place(tmp, target, started, bytes_seen, range_start, actual, color)
