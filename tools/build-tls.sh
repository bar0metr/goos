#!/bin/sh
# Build a native TLS consumer without changing installed Go or TinyGo sources.
set -eu
output=${1:?output image ELF required}
package=${2:?package required}
tags=${3:?build tags required}
opt=${4:-2}
work=$(mktemp -d /Volumes/Data/tmp/goos-tls-build.XXXXXX)
source_go=$(go env GOROOT)
source_tinygo=$(tinygo env TINYGOROOT)
mkdir "$work/tinygo" "$work/go"
go run ./tools/cryptocompat "$source_tinygo" "$work/tinygo" tinygo/runtime/native.go.txt
go run ./tools/tlscompat "$source_go" "$work/tinygo" "$work/go" tinygo/runtime/sysrand.go.txt tinygo/runtime/tls-cache.go.txt
GOROOT="$work/go" TINYGOROOT="$work/tinygo" tinygo build -tags "$tags" -target=tinygo/targets/goos-userspace-amd64.json -opt="$opt" -no-debug -o "$output" "$package"
printf 'native TLS toolchain metadata: %s\n' "$work"
