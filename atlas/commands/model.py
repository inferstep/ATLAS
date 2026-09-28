"""atlas model — registry-aware install/list/recommend/remove/verify
(PC-056, hardened in PC-056.1).

Subcommands:
    atlas model list       — table of known models with install + lens columns
    atlas model recommend  — best model for this hardware (composes tier.classify
                              + registry, honors lens_status)
    atlas model install    — download from registry's download_url with progress,
                              SHA verify, resume support, HF_TOKEN auth
    atlas model verify     — recompute SHA of installed file vs. registry
    atlas model remove     — delete a model file from ATLAS_MODELS_DIR

The lens_status field is the central truth this command surfaces. A
user installing a registry model on a compatible hardware tier gets a working
llama.cpp model but no G(x) verification — half of what makes ATLAS
*ATLAS*. Doctor warns at runtime; this command warns at install time.

Implementation notes:
- urllib (stdlib) for downloads, no third-party deps. Streams in chunks
  with a progress bar.
- **PC-056.1 hardening:**
    * SHA256 is computed during the chunk loop (hashlib.sha256.update)
      and verified against the registered hash after the download
      completes. Mismatch = delete file + exit 1. Skipped when registry
      has no expected SHA, with a warning printed.
    * Resume is supported by default. If `<target>.part` exists, the
      next install picks up from `len(.part)` via `Range: bytes=N-`,
      verifies the server returns 206 Partial Content, and appends.
      `--no-resume` deletes the .part and starts fresh.
    * HF_TOKEN env var is honored — adds `Authorization: Bearer <token>`
      to the request, which unlocks gated repos for users with HF
      access. 401 without token prints a helpful "set HF_TOKEN" message.
- ATLAS_MODELS_DIR resolution: --models-dir flag > ATLAS_MODELS_DIR env
  > ./models/ relative to atlas_root (containing docker-compose.yml).
- --dry-run prints what would happen without touching the network or disk;
  used by tests + by users who want to verify URLs without committing.
"""

import argparse
import contextlib
import hashlib
import json
import os
import shutil
import sys
import time
import urllib.error
import urllib.request
from typing import List, Optional, Tuple

from atlas import compose as compose_config
# The repo-root resolver, imported under a module-local name so tests can
# pin it with monkeypatch.setattr(model, "_find_atlas_root", ...).
from atlas.env import atlas_root as _find_atlas_root
from atlas.commands import model_registry, tier
from atlas.commands.model_registry import Model
# HF token resolution is shared publish machinery; the canonical resolver
# (which also reads HUGGINGFACE_HUB_TOKEN) lives in atlas.publishing.
from atlas.publishing import hf_token as _hf_token


# Shared ANSI colors + unicode-safe output primitives.
from atlas.display import (
    RESET, BOLD, DIM, RED, GREEN, YELLOW as YELL,
    UNICODE_OK, DASH, safe_print as _safe_print,
)


def _resolve_models_dir(arg_models_dir: Optional[str]) -> str:
    """Resolution order: --models-dir flag > ATLAS_MODELS_DIR env >
    ATLAS_MODELS_DIR in the compose .env > ./models/ relative to
    atlas_root. Relative values resolve against the atlas root (the
    compose deployment's frame of reference), not the cwd."""
    if arg_models_dir:
        return os.path.abspath(arg_models_dir)
    atlas_root = _find_atlas_root()
    env = (os.environ.get("ATLAS_MODELS_DIR")
           or compose_config.read_env_file(atlas_root).get("ATLAS_MODELS_DIR"))
    if env:
        return env if os.path.isabs(env) else \
            os.path.normpath(os.path.join(atlas_root, env))
    return os.path.join(atlas_root, "models")


# ---------------------------------------------------------------------------
# Lens-status rendering
# ---------------------------------------------------------------------------

# How a user makes a Lens bundle for a model that has none.
BUILD = "`atlas bench`, then `atlas lens build --from-results`"


def _lens_icon(status: str, color: bool) -> str:
    if not color or not UNICODE_OK:
        return {"supported": "[OK]  ", "no-artifacts": "[WARN]",
                "unverified": "[????]"}.get(status, "[????]")
    return {"supported":   f"{GREEN}✓{RESET}",
            "no-artifacts": f"{YELL}⚠{RESET}",
            "unverified":   f"{YELL}?{RESET}"}.get(status, "?")


def _lens_label(status: str) -> str:
    return {"supported": "Lens supported",
            "no-artifacts": "Lens no-artifacts",
            "unverified": "Lens unverified"}.get(status, status)


# ---------------------------------------------------------------------------
# `atlas model list`
# ---------------------------------------------------------------------------

def _filter_models(models: List[Model], args: argparse.Namespace,
                   models_dir: str) -> List[Model]:
    out = list(models)
    if args.tier:
        out = [m for m in out if m.tier == args.tier]
    if args.installed:
        out = [m for m in out if model_registry.is_installed(m, models_dir)]
    if args.lens_supported:
        out = [m for m in out if m.lens_status == "supported"]
    return out


