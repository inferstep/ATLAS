"""The three vetoes that can reject a candidate the sandbox passed: the Lens
veto (a generation pattern collapsed toward a stub), the structural veto (a
direct call that resolves to nothing) and the call-graph veto (a cross-file
call that resolves to no in-scope definition). Each takes the passing
candidates and returns the ones it keeps; a vetoed one is marked failing."""

import symbols
from candidate_records import _who

def _lens_verdict(per_step):
    """Whether one candidate's per-step Lens scores fall below the model's
    severe band. Returns (vetoed, the score read, the band it is read
    against)."""
    # Vetoes read the MEAN, not the MIN.
    #
    # gx_score_min is a minimum over every token, so it falls with
    # length whatever the content (one repeated function: 0.468 at 20
    # tokens, 0.305 at 305), and it cannot separate: real code 0.325, a
    # repetition loop 0.320 and stub spam 0.286 sit together.
    #
    # gx_score_mean holds across the same 15x length change (0.577 to
    # 0.517) and does separate: real code 0.594, repetition 0.485, stub
    # spam 0.467. Clean live scores range 0.547-0.651, so severe_mean at
    # 0.52 sits below every clean sample and above both pathologies.
    gx_mean = per_step.get("gx_score_mean")
    gx_min = per_step.get("gx_score_min")
    thresholds = per_step.get("thresholds") or {}
    severe_mean = thresholds.get("severe_mean")
    severe = thresholds.get("severe")
    if (gx_mean is not None
            and isinstance(severe_mean, (int, float))):
        vetoed_now = gx_mean < severe_mean
        gx_min, severe = gx_mean, severe_mean
    else:
        # Artifacts predating severe_mean keep the old check.
        vetoed_now = (gx_min is not None
                      and isinstance(severe, (int, float))
                      and gx_min < severe)
    return vetoed_now, gx_min, severe


def _lens_veto(passing, emit):
    """PC-207: a sandbox-passing candidate whose generation pattern the Lens
    reads as collapsed toward a stub is vetoed: marked failing, with the
    reason as its stderr. Returns the candidates it keeps."""
    # ===== LENS VETO =====
    # Hard-reject sandbox-passing candidates whose lens score sits below
    # THIS model's calibrated severe band (the per-step mean when the
    # artifact declares severe_mean, else the min). Sandbox is an ORM
    # (does it execute?), lens is a PRM (is the generation pattern
    # collapsing into a stub?): they answer different questions, and a
    # stub page can pass the sandbox. Without this filter, V3 returns
    # passed=True for it and the proxy nudges the agent to done.
    #
    # Language-agnostic by construction: the lens runs on the model's
    # residual stream; gx values don't depend on whether the file
    # being scored is HTML, Python, Rust, or Java.
    if passing:
        kept, vetoed = [], []
        for c in passing:
            per_step = c.get("per_step") or {}
            vetoed_now, gx_min, severe = _lens_verdict(per_step)
            if vetoed_now:
                # A vetoed candidate is a failing candidate: mark it so
                # the phase-3 pool (`not c.get("passed")`) picks it up
                # and the energy fallback can never return it. The veto
                # reason replaces the (empty) passing-run stderr so
                # repair sees WHY it was rejected.
                c["passed"] = False
                c["vetoed_by"] = "lens"
                c["stderr"] = (
                    f"lens veto: gx_min={gx_min:.3f} below the severe "
                    f"threshold {severe:.3f} — generation pattern "
                    f"collapsed toward a stub; the code executes but "
                    f"likely does not implement the task")
                vetoed.append(c)
                emit("lens_veto",
                     f"{_who(c)} sandbox-passed but lens-vetoed "
                     f"(gx_min={gx_min:.3f} < {severe:.3f}) — likely a stub",
                     index=c["index"], gx_score_min=gx_min,
                     first_off_rails_idx=per_step.get("first_off_rails_idx", -1))
            else:
                kept.append(c)
        if vetoed:
            print(
                f"  [lens] vetoed {len(vetoed)}/{len(passing)} sandbox-passing "
                f"candidates using per-model severe thresholds — falling "
                f"{'through to phase-3 repair' if not kept else 'back to remaining %d' % len(kept)}",
                flush=True,
            )
        passing = kept
    return passing


def _structural_veto(passing, files, smoke_language, emit):
    """GH #39 point 1: for Python, a sandbox-passing candidate with a direct
    call that resolves to nothing is vetoed. Returns the candidates kept."""
    # ===== STRUCTURAL VETO =====
    # Hard-reject candidates whose direct-identifier calls don't resolve
    # against (local defs, imports, builtins, project symbols). Sandbox
    # can pass for code where the unresolved call is in a try/except
    # ImportError fallback or a dead branch that doesn't execute under
    # the tests; tree-sitter sees the surface bug regardless. Same
    # architecture as lens veto.
    #
    # Python only, enforced. structural_score parses with the Python
    # grammar, and tree-sitter parses anything tolerantly: an HTML
    # template's `<th>Time (mins)</th>` reads as a call to Time and
    # `rgba(...)` in a stylesheet as a call to rgba, which would veto a
    # valid file for a NameError no runtime could raise.
    #
    # The resolution-order pattern generalizes to any language with
    # explicit imports + named functions (Go, Rust, JS/TS modules).
    # Adding a language adds implementation surface, not model-facing
    # API surface.
    if passing and smoke_language not in ("python", "py"):
        print(f"  [structural] veto skipped: {smoke_language} is not Python", flush=True)
    elif passing:
        passing = _keep_structurally_resolved(passing, files, emit)
    return passing


