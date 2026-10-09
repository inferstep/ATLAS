"""atlas publish — ship a model's full artifact set in one step (PC-215).

After onboarding a model, both lens halves (C(x) + G(x)) and the ASA
steering vector exist together — publishing them is one action, not two.
This command uploads the lens artifacts and the ASA vector to their HF
repos and opens a SINGLE registry PR whose entry carries both
`lens_status="supported"` and `asa_status="unverified"` (a vector is
promoted to "supported" by hand, citing an A/B result).

The per-component commands stay available for the independent cases
(`atlas lens publish` / `atlas asa publish`, or the --lens-only /
--asa-only flags here, which delegate to them). Those flows handle the
publish-while-the-other-PR-is-open sequencing via stacked PRs; the
combined command never needs it.

Usage:
    atlas publish --lens-repo USER/atlas-lens-mymodel \
                  --asa-repo  USER/atlas-asa-mymodel
    atlas publish --lens-only --lens-repo USER/REPO   # delegate to lens publish
    atlas publish --asa-only  --asa-repo  USER/REPO   # delegate to asa publish
"""

import argparse
import os
import sys
from typing import List, Optional

from atlas import publishing
from atlas import env as cli_env
from atlas.display import (
    RESET, RED, GREEN, YELLOW as YELL,
    safe_print as _safe_print,
)
from atlas.commands import lens as lens_module
from atlas.commands import asa as asa_module
from atlas.commands.lens import _render_registry_pr_body
from atlas.commands.asa import (
    _read_cvector_meta, _render_asa_pr_body,
    _configured_vector_path, _host_resolve_vector_path,
)


def _emit_publish_all(args: argparse.Namespace, color: bool) -> int:  # noqa: C901
    atlas_root = cli_env.atlas_root()

    # Resolve the model once, the same way the component flows do.
    matched = publishing.resolve_model_arg(args.model)
    model_label = matched.name if matched else (args.model or "")
    if not model_label:
        try:
            from atlas.commands import fit as fit_module
            mp = fit_module._default_model_path()
            if mp:
                model_label = os.path.splitext(os.path.basename(mp))[0]
        except Exception:  # noqa: S110
            # Model autodetection is optional here; the explicit validation
            # below tells the publisher to pass a model when it fails.
            pass
    if not model_label:
        _safe_print(f"  {RED if color else ''}No model resolved — pass one "
                    f"or set ATLAS_MODEL_FILE in .env.{RESET if color else ''}")
        return 1

    # Pre-flight both artifact sets BEFORE uploading anything: a combined
    # publish should not half-succeed on a missing vector.
    check = lens_module._check_model(args.model, atlas_root)
    if check.verdict == "incompatible" or not check.artifact_dir:
        _safe_print(f"  {RED if color else ''}Lens pre-flight failed: "
                    f"{check.reason}{RESET if color else ''}")
        return 1
    artifact_dir = check.artifact_dir
    missing = [f for f in ("cost_field.pt", "gx_xgboost.json")
               if not os.path.isfile(os.path.join(artifact_dir, f))]
    vpath = _host_resolve_vector_path(_configured_vector_path(atlas_root),
                                      atlas_root)
    if not os.path.isfile(vpath):
        missing.append(os.path.basename(vpath))
    if missing:
        _safe_print(f"  {RED if color else ''}Missing artifacts: "
                    f"{', '.join(missing)}.{RESET if color else ''}")
        _safe_print("  Run `atlas lens build` / `atlas asa build` first, or "
                    "publish one component: --lens-only / --asa-only.")
        return 1

    # Upload each component through its own flow, deferring the PR.
    _safe_print(f"{GREEN if color else ''}── Lens artifacts ──"
                f"{RESET if color else ''}")
    lens_args = ["publish", "--skip-pr"]
    if args.lens_repo:
        lens_args += ["--repo", args.lens_repo]
    if args.model:
        lens_args.insert(1, args.model)
    if args.license:
        lens_args += ["--license", args.license]
    if args.dry_run:
        lens_args.append("--dry-run")
    if getattr(args, "no_color", False):
        lens_args.append("--no-color")
    rc = lens_module.main(lens_args)
    if rc != 0:
        _safe_print(f"  {RED if color else ''}Lens upload failed (rc={rc}) — "
                    f"stopping before the ASA upload.{RESET if color else ''}")
        return rc

    _safe_print(f"{GREEN if color else ''}── ASA vector ──"
                f"{RESET if color else ''}")
    asa_args = ["publish", "--skip-pr"]
    if args.asa_repo:
        asa_args += ["--repo", args.asa_repo]
    if args.model:
        asa_args.insert(1, args.model)
    if args.license:
        asa_args += ["--license", args.license]
    if args.dry_run:
        asa_args.append("--dry-run")
    if getattr(args, "no_color", False):
        asa_args.append("--no-color")
    rc = asa_module.main(asa_args)
    if rc != 0:
        _safe_print(f"  {RED if color else ''}ASA upload failed (rc={rc}). "
                    f"Lens artifacts are uploaded; fix and re-run (uploads "
                    f"are idempotent).{RESET if color else ''}")
        return rc

    if args.dry_run:
        _safe_print(f"  {GREEN if color else ''}Dry-run complete — no PR "
                    f"opened.{RESET if color else ''}")
        return 0
    if args.skip_pr:
        _safe_print("  (--skip-pr: bodies printed above; paste into "
                    f"https://github.com/{publishing.UPSTREAM_REPO}/compare)")
        return 0

    # One registry PR carrying the complete entry: lens + ASA fields.
    _safe_print(f"{GREEN if color else ''}── Registry PR ──"
                f"{RESET if color else ''}")
    inspection = lens_module._inspect_cost_field(artifact_dir)
    dim = inspection.dim or 0
    cost_path = os.path.join(artifact_dir, "cost_field.pt")
    lens_sha = publishing.sha256_file(cost_path)
    license_id = args.license or "apache-2.0"

    lens_files = ["cost_field.pt"]
    st = os.path.join(artifact_dir, "cost_field.safetensors")
    if (os.path.isfile(st)
            and os.path.getmtime(st) >= os.path.getmtime(cost_path)):
        lens_files.append("cost_field.safetensors")
    for opt in ("gx_xgboost.json", "gx_weights.json"):
        if os.path.isfile(os.path.join(artifact_dir, opt)):
            lens_files.append(opt)

    vec_meta = _read_cvector_meta(vpath)
    vec_sha = publishing.sha256_file(vpath)
    vector_name = os.path.basename(vpath)
    layer = vec_meta.get("steered_layer") or 0

    entry_tier = "medium"
    try:
        from atlas.commands.tier import classify, probe
        entry_tier = classify(probe()).tier
    except Exception:  # noqa: S110
        # Tier is advisory registry metadata; medium is the safe fallback.
        pass
    model_file = model_label + ".gguf"
    size_gb = 0.0
    try:
        model_file = cli_env.MODEL_FILE
        base = (cli_env.MODEL_DIR if os.path.isabs(cli_env.MODEL_DIR)
                else os.path.join(atlas_root, cli_env.MODEL_DIR))
        size_gb = round(os.path.getsize(
            os.path.join(base, model_file)) / (1024 ** 3), 1)
    except Exception:  # noqa: S110
        # Size is optional publishing metadata; artifact validation and hashes
        # remain mandatory regardless of this lookup.
        pass

    entry = publishing.render_registry_entry(model_label, model_file, size_gb,
                                             entry_tier, dim, args.lens_repo,
                                             license_id, lens_files)

    def edit(content):
        # Insert the lens entry, then flip its ASA fields — composing the
        # two single-component edits gives the complete entry. For models
        # already registered upstream, fall through to the component
        # flows' own PR logic instead of guessing at an update.
        inserted = publishing.registry_insert_entry(content, model_label, entry)
        if inserted is None:
            return None
        return publishing.registry_set_asa(inserted, model_label,
                                           args.asa_repo, [vector_name])

    body = (_render_registry_pr_body(model_label, args.lens_repo,
                                     model_label, dim, lens_sha, license_id,
                                     artifact_files=lens_files)
            + "\n\n---\n\n"
            + _render_asa_pr_body(model_label, args.asa_repo, model_label,
                                  dim, layer, vec_sha, license_id))
    title = (f"Registry: add Lens + ASA artifacts for {model_label} "
             f"(via atlas publish)")
    pr_url = publishing.open_registry_pr_via_api(model_label, title, body,
                                                 edit)
    if pr_url:
        _safe_print(f"  {GREEN if color else ''}PR opened: "
                    f"{pr_url}{RESET if color else ''}")
        return 0
    _safe_print(f"  {YELL if color else ''}Could not open the PR "
                f"automatically (already registered upstream?) — bodies "
                f"printed above; or use `atlas lens publish` / `atlas asa "
                f"publish` for per-component PRs.{RESET if color else ''}")
    _safe_print("")
    _safe_print(body)
    return 0


