#!/usr/bin/env python3
"""Run the freshly authored task set through the full ATLAS agent.

The other half of the comparison scripts/novel-baseline.py starts: identical
statements, identical reference answers, identical holdout check — but this
arm gets the whole harness (agent loop, tools, gates, V3 pipeline), driven
through /v1/agent exactly the way a user session is.

The task registry and workspace plumbing are reused from the e2e suite so a
session here behaves byte-for-byte like a reliability-run session; only the
task source differs.
"""

import argparse
import hashlib
import importlib.util
import json
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "scripts"))

from novel_tasks import build_tasks  # noqa: E402


# Scoring is EXACT MATCH by decision. The output-separator ambiguity that
# once made "42, 242" a defensible reading is fixed at the source: every
# prompt now states "fields separated by single spaces". A benchmark pass
# must be the answer a user would actually see, byte for byte.


def _sha256(data) -> str:
    if isinstance(data, str):
        data = data.encode()
    return hashlib.sha256(data).hexdigest()


def _file_hash(path: Path) -> str:
    try:
        return _sha256(path.read_bytes())
    except OSError:
        return ""


class TelemetrySubscriber:
    """Reads the proxy's /events stream for the duration of a session.

    The v3 stage_end envelope is where authorization lives: the tool-result SSE
    the agent stream carries is guarded and deliberately does not expose it.
    Envelopes are appended verbatim to a JSONL, and the derived per-task
    summary below references them by position in that file rather than
    restating them.
    """

    def __init__(self, url: str, sink: Path):
        self.url = url.rstrip("/") + "/events"
        self.sink = sink
        self.lines = []
        self._stop = threading.Event()
        self._thread = None

    def start(self):
        self._thread = threading.Thread(target=self._run, daemon=True)
        self._thread.start()

    def _run(self):
        # The read timeout must clear the stream's heartbeat interval, and a
        # timeout must reconnect rather than end the subscription: the first
        # version used a 5s per-read timeout against a 15s heartbeat, so it
        # died a few seconds into every session and captured only the opening
        # envelopes. A dead subscriber looks exactly like a pipeline that never
        # ran, which is the reading it produced.
        while not self._stop.is_set():
            try:
                with urllib.request.urlopen(self.url, timeout=60) as resp:
                    for raw in resp:
                        if self._stop.is_set():
                            return
                        line = raw.decode("utf-8", "replace").strip()
                        if not line.startswith("data:"):
                            continue
                        payload = line[5:].strip()
                        if payload:
                            self.lines.append(payload)
            except Exception as exc:                   # noqa: BLE001
                if self._stop.is_set():
                    return
                self.lines.append(json.dumps(
                    {"__subscriber_reconnect__": str(exc)[:200]}))
                time.sleep(0.5)

    def stop(self):
        self._stop.set()
        if self.sink is not None:
            with open(self.sink, "a") as fh:
                for line in self.lines:
                    fh.write(line + "\n")
        return list(self.lines)


def summarize_telemetry(lines, offset: int) -> dict:
    """Derive the authorization metrics from the raw envelopes.

    `offset` is where these lines start in the persisted JSONL, so every
    derived field can be traced back to the exact source position. The raw
    file is never rewritten.
    """
    out = {
        "v3_stage_end_events": 0,
        "selection_status": [],
        "closure_eligible": [],
        "authorized": [],
        "authorization_reasons": [],
        "evidence_availability": [],
        "baseline_fallbacks": 0,
        "sanitization_revocations": 0,
        # Named for what it measures: the service called it a verified winner
        # and Go still refused delivery. The stricter question -- contract
        # verified while the LOCAL syntax/structural gate failed the same
        # bytes -- lives on the tool result, which the guarded SSE
        # deliberately does not carry, so it is not inferred here.
        "verified_but_unauthorized": 0,
        "source_positions": [],
    }
    for i, line in enumerate(lines):
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        if ev.get("type") != "stage_end" or ev.get("stage") != "v3":
            continue
        payload = ev.get("payload") or {}
        evidence = payload.get("evidence") or {}
        auth = payload.get("authorization") or {}
        out["v3_stage_end_events"] += 1
        out["source_positions"].append(offset + i)
        availability = evidence.get("availability", "")
        if availability:
            out["evidence_availability"].append(availability)
        env = evidence.get("envelope") or {}
        selection = (env.get("selection") or {}).get("status", "")
        if selection:
            out["selection_status"].append(selection)
        evaluation = env.get("evaluation") or {}
        if "closure_eligible" in evaluation:
            out["closure_eligible"].append(bool(evaluation["closure_eligible"]))
        if auth:
            out["authorized"].append(bool(auth.get("authorized")))
            reason = auth.get("reason", "")
            if reason:
                out["authorization_reasons"].append(reason)
                if "different candidate" in reason:
                    out["sanitization_revocations"] += 1
            if not auth.get("authorized"):
                out["baseline_fallbacks"] += 1
            if (selection == "verified_winner" and evaluation.get("closure_eligible")
                    and not auth.get("authorized")):
                out["verified_but_unauthorized"] += 1
    return out


