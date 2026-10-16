"""Shared publishing machinery (PC-059, PC-061, PC-215).

Everything `atlas lens publish`, `atlas asa publish`, and the combined
`atlas publish` have in common: artifact hashing, HF-token resolution,
`gh api` plumbing, registry-file editing, the registry-PR flow, and the
shared pre-flight panel. The per-artifact narrative (model cards, PR
bodies) stays with its command — lens.py and asa.py each render their
own.

Lived inside lens.py until asa/publish grew to importing nine of its
private helpers by `_name`; a concern consumed by three commands is
shared API, so it lives here with public names.
"""

import json
import os
import time
from typing import List, Optional

from atlas.commands import model_registry
from atlas.display import (
    RESET, RED, GREEN, YELLOW,
    safe_print as _safe_print,
)

UPSTREAM_REPO = "inferstep/ATLAS"
REGISTRY_PATH = "atlas/commands/model_registry.py"


def model_marker_value(value: Optional[str]) -> str:
    """The portable form of a model reference: registry name, GGUF
    filename, or container/host path in, bare stem out. This is the value
    written into artifact sidecars (model_identity.json, the ASA vector's
    model marker), so `/models/Example-Model.gguf` and `Example-Model`
    record identically."""
    text = str(value or "").strip().replace("\\", "/")
    name = text.rsplit("/", 1)[-1]
    if name.lower().endswith(".gguf"):
        name = name[:-5]
    return name


def canonical_model_identity(value: Optional[str]) -> str:
    """model_marker_value() case-folded, for comparing two model
    references that may differ in path shape, extension, or case."""
    return model_marker_value(value).casefold()



def resolve_model_arg(arg: Optional[str]) -> Optional[model_registry.Model]:
    """Best-effort lookup: registry name → Model, or path/None → None.

    `atlas lens check` accepts:
      - a registry name        (e.g. "your-model-Q4_K_M")
      - a .gguf path           (any model on disk)
      - nothing                (probe whatever llama-server has loaded)
    """
    if not arg:
        return None
    for m in model_registry.REGISTRY:
        if m.name == arg or m.model_file == os.path.basename(arg):
            return m
    return None


