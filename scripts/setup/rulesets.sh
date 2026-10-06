#!/usr/bin/env bash
# Branch and tag rules for ATLAS (GOVERNANCE: dev → staging → main).
#
# No account can push to dev, staging or main, and no account can skip a
# required check. A change reaches dev as a pull request through the merge
# queue. staging and main move forward by the release step
# (scripts/setup/release_step.py, docs/RELEASE.md).
#
# Applies, in this order, stopping at the first failure:
#   1. team access: maintainers = maintain, reviewers = write, triagers = triage
#   2. merge settings: squash or rebase only (linear history), delete merged
#      branches, offer "update branch"
#   3. nine rulesets, created or updated by name. One that is already the
#      same is left alone.
#      - Release branches: no force-push or deletion   (dev, staging, main; no bypass)
#      - Integration branch: pull request, linear history and merge queue
#                                                       (dev; no bypass)
#      - Integration branch: required checks           (dev; no bypass)
#      - Release branches: pull request and linear history
#                                                       (staging, main; no bypass)
#      - Release branches: required checks             (staging, main; no bypass;
#        the branch must be up to date)
#      - Release branches: review                      (staging, main; an admin's
#        own pull request merges without an approval, a push never does)
#      - Branches: only maintainers create or push     (all branches except dev,
#        the merge queue's branches and star-history; admins, maintainers and
#        Dependabot bypass)
#      - Release tags: only maintainers create         (v*)
#      - Release tags: never move or delete            (v*; no bypass)
#   4. removes what they replace: the rulesets with earlier names and the
#      classic branch protection on main and dev, only after step 3 reads
#      back as active
#
# --dry-run changes nothing. For each ruleset it prints whether the live one
# is the same, and what an update would change.
#
# Idempotent. Runs on macOS's bash 3.2. Needs `gh` logged in as a repo
# admin; team access also needs the org's teams to be visible to the token.
#
# Usage: scripts/setup/rulesets.sh [--dry-run] [--repo OWNER/NAME]
set -euo pipefail

REPO="inferstep/ATLAS"
DRY_RUN=0
HERE=$(cd "$(dirname "$0")" && pwd)

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }

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

TMP="${TMPDIR:-/tmp}"
OUT=$(mktemp -d "${TMP%/}/atlas-rulesets.XXXXXX")
mkdir "$OUT/live"
[[ "$DRY_RUN" == 1 ]] && echo "(dry run: nothing is changed; the ruleset JSON is written to $OUT)"
echo "repo: $REPO"

# GitHub's own apps. Every required check below is a GitHub Actions job,
# so pinning the check to that app stops another app posting a fake pass.
ACTIONS_APP_ID=$(gh api apps/github-actions --jq .id)
DEPENDABOT_APP_ID=$(gh api apps/dependabot --jq .id)
MAINTAINERS_ID=$(gh api "orgs/$ORG/teams/maintainers" --jq .id)
ADMIN_ROLE_ID=5  # the built-in repository "admin" role

# Required on dev, staging and main.
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
# Required on dev only. The job that reports this check is not on main, and
# a required check that no job reports blocks every pull request to the
# branch. Move it into CHECKS when main has the job.
DEV_ONLY_CHECKS=("code health (size)")

checks_json() {  # the given check names, each pinned to GitHub Actions
    local first=1 c
    printf '['
    for c in "$@"; do
        [[ $first == 1 ]] || printf ','
        printf '{"context":"%s","integration_id":%s}' "$c" "$ACTIONS_APP_ID"
        first=0
    done
    printf ']'
}

RELEASE_BRANCHES='"refs/heads/dev","refs/heads/staging","refs/heads/main"'
PROMOTED_BRANCHES='"refs/heads/staging","refs/heads/main"'
ADMINS='{"actor_id":1,"actor_type":"OrganizationAdmin","bypass_mode":"always"},
        {"actor_id":'"$ADMIN_ROLE_ID"',"actor_type":"RepositoryRole","bypass_mode":"always"}'
