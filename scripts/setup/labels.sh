#!/usr/bin/env bash
# Create or update the ATLAS issue label set.
#
# Idempotent: a label that already matches is left alone, one with a
# different color or description is edited, and a missing one is created.
# Labels outside this set are never touched. Runs on macOS's bash 3.2.
# Needs `gh` logged in with write access to the repo.
#
# Usage:
#   scripts/setup/labels.sh [--dry-run] [--repo OWNER/NAME]
#   scripts/setup/labels.sh --migrate-types [--dry-run] [--repo OWNER/NAME]
#
# --migrate-types moves issues labeled `bug` to the Bug issue type and
# `enhancement` to the Feature type, removes the label from each, then
# deletes both labels. Deleting a label also removes it from pull requests;
# the dry run counts them first.
set -euo pipefail

REPO="inferstep/ATLAS"
DRY_RUN=0
MIGRATE=0

usage() { sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        --migrate-types) MIGRATE=1 ;;
        --repo) REPO="${2:?--repo needs OWNER/NAME}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

command -v gh >/dev/null || { echo "error: gh (GitHub CLI) is not installed" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh is not logged in (run: gh auth login)" >&2; exit 1; }

# name|color|description
LABELS=(
    "area/proxy|1d76db|Go agent loop and SSE contract (proxy/)"
    "area/tui|1d76db|Bubbletea terminal UI (tui/)"
    "area/cli|1d76db|Python atlas CLI (atlas/)"
    "area/v3|1d76db|V3 candidate generation and selection (v3-service/)"
    "area/lens|1d76db|Geometric Lens scoring (geometric-lens/)"
    "area/inference|1d76db|llama-server images and patches (inference/)"
    "area/sandbox|1d76db|Sandboxed execution (sandbox/)"
    "area/extensions|1d76db|Editor extensions (extensions/)"
    "area/install|1d76db|Bootstrap, compose files and install scripts"
    "area/ci|1d76db|GitHub Actions workflows and release tooling"
    "area/docs|1d76db|Documentation"
    "platform/cuda|5319e7|NVIDIA CUDA backend"
    "platform/rocm|5319e7|AMD ROCm backend"
    "platform/metal|5319e7|Apple Silicon Metal backend"
    "platform/vulkan|5319e7|Vulkan backend"
    "platform/cpu|5319e7|CPU-only backend"
    "needs-triage|fbca04|New, not yet reviewed by a maintainer"
    "needs-info|d4c5f9|Waiting on more information from the reporter"
    "duplicate|cfd3d7|This issue or pull request already exists"
    "wontfix|ffffff|This will not be worked on"
    "status/ready|0e8a16|Ready to claim (mirrors the project Status; set by the bot)"
    "status/blocked|b60205|Blocked (mirrors the project Status; set by the bot)"
    "good first issue|7057ff|Good for newcomers"
    "help wanted|008672|Extra attention is needed"
    "release-blocker|b60205|Must be fixed before the next release"
    "hotfix|d93f0b|Fix for a released version, cut from main"
)

run() {
    if [[ "$DRY_RUN" == 1 ]]; then
        printf '  would run:'; printf ' %q' "$@"; printf '\n'
    else
        "$@"
    fi
}

ensure_labels() {
    local existing spec name color desc line cur_color cur_desc
    local created=0 edited=0 kept=0
    existing=$(gh label list -R "$REPO" --limit 500 \
        --json name,color,description --jq '.[] | [.name, .color, .description] | @tsv')
    for spec in "${LABELS[@]}"; do
        IFS='|' read -r name color desc <<<"$spec"
        line=$(awk -F'\t' -v n="$name" '$1 == n {print; exit}' <<<"$existing")
        if [[ -z "$line" ]]; then
            echo "create  $name"
            run gh label create "$name" -R "$REPO" --color "$color" --description "$desc"
            created=$((created + 1))
            continue
        fi
        cur_color=$(printf '%s' "$line" | cut -f2 | tr '[:upper:]' '[:lower:]')
        cur_desc=$(printf '%s' "$line" | cut -f3)
        if [[ "$cur_color" == "$color" && "$cur_desc" == "$desc" ]]; then
            kept=$((kept + 1))
            continue
        fi
        echo "edit    $name"
        run gh label edit "$name" -R "$REPO" --color "$color" --description "$desc"
        edited=$((edited + 1))
    done
    echo "labels: $created created, $edited edited, $kept already correct"
}

migrate_types() {
    local label type n numbers prs names
    # Read the whole list first: `gh … | grep -q` under pipefail fails
    # when grep exits early and gh gets SIGPIPE, which reads as "missing".
    names=$(gh label list -R "$REPO" --limit 500 --json name --jq '.[].name')
    for pair in "bug|Bug" "enhancement|Feature"; do
        IFS='|' read -r label type <<<"$pair"
        if ! grep -qxF "$label" <<<"$names"; then
            echo "label '$label' does not exist; nothing to migrate"
            continue
        fi
        numbers=$(gh issue list -R "$REPO" --label "$label" --state all --limit 1000 \
            --json number --jq '.[].number')
        prs=$(gh pr list -R "$REPO" --label "$label" --state all --limit 1000 \
            --json number --jq 'length')
        echo "'$label' → type $type: $(printf '%s' "$numbers" | grep -c . || true) issues; the label is also on $prs pull requests"
        if [[ "$DRY_RUN" == 1 ]]; then
            # shellcheck disable=SC2086  # $numbers is a newline list of integers
            echo "  would set type $type and remove '$label' on: $(printf '#%s ' $numbers)"
            echo "  would delete label '$label'"
            continue
        fi
        for n in $numbers; do
            gh api -X PATCH "repos/$REPO/issues/$n" -f type="$type" --silent
            gh issue edit "$n" -R "$REPO" --remove-label "$label" >/dev/null
            echo "  #$n → $type"
        done
        gh label delete "$label" -R "$REPO" --yes
    done
}

[[ "$DRY_RUN" == 1 ]] && echo "(dry run: nothing is changed)"
echo "repo: $REPO"
if [[ "$MIGRATE" == 1 ]]; then
    migrate_types
else
    ensure_labels
fi
