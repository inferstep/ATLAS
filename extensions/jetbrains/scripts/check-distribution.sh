#!/usr/bin/env bash
# Check the built plugin ZIP for the things a release must and must not ship.
#
# This is deliberately a script rather than a Gradle task: it reads the ZIP
# the same way a user installs it, and it can be pointed at any artifact
# (a downloaded CI artifact, for example). Run it from extensions/jetbrains:
#
#   bash scripts/check-distribution.sh
#   bash scripts/check-distribution.sh build/distributions/jetbrains-plugin-0.1.0.zip
set -euo pipefail

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

zip_path=${1:-}
if [[ -z "$zip_path" ]]; then
  candidates=$(find build/distributions -maxdepth 1 -type f -name '*.zip' | sort)
  count=$(printf '%s\n' "$candidates" | grep -c . || true)
  if [[ "$count" != "1" ]]; then
    fail "expected one ZIP in build/distributions, found $count"
  fi
  zip_path=$candidates
fi
[[ -f "$zip_path" ]] || fail "no such ZIP: $zip_path"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
unzip -q "$zip_path" -d "$work"

main_jars=$(find "$work" -path '*/lib/*.jar' -type f -name 'jetbrains-plugin-*.jar' ! -name '*searchableOptions*')
count=$(printf '%s\n' "$main_jars" | grep -c . || true)
[[ "$count" == "1" ]] || fail "expected one main plugin jar, found $count"
main_jar=$main_jars

unzip -p "$main_jar" META-INF/plugin.xml >"$work/plugin.xml"
grep -q '<id>com.inferstep.atlas</id>' "$work/plugin.xml" || fail "plugin.xml has the wrong plugin id"
grep -q 'since-build="261"' "$work/plugin.xml" || fail "plugin.xml does not declare since-build 261"
grep -q '<version>[0-9][^<]*</version>' "$work/plugin.xml" || fail "plugin.xml has no numeric version"

# The IDE reads the plugin icon from these two file names in META-INF and from
# nowhere else, so a descriptor check cannot see them: without them Settings |
# Plugins and the Marketplace listing draw the generic plugin icon. They are
# also the only files whose size the platform requires, hence the viewBox.
for icon in META-INF/pluginIcon.svg META-INF/pluginIcon_dark.svg; do
  if ! unzip -p "$main_jar" "$icon" >"$work/icon-check" 2>/dev/null; then
    fail "the main jar has no $icon"
  fi
  [[ -s "$work/icon-check" ]] || fail "$icon in the main jar is empty"
  grep -q 'viewBox="0 0 40 40"' "$work/icon-check" || fail "$icon is not a 40x40 SVG"
done

# The platform provides coroutines and the Kotlin stdlib on the plugin
# classloader. A second copy inside the ZIP shadows them, which is the
# two-runtime failure the Ktor pin exists to avoid.
coroutines=$(find "$work" -path '*/lib/*' -type f \( -name 'kotlinx-coroutines-*.jar' -o -name 'kotlin-stdlib-*.jar' \) -print)
[[ -z "$coroutines" ]] || fail "the ZIP bundles a coroutine or Kotlin stdlib jar: $coroutines"

# The transport has to be there; these are the two jars the client is built
# from, and their absence would only surface at runtime.
compgen -G "$work/*/lib/ktor-client-core-jvm-*.jar" >/dev/null || fail "ktor-client-core is missing"
compgen -G "$work/*/lib/kotlinx-serialization-json-jvm-*.jar" >/dev/null || fail "kotlinx-serialization-json is missing"

# Nothing that belongs to a checkout, a test or a credential store.
for forbidden in 'real-turn.sse' '*.env' '.env' 'secrets' '*.pem' '*.key'; do
  found=$(find "$work" -name "$forbidden" -print)
  [[ -z "$found" ]] || fail "the ZIP contains $forbidden: $found"
done
tests=$(find "$work" -type f \( -name '*Test.class' -o -name '*Test$*.class' \) -print)
[[ -z "$tests" ]] || fail "the ZIP contains test classes: $tests"

# Two jars with the same file name would be a classloader conflict even when
# their coordinates differ.
duplicates=$(find "$work" -path '*/lib/*.jar' -type f -exec basename {} \; | sort | uniq -d)
[[ -z "$duplicates" ]] || fail "duplicate jar names in the ZIP: $duplicates"

jar_count=$(find "$work" -path '*/lib/*.jar' -type f | wc -l | tr -d ' ')
version=$(grep -m1 -o '<version>[^<]*</version>' "$work/plugin.xml")
printf 'checked %s: %s jars, %s\n' "$zip_path" "$jar_count" "$version"
