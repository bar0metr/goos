#!/bin/sh
set -eu
kind=${1:?compatibility kind required}
root=${2:?compatibility root required}
cache=${3:?cache directory required}
mkdir -p "$(dirname "$root")"
root=$(cd "$(dirname "$root")" && pwd -P)/$(basename "$root")
# Hold the root's exclusive lock for the whole update (tools/rootlock). The
# marker's age tells whether another writer published while this one waited.
if [ "${GOOS_ROOT_LOCK:-}" != "exclusive $root" ]; then
    marker=$(mktemp "${TMPDIR:-/tmp}/goos-compat-wait.XXXXXX")
    exec go run ./tools/rootlock exclusive "$root" -- env GOOS_COMPAT_MARKER="$marker" sh "$0" "$@"
fi
marker=${GOOS_COMPAT_MARKER:-}
if [ -n "$marker" ] && [ -f "$root/.stamp" ] && [ "$root/.stamp" -nt "$marker" ]; then
    rm -f "$marker"
    echo "prepare-compat: $root was published while waiting"
    exit 0
fi
rm -f "$marker"
# Only the lock holder makes stages, so any stage left here is stale.
for candidate in "${root}".stage.*; do
    [ -e "$candidate" ] || continue
    rm -rf "$candidate"
done
stage=$(mktemp -d "${root}.stage.XXXXXX")
trap 'rm -rf "$stage"' EXIT HUP INT TERM
source_tinygo=$(tools/tinygo.sh env TINYGOROOT)
mkdir "$stage/tinygo"
go run ./tools/cryptocompat "$source_tinygo" "$stage/tinygo" tinygo/runtime/native.go.txt
if [ "$kind" != crypto ]; then
    source_go=$(go env GOROOT)
    mkdir "$stage/go"
    go run ./tools/tlscompat "$source_go" "$stage/tinygo" "$stage/go" tinygo/runtime/sysrand.go.txt tinygo/runtime/tls-cache.go.txt
fi
if [ "$kind" = ssh ]; then
    source_crypto=$(go mod download -json golang.org/x/crypto@v0.53.0 | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",/\1/p')
    [ -n "$source_crypto" ] || { echo 'missing pinned x/crypto source' >&2; exit 1; }
    mkdir "$stage/crypto"
    go run ./tools/sshcompat "$source_crypto" "$stage/crypto" tools/sshcompat
    go run ./tools/sshcompat --native-key-cache "$source_go" "$stage/go" tools/sshcompat/keycache.go.txt
    cp go.mod "$stage/go.mod"
    cp go.sum "$stage/go.sum"
    go mod edit -modfile="$stage/go.mod" -replace="golang.org/x/crypto=$root/crypto"
else
    printf 'module goos-compat-%s\n' "$kind" > "$stage/go.mod"
fi
touch "$stage/.stamp"
rm -rf "$cache/tinygo-home-$kind/Library/Caches/tinygo"/goroot-* "$cache/tinygo-home-$kind/cache/tinygo"/goroot-*
go run ./tools/rootlock publish "$stage" "$root"
