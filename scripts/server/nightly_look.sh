#!/bin/sh
# One look of the timer on the development server (docs/quality/gates.md,
# section "The timer").
#
#   nightly_look.sh run <usual project> <marker file> -- <the command of the nightly run, with --tick>
#   nightly_look.sh after <usual project> <marker file>
#
# run: it asks the nightly run what is due, with --look, which starts
# nothing. Status 3 says that nothing is due: then nothing else is done, and
# the look ends with status 0. Every other status than 0 is passed on, so a
# look whose settings cannot be used fails where it is seen. When a run is
# due, the containers of the usual stack that run are written to the marker
# file and stopped, the nightly run is started, and after it those
# containers are started again, also when the run failed.
#
# Why the usual stack is stopped: it computes on the graphics card and takes
# no lock, and the card does not hold two model servers. It is stopped only
# when a run is due. Nothing else is stopped: when something else holds the
# card, the nightly run starts nothing.
#
# after: it starts again the containers that the marker file names, and
# removes the file. With no marker file it does nothing. The unit calls it
# after every look, so a look that was stopped in its middle leaves no
# stack down.
#
# The marker file has to lie on disk, in a place that a restart of the
# server leaves as it is. Docker does not start a container again that was
# stopped by hand, also not after a restart. So after a restart in the
# middle of a run only the marker file says what to start, and the first
# look after the restart starts it, before anything else.
set -eu

usage() {
    echo "nightly_look.sh: $1. Usage: nightly_look.sh run <usual project> <marker file> -- <command>, or nightly_look.sh after <usual project> <marker file>." >&2
    exit 2
}

[ "$#" -ge 3 ] || usage "three words are needed"
mode=$1
usual=$2
marker=$3
shift 3

# Start again what an earlier look stopped, the oldest container first, as
# they were made. A container that does not start is named, the others are
# still started, and the marker file stays, so the next look tries again.
start_again() {
    [ -f "$marker" ] || return 0
    stopped=""
    count=0
    while read -r id; do
        [ -n "$id" ] && stopped="$id $stopped" && count=$((count + 1))
    done < "$marker"
    failed=0
    for id in $stopped; do
        if ! docker start "$id" > /dev/null; then
            echo "nightly_look.sh: the container $id of the usual stack did not start again. The next look tries again. Fix: look at it with 'docker ps --all', and start the usual stack by hand. When that container is gone for good, remove the marker file of the look: every look fails here while the file names it." >&2
            failed=1
        fi
    done
    [ "$failed" -eq 0 ] || return 1
    rm -f -- "${marker:?}"
    echo "nightly_look.sh: the usual stack runs again: $count container(s) that a look had stopped were started."
}

case "$mode" in
after)
    start_again
    exit 0
    ;;
run)
    [ "${1:-}" = "--" ] || usage "the command of the nightly run comes after --"
    shift
    [ "$#" -ge 1 ] || usage "no command of the nightly run was given"
    ;;
*)
    usage "the first word is 'run' or 'after', not '$mode'"
    ;;
esac

# A marker file that is there was left by a look that did not end: first
# start again what that look stopped.
start_again

due=0
"$@" --look || due=$?
if [ "$due" -eq 3 ]; then
    exit 0
fi
if [ "$due" -ne 0 ]; then
    exit "$due"
fi

running=$(docker ps --quiet --filter "label=com.docker.compose.project=$usual")
if [ -n "$running" ]; then
    # The marker file is written before the stop, so a stop that ends in its
    # middle still leaves the names of what was stopped.
    printf '%s\n' "$running" > "$marker"
    # shellcheck disable=SC2086  # each line of the list is one container
    docker stop $running > /dev/null
fi

# When this look is told to stop, the nightly run is told too, and it stops
# its own stack first. The shell waits for it, and only then starts the
# usual stack again: two model servers do not fit on the card.
trap : TERM INT
status=0
"$@" || status=$?
start_again || status=1
exit "$status"
