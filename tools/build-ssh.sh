#!/bin/sh
# Build the pinned scheduler-free SSH backend in private toolchain roots.
set -eu
work=$(go run ./tools/scratch goos-ssh-build.)
source_go=$(go env GOROOT)
source_tinygo=$(tinygo env TINYGOROOT)
# go mod download verifies the zip against go.sum and the Go checksum database.
source_crypto=$(go mod download -json golang.org/x/crypto@v0.53.0 | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",/\1/p')
[ -n "$source_crypto" ] || { echo 'missing pinned x/crypto source' >&2; exit 1; }
mkdir "$work/tinygo" "$work/go" "$work/crypto"
go run ./tools/cryptocompat "$source_tinygo" "$work/tinygo" tinygo/runtime/native.go.txt
go run ./tools/tlscompat "$source_go" "$work/tinygo" "$work/go" tinygo/runtime/sysrand.go.txt tinygo/runtime/tls-cache.go.txt
go run ./tools/sshcompat "$source_crypto" "$work/crypto" tools/sshcompat
go run ./tools/sshcompat --native-key-cache "$source_go" "$work/go" tools/sshcompat/keycache.go.txt
cp go.mod "$work/go.mod"
cp go.sum "$work/go.sum"
go mod edit -modfile="$work/go.mod" -replace="golang.org/x/crypto=$work/crypto"
if [ "${1:-}" = --host-test ]; then
    SSHCOMPAT_TEST_SOURCE="$source_crypto" go test ./tools/sshcompat ./userspace/runtime/arena
    go test -modfile="$work/go.mod" -tags=goos_sshcompat ./userspace/sshserver
    go test -modfile="$work/go.mod" golang.org/x/crypto/ssh -run GoOSPump -count=1 -timeout=60s
else
    output=${1:?output image ELF required}
    package=${2:?package required}
    tags=${3:?build tags required}
    tags=$(printf '%s' "$tags" | tr ',' ' ')
    opt=${4:-2}
    # The replacement belongs only to this invocation, not the repository.
    GOROOT="$work/go" TINYGOROOT="$work/tinygo" GOFLAGS="-modfile=$work/go.mod" tinygo build -tags "$tags goos_sshcompat" -target=tinygo/targets/goos-userspace-amd64.json -opt="$opt" -no-debug -o "$output" "$package"
fi
printf 'native SSH toolchain metadata: %s\n' "$work"
