#!/bin/sh
# Runs a static Linux fixture on a real Linux kernel in an amd64 container,
# to compare its results with a GoOS run.
#
# The binary is piped into the container, so no host directory has to be
# shared with the Docker VM. TRACE=1 prints the calls it makes. Under
# user-mode emulation, as on an arm64 host, ptrace is unavailable and strace
# cannot attach; QEMU's own call trace (QEMU_STRACE) takes its place there.
#
# usage: userspace/linux/run-on-linux.sh [fixture] [argument...]
set -eu

here=$(cd "$(dirname "$0")" && pwd)
fixture=${1:-abi-probe}
[ $# -gt 0 ] && shift
[ $# -gt 0 ] || set -- one two
image=${IMAGE:-alpine:3.22}

run="GOOS_LINUX_PROBE=musl"
[ "${TRACE:-0}" = 0 ] || run="$run QEMU_STRACE=1"

docker run --rm -i --platform linux/amd64 "$image" sh -c '
	mkdir -p /system/linux && cd /system/linux &&
	cat > '"$fixture"' && chmod +x '"$fixture"' &&
	'"$run"' /system/linux/'"$fixture"' "$@" 2>&1
	echo "exit=$?"' sh "$@" < "$here/prebuilt/$fixture"