def _keep_structurally_resolved(passing, files, emit):
    """The structural veto's check of each passing candidate against its own
    imports and definitions, the builtins and the project's symbols."""
    # #147: gate on `passing` alone, not `passing and files`. The
    # edit path (improveContentWithV3) frequently sends no
    # project_context, so `files` was empty and the whole veto was
    # skipped — a NameError edit (render_template called with only
    # render_template_string imported) sailed through and landed as
    # verified. structural_score resolves against the candidate's
    # OWN imports/defs/builtins, so it catches an unresolved direct
    # call with empty project_symbols; project symbols only add
    # lenient cross-file crediting.
    project_symbols = symbols.build_project_symbols(files or {})
    kept = []
    for c in passing:
        struct = symbols.structural_score(project_symbols, c.get("code", ""))
        if struct.get("ok") and struct.get("n_unresolved", 0) >= 1:
            # Same contract as the lens veto: vetoed = failing.
            c["passed"] = False
            c["vetoed_by"] = "structural"
            c["stderr"] = (
                "structural veto: unresolved direct call(s) that "
                "would raise NameError at runtime: "
                + ", ".join(struct["unresolved_calls"][:5]))
            emit("structural_veto",
                 f"{_who(c)} sandbox-passed but "
                 f"{struct['n_unresolved']} unresolved call(s): "
                 f"{', '.join(struct['unresolved_calls'][:3])}",
                 index=c["index"],
                 n_unresolved=struct["n_unresolved"],
                 unresolved_calls=struct["unresolved_calls"][:5],
                 n_calls_total=struct["n_calls_total"])
            print(
                f"  [structural] vetoed cand {c['index']} — "
                f"{struct['n_unresolved']} unresolved: {struct['unresolved_calls'][:5]}",
                flush=True,
            )
            continue
        if struct.get("ok"):
            c["structural"] = struct  # stash for phase 3 / repair
        kept.append(c)
    if len(kept) < len(passing):
        print(
            f"  [structural] kept {len(kept)}/{len(passing)} candidates after structural veto"
            f"{' — falling through to phase-3 repair' if not kept else ''}",
            flush=True,
        )
    passing = kept
    return passing


def _call_graph_veto(passing, files, file_path, smoke_language, emit):
    """Issue #39 phase 1: for Python with project files, a candidate whose
    direct calls resolve to no in-scope definition in the import graph is
    vetoed, unless that would veto them all. Returns the candidates kept."""
    # ===== CALL-GRAPH VETO (issue #39, Phase 1) =====
    # Deepens the structural veto using the import graph: reject a candidate
    # whose direct calls don't resolve to a real, in-scope definition (local,
    # builtin, imported, or supplied by a resolved wildcard) — not merely
    # "some project file defines that name." Catches broken cross-file
    # references the shipped veto accepts. Conservative — stays lenient on
    # opaque wildcards and never empties the candidate set (a fully-failing
    # set falls through intact to repair).
    #
    # Python only, like the structural veto above, and for the same
    # reason: the resolver parses with the Python grammar, and an HTML
    # page's <script> calling setInterval read as an unresolved call.
    if passing and files and file_path and smoke_language not in ("python", "py"):
        print(f"  [call_graph] veto skipped: {smoke_language} is not Python", flush=True)
    elif passing and files and file_path:
        try:
            from graph import unresolved_calls
            _cg_on = True
        except Exception as cge:
            print(f"  [call_graph] veto unavailable: {cge}", flush=True)
            _cg_on = False
        if _cg_on:
            passing = _prune_unresolved_calls(passing, files, file_path, unresolved_calls, emit)
    return passing


def _prune_unresolved_calls(passing, files, file_path, unresolved_calls, emit):
    """The call-graph veto's resolution of each candidate; it marks and
    drops the unresolved ones only when at least one candidate survives."""
    cg_kept, cg_vetoed = [], []
    for c in passing:
        try:
            res = unresolved_calls(
                file_path, c.get("code", ""), files, strict=True)
        except Exception as cge:
            print(f"  [call_graph] veto skipped for cand {c.get('index')}: {cge}",
                  flush=True)
            cg_kept.append(c)
            continue
        if res.get("ok") and res.get("unresolved"):
            cg_vetoed.append((c, res["unresolved"]))
            continue
        cg_kept.append(c)
    if cg_kept:  # only prune when at least one candidate survives
        # Marking happens only when the prune actually applies —
        # the conservative all-vetoed case keeps the full set
        # (and its passed flags) intact.
        for c, unresolved in cg_vetoed:
            c["passed"] = False
            c["vetoed_by"] = "call_graph"
            c["stderr"] = (
                "call-graph veto: cross-file call(s) that resolve "
                "to no in-scope definition: "
                + ", ".join(unresolved[:5]))
            emit("call_graph_veto",
                 f"{_who(c)} has unresolved call(s): "
                 f"{', '.join(unresolved[:3])}",
                 index=c.get("index"), unresolved=unresolved[:5])
            print(f"  [call_graph] vetoed cand {c.get('index')} — "
                  f"unresolved: {unresolved[:5]}", flush=True)
        passing = cg_kept
    return passing
