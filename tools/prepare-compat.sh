#!/bin/sh
set -eu
kind=${1:?compatibility kind required}
root=${2:?compatibility root required}
root=$(cd "$(dirname "$root")" && pwd)/$(basename "$root")
cache=${3:?cache directory required}
for candidate in "${root}".stage.*; do
    [ -d "$candidate" ] || continue
    pid=${candidate##*.stage.}
    pid=${pid%%.*}
    case "$pid" in *[!0-9]*|'') continue ;; esac
    if ! kill -0 "$pid" 2>/dev/null; then rm -rf "$candidate"; fi
done
stage=$(mktemp -d "${root}.stage.$$.XXXXXX")
old="${root}.old.$$"
cleanup() {
    rm -rf "$stage"
    if [ -d "$old" ] && [ ! -e "$root" ]; then mv "$old" "$root"; fi
    rm -rf "$old"
}
trap cleanup EXIT HUP INT TERM
source_go=$(go env GOROOT)
source_tinygo=$(tools/tinygo.sh env TINYGOROOT)
mkdir "$stage/tinygo" "$stage/go"
go run ./tools/cryptocompat "$source_tinygo" "$stage/tinygo" tinygo/runtime/native.go.txt
go run ./tools/tlscompat "$source_go" "$stage/tinygo" "$stage/go" tinygo/runtime/sysrand.go.txt tinygo/runtime/tls-cache.go.txt
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
    printf 'module goos-compat-tls\n' > "$stage/go.mod"
fi
touch "$stage/.stamp"
rm -rf "$cache/tinygo-home-$kind/Library/Caches/tinygo"/goroot-* "$cache/tinygo-home-$kind/cache/tinygo"/goroot-*
if [ -e "$root" ]; then mv "$root" "$old"; fi
mv "$stage" "$root"