def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(
        prog="atlas publish",
        description="Publish a model's lens artifacts + ASA vector and open "
                    "one registry PR covering both.")
    ap.add_argument("model", nargs="?", default=None,
        help="registry name (default: the configured model from .env)")
    ap.add_argument("--lens-repo", default=None,
        help="HF repo for the lens artifacts (USER/atlas-lens-<model>)")
    ap.add_argument("--asa-repo", default=None,
        help="HF repo for the ASA vector (USER/atlas-asa-<model>)")
    ap.add_argument("--lens-only", action="store_true",
        help="publish only the lens (delegates to `atlas lens publish`)")
    ap.add_argument("--asa-only", action="store_true",
        help="publish only the ASA vector (delegates to `atlas asa publish`)")
    ap.add_argument("--license", default=None,
        help="SPDX license for the artifacts (default apache-2.0)")
    ap.add_argument("--dry-run", action="store_true",
        help="hash + render everything, upload nothing, open no PR")
    ap.add_argument("--skip-pr", action="store_true",
        help="upload to HF but print the PR bodies instead of opening one")
    ap.add_argument("--no-color", action="store_true")
    args = ap.parse_args(argv)
    color = not args.no_color and sys.stdout.isatty()

    if args.lens_only and args.asa_only:
        _safe_print("  --lens-only and --asa-only are mutually exclusive "
                    "(omit both to publish both).")
        return 2
    if args.lens_only:
        sub = ["publish"]
        if args.model:
            sub.append(args.model)
        if args.lens_repo:
            sub += ["--repo", args.lens_repo]
        if args.license:
            sub += ["--license", args.license]
        for flag in ("dry_run", "skip_pr", "no_color"):
            if getattr(args, flag):
                sub.append("--" + flag.replace("_", "-"))
        return lens_module.main(sub)
    if args.asa_only:
        sub = ["publish"]
        if args.model:
            sub.append(args.model)
        if args.asa_repo:
            sub += ["--repo", args.asa_repo]
        if args.license:
            sub += ["--license", args.license]
        for flag in ("dry_run", "skip_pr", "no_color"):
            if getattr(args, flag):
                sub.append("--" + flag.replace("_", "-"))
        return asa_module.main(sub)

    if not args.dry_run and (not args.lens_repo or not args.asa_repo):
        _safe_print("  Both --lens-repo and --asa-repo are required for a "
                    "combined publish (or --lens-only / --asa-only for one "
                    "component).")
        return 2
    return _emit_publish_all(args, color)


if __name__ == "__main__":
    raise SystemExit(main())