# "pull_request" mode lets an admin merge an own pull request with the
# button. It does not let anyone push.
ADMINS_OWN_PULL_REQUEST='{"actor_id":1,"actor_type":"OrganizationAdmin","bypass_mode":"pull_request"},
        {"actor_id":'"$ADMIN_ROLE_ID"',"actor_type":"RepositoryRole","bypass_mode":"pull_request"}'
MAINTAINERS='{"actor_id":'"$MAINTAINERS_ID"',"actor_type":"Team","bypass_mode":"always"}'
DEPENDABOT='{"actor_id":'"$DEPENDABOT_APP_ID"',"actor_type":"Integration","bypass_mode":"always"}'

# Every change is a pull request with its conversations resolved. No
# approval is asked here: on staging and main the review ruleset asks for
# it, and on dev the merge queue could not merge a pull request that needs
# a bypass.
PULL_REQUEST='{"type":"pull_request","parameters":{
      "required_approving_review_count":0,"require_code_owner_review":false,
      "dismiss_stale_reviews_on_push":false,"require_last_push_approval":false,
      "required_review_thread_resolution":true,"allowed_merge_methods":["squash","rebase"]}}'
# The queue runs the required checks on the merged result and squashes each
# pull request into one commit.
MERGE_QUEUE='{"type":"merge_queue","parameters":{
      "merge_method":"SQUASH","grouping_strategy":"ALLGREEN",
      "max_entries_to_build":5,"min_entries_to_merge":1,"max_entries_to_merge":5,
      "min_entries_to_merge_wait_minutes":1,"check_response_timeout_minutes":60}}'

checks_ruleset() {  # name, branch list, strict (true: the branch must be up to date), check names
    local name="$1" branches="$2" strict="$3"
    shift 3
    cat <<EOF
{"name":"$name","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$branches],"exclude":[]}},
 "rules":[
   {"type":"required_status_checks","parameters":{
      "strict_required_status_checks_policy":$strict,"do_not_enforce_on_create":false,
      "required_status_checks":$(checks_json "$@")}}],
 "bypass_actors":[]}
EOF
}

write_rulesets() {
    cat > "$OUT/1-release-branches-lock.json" <<EOF
{"name":"Release branches: no force-push or deletion","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$RELEASE_BRANCHES],"exclude":[]}},
 "rules":[{"type":"non_fast_forward"},{"type":"deletion"}],
 "bypass_actors":[]}
EOF
    cat > "$OUT/2-integration-branch-merge.json" <<EOF
{"name":"Integration branch: pull request, linear history and merge queue","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/heads/dev"],"exclude":[]}},
 "rules":[$PULL_REQUEST,{"type":"required_linear_history"},$MERGE_QUEUE],
 "bypass_actors":[]}
EOF
    # dev moves often, so a pull request need not be up to date with it: the
    # merge queue runs the checks on the merged result.
    checks_ruleset "Integration branch: required checks" '"refs/heads/dev"' false \
        "${CHECKS[@]}" "${DEV_ONLY_CHECKS[@]}" > "$OUT/3-integration-branch-checks.json"
    cat > "$OUT/4-release-branches-merge.json" <<EOF
{"name":"Release branches: pull request and linear history","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$PROMOTED_BRANCHES],"exclude":[]}},
 "rules":[$PULL_REQUEST,{"type":"required_linear_history"}],
 "bypass_actors":[]}
EOF
    checks_ruleset "Release branches: required checks" "$PROMOTED_BRANCHES" true \
        "${CHECKS[@]}" > "$OUT/5-release-branches-checks.json"
    cat > "$OUT/6-release-branches-review.json" <<EOF
{"name":"Release branches: review","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":[$PROMOTED_BRANCHES],"exclude":[]}},
 "rules":[
   {"type":"pull_request","parameters":{
      "required_approving_review_count":1,"require_code_owner_review":true,
      "dismiss_stale_reviews_on_push":true,"require_last_push_approval":true,
      "required_review_thread_resolution":true,"allowed_merge_methods":["squash","rebase"],
      "dismissal_restriction":{"enabled":true,"allowed_actors":[{"id":$MAINTAINERS_ID,"type":"Team"}]}}}],
 "bypass_actors":[$ADMINS_OWN_PULL_REQUEST]}
