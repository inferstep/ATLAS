#!/usr/bin/env bash
# Branch and tag rules for ATLAS (GOVERNANCE: dev → staging → main).
#
# Applies, in this order, stopping at the first failure:
#   1. team access: maintainers = maintain, reviewers = write, triagers = triage
#   2. merge settings: squash or rebase only (linear history), delete merged
#      branches, offer "update branch"
#   3. five rulesets, created or updated by name:
#      - Release branches: no force-push or deletion   (dev, staging, main; no bypass)
#      - Release branches: checks, history and review  (dev, staging, main; admins bypass)
#      - Branches: only maintainers create or push     (all branches except
#        star-history; admins, maintainers and Dependabot bypass)
#      - Release tags: only maintainers create          (v*)
#      - Release tags: never move or delete             (v*; no bypass)
#   4. removes what they replace: the "Protect all branches" ruleset and the
#      classic branch protection on main and dev, only after step 3 reads
#      back as active
#
# Idempotent: re-running updates the rulesets in place. Runs on macOS's
# bash 3.2. Needs `gh` logged in as a repo admin; team access also needs
# the org's teams to be visible to the token.
#
# Usage: scripts/setup/rulesets.sh [--dry-run] [--repo OWNER/NAME]
set -euo pipefail

REPO="inferstep/ATLAS"
DRY_RUN=0

usage() { sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run) DRY_RUN=1 ;;
        --repo) REPO="${2:?--repo needs OWNER/NAME}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
    esac
    shift
done
ORG="${REPO%%/*}"

command -v gh >/dev/null || { echo "error: gh (GitHub CLI) is not installed" >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo "error: gh is not logged in (run: gh auth login)" >&2; exit 1; }

OUT=$(mktemp -d "${TMPDIR:-/tmp}/atlas-rulesets.XXXXXX")
[[ "$DRY_RUN" == 1 ]] && echo "(dry run: nothing is changed; the ruleset JSON is written to $OUT)"
echo "repo: $REPO"

# GitHub's own apps. Every required check below is a GitHub Actions job,
# so pinning the check to that app stops another app posting a fake pass.
ACTIONS_APP_ID=$(gh api apps/github-actions --jq .id)
DEPENDABOT_APP_ID=$(gh api apps/dependabot --jq .id)
MAINTAINERS_ID=$(gh api "orgs/$ORG/teams/maintainers" --jq .id)
ADMIN_ROLE_ID=5  # the built-in repository "admin" role

# The 21 checks main and dev required before the move to rulesets, plus
# the two PR-only checks from dependency-review.yml and pr-title.yml.
CHECKS=(
    "go test (proxy)" "go test (tui)" "pytest (tests/v3)" "pytest (tests/cli)"
    "shellcheck" "docker compose config" "yamllint (workflows)"
    "bootstrap on ubuntu-22.04" "bootstrap on ubuntu-24.04" "bootstrap on debian-12"
    "bootstrap on rockylinux-9" "ruff (python lint)" "codeql (python)" "codeql (go)"
    "pytest (tests/v3-service)" "pytest (tests/contracts)" "pytest (tests/infrastructure)"
    "pytest (geometric-lens/tests)" "llama.cpp patches apply to pinned SHA"
    "e2e acceptance (proxy + sandbox + fake llama)" "bootstrap via sudo for a regular user"
    "dependency review" "pr title"
)
checks_json() {
    local first=1 c
    printf '['
    for c in "${CHECKS[@]}"; do
        [[ $first == 1 ]] || printf ','
        printf '{"context":"%s","integration_id":%s}' "$c" "$ACTIONS_APP_ID"
        first=0
    done
    printf ']'
}

RELEASE_BRANCHES='"refs/heads/dev","refs/heads/staging","refs/heads/main"'
ADMINS='{"actor_id":1,"actor_type":"OrganizationAdmin","bypass_mode":"always"},
        {"actor_id":'"$ADMIN_ROLE_ID"',"actor_type":"RepositoryRole","bypass_mode":"always"}'
MAINTAINERS='{"actor_id":'"$MAINTAINERS_ID"',"actor_type":"Team","bypass_mode":"always"}'
DEPENDABOT='{"actor_id":'"$DEPENDABOT_APP_ID"',"actor_type":"Integration","bypass_mode":"always"}'

