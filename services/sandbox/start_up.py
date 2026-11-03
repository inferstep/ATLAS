"""How the executor starts: the address it listens on, and the workspace base
that every request's workspace is made in."""

import os
import stat
from pathlib import Path

# The address the executor listens on. On the host itself (bare metal) that is
# loopback only; the image sets every interface, as the other containers of the
# stack reach it over their network.
LISTEN_HOST = os.getenv("ATLAS_SANDBOX_HOST", "127.0.0.1")


def prepare_workspace_base(base: Path) -> None:
    """Make the workspace base this process's own folder, or refuse to start.

    Every request's workspace is made inside it, so the base must not be a link
    or a folder that another user owns. A new base is made 0700, and an existing
    one of this process is set to 0700.
    """
    base.mkdir(mode=0o700, parents=True, exist_ok=True)
    found = base.lstat()
    if stat.S_ISLNK(found.st_mode):
        raise SystemExit(f"the workspace base {base} is a symbolic link, so the executor does not start. "
                         f"Fix: remove the link, or set WORKSPACE_BASE to a folder of your own.")
    if found.st_uid != os.getuid():
        raise SystemExit(f"the workspace base {base} belongs to uid {found.st_uid}, not to this process "
                         f"(uid {os.getuid()}), so the executor does not start; an earlier run as another "
                         f"user may have left it. Fix: remove {base}, or set WORKSPACE_BASE to a folder of your own.")
    base.chmod(0o700)


def serve(app, workspace_base: Path) -> None:
    """Make the workspace base ready, then serve the executor's API on port 8020."""
    import uvicorn
    prepare_workspace_base(workspace_base)
    uvicorn.run(app, host=LISTEN_HOST, port=8020)
