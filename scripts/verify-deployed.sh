#!/usr/bin/env bash
# Verify each running container is on the newest build of its image.
#
# `docker compose build` produces a new image; it does NOT restart anything.
# A container started before that build keeps running the old code, and every
# subsequent measurement describes a build that is no longer the source tree.
# That has happened twice: a YAML-checker fix sat built-but-not-running for a
# full cycle, and earlier a proxy/sandbox mount split invalidated a day of
# results. Both were invisible because the containers were healthy — they were
# just healthy copies of the wrong thing.
#
# Exit 0 when every service is current, 1 otherwise. Prints the fix command.
#
#   scripts/verify-deployed.sh [service ...]     (default: all atlas services)
set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

SERVICES=("$@")
if [ ${#SERVICES[@]} -eq 0 ]; then
    SERVICES=(atlas-proxy v3-service geometric-lens sandbox)
fi

stale=()
for svc in "${SERVICES[@]}"; do
    cid=$(docker compose ps -q "$svc" 2>/dev/null)
    if [ -z "$cid" ]; then
        echo "  [SKIP] $svc — not running"
        continue
    fi

    running_image=$(docker inspect "$cid" --format '{{.Image}}' 2>/dev/null)
    image_ref=$(docker inspect "$cid" --format '{{.Config.Image}}' 2>/dev/null)
    newest_image=$(docker images -q "$image_ref" 2>/dev/null | head -1)

    if [ -z "$newest_image" ]; then
        echo "  [WARN] $svc — cannot resolve $image_ref locally"
        continue
    fi

    # Compare by ID, not by tag: a rebuild moves the tag to a new ID while a
    # container keeps holding the old one by digest.
    if [ "${running_image#sha256:}" == "${newest_image#sha256:}" ] || \
       [[ "${running_image}" == *"${newest_image}"* ]]; then
        # Container is on the newest IMAGE — now ask whether that image was
        # built from the current source. "Built but not recreated" and "edited
        # but not built" break a measurement identically, and both look
        # healthy from the outside.
        src_dir=""
        case "$svc" in
            atlas-proxy)     src_dir="proxy" ;;
            v3-service)      src_dir="v3-service" ;;
            geometric-lens)  src_dir="geometric-lens" ;;
            sandbox)         src_dir="sandbox" ;;
        esac
        built_at=$(docker inspect "$newest_image" --format '{{.Created}}' 2>/dev/null)
        built_epoch=$(date -d "$built_at" +%s 2>/dev/null || echo 0)
        newest_src=0
        if [ -n "$src_dir" ] && [ -d "$src_dir" ]; then
            newest_src=$(find "$src_dir" -type f \( -name '*.go' -o -name '*.py' -o -name 'Dockerfile*' -o -name 'requirements*.txt' \) \
                -not -path '*/__pycache__/*' -printf '%T@\n' 2>/dev/null | sort -rn | head -1 | cut -d. -f1)
            newest_src=${newest_src:-0}
        fi
        if [ "$newest_src" -gt "$built_epoch" ] 2>/dev/null; then
            echo "  [STALE] $svc — source in $src_dir/ is newer than the image it is running"
            stale+=("$svc")
        else
            echo "  [OK]   $svc — running the newest build, image newer than its source"
        fi
    else
        echo "  [STALE] $svc — running ${running_image:7:12}, newest is ${newest_image:0:12}"
        stale+=("$svc")
    fi
done

if [ ${#stale[@]} -gt 0 ]; then
    echo
    echo "Stale: ${stale[*]}"
    # Recreating a service mid-session kills the in-flight request: the stream
    # dies without a done event and the run records harness defects that are
    # the restart's fault, not ATLAS's. That happened — a proxy recreate
    # during a live task produced two phantom protocol defects and a wrong
    # answer. Warn before handing over the fix command.
    if pgrep -f "[e]2e-reliability.py" >/dev/null 2>&1; then
        echo
        echo "  !! A reliability run is IN FLIGHT. Recreating now will kill its"
        echo "     current session and record protocol defects caused by you,"
        echo "     not by ATLAS. Wait for it to finish, then recreate."
    fi
    # The token goes through stdin, so it never shows in a process list.
    if slots=$(printf 'header = "Authorization: Bearer %s"\n' "$(cat secrets/service-token 2>/dev/null)" \
               | curl -sf --max-time 5 -K - http://localhost:8080/slots 2>/dev/null); then
        busy=$(grep -o '"is_processing":true' <<<"$slots" | wc -l)
        if [[ "$busy" -gt 0 ]]; then
            echo "  !! llama-server has ${busy} slot(s) processing — something is mid-turn."
        fi
    else
        echo "  !! Could not read llama-server's /slots, so a turn may be in flight."
    fi
    echo
    echo "Fix:   docker compose up -d --force-recreate ${stale[*]} --no-deps"
    exit 1
fi
echo
echo "All checked services are running their newest build."