write_rulesets() {
    cat > "$OUT/1-release-branches-lock.json" <<EOF
{"name":"Release branches: no force-push or deletion","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$RELEASE_BRANCHES],"exclude":[]}},
 "rules":[{"type":"non_fast_forward"},{"type":"deletion"}],
 "bypass_actors":[]}
EOF
    cat > "$OUT/2-release-branches-gates.json" <<EOF
{"name":"Release branches: checks, history and review","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$RELEASE_BRANCHES],"exclude":[]}},
 "rules":[
   {"type":"required_linear_history"},
   {"type":"required_status_checks","parameters":{
      "strict_required_status_checks_policy":true,"do_not_enforce_on_create":false,
      "required_status_checks":$(checks_json)}},
   {"type":"pull_request","parameters":{
      "required_approving_review_count":1,"require_code_owner_review":true,
      "dismiss_stale_reviews_on_push":true,"require_last_push_approval":true,
      "required_review_thread_resolution":true,"allowed_merge_methods":["squash","rebase"],
      "dismissal_restriction":{"enabled":true,"allowed_actors":[{"id":$MAINTAINERS_ID,"type":"Team"}]}}}],
 "bypass_actors":[$ADMINS]}
EOF
    cat > "$OUT/3-branches-maintainers-only.json" <<EOF
{"name":"Branches: only maintainers create or push","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["~ALL"],"exclude":["refs/heads/star-history"]}},
 "rules":[{"type":"creation"},{"type":"update","parameters":{"update_allows_fetch_and_merge":false}},{"type":"deletion"}],
 "bypass_actors":[$ADMINS,$MAINTAINERS,$DEPENDABOT]}
EOF
    cat > "$OUT/4-release-tags-create.json" <<EOF
{"name":"Release tags: only maintainers create","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "rules":[{"type":"creation"}],
 "bypass_actors":[$ADMINS,$MAINTAINERS]}
EOF
    cat > "$OUT/5-release-tags-immutable.json" <<EOF
{"name":"Release tags: never move or delete","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "rules":[{"type":"update","parameters":{"update_allows_fetch_and_merge":false}},{"type":"deletion"},{"type":"non_fast_forward"}],
 "bypass_actors":[]}
EOF
    local f
    for f in "$OUT"/*.json; do
        python3 -m json.tool "$f" >/dev/null || { echo "error: $f is not valid JSON" >&2; exit 1; }
    done
}

ruleset_id() {  # print the id of the ruleset with this exact name, or nothing
    gh api "repos/$REPO/rulesets" --paginate --jq ".[] | select(.name == \"$1\") | .id"
}

step_teams() {
    echo "== 1. team access"
    local current pair slug perm have
    current=$(gh api "repos/$REPO/teams" --jq '.[] | "\(.slug)=\(.permission)"')
    for pair in maintainers=maintain reviewers=push triagers=triage; do
        slug="${pair%%=*}"; perm="${pair#*=}"
        have=$(printf '%s\n' "$current" | awk -F= -v s="$slug" '$1 == s {print $2; exit}')
        if [[ "$have" == "$perm" ]]; then
            echo "  keep    $slug: $perm"
        elif [[ "$DRY_RUN" == 1 ]]; then
            echo "  set     $slug: ${have:-none} → $perm"
        else
            if gh api -X PUT "orgs/$ORG/teams/$slug/repos/$REPO" -f permission="$perm" --silent; then
                echo "  set     $slug: ${have:-none} → $perm"
            else
                # Not fatal: nothing below depends on team access.
                echo "  FAILED  $slug → $perm. Set it by hand: repo Settings → Collaborators and teams." >&2
            fi
        fi
    done
}

step_merge_settings() {
    echo "== 2. merge settings"
    local now
    now=$(gh api "repos/$REPO" --jq '"merge commit=\(.allow_merge_commit) squash=\(.allow_squash_merge) rebase=\(.allow_rebase_merge) delete-on-merge=\(.delete_branch_on_merge) update-branch=\(.allow_update_branch)"')
    echo "  now:    $now"
    echo "  target: merge commit=false squash=true rebase=true delete-on-merge=true update-branch=true (squash title = PR title)"
    [[ "$DRY_RUN" == 1 ]] && return
    gh api -X PATCH "repos/$REPO" --silent \
        -F allow_merge_commit=false -F allow_squash_merge=true -F allow_rebase_merge=true \
        -F delete_branch_on_merge=true -F allow_update_branch=true \
        -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY
}

step_rulesets() {
    echo "== 3. rulesets"
    local f name id
    for f in "$OUT"/*.json; do
        name=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$f")
        id=$(ruleset_id "$name")
        if [[ -n "$id" ]]; then
            echo "  update  $name (id $id)"
            [[ "$DRY_RUN" == 1 ]] || gh api -X PUT "repos/$REPO/rulesets/$id" --input "$f" --silent
        else
            echo "  create  $name"
            [[ "$DRY_RUN" == 1 ]] || gh api -X POST "repos/$REPO/rulesets" --input "$f" --silent
        fi
    done
    [[ "$DRY_RUN" == 1 ]] && return
    for f in "$OUT"/*.json; do
        name=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$f")
        id=$(ruleset_id "$name")
        [[ -n "$id" ]] && [[ "$(gh api "repos/$REPO/rulesets/$id" --jq .enforcement)" == active ]] \
            || { echo "error: '$name' did not read back as active; not removing the old protection" >&2; exit 1; }
    done
    echo "  all five read back as active"
}

step_remove_replaced() {
    echo "== 4. remove what the rulesets replace"
    local id b
    id=$(ruleset_id "Protect all branches")
    if [[ -n "$id" ]]; then
        echo "  delete  ruleset 'Protect all branches' (id $id)"
        [[ "$DRY_RUN" == 1 ]] || gh api -X DELETE "repos/$REPO/rulesets/$id" --silent
    else
        echo "  keep    (no 'Protect all branches' ruleset)"
    fi
    for b in main dev; do
        if gh api "repos/$REPO/branches/$b/protection" >/dev/null 2>&1; then
            echo "  delete  classic branch protection on $b"
            [[ "$DRY_RUN" == 1 ]] || gh api -X DELETE "repos/$REPO/branches/$b/protection" --silent
        else
            echo "  keep    (no classic protection on $b)"
        fi
    done
}

write_rulesets
step_teams
step_merge_settings
step_rulesets
step_remove_replaced
[[ "$DRY_RUN" == 1 ]] && echo "dry run done. Review $OUT/*.json, then run without --dry-run."
[[ "$DRY_RUN" == 1 ]] || echo "done."