EOF
    # dev and the queue's own branches are left out: the merge queue creates
    # and updates them, and the queue never uses a bypass. The two dev
    # rulesets above protect dev, with no bypass.
    cat > "$OUT/7-branches-maintainers-only.json" <<EOF
{"name":"Branches: only maintainers create or push","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["~ALL"],
   "exclude":["refs/heads/star-history","refs/heads/dev","refs/heads/gh-readonly-queue/**/*"]}},
 "rules":[{"type":"creation"},{"type":"update"},{"type":"deletion"}],
 "bypass_actors":[$ADMINS,$MAINTAINERS,$DEPENDABOT]}
EOF
    cat > "$OUT/8-release-tags-create.json" <<EOF
{"name":"Release tags: only maintainers create","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "rules":[{"type":"creation"}],
 "bypass_actors":[$ADMINS,$MAINTAINERS]}
EOF
    cat > "$OUT/9-release-tags-immutable.json" <<EOF
{"name":"Release tags: never move or delete","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "rules":[{"type":"update"},{"type":"deletion"},{"type":"non_fast_forward"}],
 "bypass_actors":[]}
EOF
    local f
    for f in "$OUT"/*.json; do
        python3 -m json.tool "$f" >/dev/null || { echo "error: $f is not valid JSON" >&2; exit 1; }
    done
}

ruleset_name() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["name"])' "$1"; }

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
    local f name id rc same=0 total=0
    for f in "$OUT"/*.json; do
        name=$(ruleset_name "$f")
        id=$(ruleset_id "$name")
        total=$((total + 1))
        if [[ -z "$id" ]]; then
            echo "  create  $name"
            [[ "$DRY_RUN" == 1 ]] || gh api -X POST "repos/$REPO/rulesets" --input "$f" --silent
            continue
        fi
        gh api "repos/$REPO/rulesets/$id" > "$OUT/live/$id.json"
        rc=0
        python3 "$HERE/ruleset_diff.py" "$f" "$OUT/live/$id.json" > "$OUT/live/$id.diff" || rc=$?
        case "$rc" in
            0)  echo "  same    $name (id $id)"
                same=$((same + 1)) ;;
            1)  echo "  update  $name (id $id)"
                sed 's/^/            /' "$OUT/live/$id.diff"
                [[ "$DRY_RUN" == 1 ]] || gh api -X PUT "repos/$REPO/rulesets/$id" --input "$f" --silent ;;
            *)  echo "error: could not compare '$name' with the live ruleset" >&2
                exit 1 ;;
        esac
    done
    echo "  $same of $total are the same as the live rulesets"
    [[ "$DRY_RUN" == 1 ]] && return
    for f in "$OUT"/*.json; do
        name=$(ruleset_name "$f")
        id=$(ruleset_id "$name")
        [[ -n "$id" ]] && [[ "$(gh api "repos/$REPO/rulesets/$id" --jq .enforcement)" == active ]] \
            || { echo "error: '$name' did not read back as active; not removing the old protection" >&2; exit 1; }
    done
    echo "  all $total read back as active"
}

step_remove_replaced() {
    echo "== 4. remove what the rulesets replace"
    local name id b
    # These hold rules that the rulesets above now hold, with an admin
    # bypass for pushes. Left in place, the review rule in them would also
    # stop the merge queue.
    for name in "Protect all branches" \
                "Integration branch: checks, history and review" \
                "Release branches: checks, history and review"; do
        id=$(ruleset_id "$name")
        if [[ -n "$id" ]]; then
            echo "  delete  ruleset '$name' (id $id)"
            [[ "$DRY_RUN" == 1 ]] || gh api -X DELETE "repos/$REPO/rulesets/$id" --silent
        else
            echo "  keep    (no '$name' ruleset)"
        fi
    done
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
