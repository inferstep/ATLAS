#!/usr/bin/env python3
"""Compare a ruleset this repository wants with the one GitHub has.

  ruleset_diff.py WANTED.json LIVE.json

Prints one line per difference and exits 1. Prints nothing and exits 0 when
the live ruleset is what WANTED asks for. `rulesets.sh` uses it to leave a
matching ruleset alone and to show what an update would change.

GitHub returns more than it is sent: ids, links, and a default for every
parameter that was left out. A live parameter that WANTED does not set is a
difference only when it holds a value, because an empty or false default is
not a setting.
"""

from __future__ import annotations

import json
import sys

# GitHub sets this one itself and its API reference does not list it, so
# rulesets.sh does not write it.
SERVER_SET = {"require_extra_approval_for_unattributed_changes"}


def canonical(value):
    """The value with every list sorted, so that order never counts."""
    if isinstance(value, dict):
        return {k: canonical(v) for k, v in value.items()}
    if isinstance(value, list):
        return sorted((canonical(v) for v in value), key=lambda v: json.dumps(v, sort_keys=True))
    return value


def holds_a_value(value) -> bool:
    if isinstance(value, dict):
        return any(holds_a_value(v) for v in value.values())
    return bool(value)


def actors(ruleset: dict) -> list:
    """Bypass actors as comparable rows. Organization admins have no id of their own."""
    return sorted(
        f"{a['actor_type']}:{'' if a['actor_type'] == 'OrganizationAdmin' else a.get('actor_id')}:{a['bypass_mode']}"
        for a in ruleset.get("bypass_actors") or []
    )


def parameter_differences(kind: str, wanted: dict, live: dict) -> list:
    out = []
    for key, value in wanted.items():
        if canonical(value) != canonical(live.get(key)):
            out.append(f"{kind}.{key}: wants {json.dumps(value)}, live has {json.dumps(live.get(key))}")
    for key, value in live.items():
        if key not in wanted and key not in SERVER_SET and holds_a_value(value):
            out.append(f"{kind}.{key}: live has {json.dumps(value)}, which the wanted ruleset does not set")
    return out


def scope_differences(wanted: dict, live: dict) -> list:
    """Differences in what the ruleset is, where it applies, and who can bypass it."""
    out = []
    for key in ("name", "target", "enforcement"):
        if wanted.get(key) != live.get(key):
            out.append(f"{key}: wants {wanted.get(key)!r}, live has {live.get(key)!r}")
    for side in ("include", "exclude"):
        want = sorted(wanted["conditions"]["ref_name"].get(side) or [])
        have = sorted(live["conditions"]["ref_name"].get(side) or [])
        if want != have:
            out.append(f"branches or tags, {side}: wants {want}, live has {have}")
    if actors(wanted) != actors(live):
        out.append(f"bypass: wants {actors(wanted) or 'nobody'}, live has {actors(live) or 'nobody'}")
    return out


def rule_differences(wanted: dict, live: dict) -> list:
    out = []
    want_rules = {r["type"]: r.get("parameters") or {} for r in wanted["rules"]}
    live_rules = {r["type"]: r.get("parameters") or {} for r in live["rules"]}
    for kind in sorted(set(want_rules) - set(live_rules)):
        out.append(f"rule {kind}: wanted, not live")
    for kind in sorted(set(live_rules) - set(want_rules)):
        out.append(f"rule {kind}: live, not wanted")
    for kind in sorted(set(want_rules) & set(live_rules)):
        out.extend(parameter_differences(kind, want_rules[kind], live_rules[kind]))
    return out


def differences(wanted: dict, live: dict) -> list:
    return scope_differences(wanted, live) + rule_differences(wanted, live)


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    try:
        with open(sys.argv[1]) as f:
            wanted = json.load(f)
        with open(sys.argv[2]) as f:
            live = json.load(f)
        found = differences(wanted, live)
    except (OSError, ValueError, KeyError, TypeError) as e:
        print(f"error: cannot compare {sys.argv[1]} with {sys.argv[2]}: {type(e).__name__}: {e}", file=sys.stderr)
        return 2
    for line in found:
        print(line)
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main())
