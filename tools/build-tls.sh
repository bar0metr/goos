#!/bin/sh
set -eu
output=${1:?output image ELF required}
package=${2:?package required}
tags=${3:?build tags required}
opt=${4:-2}
root=${COMPAT_ROOT:-$(pwd)/build/toolchains/tls}
if [ -z "${COMPAT_ROOT:-}" ]; then
    make -j4 build/toolchains/tls/.stamp
fi
GOROOT="$root/go" TINYGOROOT="$root/tinygo" TINYGO_CACHE_NAMESPACE=tls tools/tinygo.sh build -tags "$tags" -target=tinygo/targets/goos-userspace-amd64.json -opt="$opt" -no-debug -o "$output" "$package"
