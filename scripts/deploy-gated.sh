#!/usr/bin/env bash
# Deploy this checkout to the local Compose stack, gated.
#
# Runs the suites, rebuilds the services this repo builds, recreates them one
# at a time and waits for each to be healthy, then records what is running
# against the commit it claims to be. DEPLOYED_SHA is written only when every
# step and every check passed. A failure prints why and leaves the previous
# DEPLOYED_SHA in place.
#
#   scripts/deploy-gated.sh
#
# ATLAS_DEPLOY_DIR holds the records (default ~/atlas-ralph):
#   DEPLOYED_SHA                    the commit the stack runs
#   deployed/prev-<stamp>.json      the images replaced, kept as atlas-prev:*
#                                   so a rollback target exists
#   deployed/running-<sha>.json     the image each service runs, pinned as
#                                   atlas-built:<service>-<sha>
#
# llama-server is rebuilt and recreated when inference/ changed since the last
# deploy, or when the model server running now was not put there by this
# script: a recreate reloads the model. The other four are rebuilt every time.
set -uo pipefail
export PATH="$HOME/.local/bin:$PATH"
cd "$(dirname "$0")/.." || exit 2

RECORD=${ATLAS_DEPLOY_DIR:-$HOME/atlas-ralph}
mkdir -p "$RECORD/deployed"
SHA=$(git rev-parse --short HEAD)
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
PREV_SHA=$(cat "$RECORD/DEPLOYED_SHA" 2>/dev/null || true)
ALL="llama-server geometric-lens v3-service sandbox atlas-proxy"

fail() { echo "NOT DEPLOYED: $*"; exit 1; }
cid() { docker compose ps -a -q "$1" 2>/dev/null | head -1; }
image_of() { docker inspect -f '{{.Image}}' "$(cid "$1")" 2>/dev/null || true; }
port() {  # .env key, default
  local v
  v=$(grep -E "^$1=" .env 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"')
  echo "${v:-$2}"
}

echo "=== deploying $SHA (last deployed: ${PREV_SHA:-none}) ==="
git log --oneline -1

# A deploy describes a commit, so the tree must be that commit. Ignored files
# (.env, local fuzz tests) are not seen here.
[ -z "$(git status --porcelain)" ] || fail "the checkout has uncommitted changes"

SERVICES="geometric-lens v3-service sandbox atlas-proxy"
llama_pin=""
[ -n "$PREV_SHA" ] &&
  llama_pin=$(docker image inspect -f '{{.Id}}' "atlas-built:llama-server-$PREV_SHA" 2>/dev/null || true)
if [ -z "$llama_pin" ] || [ "$llama_pin" != "$(image_of llama-server)" ] ||
   ! git diff --quiet "$PREV_SHA" HEAD -- inference/ 2>/dev/null; then
  SERVICES="llama-server $SERVICES"
fi
echo "rebuilding: $SERVICES"

# Full output goes to the record directory; the tail is enough to read here.
echo "=== go test (proxy) ==="
( cd proxy && go test ./... > "$RECORD/deployed/gotest-$STAMP.txt" 2>&1 ) ||
  { tail -20 "$RECORD/deployed/gotest-$STAMP.txt"; fail "go test failed (deployed/gotest-$STAMP.txt)"; }
tail -3 "$RECORD/deployed/gotest-$STAMP.txt"
echo "=== pytest tests/v3 tests/v3-service tests/cli ==="
python3 -m pytest -q tests/v3 tests/v3-service tests/cli > "$RECORD/deployed/pytest-$STAMP.txt" 2>&1 ||
  { tail -20 "$RECORD/deployed/pytest-$STAMP.txt"; fail "pytest failed (deployed/pytest-$STAMP.txt)"; }
tail -2 "$RECORD/deployed/pytest-$STAMP.txt"

# --- what runs now, pinned before anything replaces it -----------------------
# An image id is immutable; a tag is not. The outgoing images get an
# id-bearing name first, so a rollback target exists after the tags move.
PREV=$RECORD/deployed/prev-$STAMP.json
{
  printf '{\n  "stamp": "%s",\n  "replacing_sha": "%s",\n  "incoming_sha": "%s",\n  "outgoing": {' \
    "$STAMP" "${PREV_SHA:-unknown}" "$SHA"
  sep=""
  for svc in $SERVICES; do
    id=$(image_of "$svc")
    tag=none
    if [ -n "$id" ]; then
      tag="atlas-prev:${svc}-${STAMP}"
      docker tag "$id" "$tag" >/dev/null 2>&1 || tag=UNTAGGABLE
    fi
    printf '%s\n    "%s": {"image_id": "%s", "preserved_as": "%s"}' "$sep" "$svc" "${id:-none}" "$tag"
    sep=","
  done
  printf '\n  }\n}\n'
} > "$PREV"
cat "$PREV"

echo "=== build: $SERVICES ==="
# shellcheck disable=SC2086
docker compose build $SERVICES 2>&1 | tail -15
[ "${PIPESTATUS[0]}" = 0 ] || fail "docker compose build failed"

# --- recreate, in dependency order, each one healthy before the next --------
wait_healthy() {  # service, deadline in seconds
  local s=none i
  for i in $(seq 1 $(( $2 / 5 ))); do
    s=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}no-healthcheck{{end}}' \
      "$(cid "$1")" 2>/dev/null || echo none)
    [ "$s" = healthy ] && { echo "$1 healthy"; return 0; }
    sleep 5
  done
  echo "$1 is '$s' after $2 s"
  return 1
}
for svc in $SERVICES; do
  echo "=== recreate $svc ==="
  docker compose up -d --force-recreate --no-deps "$svc" 2>&1 | tail -3
  [ "${PIPESTATUS[0]}" = 0 ] || fail "docker compose up $svc failed"
  deadline=240
  [ "$svc" = llama-server ] && deadline=900  # the model loads first
  wait_healthy "$svc" "$deadline" || fail "$svc did not become healthy"