def _emit_list(args: argparse.Namespace, color: bool) -> int:
    models_dir = _resolve_models_dir(args.models_dir)
    models = _filter_models(model_registry.all_models(), args, models_dir)

    if args.json:
        out = []
        for m in models:
            d = model_registry.as_dict(m)
            d["installed"] = model_registry.is_installed(m, models_dir)
            d["installed_size_gb"] = model_registry.installed_size_gb(m, models_dir)
            out.append(d)
        print(json.dumps({"models_dir": models_dir, "models": out},
                         indent=2, ensure_ascii=not UNICODE_OK))
        return 0

    hdr = f"{BOLD}ATLAS model registry{RESET}" if color else "ATLAS model registry"
    _safe_print(f"{hdr} {DASH} models dir: {models_dir}")
    _safe_print()
    if not models:
        _safe_print("  (no models match these filters)")
        return 0

    # Compact table. Columns: lens-icon, name, tier, size, install-state.
    # PC-056.1 install-state precedence:
    #   installed? → "installed"
    #   no URL at all? → "(no download URL)"
    #   gated + no token? → "(requires HF_TOKEN)"
    #   gated + token present? → "(gated, HF_TOKEN OK)"
    #   else → "not installed"
    have_token = bool(_hf_token())
    for m in models:
        installed = model_registry.is_installed(m, models_dir)
        if installed:
            inst_marker = (f"{GREEN}installed{RESET}" if color else "installed")
        elif not m.can_install:
            inst_marker = (f"{DIM}(no download URL){RESET}" if color
                           else "(no download URL)")
        elif m.requires_hf_token and not have_token:
            inst_marker = (f"{YELL}(requires HF_TOKEN){RESET}" if color
                           else "(requires HF_TOKEN)")
        elif m.requires_hf_token and have_token:
            inst_marker = (f"{DIM}gated, HF_TOKEN present{RESET}" if color
                           else "gated, HF_TOKEN present")
        else:
            inst_marker = (f"{DIM}not installed{RESET}" if color
                           else "not installed")
        icon = _lens_icon(m.lens_status, color)
        name_col = f"{BOLD}{m.name}{RESET}" if color else m.name
        _safe_print(f"  {icon}  {name_col}")
        _safe_print(f"      tier: {m.tier:6s}  size: {m.model_size_gb:5.1f} GB  "
                    f"{_lens_label(m.lens_status)}  {DASH}  {inst_marker}")
        if m.lens_status == "supported" and not m.lens_calibrated:
            _safe_print(f"      {YELL if color else ''}Lens calibration: "
                        f"legacy bundle; run `atlas lens build` before "
                        f"production use{RESET if color else ''}")
        if installed:
            cur = model_registry.installed_size_gb(m, models_dir)
            if cur is not None and abs(cur - m.model_size_gb) > 0.5:
                _safe_print(f"      {YELL if color else ''}note: on-disk size "
                            f"{cur:.1f} GB differs from registered "
                            f"{m.model_size_gb:.1f} GB{RESET if color else ''}")
        _safe_print()
    _safe_print(f"  {DIM if color else ''}Run `atlas model install <name>` "
                f"to download. A model marked Lens no-artifacts installs only "
                f"with --no-lens, and ATLAS stops agent work on it until it "
                f"has its own Lens bundle ({BUILD}).{RESET if color else ''}")
    return 0


# ---------------------------------------------------------------------------
# `atlas model recommend`
# ---------------------------------------------------------------------------

def _emit_recommend(args: argparse.Namespace, color: bool) -> int:
    p = tier.probe(install_dir=args.install_dir)
    t = tier.classify(p)
    rec = model_registry.for_tier(t.tier)

    if args.json:
        out = {
            "host_tier": t.tier,
            "recommendation": (model_registry.as_dict(rec) if rec else None),
            "fallback": None,
        }
        # If the tier recommendation has no published weights, surface the
        # best available Lens bundle as a fallback.
        if rec is None or rec.lens_status != "supported":
            supported = sorted(model_registry.supported_models(),
                               key=lambda item: not item.lens_calibrated)
            if supported:
                out["fallback"] = model_registry.as_dict(supported[0])
        print(json.dumps(out, indent=2, ensure_ascii=not UNICODE_OK))
        return 0

    hdr = f"{BOLD}ATLAS model recommend{RESET}" if color else "ATLAS model recommend"
    _safe_print(f"{hdr} {DASH} matching registry to your hardware tier")
    _safe_print()
    _safe_print(f"  Detected tier: {t.tier}  ({p.gpu_name or 'no GPU'}, "
                f"{p.vram_gb:.1f} GB VRAM)")
    _safe_print()
    if rec is None:
        _safe_print(f"  {YELL if color else ''}No registered model for tier "
                    f"`{t.tier}`.{RESET if color else ''}")
        return 1
    icon = _lens_icon(rec.lens_status, color)
    _safe_print(f"  {icon}  Tier-default: {BOLD if color else ''}{rec.name}{RESET if color else ''} "
                f"({rec.model_display}, {rec.model_size_gb:.1f} GB)")
    _safe_print(f"      Lens status: {_lens_label(rec.lens_status)}")
    if rec.lens_status == "supported":
        if not rec.lens_calibrated:
            _safe_print(f"      {YELL if color else ''}Calibration required: "
                        f"published weights predate per-model C(x)/G(x) "
                        f"calibration. Run `atlas lens build` before "
                        f"production use.{RESET if color else ''}")
        if rec.can_install:
            _safe_print(f"      {GREEN if color else ''}Ready to install:"
                        f"{RESET if color else ''} "
                        f"`atlas model install {rec.name}`")
        else:
            _safe_print(f"      {YELL if color else ''}Upstream is gated; "
                        f"see SETUP.md for manual download.{RESET if color else ''}")
        return 0

    # Tier-default has no Lens artifacts. Surface a published bundle.
    _safe_print()
    _safe_print(f"  {YELL if color else ''}This tier's recommended model has "
                f"no Lens artifacts.{RESET if color else ''} The lens could not "
                f"score it, so ATLAS would stop agent work on it until you "
                f"build a bundle for it.")
    supported = sorted(model_registry.supported_models(),
                       key=lambda item: not item.lens_calibrated)
    if supported:
        f = supported[0]
        _safe_print()
        fallback_label = ("calibrated end-to-end" if f.lens_calibrated
                          else "published Lens weights; calibration required")
        _safe_print(f"  {GREEN if color else ''}Recommended fallback "
                    f"({fallback_label}):{RESET if color else ''} "
                    f"{BOLD if color else ''}{f.name}{RESET if color else ''}")
        _safe_print(f"      tier: {f.tier} (your hardware: {t.tier} {DASH} "
                    f"{'over-provisioned, fine' if t.tier in ('large','xlarge') else 'under-provisioned, may run slow'})")
        _safe_print(f"      `atlas model install {f.name}`")
    return 0


# ---------------------------------------------------------------------------
# `atlas model install`
# ---------------------------------------------------------------------------

def _human_bytes(n: float) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024:
            return f"{n:.1f} {unit}"
        n /= 1024
    return f"{n:.1f} PB"


def _remote_size_gb(url: str, token: Optional[str]) -> float:
    """Best-effort HEAD to read Content-Length (GB); 0.0 if unknown. Follows
    redirects (HF resolve → CDN). Used to disk-check --url installs that carry
    no registry size."""
    try:
        req = urllib.request.Request(url, method="HEAD")
        if token:
            req.add_header("Authorization", f"Bearer {token}")
        with urllib.request.urlopen(req, timeout=15) as resp:
            cl = resp.headers.get("Content-Length")
            return int(cl) / (1024 ** 3) if cl else 0.0
    except Exception:
        return 0.0


