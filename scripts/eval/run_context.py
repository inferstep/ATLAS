"""What ties each record to its suite, grader image, driver, sandbox and model
(#275). Gathered once per run; every record of the run carries it."""
from __future__ import annotations

import subprocess
import urllib.error
from pathlib import Path
from typing import Optional

from baseline_arm import get_json, model_identity, server_props
from grading import resolve_image
from suite import suite_digest

REPO = Path(__file__).resolve().parents[2]


def run_context(args, suite_root: Path, run=subprocess.run, get=None) -> tuple:
    """The fields every record of this run carries, and why the run must not
    start, if it must not."""
    problems = []
    image_id = resolve_image(args.image, run)
    if not image_id:
        problems.append(f"docker cannot resolve the grader image {args.image!r} to an image ID")
    timeout = proxy_session_timeout(args.proxy_url, get)
    ctx = {"suite_sha256": suite_digest(suite_root),
           "grader_image": {"ref": args.image, "id": image_id},
           "driver": driver_identity(run),
           "sandbox_network": sandbox_network(args.compose_project, run),
           "session_timeout_s": timeout}
    if timeout is not None and int(args.budget_s) != timeout:
        problems.append(f"--budget-s {args.budget_s:g} is not the stack's session timeout ({timeout} s)")
    if args.arm == "baseline":
        try:
            ctx["model"] = model_identity(server_props(args.llama_url, get))
        except (urllib.error.URLError, OSError, ValueError, KeyError) as e:
            problems.append(f"the model server's /props could not be read: {e}")
        else:
            args.context_tokens = args.context_tokens or ctx["model"]["n_ctx"]
            ctx["context_tokens"] = args.context_tokens
    return ctx, problems


def proxy_session_timeout(proxy_url: str, get=None) -> Optional[int]:
    """The session limit the proxy reports on /version, or None: an older proxy
    does not report it, and the record then says so by the None."""
    try:
        version = (get or get_json)(f"{proxy_url}/version", 10)
    except (urllib.error.URLError, OSError, ValueError):
        return None
    value = version.get("session_timeout_s")
    return value if isinstance(value, int) else None


def driver_identity(run=subprocess.run) -> dict:
    """The driver's own commit, apart from the stack's, and whether its files
    differ from that commit."""
    head = run(["git", "-C", str(REPO), "rev-parse", "HEAD"],
               capture_output=True, text=True, timeout=30)
    status = run(["git", "-C", str(REPO), "status", "--porcelain", "--", "scripts/eval"],
                 capture_output=True, text=True, timeout=30)
    return {"commit": (head.stdout or "").strip() if head.returncode == 0 else "",
            "dirty": bool((status.stdout or "").strip())}


def sandbox_network(project: str, run=subprocess.run) -> dict:
    """Whether the stack's sandbox could reach the network: each network it is
    on, and whether that network is internal (no route out)."""
    ps = run(["docker", "ps", "-q", "--filter", f"label=com.docker.compose.project={project}",
              "--filter", "label=com.docker.compose.service=sandbox"],
             capture_output=True, text=True, timeout=30)
    ids = (ps.stdout or "").split()
    if not ids:
        return {"egress": None, "networks": {}, "note": "no sandbox container"}
    names = run(["docker", "inspect", "--format",
                 "{{range $k, $v := .NetworkSettings.Networks}}{{println $k}}{{end}}", ids[0]],
                capture_output=True, text=True, timeout=30).stdout.split()
    internal = {n: n == "none" or _is_internal(n, run) for n in names}
    return {"egress": any(not v for v in internal.values()), "networks": internal}


def _is_internal(network: str, run) -> bool:
    p = run(["docker", "network", "inspect", "--format", "{{.Internal}}", network],
            capture_output=True, text=True, timeout=30)
    return (p.stdout or "").strip() == "true"