def summarize_events(events) -> dict:
    """Model calls, tool calls and failures, from the agent stream."""
    tool_calls = failed = model_calls = 0
    for ev in events:
        t = ev.get("type", "")
        data = ev.get("data") or {}
        if t in ("tool_call", "tool_start"):
            tool_calls += 1
        if t in ("tool_result", "tool_end"):
            ok = data.get("success")
            if ok is False:
                failed += 1
        if t in ("llm_start", "assistant_start", "turn_start"):
            model_calls += 1
    return {"tool_calls": tool_calls, "failed_tool_calls": failed,
            "model_calls": model_calls}


def _load_e2e():
    spec = importlib.util.spec_from_file_location(
        "e2e_suite", REPO / "scripts" / "e2e-reliability.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules["e2e_suite"] = mod
    spec.loader.exec_module(mod)
    return mod


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--count", type=int, default=50)
    ap.add_argument("--seed", type=int, default=20260806)
    ap.add_argument("--url", default="")
    ap.add_argument("--workspace", required=True,
                    help="host path the proxy has mounted (e.g. ~/demo2/e2e)")
    ap.add_argument("--subdir", default="e2e")
    ap.add_argument("--timeout", type=int, default=900)
    ap.add_argument("--start", type=int, default=0,
                    help="skip the first N tasks (resume support)")
    ap.add_argument("--json", dest="json_out", default="")
    ap.add_argument("--save-events", default="",
                    help="directory for per-session event dumps")
    ap.add_argument("--seed-manifest", default="",
                    help="write the ordered frozen task manifest here and exit")
    ap.add_argument("--artifacts", default="",
                    help="directory the delivered artifact of each task is "
                         "copied into (it is otherwise wiped by the next task)")
    args = ap.parse_args()

    import os
    e2e = _load_e2e()
    url = args.url or os.environ.get("ATLAS_PROXY_URL", "http://localhost:8090")

    tasks = build_tasks(args.count, args.seed)[args.start:]
    workspace = Path(args.workspace).expanduser()

    # Provenance: what was measured, by what, against which system.
    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(REPO),
                          capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain"], cwd=str(REPO),
                           capture_output=True, text=True).stdout.strip()
    provenance = {
        "head": head,
        "tree_dirty": bool(dirty),
        "seed": args.seed,
        "count": args.count,
        "url": url,
        "runner_sha256": _file_hash(Path(__file__)),
        "tasks_sha256": _file_hash(REPO / "scripts" / "novel_tasks.py"),
        "e2e_sha256": _file_hash(REPO / "scripts" / "e2e-reliability.py"),
        # What the proxy says it runs: grammar mode, lens and steering state.
        "stack": e2e.stack_identity(url),
        "images": {},
    }
    for name in ("atlas-atlas-proxy-1", "atlas-v3-service-1", "atlas-geometric-lens-1",
                 "atlas-sandbox-1", "atlas-llama-server-1"):
        ident = subprocess.run(
            ["docker", "inspect", "--format", "{{.Image}} {{.State.StartedAt}}", name],
            capture_output=True, text=True).stdout.strip()
        provenance["images"][name] = ident

    evdir = Path(args.save_events) if args.save_events else None
    if evdir:
        evdir.mkdir(parents=True, exist_ok=True)
    artdir = Path(args.artifacts) if args.artifacts else None
    if artdir:
        artdir.mkdir(parents=True, exist_ok=True)

    results = []
    for i, nt in enumerate(tasks):
        def make_check(nt):
            def check(ws: Path, s=None):
                prog = ws / "solve.py"
                if not prog.exists():
                    return False, "solve.py was never created"
                import subprocess
                def run(data: str):
                    (ws / "input.txt").write_text(data)
                    try:
                        p = subprocess.run([sys.executable, "solve.py"],
                                           cwd=str(ws), capture_output=True,
                                           text=True, timeout=60)
                    except subprocess.TimeoutExpired:
                        return False, "timeout"
                    if p.returncode != 0:
                        return False, f"failed: {p.stderr.strip()[:120]}"
                    return True, p.stdout.strip()
                ok, got = run(nt.input_text)
                if not ok:
                    return False, got
                if got != nt.expected:
                    return False, f"got {got!r}, want {nt.expected!r}"
                ok2, got2 = run(nt.holdout_text)
                (ws / "input.txt").write_text(nt.input_text)
                if not ok2 or got2 != nt.holdout_expected:
                    return False, (f"holdout mismatch: got {got2!r}, want "
                                   f"{nt.holdout_expected!r}")
                return True, f"{got} correct, and correct on the holdout"
            return check

        task = e2e.Task(
            name=nt.name,
            prompt=nt.prompt,
            files={"input.txt": nt.input_text},
            check=make_check(nt),
            must_exist=("input.txt",),
            immutable=(),  # solve.py may legitimately rewrite nothing else
        )
        print(f"[{args.start + i + 1}/{args.start + len(tasks)}] {nt.name} ...",
              flush=True)
        t0 = time.time()
        telemetry_sink = (evdir / f"{nt.name}.telemetry.jsonl") if evdir else None
        subscriber = TelemetrySubscriber(url, telemetry_sink)
        subscriber.start()
        raw_path = (evdir / f"{nt.name}.raw.sse") if evdir else None
        raw_fh = open(raw_path, "w") if raw_path else None
        try:
            session = e2e.run_session(task, 1, url, workspace, args.subdir,
                                      args.timeout, raw_sink=raw_fh)
        finally:
            if raw_fh:
                raw_fh.close()
        telemetry_lines = subscriber.stop()
        if evdir:
            with open(evdir / f"{nt.name}.jsonl", "w") as fh:
                for ev in getattr(session, "events", []):
                    fh.write(json.dumps(ev) + "\n")

        # The delivered artifact, kept and hashed before the next task wipes
        # the workspace. This is what is scored -- never an internal candidate.
        delivered = workspace / "solve.py"
        delivered_hash = _file_hash(delivered)
        if artdir and delivered.exists():
            (artdir / f"{nt.name}.solve.py").write_bytes(delivered.read_bytes())

        ok, detail = task.check(workspace)
        row = {
            "task": nt.name, "family": nt.family, "passed": ok,
            "detail": detail, "turns": getattr(session, "turns", 0),
            "wall_s": round(time.time() - t0, 1),
            "delivered_sha256": delivered_hash,
            "delivered_bytes": delivered.stat().st_size if delivered.exists() else 0,
            "stream_ok": getattr(session, "stream_ok", None),
            "quality": getattr(session, "quality", {}),
            "defects": getattr(session, "defects", []),
            "raw_sse_sha256": _file_hash(raw_path) if raw_path else "",
        }
        row.update(summarize_events(getattr(session, "events", [])))
        # What V3 did in this session: generations and delivered candidates.
        row["v3"] = session.v3 if hasattr(session, "v3") else {}
        row["telemetry"] = summarize_telemetry(telemetry_lines, 0)
        row["telemetry"]["source_file"] = (
            str(telemetry_sink) if telemetry_sink else "")
        results.append(row)
        print(f"      {'PASS' if ok else 'fail'} turns="
              f"{getattr(session, 'turns', '?')} "
              f"{results[-1]['wall_s']}s — {detail[:80]}", flush=True)
        if args.json_out:
            Path(args.json_out).write_text(json.dumps(
                {"provenance": provenance, "results": results}, indent=1))

    p = sum(1 for r in results if r["passed"])
    print(f"\nATLAS TOTAL {p}/{len(results)} ({100.0 * p / len(results):.0f}%)")
    writes = sum((r.get("v3") or {}).get("write_calls", 0) for r in results)
    generated = sum((r.get("v3") or {}).get("generated", 0) for r in results)
    delivered = sum((r.get("v3") or {}).get("delivered", 0) for r in results)
    print(f"V3 generation ran on {generated}/{writes} write calls, "
          f"delivered {delivered} candidate(s)")
    if writes and not generated:
        print("  ! no write reached V3 generation: this arm measured the agent "
              "loop without V3")
    print(f"stack: {provenance['stack']}")
    byfam = {}
    for r in results:
        got, tot = byfam.get(r["family"], (0, 0))
        byfam[r["family"]] = (got + (1 if r["passed"] else 0), tot + 1)
    for fam in sorted(byfam):
        got, tot = byfam[fam]
        print(f"   {fam:10} {got}/{tot}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