def _emit_install(args: argparse.Namespace, color: bool) -> int:
    if getattr(args, "url", None):
        # --url: install an UNREGISTERED model (drop-in / BYO). Synthesize a
        # registry entry with no SHA pin and no lens artifacts; the lens-status
        # gate and registry lookup below are bypassed. The download path handles
        # sha256=None (skips verification) and model_size_gb=0.0 (uses the
        # Content-Length header for progress). See docs/CONFIGURATION.md
        # "Adding your own model".
        from urllib.parse import urlparse, unquote
        fname = args.file or unquote(os.path.basename(urlparse(args.url).path))
        if not fname or not fname.lower().endswith(".gguf"):
            _safe_print(f"  {RED if color else ''}Could not infer a .gguf "
                        f"filename from the URL.{RESET if color else ''}")
            _safe_print("  Pass --file <name.gguf> to set the on-disk filename.")
            return 1
        m = Model(name=fname.rsplit(".", 1)[0], tier="unknown", model_file=fname,
                  model_display=fname, model_size_gb=0.0,
                  lens_status="no-artifacts", download_url=args.url, sha256=None,
                  notes="unregistered (installed via --url)")
        models_dir = _resolve_models_dir(args.models_dir)
        _safe_print(f"  {YELL if color else ''}Unregistered model (--url): no "
                    f"SHA pin, no bundled Lens artifacts.{RESET if color else ''}")
        _safe_print("  After download: set ATLAS_MODEL_FILE + ATLAS_MODEL_NAME "
                    "in .env, then run `atlas onboard` to finish (rebuild check "
                    "+ Lens retrain). See docs/CONFIGURATION.md \"Adding your "
                    "own model\".")
        _safe_print()
    else:
        if not args.name:
            _safe_print(f"  {RED if color else ''}Provide a registry model name "
                        f"or --url.{RESET if color else ''}")
            _safe_print("  `atlas model list` for names, or "
                        "`atlas model install --url <hf-url>` for your own model.")
            return 1
        m = model_registry.by_name(args.name)
        if m is None:
            _safe_print(f"  {RED if color else ''}Unknown model: `{args.name}`"
                        f"{RESET if color else ''}")
            _safe_print("  Run `atlas model list` to see available names.")
            return 1

        models_dir = _resolve_models_dir(args.models_dir)

        # Lens-status gate: refuse no-artifacts unless --no-lens.
        if m.lens_status != "supported" and not args.no_lens:
            _safe_print(f"  {YELL if color else ''}Refusing to install `{m.name}`: "
                        f"Lens status `{m.lens_status}`.{RESET if color else ''}")
            _safe_print()
            _safe_print("  This model has no Lens bundle of its own. The lens is "
                        "required: it cannot score this model, so ATLAS stops "
                        "agent work on it and says why. The lens loads a bundle "
                        "only for the model it was built for, so a bundle for "
                        "another quant of the same model does not load.")
            _safe_print()
            _safe_print("  To build one: install with `--no-lens`, then run "
                        f"{BUILD}.")
            return 1
        if m.lens_status == "supported" and not m.lens_calibrated:
            _safe_print(f"  {YELL if color else ''}Note: `{m.name}` has a "
                        f"legacy Lens weight bundle without current per-model "
                        f"C(x)/G(x) calibration. Installation may continue, "
                        f"but Lens interventions remain disabled until you run "
                        f"`atlas lens build`.{RESET if color else ''}")
            _safe_print()

    if not m.can_install:
        _safe_print(f"  {RED if color else ''}Cannot install `{m.name}`: "
                    f"no known download URL.{RESET if color else ''}")
        _safe_print(f"  Notes: {m.notes}")
        return 1

    # PC-056.1: HF_TOKEN gate for known-gated repos. Refuse early with
    # a helpful message rather than letting the download path 401.
    if m.requires_hf_token and not _hf_token():
        _safe_print(f"  {YELL if color else ''}`{m.name}` upstream "
                    f"({m.download_url}) requires HuggingFace authentication."
                    f"{RESET if color else ''}")
        _safe_print()
        _safe_print("  Set the HF_TOKEN env var to a HuggingFace access "
                    "token with read access:")
        _safe_print("    export HF_TOKEN='hf_xxxxxxxxxxxxxxxx'")
        _safe_print(f"    atlas model install {m.name}")
        _safe_print("  Get one at https://huggingface.co/settings/tokens")
        _safe_print()
        if m.lens_status != "supported":
            _safe_print(f"  Note: even with auth, this model has Lens "
                        f"status `{m.lens_status}`: ATLAS stops agent work on "
                        f"it until it has its own Lens bundle.")
        return 1

    target = os.path.join(models_dir, m.model_file)

    if args.dry_run:
        _safe_print("  [DRY-RUN] Would download:")
        _safe_print(f"    URL:    {m.download_url}")
        _safe_print(f"    Target: {target}")
        _safe_print(f"    Size:   ~{m.model_size_gb:.1f} GB")
        if m.sha256:
            _safe_print(f"    SHA256: {m.sha256}")
        return 0

    # Confirm before clobbering an existing file.
    if os.path.exists(target) and not args.yes:
        cur = model_registry.installed_size_gb(m, models_dir) or 0.0
        _safe_print(f"  Target file already exists: {target} ({cur:.1f} GB)")
        _safe_print(f"  Re-download will overwrite it. Pass `--yes` to "
                    f"proceed, or `atlas model remove {m.name}` first.")
        return 1

    # Make sure models_dir exists.
    try:
        os.makedirs(models_dir, exist_ok=True)
    except OSError as e:
        _safe_print(f"  {RED if color else ''}Cannot create models dir "
                    f"`{models_dir}`: {e}{RESET if color else ''}")
        return 1

    # Free-disk sanity check: refuse if free disk < 1.2 * model size.
    try:
        free_gb = shutil.disk_usage(models_dir).free / (1024 ** 3)
    except OSError:
        free_gb = 0.0
    needed = m.model_size_gb * 1.2
    if needed == 0:
        # Unregistered (--url) models carry no registry size. Probe the remote
        # Content-Length so the disk check still fails fast; if the server
        # won't report it, proceed (the mid-download OSError path keeps the
        # .part for retry) rather than refuse with an arbitrary floor.
        remote_gb = _remote_size_gb(m.download_url, _hf_token())
        if remote_gb > 0:
            needed = remote_gb * 1.2
            _safe_print(f"  Remote size ~{remote_gb:.1f} GB (from Content-Length)")
        else:
            _safe_print("  (remote size unknown — skipping disk pre-check; "
                        "ensure the partition has room)")
    if free_gb < needed:
        _safe_print(f"  {RED if color else ''}Insufficient disk: "
                    f"{free_gb:.1f} GB free, need ~{needed:.1f} GB "
                    f"(model + headroom).{RESET if color else ''}")
        _safe_print("  Free up space or pass `--models-dir` pointing "
                    "at a larger partition.")
        return 1

    _safe_print(f"  Downloading {m.name} ({m.model_size_gb:.1f} GB)")
    _safe_print(f"    From: {m.download_url}")
    _safe_print(f"    To:   {target}")
    if _hf_token():
        _safe_print("    Auth: HF_TOKEN present (will send Authorization header)")
    _safe_print()

    rc = _stream_download(m, target, color, resume=not args.no_resume)
    if rc != 0:
        return rc

    # After the gguf lands, fetch lens + asa artifacts if their URL bases
    # are populated and the corresponding *_status is supported. Without
    # this the user has a model that loads but with G(x) scoring no-op'd
    # and ASA steering disabled — the doctor flags both as failures and
    # the user has to know to download them by hand. This closes that gap.
    if not args.no_artifacts:
        artifact_rc = _install_artifacts(m, models_dir, color, args)
        if artifact_rc != 0:
            # Non-fatal: gguf is installed, just artifacts failed. Warn
            # and continue so the user can still run the model (G(x) will
            # no-op, ASA steering disabled, both surfaced by atlas doctor).
            _safe_print(f"  {YELL if color else ''}Warning: artifact "
                        f"download had failures. Run `atlas model "
                        f"install-artifacts {m.name}` to retry.{RESET if color else ''}")
    return 0