done

# --- every service runs the image this deploy stands for ---------------------
# A rebuilt service must run the image its tag resolves to now, which proves
# it was recreated onto this build and did not keep an older one. A model
# server left in place must still run the image pinned at the last deploy.
for svc in $ALL; do
  c=$(cid "$svc")
  got=$(docker inspect -f '{{.Image}}' "$c" 2>/dev/null || true)
  case " $SERVICES " in
    *" $svc "*)
      ref=$(docker inspect -f '{{.Config.Image}}' "$c" 2>/dev/null || true)
      want=$(docker image inspect -f '{{.Id}}' "$ref" 2>/dev/null || true) ;;
    *)
      want=$llama_pin ;;
  esac
  if [ -z "$got" ] || [ "$got" != "$want" ]; then
    fail "image identity mismatch for $svc: runs ${got:-nothing}, expected ${want:-nothing}"
  fi
  docker tag "$got" "atlas-built:${svc}-${SHA}" >/dev/null 2>&1
  echo "$svc runs $got (pinned as atlas-built:${svc}-${SHA})"
done

RUNNING=$RECORD/deployed/running-$SHA.json
{
  printf '{\n  "sha": "%s", "stamp": "%s", "previous": "%s", "rebuilt": "%s",\n' \
    "$SHA" "$STAMP" "$(basename "$PREV")" "$SERVICES"
  printf '  "llama_entrypoint_sha256": "%s",\n' "$(sha256sum inference/entrypoint-v3.1.sh | cut -d' ' -f1)"
  printf '  "llama_args": %s,\n' "$(docker inspect -f '{{json .Args}}' "$(cid llama-server)" 2>/dev/null || echo null)"
  printf '  "running": {'
  sep=""
  for svc in $ALL; do
    printf '%s\n    "%s": {"image_id": "%s", "pinned_as": "atlas-built:%s-%s"}' \
      "$sep" "$svc" "$(image_of "$svc")" "$svc" "$SHA"
    sep=","
  done
  printf '\n  }\n}\n'
} > "$RUNNING"
cat "$RUNNING"

# --- the running stack does what this commit does ----------------------------
echo "=== post-deploy checks ==="
PROXY=$(port ATLAS_PROXY_PORT 8090) V3=$(port ATLAS_V3_PORT 8070) \
LENS=$(port ATLAS_LENS_PORT 8099) SANDBOX=$(port ATLAS_SANDBOX_PORT 30820) \
python3 - <<'PY' || fail "a post-deploy check failed"
import json, os, sys, urllib.request

def get(port, path):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=20) as r:
        return r.status, json.loads(r.read() or b"null")

def syntax(code, language, filename):
    body = json.dumps({"code": code, "language": language, "filename": filename}).encode()
    req = urllib.request.Request(f"http://127.0.0.1:{os.environ['SANDBOX']}/syntax-check",
                                 data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read())

def proxy_upstreams():
    _, d = get(os.environ["PROXY"], "/health")
    return d.get("status") == "ok", d

def v3_health():
    code, d = get(os.environ["V3"], "/health")
    return code == 200, d

def lens_is_this_commit():
    # The pattern cache was removed; its routes must be gone from the lens.
    _, spec = get(os.environ["LENS"], "/openapi.json")
    paths = sorted(spec.get("paths", {}))
    return not any("/patterns" in p for p in paths), paths

def jinja_gate():
    d = syntax("<html>{% for x in y %)Z{% endfor %}</html>", "html", "templates/index.html")
    return (not d.get("valid")) and bool(d.get("errors")), d

def subdir_check():
    d = syntax("x = 1\n", "python", "src/pkg/app.py")
    return bool(d.get("valid")), d

failed = False
for name, check in [("proxy sees every upstream", proxy_upstreams), ("v3-service health", v3_health),
                    ("lens runs this commit", lens_is_this_commit), ("sandbox Jinja gate", jinja_gate),
                    ("sandbox subdir syntax check", subdir_check)]:
    try:
        ok, detail = check()
    except Exception as e:
        ok, detail = False, repr(e)
    print(("OK   " if ok else "FAIL ") + name + ("" if ok else f": {json.dumps(detail)[:300]}"))
    failed |= not ok
sys.exit(1 if failed else 0)
PY

# v3-service calls the sandbox with the caller's filename; the proxy sends a
# resolved absolute path. Checked through the running v3-service, because a
# unit test cannot see a container that runs an older image.
V3PATH=$(docker exec "$(cid v3-service)" python -c "
import sys; sys.path.insert(0, '/app')
import adapters as A
ok, _, err = A.SandboxAdapter().syntax_check('x = 1\n', 'python', '/workspace/sub/app.py')
print('OK' if ok else 'BROKEN:' + err[:120])
" 2>&1 | tail -1)
echo "v3 absolute-path syntax check: $V3PATH"
[ "$V3PATH" = OK ] || fail "v3-service cannot syntax-check an absolute path"

echo "$SHA" > "$RECORD/DEPLOYED_SHA"
echo "DEPLOYED $SHA  (identities in $RUNNING, rollback targets in $PREV)"