def sha256_file(path: str) -> str:
    """Stream-hash a file (large .pt artifacts shouldn't blow memory)."""
    import hashlib
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(64 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def hf_token() -> Optional[str]:
    """Resolve the HF token from the standard places huggingface_hub looks."""
    return (os.environ.get("HF_TOKEN")
            or os.environ.get("HUGGINGFACE_HUB_TOKEN")
            or os.environ.get("HUGGING_FACE_HUB_TOKEN"))


def gh_api(api_args: List[str], payload: Optional[dict] = None):
    """Run `gh api …`, return (ok, parsed-json-or-text, stderr)."""
    import subprocess
    cmd = ["gh", "api"] + api_args
    stdin = None
    if payload is not None:
        cmd += ["--input", "-"]
        stdin = json.dumps(payload)
    try:
        r = subprocess.run(cmd, capture_output=True, text=True,
                           input=stdin, timeout=60)
    except (subprocess.TimeoutExpired, FileNotFoundError) as e:
        return False, None, str(e)
    if r.returncode != 0:
        return False, None, r.stderr.strip()[:300]
    try:
        return True, json.loads(r.stdout), ""
    except json.JSONDecodeError:
        return True, r.stdout.strip(), ""


def render_registry_entry(model_label: str, model_file: str,
                          size_gb: float, tier: str, dim: int,
                          hf_repo: str, license_id: str,
                          artifact_files: List[str]) -> str:
    """A complete, committable Model(...) entry for an unregistered model."""
    files = ", ".join(f'"{f}"' for f in artifact_files)
    dim_label = str(dim) if dim else "unknown"
    return f'''    Model(
        name="{model_label}",
        tier="{tier}",
        model_file="{model_file}",
        model_display="{model_label}",
        model_size_gb={size_gb},
        lens_status="supported",
        lens_calibrated=True,
        download_url=None,
        sha256=None,
        license="{license_id}",
        lens_artifact_files=[{files}],
        lens_hf_repo="{hf_repo}",
        notes="Added via `atlas lens publish` — lens artifacts "
              "({dim_label}-dim) at https://huggingface.co/{hf_repo}. "
              "download_url not captured at publish time; maintainers "
              "can fill it in for `atlas model install` support.",
    ),
'''


def registry_insert_entry(content: str, model_label: str,
                          entry: str) -> Optional[str]:
    """Insert a Model entry before the REGISTRY list's closing bracket.
    Returns the new content, or None when insertion isn't safe (model
    already present, or the anchor isn't found).

    The upstream registry can be older than the publisher's install —
    kwargs the upstream Model dataclass doesn't declare yet are stripped
    from the entry so the committed file stays importable.
    """
    if f'name="{model_label}"' in content:
        return None
    anchor = "REGISTRY: List[Model] = ["
    start = content.find(anchor)
    if start < 0:
        return None
    close = content.find("\n]", start)
    if close < 0:
        return None
    schema = content[:start]   # the Model dataclass definition lives above
    for field_name in ("lens_calibrated", "lens_hf_repo", "asa_hf_repo"):
        if f"{field_name}:" not in schema:
            entry = "\n".join(l for l in entry.splitlines()
                              if not l.strip().startswith(f"{field_name}="))
            if not entry.endswith("\n"):
                entry += "\n"
    return content[:close + 1] + entry + content[close + 1:]


def registry_set_lens(content: str, model_label: str, hf_repo: str,
                      artifact_files: List[str]) -> Optional[str]:
    """Mark an existing registry entry's Lens bundle as current.

    Publishing is also the upgrade path for legacy or unverified entries, so
    it must update those entries rather than only knowing how to insert a new
    model. The upstream schema may lag behind the publisher; in that case the
    download location is retained as a comment until the field lands.
    """
    import re
    match = re.search(
        rf'(    Model\(\s*\n\s*name="{re.escape(model_label)}".*?\n    \),)',
        content,
        re.DOTALL,
    )
    if not match:
        return None

    block = match.group(1)
    files = ", ".join(f'"{name}"' for name in artifact_files)
    lens_lines = (
        '        lens_status="supported",\n'
        f'        lens_artifact_files=[{files}],\n'
    )
    schema = content[:match.start()]
    if "lens_calibrated:" in schema:
        lens_lines += '        lens_calibrated=True,\n'
    else:
        lens_lines += '        # Bundle includes current C(x)/G(x) calibration.\n'
    if "lens_hf_repo:" in schema:
        lens_lines += f'        lens_hf_repo="{hf_repo}",\n'
    else:
        lens_lines = (
            f'        # Lens artifacts: https://huggingface.co/{hf_repo}\n'
            '        # (promote to lens_hf_repo= once the registry schema carries it)\n'
        ) + lens_lines

    new_block = re.sub(r'\n\s*lens_status="[^"]*",', "", block)
    new_block = re.sub(r'\n\s*lens_calibrated=(?:True|False),', "", new_block)
    new_block = re.sub(r'\n\s*lens_artifact_files=\[[^\]]*\],', "", new_block)
    new_block = re.sub(r'\n\s*lens_hf_repo="[^"]*",', "", new_block)
    new_block = new_block.replace("\n    ),", "\n" + lens_lines + "    ),")
    return content.replace(block, new_block)


def registry_set_asa(content: str, model_label: str, hf_repo: str,
                     artifact_files: List[str]) -> Optional[str]:
    """Within the named entry's block, set asa_status to unverified and
    record the vector's HF repo + files. Returns new content or None when
    the entry (or a safe edit point) can't be found.

    Publishing shows a vector exists, not that it helps. Promoting an entry
    to "supported" is a manual edit that cites an A/B result."""
    import re
    m = re.search(rf'(    Model\(\s*\n\s*name="{re.escape(model_label)}".*?'
                  rf'\n    \),)', content, re.DOTALL)
    if not m:
        return None
    block = m.group(1)
    files = ", ".join(f'"{f}"' for f in artifact_files)
    asa_lines = (f'        asa_status="unverified",\n'
                 f'        asa_artifact_files=[{files}],\n')
    # Only set fields the upstream dataclass declares (it can be older
    # than the publisher's install) — but never drop the download
    # location: without `asa_hf_repo` in the schema, record the vector's
    # HF repo as a comment so the entry still points at it (promotable
    # to the field once the newer schema lands).
    if "asa_hf_repo:" in content[:m.start()]:
        asa_lines += f'        asa_hf_repo="{hf_repo}",\n'
    else:
        asa_lines = (f'        # ASA vector: https://huggingface.co/'
                     f'{hf_repo}\n'
                     f'        # (promote to asa_hf_repo= once the '
                     f'registry schema carries it)\n') + asa_lines
    new_block = re.sub(r'\n\s*asa_status="[^"]*",', "", block)
    new_block = re.sub(r'\n\s*asa_artifact_files=\[[^\]]*\],', "", new_block)
    new_block = re.sub(r'\n\s*asa_hf_repo="[^"]*",', "", new_block)
    new_block = new_block.replace("\n    ),", "\n" + asa_lines + "    ),")
    return content.replace(block, new_block)


def open_registry_pr_via_api(model_label: str, title: str, body: str,  # noqa: C901
                             edit_fn) -> Optional[str]:
    """Open a registry PR through the GitHub API — no local git checkout.

    edit_fn(content) -> new content or None. Flow: resolve user + base
    branch (prefers `dev`), fork if the user can't push upstream, branch,
    commit the edited registry file, open the PR against the upstream.
    Returns the PR URL, or None on any failure (caller falls back to
    printing the body).
    """
    import base64

    upstream = os.environ.get("ATLAS_UPSTREAM_REPO", UPSTREAM_REPO)
    owner = upstream.split("/")[0]

    ok, login, err = gh_api(["user", "-q", ".login"])
    if not ok:
        _safe_print(f"  gh api user failed: {err}")
        return None
    login = str(login).strip()

    ok, _, _ = gh_api([f"repos/{upstream}/branches/dev"])
    base = "dev" if ok else None
    if base is None:
        ok, repo_info, err = gh_api([f"repos/{upstream}"])
        if not ok:
            _safe_print(f"  gh api repos/{upstream} failed: {err}")
            return None
        base = repo_info.get("default_branch", "main")

    if login == owner:
        target = upstream
    else:
        ok, fork, err = gh_api([f"repos/{upstream}/forks", "-X", "POST"])
        if not ok:
            _safe_print(f"  fork failed: {err}")
            return None
        target = fork.get("full_name", f"{login}/{upstream.split('/')[1]}")
        # Best-effort: bring the fork's base branch up to date.
        gh_api([f"repos/{target}/merge-upstream", "-X", "POST"],
               payload={"branch": base})

    ok, br, err = gh_api([f"repos/{target}/branches/{base}"])
    if not ok:
        _safe_print(f"  branch {base} not found on {target}: {err}")
        return None
    head_sha = br["commit"]["sha"]

    ok, blob, err = gh_api(
        [f"repos/{target}/contents/{REGISTRY_PATH}?ref={base}"])
    if not ok:
        _safe_print(f"  fetch registry file failed: {err}")
        return None
    content = base64.b64decode(blob["content"]).decode("utf-8")

    new_content = edit_fn(content)
    if new_content is None:
        # The edit doesn't apply to the base branch. Common cause: a prior
        # `atlas * publish` PR holding this model's entry is still open —
        # stack this edit onto that PR's branch instead of failing.
        slug = "".join(c if c.isalnum() else "-" for c in model_label.lower())
        ok, prs, _ = gh_api(
            [f"repos/{upstream}/pulls?state=open&base={base}"])
        for pr in (prs if ok and isinstance(prs, list) else []):
            head_ref = pr.get("head", {}).get("ref", "")
            if not (head_ref.startswith("atlas-publish/")
                    and slug in head_ref):
                continue
            head_repo = pr.get("head", {}).get("repo", {}).get("full_name")
            if not head_repo:
                continue
            ok, pr_blob, err = gh_api(
                [f"repos/{head_repo}/contents/{REGISTRY_PATH}"
                 f"?ref={head_ref}"])
            if not ok:
                continue
            pr_content = base64.b64decode(pr_blob["content"]).decode("utf-8")
            stacked = edit_fn(pr_content)
            if stacked is None:
                continue
            pr_num = pr.get("number")
            if head_repo == upstream:
                # The pending branch lives upstream — open a SEPARATE PR
                # based on it. Its diff shows only this edit; when the
                # earlier PR merges (delete its branch), GitHub retargets
                # this one to the base branch automatically.
                ok, head_br, err = gh_api(
                    [f"repos/{upstream}/branches/{head_ref}"])
                if not ok:
                    continue
                new_branch = f"{head_ref}-next-{int(time.time())}"
                ok, _, err = gh_api(
                    [f"repos/{upstream}/git/refs", "-X", "POST"],
                    payload={"ref": f"refs/heads/{new_branch}",
                             "sha": head_br["commit"]["sha"]})
                if not ok:
                    _safe_print(f"  branch off PR #{pr_num} failed: {err}")
                    continue
                ok, _, err = gh_api(
                    [f"repos/{upstream}/contents/{REGISTRY_PATH}",
                     "-X", "PUT"],
                    payload={"message": title,
                             "content": base64.b64encode(
                                 stacked.encode("utf-8")).decode("ascii"),
                             "sha": pr_blob["sha"],
                             "branch": new_branch})
                if not ok:
                    _safe_print(f"  commit failed: {err}")
                    continue
                note = (f"\n\n---\n*Stacked on #{pr_num} (this model's "
                        f"entry lands there). Merge #{pr_num} with branch "
                        f"deletion and GitHub retargets this PR to `{base}` "
                        f"automatically — its diff shows only this change.*")
                ok, pr2, err = gh_api(
                    [f"repos/{upstream}/pulls", "-X", "POST"],
                    payload={"title": title, "body": body + note,
                             "head": new_branch, "base": head_ref})
                if not ok:
                    _safe_print(f"  open stacked PR failed: {err}")
                    continue
                _safe_print(f"  Entry is pending in open PR #{pr_num} — "
                            f"opened a separate PR stacked on it (diff "
                            f"shows only this change).")
                return pr2.get("html_url")
            # Fork case: a PR's base must be an upstream branch, so a
            # separate stacked PR isn't expressible — commit onto the
            # pending PR's branch instead (one PR, both changes).
            ok, _, err = gh_api(
                [f"repos/{head_repo}/contents/{REGISTRY_PATH}", "-X", "PUT"],
                payload={"message": title,
                         "content": base64.b64encode(
                             stacked.encode("utf-8")).decode("ascii"),
                         "sha": pr_blob["sha"],
                         "branch": head_ref})
            if not ok:
                _safe_print(f"  stacking onto PR #{pr_num} failed: {err}")
                continue
            _safe_print(f"  Entry is pending in open PR #{pr_num} — this "
                        f"edit was committed onto its branch (forks can't "
                        f"host a separate stacked PR; one PR, both "
                        f"changes).")
            return pr.get("html_url")
        _safe_print("  registry edit not applicable (entry state differs "
                    "upstream, and no open publish PR holds it) — falling "
                    "back to the printed body.")
        return None

    slug = "".join(c if c.isalnum() else "-" for c in model_label.lower())
    branch = f"atlas-publish/{slug}-{int(time.time())}"
    ok, _, err = gh_api([f"repos/{target}/git/refs", "-X", "POST"],
                        payload={"ref": f"refs/heads/{branch}",
                                 "sha": head_sha})
    if not ok:
        _safe_print(f"  create branch failed: {err}")
        return None

    ok, _, err = gh_api(
        [f"repos/{target}/contents/{REGISTRY_PATH}", "-X", "PUT"],
        payload={"message": title,
                 "content": base64.b64encode(
                     new_content.encode("utf-8")).decode("ascii"),
                 "sha": blob["sha"],
                 "branch": branch})
    if not ok:
        _safe_print(f"  commit failed: {err}")
        return None

    head = branch if target == upstream else f"{login}:{branch}"
    ok, pr, err = gh_api([f"repos/{upstream}/pulls", "-X", "POST"],
                         payload={"title": title, "body": body,
                                  "head": head, "base": base})
    if not ok:
        _safe_print(f"  open PR failed: {err}")
        return None
    return pr.get("html_url")


def gh_available() -> bool:
    """Best-effort probe for the gh CLI on PATH. Used by the publish
    pre-flight so we can tell the user up front whether the registry PR
    will auto-open or whether they'll need to paste the body manually."""
    import shutil
    return shutil.which("gh") is not None


def huggingface_hub_available() -> bool:
    """Probe-only check that huggingface_hub imports. Lazy and quiet —
    we don't want to fail the whole CLI just because the user hasn't
    installed it yet; publish itself catches the ImportError too."""
    try:
        import huggingface_hub
        return True
    except ImportError:
        return False


def publish_preflight(kind: str, dry_run: bool, color: bool) -> bool:
    """Print the requirements block at the start of a publish run.

    Shared by both `atlas lens publish` and `atlas asa publish` so the
    contributor sees the same explicit "here's what publishing involves
    and what you need" panel regardless of which artifact they're shipping.

    Returns False if a hard requirement is missing AND we're not in
    --dry-run mode; the caller should bail with exit 1 in that case.
    Dry-run skips the auth gates entirely since nothing leaves the host.

    `kind` is "lens" or "asa" — only affects the printed wording.
    """
    GREEN_ = GREEN if color else ""
    RED_ = RED if color else ""
    YELL_ = YELLOW if color else ""
    RESET_ = RESET if color else ""

    _safe_print("")
    _safe_print(f"  atlas {kind} publish — submission pre-flight")
    _safe_print("  ──────────────────────────────────────────")
    _safe_print("  Publish does TWO things in one command:")
    _safe_print("    1. Uploads the artifact to a HuggingFace repo you own")
    _safe_print("    2. Opens a registry PR against github.com/inferstep/ATLAS")
    _safe_print("  Full walkthrough: docs/PUBLISHING.md")
    _safe_print("")

    token_ok = bool(hf_token())
    hf_pkg_ok = huggingface_hub_available()
    gh_ok = gh_available()

    # In dry-run mode the auth gates aren't enforced, so rendering a red
    # ✗ next to "HF_TOKEN required" is alarming and confusing — users
    # think they did something wrong when --dry-run is exactly the path
    # for previewing without setting any of this up. Render missing
    # items as dim ○ in dry-run, ✗/⚠ in real-run.
    def _row(label: str, ok: bool, required: bool, hint: str) -> None:
        if ok:
            mark = f"{GREEN_}✓{RESET_}"
        elif dry_run:
            mark = "○"  # neutral — not enforced
        elif required:
            mark = f"{RED_}✗{RESET_}"
        else:
            mark = f"{YELL_}⚠{RESET_}"
        if ok or dry_run:
            # Don't print the "required" hint when we're not enforcing it —
            # it adds visual noise that contradicts the dry-run header.
            suffix = "" if ok else "  (would be needed for a real upload)"
        else:
            suffix = f"  {hint}"
        _safe_print(f"  {mark} {label}{suffix}")

    _row("HF_TOKEN env var",
         token_ok, required=True,
         hint=(f"{RED_}required{RESET_} — get a write token at "
               "https://huggingface.co/settings/tokens, then "
               "`export HF_TOKEN=hf_...`"))
    _row("huggingface_hub Python pkg",
         hf_pkg_ok, required=True,
         hint=(f"{RED_}required{RESET_} — `pip install huggingface_hub`"))
    _row("gh CLI",
         gh_ok, required=False,
         hint=(f"{YELL_}optional{RESET_} — without it we'll print the PR "
               "body for you to paste at "
               "https://github.com/inferstep/ATLAS/compare"))
    _safe_print("")

    if dry_run:
        _safe_print(f"  {YELL_}--dry-run{RESET_}: nothing will leave the host "
                    f"(no upload, no PR opened, no auth enforced)")
        _safe_print("")
        return True

    missing_required = (not token_ok) or (not hf_pkg_ok)
    if missing_required:
        _safe_print(f"  {RED_}Cannot continue: missing required credentials.{RESET_}")
        _safe_print("  Fix the items marked ✗ above, then re-run. "
                    "Or use --dry-run to preview the PR body without uploading.")
        _safe_print("")
        return False

    return True