def _emit_install_artifacts(args: argparse.Namespace, color: bool) -> int:
    """`atlas model install-artifacts <name>` — fetch lens + asa artifacts
    without re-downloading the gguf. Recovery path for users who got the
    model before auto-artifact-download landed.
    """
    m = model_registry.by_name(args.name)
    if m is None:
        _safe_print(f"  {RED if color else ''}Unknown model: `{args.name}`"
                    f"{RESET if color else ''}")
        return 1
    # Distinguish "nothing registered for direct download" from a real
    # install: without a URL base there is nothing this command can fetch,
    # and reporting success would be misleading.
    has_lens_urls = (m.lens_status in ("supported", "unverified")
                     and m.lens_artifact_url_base and m.lens_artifact_files)
    has_asa_urls = (m.asa_status in ("supported", "unverified")
                    and m.asa_artifact_url_base and m.asa_artifact_files)
    if not has_lens_urls and not has_asa_urls:
        _safe_print(f"  {YELL if color else ''}No artifacts are registered "
                    f"for direct download for `{m.name}`."
                    f"{RESET if color else ''}")
        if m.lens_hf_repo:
            _safe_print(f"  Lens artifacts are published at "
                        f"https://huggingface.co/{m.lens_hf_repo} "
                        f"(fetch manually or via ATLAS_LENS_MODELS).")
        if m.asa_hf_repo:
            _safe_print(f"  ASA vector is published at "
                        f"https://huggingface.co/{m.asa_hf_repo}.")
        if not m.lens_hf_repo and not m.asa_hf_repo:
            _safe_print("  Train locally with `atlas lens build` / "
                        "`atlas asa build`.")
        return 3
    models_dir = _resolve_models_dir(args.models_dir)
    try:
        os.makedirs(models_dir, exist_ok=True)
    except OSError as e:
        _safe_print(f"  {RED if color else ''}Cannot create models dir "
                    f"`{models_dir}`: {e}{RESET if color else ''}")
        return 1
    # Reuse the same _install_artifacts helper used by the gguf-install
    # path. Need to inject the flags the helper expects.
    args.force_artifacts = getattr(args, "force_artifacts", False)
    rc = _install_artifacts(m, models_dir, color, args)
    if rc == 0:
        _safe_print(f"  {GREEN if color else ''}Artifacts for {m.name} "
                    f"installed.{RESET if color else ''}")
    return rc


def _install_artifacts(m: model_registry.Model, models_dir: str,
                        color: bool, args: argparse.Namespace) -> int:
    """Download lens + asa artifacts for the given model. Returns 0 if
    everything either succeeded or had no URL to attempt, non-zero if
    any download we DID attempt failed.

    Skip rules:
      - lens_status != 'supported' AND lens_status != 'unverified': skip
        lens. The 'no-artifacts' case has nothing to download.
      - lens_artifact_url_base is None: skip lens (e.g. for models where
        artifacts must be trained locally — `atlas lens build`).
      - Same logic for ASA via asa_*.
    """
    failures = 0
    attempted = 0

    # ----- Lens artifacts ---------------------------------------------
    if (m.lens_status in ("supported", "unverified")
            and m.lens_artifact_url_base
            and m.lens_artifact_files):
        # Target dir: the registry's per-model resolution (model override
        # > ATLAS_LENS_MODELS > <atlas_root>/geometric-lens/geometric_lens/
        # models/) so downloads land where the lens service reads them.
        atlas_root = _find_atlas_root()
        lens_dir = model_registry.lens_artifact_dir_for(m, atlas_root)
        if not lens_dir:
            # `unverified` entries with a URL base resolve like the
            # global default lens_artifact_dir_for uses.
            env_dir = (os.environ.get("ATLAS_LENS_MODELS")
                       or compose_config.read_env_file(atlas_root).get(
                           "ATLAS_LENS_MODELS"))
            if env_dir:
                lens_dir = env_dir if os.path.isabs(env_dir) else \
                    os.path.normpath(os.path.join(atlas_root, env_dir))
            else:
                lens_dir = os.path.normpath(os.path.join(
                    atlas_root, "geometric-lens", "geometric_lens", "models"))
        try:
            os.makedirs(lens_dir, exist_ok=True)
        except OSError as e:
            _safe_print(f"  Lens dir {lens_dir} not writable: {e}")
            return 1
        _safe_print(f"  Fetching {len(m.lens_artifact_files)} Lens artifact(s) "
                    f"→ {lens_dir}")
        for fname in m.lens_artifact_files:
            target_path = os.path.join(lens_dir, fname)
            if os.path.isfile(target_path) and not args.force_artifacts:
                _safe_print(f"    [skip] {fname} already present")
                continue
            attempted += 1
            url = m.lens_artifact_url_base + fname
            if _download_artifact(url, target_path, color,
                                  expected_sha256=m.lens_artifact_sha256.get(fname)) != 0:
                failures += 1

    # ----- ASA artifacts ----------------------------------------------
    if (m.asa_status in ("supported", "unverified")
            and m.asa_artifact_url_base
            and m.asa_artifact_files):
        # ASA vectors live alongside the gguf in models_dir — llama-server's
        # --control-vector-scaled takes a path relative to its model dir.
        _safe_print(f"  Fetching {len(m.asa_artifact_files)} ASA artifact(s) "
                    f"→ {models_dir}")
        for fname in m.asa_artifact_files:
            target_path = os.path.join(models_dir, fname)
            marker_path = target_path + ".model"
            marker = ""
            try:
                with open(marker_path) as marker_file:
                    marker = marker_file.read().strip()
            except OSError:
                # Missing/unreadable marker means the artifact is unverified
                # and must be replaced; marker remains empty intentionally.
                pass
            if (os.path.isfile(target_path) and not args.force_artifacts
                    and marker in (m.name, m.model_file)):
                _safe_print(f"    [skip] {fname} already present")
                continue
            if os.path.isfile(target_path) and not args.force_artifacts:
                _safe_print(f"    [replace] {fname} is unmarked or belongs "
                            f"to {marker or 'another model'}")
            attempted += 1
            url = m.asa_artifact_url_base + fname
            if _download_artifact(url, target_path, color,
                                  expected_sha256=m.asa_artifact_sha256.get(fname)) != 0:
                failures += 1
            else:
                try:
                    with open(marker_path, "w") as marker_file:
                        marker_file.write(m.name + "\n")
                except OSError as e:
                    _safe_print(f"    could not write ASA model marker: {e}")
                    failures += 1

    if attempted == 0:
        return 0  # nothing to do or everything already present
    if failures:
        return 1
    _safe_print(f"  {GREEN if color else ''}All artifacts present.{RESET if color else ''}")
    return 0


