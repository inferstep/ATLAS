#!/usr/bin/env python3
"""Create or update the public "ATLAS Roadmap" org project and its fields.

Idempotent: the project is found by title; a missing field is created, and a
single-select field whose options differ is updated in place (options
matched by name keep their ids, so existing items keep their values). Also
sets the project public, writes its README and links it to the repository.

Also creates or updates the eight views (name, layout, filter, visible
fields). Sorting, grouping, roadmap dates and the built-in workflows are
not in GitHub's API; they are set in the browser.

Needs `gh` logged in with the `project` scope (gh auth refresh -s project).

Usage: scripts/setup/project.py [--dry-run] [--owner ORG] [--repo NAME]
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys

TITLE = "ATLAS Roadmap"
SHORT = "Roadmap and work queue for ATLAS"

# (name, color, description). Colors: GRAY BLUE GREEN YELLOW ORANGE RED PINK PURPLE
SINGLE_SELECT = {
    "Status": [
        ("Triage", "GRAY", "New, not reviewed"),
        ("Needs Design", "PINK", "RFC open or decision pending"),
        ("Backlog", "BLUE", "Accepted, not specced"),
        ("Ready", "GREEN", "Acceptance criteria and a Shepherd. Claimable."),
        ("In Progress", "YELLOW", "Claimed"),
        ("In Review", "ORANGE", "PR open"),
        ("Blocked", "RED", "Waiting; the reason is in a comment"),
        ("Done", "PURPLE", "Merged or closed"),
    ],
    "Priority": [
        ("P0", "RED", "Drop everything"),
        ("P1", "ORANGE", "Next up"),
        ("P2", "YELLOW", "Planned"),
        ("P3", "GRAY", "Some day"),
    ],
    "Size": [
        ("XS", "GRAY", "Under an hour"),
        ("S", "BLUE", "A few hours"),
        ("M", "GREEN", "A day or two"),
        ("L", "YELLOW", "About a week"),
        ("XL", "ORANGE", "More than a week; split it"),
    ],
    "Critical Path": [
        ("Yes", "RED", "On the path to the next capability or reliability proof"),
        ("No", "GRAY", ""),
    ],
    "Contributor Level": [
        ("Starter", "GREEN", "Good first contribution"),
        ("Intermediate", "BLUE", "Needs some ATLAS context"),
        ("Advanced", "PURPLE", "Needs deep knowledge of a subsystem"),
        ("Maintainer-only", "RED", "Security, release or cross-cutting work"),
    ],
    "Hardware Needed": [
        ("None", "GRAY", "Runs without a GPU"),
        ("Any GPU", "BLUE", "Any supported accelerator"),
        ("NVIDIA", "GREEN", "CUDA"),
        ("AMD", "RED", "ROCm"),
        ("Apple Silicon", "PURPLE", "Metal"),
    ],
}
OTHER_FIELDS = {"Shepherd": "TEXT", "Start Date": "DATE", "Target Date": "DATE"}

# (name, layout, filter). The API sets name, layout, filter and visible
# fields; sorting, grouping and roadmap dates are set in the browser.
# Custom fields with spaces are filtered by their hyphenated name.
# Milestones have no @current keyword, so "Current Release" names the
# release in progress; change it when that release ships.
VIEWS = [
    ("Start Here", "TABLE_LAYOUT", "status:Ready contributor-level:Starter no:assignee -type:Epic"),
    ("Help Wanted", "TABLE_LAYOUT", 'status:Ready no:assignee -contributor-level:"Maintainer-only" -type:Epic'),
    ("Current Release", "BOARD_LAYOUT", 'milestone:"v3.2.0"'),
    ("Critical Path", "TABLE_LAYOUT", "critical-path:Yes -status:Done"),
    ("Roadmap", "ROADMAP_LAYOUT", "type:Epic"),
    ("RFCs", "TABLE_LAYOUT", "type:RFC is:open"),
    ("Triage", "TABLE_LAYOUT", "status:Triage"),
    ("Blocked", "TABLE_LAYOUT", "status:Blocked"),
    ("All work", "BOARD_LAYOUT", "-type:Epic"),
]
VISIBLE = ["Title", "Status", "Priority", "Size", "Contributor Level",
           "Hardware Needed", "Assignees", "Labels"]
DEFAULT_VIEW = "View 1"  # the view every new project starts with; becomes Start Here

README = """# ATLAS Roadmap

