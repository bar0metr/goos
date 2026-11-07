#!/bin/sh
set -eu
root=${COMPAT_ROOT:-$(pwd)/build/toolchains/ssh}
# Builds and host tests read the root under its shared lock (tools/rootlock).
if [ "${GOOS_ROOT_LOCK:-}" != "shared $root" ]; then
    if [ -z "${COMPAT_ROOT:-}" ]; then
        make -j4 build/toolchains/ssh/.stamp
    fi
    exec go run ./tools/rootlock shared -stamp .stamp "$root" -- sh "$0" "$@"
fi
if [ "${1:-}" = --host-test ]; then
    source_crypto=$(go mod download -json golang.org/x/crypto@v0.53.0 | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",/\1/p')
    [ -n "$source_crypto" ] || { echo 'missing pinned x/crypto source' >&2; exit 1; }
    SSHCOMPAT_TEST_SOURCE="$source_crypto" go test ./tools/sshcompat ./userspace/runtime/arena
    go test -modfile="$root/go.mod" -tags=goos_sshcompat ./userspace/sshserver
    go test -modfile="$root/go.mod" golang.org/x/crypto/ssh -run GoOSPump -count=1 -timeout=60s
else
    output=${1:?output image ELF required}
    package=${2:?package required}
    tags=${3:?build tags required}
    tags=$(printf '%s' "$tags" | tr ',' ' ')
    opt=${4:-2}
    GOROOT="$root/go" TINYGOROOT="$root/tinygo" GOFLAGS="-modfile=$root/go.mod" TINYGO_CACHE_NAMESPACE=ssh tools/tinygo.sh build -tags "$tags goos_sshcompat" -target=tinygo/targets/goos-userspace-amd64.json -opt="$opt" -no-debug -o "$output" "$package"
fi