def _download_artifact(url: str, target_path: str, color: bool,
                       expected_sha256: Optional[str] = None) -> int:
    """Download a single artifact file (lens .pt or asa .gguf). These
    are small (KB to a few MB) compared to the model gguf, so no
    progress bar or resume — just a one-shot urlretrieve-style fetch
    with the HF token header if available.

    Lens .pt files are torch checkpoints and ASA vectors are fed to
    llama-server, so when the registry carries a hash the bytes are
    verified before the file is moved into place; a mismatch is a hard
    failure. Files without a registry hash download with an explicit
    "unverified" note rather than a silent [ok].
    """
    token = _hf_token()
    req = _build_request(url, range_start=0, token=token)
    fname = os.path.basename(target_path)
    tmp_path = target_path + ".part"
    try:
        hasher = hashlib.sha256()
        with urllib.request.urlopen(req, timeout=60) as resp:
            with open(tmp_path, "wb") as out:
                while True:
                    chunk = resp.read(64 * 1024)
                    if not chunk:
                        break
                    hasher.update(chunk)
                    out.write(chunk)
        if expected_sha256:
            actual = hasher.hexdigest()
            if actual != expected_sha256.lower():
                _safe_print(f"    [fail] {fname}: SHA-256 mismatch — "
                            f"expected {expected_sha256[:16]}…, got {actual[:16]}…. "
                            f"Refusing to install; the artifact may have been "
                            f"re-published (update the registry hash) or tampered with.")
                with contextlib.suppress(OSError):
                    os.remove(tmp_path)
                return 1
        os.replace(tmp_path, target_path)
        size_kb = os.path.getsize(target_path) / 1024
        if expected_sha256:
            _safe_print(f"    [ok] {fname} ({size_kb:.1f} KB, sha256 verified)")
        else:
            _safe_print(f"    [ok] {fname} ({size_kb:.1f} KB, unverified — "
                        f"no registry hash for this file)")
        return 0
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        _safe_print(f"    [fail] {fname}: {e}")
        if os.path.exists(tmp_path):
            with contextlib.suppress(OSError):
                os.remove(tmp_path)
        return 1


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


# ---------------------------------------------------------------------------
# Concurrent-install protection (PC-056.2 item A)
# ---------------------------------------------------------------------------