Everything planned for [ATLAS](https://github.com/{owner}/{repo}), in one place.

**New here?** Open the **Start Here** view: Ready issues for newcomers that nobody has claimed yet. Read [CONTRIBUTING](https://github.com/{owner}/{repo}/blob/main/CONTRIBUTING.md), then comment `/claim` on the issue you want.

**Status:** Triage → Needs Design (RFCs) → Backlog → Ready → In Progress → In Review → Done. Blocked means waiting, with the reason in a comment.

**Fields:** Priority (P0 highest), Size (XS–XL), Contributor Level (Starter to Maintainer-only), Hardware Needed (None means no GPU), Critical Path, and Shepherd: the maintainer who answers your questions on that issue.
"""


def gh(*args: str, input_json: dict | None = None) -> str:
    data = json.dumps(input_json) if input_json is not None else None
    proc = subprocess.run(["gh", *args], input=data, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.exit(f"error: gh {' '.join(args[:3])}…: {proc.stderr.strip()}")
    return proc.stdout


def graphql(query: str, **variables) -> dict:
    out = gh("api", "graphql", "--input", "-",
             input_json={"query": query, "variables": variables})
    data = json.loads(out)
    if data.get("errors"):
        sys.exit(f"error: {data['errors']}")
    return data["data"]


def find_project(owner: str) -> dict | None:
    data = graphql("""query($owner: String!) { organization(login: $owner) {
        projectsV2(first: 100, query: "") { nodes { id number title public
            shortDescription readme repositories(first: 20) { nodes { nameWithOwner } } } } } }""",
                   owner=owner)
    for p in data["organization"]["projectsV2"]["nodes"]:
        if p["title"] == TITLE:
            return p
    return None


def fields(project_id: str) -> dict:
    data = graphql("""query($id: ID!) { node(id: $id) { ... on ProjectV2 {
        fields(first: 50) { nodes {
          ... on ProjectV2FieldCommon { id name dataType }
          ... on ProjectV2SingleSelectField { options { id name color description } } } } } } }""",
                   id=project_id)
    return {f["name"]: f for f in data["node"]["fields"]["nodes"]}


def option_input(spec, existing) -> list:
    by_name = {o["name"]: o for o in existing or []}
    out = []
    for name, color, desc in spec:
        opt = {"name": name, "color": color, "description": desc}
        if name in by_name:
            opt["id"] = by_name[name]["id"]  # keep the id so items keep their value
        out.append(opt)
    return out


def options_match(spec, existing) -> bool:
    have = [(o["name"], o["color"], o["description"]) for o in existing or []]
    return have == [tuple(s) for s in spec]


def main() -> int:  # noqa: C901
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--owner", default="inferstep")
    ap.add_argument("--repo", default="ATLAS")
    args = ap.parse_args()
    dry = args.dry_run
    if dry:
        print("(dry run: nothing is changed)")

    project = find_project(args.owner)
    if project is None:
        print(f"create  project '{TITLE}' in {args.owner}")
        if dry:
            print("  (fields are shown once the project exists)")
            return 0
        created = json.loads(gh("project", "create", "--owner", args.owner,
                                "--title", TITLE, "--format", "json"))
        project = find_project(args.owner)
        assert project and project["number"] == created["number"]
    else:
        print(f"keep    project '{TITLE}' (#{project['number']})")
    pid, num = project["id"], str(project["number"])

    readme = README.format(owner=args.owner, repo=args.repo)
    if not project["public"] or project["readme"] != readme or project["shortDescription"] != SHORT:
        print("edit    visibility public, README, short description")
        if not dry:
            graphql("""mutation($id: ID!, $readme: String!, $short: String!) {
                updateProjectV2(input: {projectId: $id, public: true, readme: $readme,
                  shortDescription: $short}) { projectV2 { id } } }""",
                    id=pid, readme=readme, short=SHORT)
    else:
        print("keep    visibility, README, short description")

    repo_full = f"{args.owner}/{args.repo}"
    linked = [r["nameWithOwner"] for r in project["repositories"]["nodes"]]
    if repo_full in linked:
        print(f"keep    link to {repo_full}")
    else:
        print(f"link    {repo_full}")
        if not dry:
            gh("project", "link", num, "--owner", args.owner, "--repo", repo_full)

    have = fields(pid)
    for name, spec in SINGLE_SELECT.items():
        f = have.get(name)
        if f is None:
            print(f"create  field {name} (single select: {', '.join(s[0] for s in spec)})")
            if not dry:
                graphql("""mutation($id: ID!, $name: String!, $opts: [ProjectV2SingleSelectFieldOptionInput!]!) {
                    createProjectV2Field(input: {projectId: $id, dataType: SINGLE_SELECT, name: $name,
                      singleSelectOptions: $opts}) { projectV2Field { ... on ProjectV2FieldCommon { id } } } }""",
                        id=pid, name=name, opts=option_input(spec, None))
        elif options_match(spec, f.get("options")):
            print(f"keep    field {name}")
        else:
            print(f"update  field {name} options → {', '.join(s[0] for s in spec)}")
            if not dry:
                graphql("""mutation($id: ID!, $opts: [ProjectV2SingleSelectFieldOptionInput!]!) {
                    updateProjectV2Field(input: {fieldId: $id, singleSelectOptions: $opts}) {
                      projectV2Field { ... on ProjectV2FieldCommon { id } } } }""",
                        id=f["id"], opts=option_input(spec, f.get("options")))
    for name, dtype in OTHER_FIELDS.items():
        f = have.get(name)
        if f is None:
            print(f"create  field {name} ({dtype.lower()})")
            if not dry:
                gh("project", "field-create", num, "--owner", args.owner,
                   "--name", name, "--data-type", dtype)
        elif f["dataType"] != dtype:
            print(f"WARNING field {name} exists as {f['dataType']}, expected {dtype}; left alone")
        else:
            print(f"keep    field {name}")

    sync_views(pid, dry)
    print(f"done. https://github.com/orgs/{args.owner}/projects/{num}")
    return 0


def views(project_id: str) -> list:
    data = graphql("""query($id: ID!) { node(id: $id) { ... on ProjectV2 {
        views(first: 50) { nodes { id name layout filter fields(first: 50) { nodes {
          ... on ProjectV2FieldCommon { id name } } } } } } } }""", id=project_id)
    return data["node"]["views"]["nodes"]


def sync_views(project_id: str, dry: bool) -> None:
    field_ids = {name: f["id"] for name, f in fields(project_id).items()}
    visible = [field_ids[n] for n in VISIBLE if n in field_ids]
    have = {v["name"]: v for v in views(project_id)}
    if VIEWS[0][0] not in have and DEFAULT_VIEW in have:
        have[VIEWS[0][0]] = have.pop(DEFAULT_VIEW)  # rename rather than add
    for name, layout, flt in VIEWS:
        v = have.get(name)
        # Roadmap views take no column list; GitHub returns columns in its
        # own order, so they are compared as a set.
        cols = [] if layout == "ROADMAP_LAYOUT" else visible
        if v is None:
            print(f"create  view {name} ({layout.split('_')[0].lower()}: {flt})")
            if not dry:
                made = graphql("""mutation($id: ID!, $name: String!, $layout: ProjectV2ViewLayout!) {
                    createProjectV2View(input: {projectId: $id, name: $name, layout: $layout}) {
                    projectV2View { id } } }""", id=project_id, name=name, layout=layout)
                update_view(made["createProjectV2View"]["projectV2View"]["id"], name, layout, flt, cols)
            continue
        shown = {f["id"] for f in v["fields"]["nodes"] if f.get("id")}
        if (v["layout"], v["filter"] or "") == (layout, flt) and v["name"] == name \
                and (not cols or shown == set(cols)):
            print(f"keep    view {name}")
            continue
        print(f"update  view {name} ({layout.split('_')[0].lower()}: {flt})")
        if not dry:
            update_view(v["id"], name, layout, flt, cols)


def update_view(view_id: str, name: str, layout: str, flt: str, cols: list) -> None:
    config = ", configuration: {visibleFieldIds: $visible}" if cols else ""
    graphql(f"""mutation($id: ID!, $name: String!, $layout: ProjectV2ViewLayout!,
        $filter: String!{', $visible: [ID!]' if cols else ''}) {{ updateProjectV2View(input: {{
        viewId: $id, name: $name, layout: $layout, filter: $filter{config}}}) {{
        projectV2View {{ id }} }} }}""",
            id=view_id, name=name, layout=layout, filter=flt,
            **({"visible": cols} if cols else {}))


if __name__ == "__main__":
    sys.exit(main())
