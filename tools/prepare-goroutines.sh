#!/bin/sh
# Make the goroutine TinyGo root (ADR 0204) under its exclusive lock and
# publish it in one rename; see tools/prepare-compat.sh.
set -eu
root=${1:?goroutine root required}
cache=${2:?cache directory required}
mkdir -p "$(dirname "$root")"
root=$(cd "$(dirname "$root")" && pwd -P)/$(basename "$root")
if [ "${GOOS_ROOT_LOCK:-}" != "exclusive $root" ]; then
    marker=$(mktemp "${TMPDIR:-/tmp}/goos-goroutines-wait.XXXXXX")
    exec go run ./tools/rootlock exclusive "$root" -- env GOOS_COMPAT_MARKER="$marker" sh "$0" "$@"
fi
marker=${GOOS_COMPAT_MARKER:-}
if [ -n "$marker" ] && [ -f "$root/goroutineroot.json" ] && [ "$root/goroutineroot.json" -nt "$marker" ]; then
    rm -f "$marker"
    echo "prepare-goroutines: $root was published while waiting"
    exit 0
fi
rm -f "$marker"
for candidate in "${root}".stage.*; do
    [ -e "$candidate" ] || continue
    rm -rf "$candidate"
done
stage=$(mktemp -d "${root}.stage.XXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
go run ./tools/goroutineroot "$(tools/tinygo.sh env TINYGOROOT)" "$stage" tinygo/goroutines
rm -rf "$cache/tinygo-home-goroutines/Library/Caches/tinygo"/goroot-* "$cache/tinygo-home-goroutines/cache/tinygo"/goroot-*
go run ./tools/rootlock publish "$stage" "$root"