def _pid_alive(pid: int) -> bool:
    """POSIX trick: kill(pid, 0) raises ProcessLookupError if no such PID,
    PermissionError if PID exists but we can't signal it (other user).
    Both 'exists' cases return True — a PID owned by another user is
    still a real running process from our perspective."""
    try:
        os.kill(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    except OSError:
        # Unknown — be conservative, assume alive.
        return True


def _acquire_install_lock(target: str, color: bool) -> Optional[str]:
    """Try to atomically create <target>.lock. Return the lock path on
    success, None if another live install is in progress.

    Stale lock recovery: if the lock file's PID is no longer alive,
    delete the stale lock and try again. This handles the SIGKILL'd
    process case so users don't have to manually clean .lock files.

    Lock contents: "<pid>\\n<unix_timestamp>\\n" — useful for the
    "install in progress (PID X, started Y)" message.
    """
    lock_path = target + ".lock"
    pid = os.getpid()
    payload = f"{pid}\n{int(time.time())}\n".encode()

    # Bounded retries — at most one stale-lock reclaim per call. Without
    # the bound, a permission error reading the lock could loop forever.
    for _ in range(2):
        try:
            # 0o600 (owner-only): the lock content (PID + timestamp)
            # isn't sensitive, but tightening from 0o644 keeps CodeQL
            # happy (py/overly-permissive-file) and matches the
            # convention for runtime-control files. Other users on the
            # host don't need to read the lock — just .exists() check.
            fd = os.open(lock_path,
                          os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            try:
                os.write(fd, payload)
            finally:
                os.close(fd)
            return lock_path
        except FileExistsError:
            # Inspect existing lock — alive or stale?
            try:
                with open(lock_path) as f:
                    parts = f.read().strip().split("\n")
                other_pid = int(parts[0]) if parts and parts[0] else 0
                started = int(parts[1]) if len(parts) > 1 else 0
            except (OSError, ValueError, IndexError):
                # Can't read or parse — treat as stale, try to remove.
                try:
                    os.unlink(lock_path)
                    continue
                except OSError:
                    return None

            if other_pid and _pid_alive(other_pid):
                age_s = max(int(time.time()) - started, 0) if started else None
                age_msg = (f", started ~{age_s}s ago" if age_s is not None
                           else "")
                _safe_print(f"  {RED if color else ''}Another install is "
                            f"already in progress: PID {other_pid}{age_msg}."
                            f"{RESET if color else ''}")
                _safe_print("  Wait for it to finish, or if you're sure "
                            "it's hung, manually remove the lock:")
                _safe_print(f"    rm {lock_path}")
                return None

            # Stale — process is gone. Reclaim by deleting + retrying.
            try:
                os.unlink(lock_path)
            except OSError:
                return None
            # loop continues for one more attempt
    return None


def _release_install_lock(lock_path: Optional[str]) -> None:
    """Best-effort lock release. Errors ignored — we'd rather leak a
    lock file than crash the CLI on the cleanup path."""
    if lock_path is None:
        return
    try:
        os.unlink(lock_path)
    except OSError:
        # best-effort: swallow on failure (caller continues)
        pass


def _stream_download(m: Model, target: str, color: bool,
                     resume: bool = True) -> int:
    """Stream-download the model with progress bar, SHA verification,
    resume support, HF_TOKEN auth (PC-056.1), concurrent-install lock,
    and oversized .part detection (PC-056.2).

    Resume strategy:
      - If <target>.part exists and resume=True: send Range: bytes=N-,
        expect 206 Partial Content, append-write, hash from start by
        also reading the existing .part contents into the SHA digest.
      - If resume=False: delete any existing .part, start fresh.
      - If the server returns 200 OK to a Range request (some endpoints
        ignore Range), we restart from byte 0 cleanly.
      - PC-056.2: if existing .part is suspiciously larger than the
        registered model size (>5% over), refuse — likely user-corrupted.

    SHA verification:
      - hashlib.sha256.update() runs alongside file.write() during the
        chunk loop. After download completes, hexdigest() is compared
        to model.sha256. Mismatch deletes the file and returns 1.
      - When model.sha256 is None (no expected hash), we skip the
        comparison and print a warning so users know integrity wasn't
        verified end-to-end.

    HF auth:
      - HF_TOKEN env var (or HUGGING_FACE_HUB_TOKEN as alt spelling) is
        added as Authorization: Bearer header. Unlocks gated repos for
        authenticated users.
      - On 401 without a token: helpful message points at HF_TOKEN.

    Concurrent-install lock (PC-056.2):
      - <target>.part.lock acquired with O_CREAT|O_EXCL before any I/O.
      - Stale locks from SIGKILL'd processes are reclaimed via
        os.kill(pid, 0) liveness check.
      - Released on any exit path (success, error, KeyboardInterrupt).
    """
    tmp = target + ".part"
    chunk = 1024 * 1024  # 1 MiB
    started = time.monotonic()
    token = _hf_token()

    # PC-056.2 item A: acquire lock before touching .part.
    lock_path = _acquire_install_lock(tmp, color)
    if lock_path is None:
        return 1

    try:
        return _stream_download_locked(m, target, tmp, chunk, started,
                                        token, color, resume)
    finally:
        _release_install_lock(lock_path)


def _stream_download_locked(m: Model, target: str, tmp: str, chunk: int,
                              started: float, token: Optional[str],
                              color: bool, resume: bool) -> int:
    """The actual download logic, factored out so the lock-release in
    `_stream_download`'s `finally` always runs even on early returns
    here. No new behavior — just a structural split."""
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
                return 1
            file_mode = "ab"
            _safe_print(f"  Resuming from byte {range_start} "
                        f"({_human_bytes(range_start)} already on disk)")
    elif not resume and os.path.exists(tmp):
        try:
            os.unlink(tmp)
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass

    bytes_seen = range_start  # for progress
    last_print = 0.0
    h = hashlib.sha256()

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
            return 1

    try:
        req = _build_request(m.download_url, range_start=range_start, token=token)
        with urllib.request.urlopen(req, timeout=60) as resp:
            status = resp.getcode()
            # If we sent Range and got 200, server ignored it — restart.
            if range_start > 0 and status == 200:
                _safe_print(f"  {YELL if color else ''}Server ignored "
                            f"Range header — restarting from byte 0."
                            f"{RESET if color else ''}")
                file_mode = "wb"
                range_start = 0
                bytes_seen = 0
                h = hashlib.sha256()
            elif range_start > 0 and status != 206:
                _safe_print(f"  {RED if color else ''}Unexpected response "
                            f"to Range request: HTTP {status}. Aborting."
                            f"{RESET if color else ''}")
                return 1

            # Total size — for resume, Content-Length is the REMAINING
            # bytes; we want absolute total for progress display.
            cl = resp.headers.get("Content-Length")
            if status == 206 and cl:
                total_n = int(cl) + range_start
            else:
                total_n = int(cl) if cl else 0

            with open(tmp, file_mode) as f:
                while True:
                    buf = resp.read(chunk)
                    if not buf:
                        break
                    f.write(buf)
                    h.update(buf)
                    bytes_seen += len(buf)
                    now = time.monotonic()
                    if now - last_print > 0.25 or (total_n and bytes_seen >= total_n):
                        last_print = now
                        _print_progress(bytes_seen, total_n, started, color)
    except KeyboardInterrupt:
        _safe_print()
        _safe_print(f"  {YELL if color else ''}Interrupted. .part file "
                    f"kept for resume — re-run install to continue."
                    f"{RESET if color else ''}")
        return 130
    except urllib.error.HTTPError as e:
        _safe_print()
        if e.code == 416 and range_start > 0:
            # Requested range starts at/after the end of the file — the
            # .part usually already holds the complete download. Verify
            # it (hash when the registry has one, size otherwise) and
            # finalize; on verification failure delete the .part so the
            # next attempt starts clean.
            return _finalize_complete_part(m, tmp, target, range_start,
                                            h, e, color)
        if e.code == 401:
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
        else:
            _safe_print(f"  {RED if color else ''}Download failed: HTTP "
                        f"{e.code} {e.reason}{RESET if color else ''}")
        # Don't delete .part on auth/HTTP errors — user may fix env and resume.
        return 1
    except (urllib.error.URLError, OSError) as e:
        _safe_print()
        _safe_print(f"  {RED if color else ''}Download failed: {e}"
                    f"{RESET if color else ''}")
        # Network errors: keep .part for retry.
        return 1

    _safe_print()

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
        return 1

    # SHA256 verification (PC-056.1).
    if m.sha256:
        actual_hash = h.hexdigest()
        if actual_hash != m.sha256:
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
            return 1
        _safe_print(f"  {GREEN if color else ''}SHA256 verified.{RESET if color else ''} "
                    f"({m.sha256[:16]}…)")
    else:
        _safe_print(f"  {YELL if color else ''}Note: registry has no "
                    f"expected SHA256 for this model — download integrity "
                    f"NOT verified end-to-end.{RESET if color else ''}")

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


def _finalize_complete_part(m: Model, tmp: str, target: str,
                             range_start: int, h: "hashlib._Hash",
                             e: "urllib.error.HTTPError", color: bool) -> int:
    """HTTP 416 on resume: the .part already spans the whole file. Verify
    it (SHA256 when the registry pins one, otherwise the total from the
    416's Content-Range header) and promote it via os.replace. A .part that
    can't be verified — failing SHA256, or with neither a SHA256 pin nor a
    Content-Range to check against — is deleted with retry guidance rather
    than promoted, so a corrupt or mismatched part is never finalized."""
    # Total size, when the server reports it ("Content-Range: bytes */N").
    total = None
    content_range = ""
    try:
        content_range = (e.headers.get("Content-Range") or "") if e.headers else ""
    except AttributeError:
        content_range = ""
    if content_range.startswith("bytes */"):
        try:
            total = int(content_range.split("/", 1)[1])
        except ValueError:
            total = None

    # Decide whether the on-disk .part is trustworthy enough to promote.
    # Two accepted proofs, in priority order:
    #   1. SHA256 match, when the registry pins one. Authoritative — if a
    #      hash is registered it ALONE decides, and a mismatch deletes the
    #      .part rather than falling back to a weaker size check (otherwise a
    #      corrupt part whose size happens to line up would be promoted).
    #   2. A Content-Range total from the 416 response that the .part size
    #      matches within the existing tolerance. Used only when no SHA256 is
    #      registered (BYO / --url models).
    # When NEITHER proof is available (no SHA256 and no Content-Range), we
    # cannot tell a complete download from a corrupt or mismatched .part —
    # e.g. the user re-ran with a different --url or a smaller file under the
    # same --file — so we refuse to finalize and delete the .part instead of
    # promoting it blindly.
    verified = False
    how = ""
    if m.sha256:
        verified = h.hexdigest() == m.sha256
        how = "SHA256"
    elif total is not None:
        verified = abs(range_start - total) <= int(total * 0.05)
        how = "size (Content-Range)"

    if not verified:
        detail = how or "no SHA256 pin or Content-Range header to verify against"
        _safe_print(f"  {RED if color else ''}Server reports the download "
                    f"is complete (HTTP 416), but the existing .part could "
                    f"not be verified ({detail})."
                    f"{RESET if color else ''}")
        try:
            os.unlink(tmp)
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
        _safe_print("  Removed the .part file. Re-run the install to "
                    "download a fresh copy from scratch.")
        return 1

    try:
        os.replace(tmp, target)
    except OSError as replace_err:
        _safe_print(f"  {RED if color else ''}Failed to move into place: "
                    f"{replace_err}{RESET if color else ''}")
        return 1
    _safe_print(f"  {GREEN if color else ''}Download was already complete "
                f"(verified via {how}).{RESET if color else ''} "
                f"{_human_bytes(range_start)} moved into place.")
    return 0


def _print_progress(seen: int, total: int, started: float, color: bool) -> None:
    elapsed = max(time.monotonic() - started, 0.001)
    rate = seen / elapsed
    if total:
        pct = seen / total * 100
        eta = (total - seen) / rate if rate > 0 else 0
        bar_w = 30
        fill = int(bar_w * seen / total)
        bar = "=" * fill + ">" + " " * max(bar_w - fill - 1, 0)
        msg = (f"  [{bar[:bar_w]}] {pct:5.1f}%  "
               f"{_human_bytes(seen)} / {_human_bytes(total)}  "
               f"{_human_bytes(rate)}/s  ETA {eta:5.0f}s")
    else:
        msg = (f"  {_human_bytes(seen)} downloaded  "
               f"{_human_bytes(rate)}/s")
    sys.stdout.write("\r" + msg)
    sys.stdout.flush()


# ---------------------------------------------------------------------------
# `atlas model verify` (PC-056.1)
# ---------------------------------------------------------------------------

def _verify_one(m: Model, models_dir: str) -> Tuple[str, str]:
    """Run verify_installed on a single model and return
    (status, message) for table rendering. Status is one of
    'ok', 'mismatch', 'no-expected', 'missing'."""
    result = model_registry.verify_installed(m, models_dir)
    match = result["match"]
    if match == "missing":
        return "missing", "not installed"
    if match == "no-expected":
        sz = result["actual_size_gb"]
        return "no-expected", (f"installed ({sz:.1f} GB) but registry has "
                                f"no expected SHA256 — cannot verify")
    if match == "ok":
        return "ok", (f"SHA256 OK ({result['actual_sha256'][:16]}…)")
    # mismatch
    return "mismatch", (f"SHA256 MISMATCH "
                         f"expected {result['expected_sha256'][:16]}… "
                         f"got {result['actual_sha256'][:16]}…")


def _verify_icon(status: str, color: bool) -> str:
    if not color or not UNICODE_OK:
        return {"ok": "[OK]    ", "mismatch": "[FAIL]  ",
                "no-expected": "[?]     ", "missing": "[skip]  "}[status]
    return {"ok": f"{GREEN}✓{RESET}", "mismatch": f"{RED}✗{RESET}",
            "no-expected": f"{YELL}?{RESET}",
            "missing": f"{DIM}-{RESET}"}[status]


def _emit_verify(args: argparse.Namespace, color: bool) -> int:
    """Verify one model (if name given) or all installed models.

    Exit codes:
      0 — every checked model matched (or had nothing to check)
      1 — at least one mismatch (corrupted file or stale registry)
    """
    models_dir = _resolve_models_dir(args.models_dir)
    if args.name:
        m = model_registry.by_name(args.name)
        if m is None:
            _safe_print(f"  {RED if color else ''}Unknown model: `{args.name}`"
                        f"{RESET if color else ''}")
            return 1
        targets = [m]
    else:
        targets = [m for m in model_registry.all_models()
                    if model_registry.is_installed(m, models_dir)]

    if args.json:
        results = []
        for m in targets:
            r = model_registry.verify_installed(m, models_dir)
            r["name"] = m.name
            results.append(r)
        any_mismatch = any(r["match"] == "mismatch" for r in results)
        print(json.dumps({"models_dir": models_dir, "results": results,
                          "any_mismatch": any_mismatch},
                          indent=2, ensure_ascii=not UNICODE_OK))
        return 1 if any_mismatch else 0

    hdr = (f"{BOLD}ATLAS model verify{RESET}" if color
           else "ATLAS model verify")
    _safe_print(f"{hdr} {DASH} hashing installed models in {models_dir}")
    _safe_print()
    if not targets:
        _safe_print("  No installed models to verify.")
        return 0
    any_mismatch = False
    for m in targets:
        status, msg = _verify_one(m, models_dir)
        icon = _verify_icon(status, color)
        name = f"{BOLD}{m.name}{RESET}" if color else m.name
        _safe_print(f"  {icon}  {name}")
        _safe_print(f"      {msg}")
        if status == "mismatch":
            any_mismatch = True
    _safe_print()
    if any_mismatch:
        _safe_print(f"  {RED if color else ''}One or more models failed "
                    f"SHA verification.{RESET if color else ''} The file "
                    f"may be corrupted, OR the registry's expected SHA is "
                    f"stale (upstream re-uploaded). Re-install with "
                    f"`atlas model install <name> --no-resume` to fetch "
                    f"a fresh copy.")
        return 1
    return 0


# ---------------------------------------------------------------------------
# `atlas model remove`
# ---------------------------------------------------------------------------

def _emit_remove(args: argparse.Namespace, color: bool) -> int:
    m = model_registry.by_name(args.name)
    if m is None:
        _safe_print(f"  {RED if color else ''}Unknown model: `{args.name}`"
                    f"{RESET if color else ''}")
        return 1
    models_dir = _resolve_models_dir(args.models_dir)
    target = os.path.join(models_dir, m.model_file)
    if not os.path.exists(target):
        _safe_print(f"  Model `{m.name}` is not installed at {target}.")
        return 0
    if not args.yes:
        cur = model_registry.installed_size_gb(m, models_dir) or 0.0
        _safe_print(f"  About to delete: {target} ({cur:.1f} GB)")
        _safe_print("  Pass `--yes` to confirm.")
        return 1
    try:
        os.unlink(target)
    except OSError as e:
        _safe_print(f"  {RED if color else ''}Failed to delete: {e}"
                    f"{RESET if color else ''}")
        return 1
    _safe_print(f"  {GREEN if color else ''}Removed:{RESET if color else ''} "
                f"{target}")
    return 0


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas model",
        description="Model registry: list, install, remove, recommend (PC-056)")
    sub = parser.add_subparsers(dest="subcommand")

    p_list = sub.add_parser("list", help="show known models")
    p_list.add_argument("--tier", choices=["cpu","small","medium","large","xlarge"],
        help="filter to a specific tier")
    p_list.add_argument("--installed", action="store_true",
        help="show only models already on disk")
    p_list.add_argument("--lens-supported", action="store_true",
        help="show only models with trained Lens artifacts")
    p_list.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    p_list.add_argument("--json", action="store_true",
        help="machine output")
    p_list.add_argument("--no-color", action="store_true")

    p_rec = sub.add_parser("recommend",
        help="best model for this hardware (composes atlas tier + registry)")
    p_rec.add_argument("--install-dir", default=None,
        help="probe disk free against this path (defaults to /)")
    p_rec.add_argument("--json", action="store_true")
    p_rec.add_argument("--no-color", action="store_true")

    p_inst = sub.add_parser("install", help="download a model into ATLAS_MODELS_DIR")
    p_inst.add_argument("name", nargs="?", default=None,
        help="registry model name (see `atlas model list`); omit when using --url")
    p_inst.add_argument("--url", default=None,
        help="direct download URL for an UNREGISTERED model (e.g. a HuggingFace "
             "resolve/main/*.gguf link). Skips the registry + SHA pin; pair with "
             "--file to set the on-disk name. See docs/CONFIGURATION.md "
             "\"Adding your own model\".")
    p_inst.add_argument("--file", default=None,
        help="on-disk filename for --url installs (default: basename of the URL)")
    p_inst.add_argument("--dry-run", action="store_true",
        help="print what would happen, no network or disk writes")
    p_inst.add_argument("--no-lens", action="store_true",
        help="install a model that has no Lens bundle yet, to build one "
             "(ATLAS stops agent work on it until the bundle exists)")
    p_inst.add_argument("--yes", action="store_true",
        help="overwrite existing file without prompt")
    p_inst.add_argument("--no-resume", action="store_true",
        help="ignore any existing .part file and start the download "
             "from byte 0 (default: resume from .part if present)")
    p_inst.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    p_inst.add_argument("--no-artifacts", action="store_true",
        help="skip auto-download of Lens + ASA artifacts after the "
             "gguf lands. Useful for air-gapped installs or when you "
             "want to train Lens locally via `atlas lens build`.")
    p_inst.add_argument("--force-artifacts", action="store_true",
        help="re-download artifacts even when they're already present "
             "on disk (default: skip already-present files)")
    p_inst.add_argument("--no-color", action="store_true")

    # `atlas model install-artifacts <name>` — fetch lens + asa artifacts
    # without re-downloading the gguf. The recovery path for users who
    # installed the model before auto-artifact-download landed (or who
    # used --no-artifacts and now want them).
    p_artifacts = sub.add_parser("install-artifacts",
        help="download Lens + ASA artifacts for a model without "
             "re-downloading the gguf")
    p_artifacts.add_argument("name", help="model name (see `atlas model list`)")
    p_artifacts.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    p_artifacts.add_argument("--force-artifacts", action="store_true",
        help="re-download even when files are already present")
    p_artifacts.add_argument("--no-color", action="store_true")

    p_ver = sub.add_parser("verify",
        help="recompute SHA256 of installed file(s) vs the registry "
             "(PC-056.1)")
    p_ver.add_argument("name", nargs="?", default=None,
        help="optional: verify only this model. Default: verify all "
             "installed models.")
    p_ver.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    p_ver.add_argument("--json", action="store_true")
    p_ver.add_argument("--no-color", action="store_true")

    p_rm = sub.add_parser("remove", help="delete a model file from ATLAS_MODELS_DIR")
    p_rm.add_argument("name", help="model name (see `atlas model list`)")
    p_rm.add_argument("--yes", action="store_true", help="skip confirmation")
    p_rm.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    p_rm.add_argument("--no-color", action="store_true")

    args = parser.parse_args(argv)
    if args.subcommand is None:
        parser.print_help()
        return 1

    color = (sys.stdout.isatty() and not getattr(args, "no_color", False)
             and not getattr(args, "json", False))

    if args.subcommand == "list":
        return _emit_list(args, color)
    if args.subcommand == "recommend":
        return _emit_recommend(args, color)
    if args.subcommand == "install":
        return _emit_install(args, color)
    if args.subcommand == "install-artifacts":
        return _emit_install_artifacts(args, color)
    if args.subcommand == "verify":
        return _emit_verify(args, color)
    if args.subcommand == "remove":
        return _emit_remove(args, color)
    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
