#!/usr/bin/env bash
# Deployment environments for the dev → staging → main flow.
#
#   dev         deployable from branch dev. No approval.
#   staging     deployable from branch staging and release-candidate tags
#               (v*-*). No approval.
#   production  deployable from branch main and release tags (v*). Waits for
#               the release owner's approval.
#   bots        usable from branch main only: holds the atlas-bot app's
#               client ID (a variable, set here) and private key (a secret,
#               pasted in the UI, never by this script). Bot workflows run
#               from main, so a workflow pushed to any other branch can't
#               read the key.
#
# build-images.yml's promote job and staging-promotion.yml deploy into
# these, so every promotion leaves a timestamped deployment record, and
# the images for a release are published only after approval.
#
# Idempotent: the environments are created or updated in place, and a
# missing branch or tag policy is added. Policies not listed here are
# reported but left alone. Runs on macOS's bash 3.2. Needs `gh` logged in
# as a repo admin.
#
# Usage: scripts/setup/environments.sh [--dry-run] [--repo OWNER/NAME] [--approver LOGIN]
set -euo pipefail
set -f  # no globbing: the tag patterns (v*, v*-*) are data, not file names

REPO="inferstep/ATLAS"
APPROVER="itigges22"
DRY_RUN=0

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        --repo) REPO="${2:?--repo needs OWNER/NAME}"; shift ;;
        --approver) APPROVER="${2:?--approver needs a GitHub login}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done

command -v gh >/dev/null || { echo "error: gh (GitHub CLI) is not installed" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh is not logged in (run: gh auth login)" >&2; exit 1; }
[[ "$DRY_RUN" == 1 ]] && echo "(dry run: nothing is changed)"
echo "repo: $REPO"

APPROVER_ID=$(gh api "users/$APPROVER" --jq .id)

# environment|reviewer json|policy list (type:name, space separated)
ENVS=(
    "dev|[]|branch:dev"
    "staging|[]|branch:staging tag:v*-*"
    "production|[{\"type\":\"User\",\"id\":$APPROVER_ID}]|branch:main tag:v*"
    "bots|[]|branch:main"
)
BOT_APP="inferstep-atlas-bot"

for spec in "${ENVS[@]}"; do
    IFS='|' read -r env reviewers policies <<<"$spec"
    exists=0
    gh api "repos/$REPO/environments/$env" >/dev/null 2>&1 && exists=1
    if [[ "$reviewers" == "[]" ]]; then who="no approval"; else who="approval by $APPROVER"; fi
    if [[ $exists == 1 ]]; then echo "update  $env ($who)"; else echo "create  $env ($who)"; fi
    if [[ "$DRY_RUN" != 1 ]]; then
        # prevent_self_review stays false: the approver is also the one
        # who pushes the release, and must be able to approve it.
        gh api -X PUT "repos/$REPO/environments/$env" --input - --silent <<EOF
{"wait_timer":0,"prevent_self_review":false,"reviewers":$reviewers,
 "deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
EOF
    fi
    have=""
    if [[ $exists == 1 || "$DRY_RUN" != 1 ]]; then
        have=$(gh api "repos/$REPO/environments/$env/deployment-branch-policies" \
            --jq '.branch_policies[] | "\(.type // "branch"):\(.name)"')
    fi
    for p in $policies; do
        if grep -qxF "$p" <<<"$have"; then
            echo "  keep    $p"
        else
            echo "  add     $p"
            [[ "$DRY_RUN" == 1 ]] || gh api -X POST "repos/$REPO/environments/$env/deployment-branch-policies" \
                -f name="${p#*:}" -f type="${p%%:*}" --silent
        fi
    done
    for p in $have; do
        case " $policies " in *" $p "*) ;; *) echo "  note    $p is not in this script; left alone" ;; esac
    done
done
# The bot's client ID is public (it's in the app's page); only the private
# key is secret, and that one is pasted in the UI.
client_id=$(gh api "apps/$BOT_APP" --jq .client_id)
# On a 404, gh prints the error body to stdout, so test the exit status,
# not the output.
if ! current=$(gh api "repos/$REPO/environments/bots/variables/ATLAS_BOT_CLIENT_ID" --jq .value 2>/dev/null); then
    current=""
fi
if [[ "$current" == "$client_id" ]]; then
    echo "keep    bots variable ATLAS_BOT_CLIENT_ID"
else
    echo "set     bots variable ATLAS_BOT_CLIENT_ID = $client_id"
    if [[ "$DRY_RUN" != 1 ]]; then
        if [[ -n "$current" ]]; then
            gh api -X PATCH "repos/$REPO/environments/bots/variables/ATLAS_BOT_CLIENT_ID" -f value="$client_id" --silent
        else
            gh api -X POST "repos/$REPO/environments/bots/variables" -f name=ATLAS_BOT_CLIENT_ID -f value="$client_id" --silent
        fi
    fi
fi
if gh api "repos/$REPO/environments/bots/secrets/ATLAS_BOT_PRIVATE_KEY" >/dev/null 2>&1; then
    echo "keep    bots secret ATLAS_BOT_PRIVATE_KEY (present)"
else
    echo "todo    bots secret ATLAS_BOT_PRIVATE_KEY: paste it in Settings → Environments → bots"
fi
[[ "$DRY_RUN" == 1 ]] && echo "dry run done." || echo "done."
