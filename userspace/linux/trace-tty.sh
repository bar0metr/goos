#!/bin/sh
# Traces static x86_64 terminal programs (GNU nano, BusyBox sh) on a real
# Linux PTY with scripted keystrokes. The programs run in an amd64 container
# under qemu-user, which prints every guest syscall when QEMU_STRACE is set.
# The PTY, termios, process groups and job-control signals are the real
# Linux kernel of the container host.
#
# The binaries are piped into the container as a tar stream on stdin, so
# nothing needs to be shared with the container VM. The traces come back
# the same way and land in OUTDIR, one directory per scenario:
#
#   strace.<pid>.log   qemu-user syscall trace, one file per process image
#   screen.raw         every byte the program wrote to the terminal
#   events.json        keystrokes, resizes, signals, termios snapshots
#
# usage: userspace/linux/trace-tty.sh NANO BUSYBOX OUTDIR [scenario ...]
#
# TERMINFO_DB, when set, names a compiled terminfo tree that is copied in for
# the nano-term-goos scenario (default: userspace/linux/terminfo/db).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
nano=${1:?path to a static x86_64 nano required}
busybox=${2:?path to a static x86_64 BusyBox required}
out=${3:?output directory required}
shift 3
image=goos-tty-trace
db=${TERMINFO_DB:-$here/terminfo/db}

if ! docker image inspect "$image" >/dev/null 2>&1; then
	docker build --platform linux/amd64 -t "$image" - <<'DOCKERFILE'
FROM alpine:3.22
RUN apk add --no-cache python3 ncurses ncurses-terminfo-base
DOCKERFILE
fi

stage=$(mktemp -d "${TMPDIR:-/tmp}/goos-tty-trace.XXXXXX")
trap 'rm -rf "$stage"' EXIT
mkdir -p "$stage/bin"
cp "$nano" "$stage/bin/nano"
cp "$busybox" "$stage/bin/busybox"
chmod 755 "$stage/bin/nano" "$stage/bin/busybox"
for applet in sh sleep cat ls stty; do
	ln -s busybox "$stage/bin/$applet"
done
cp "$here/tty-trace/driver.py" "$stage/driver.py"
if [ -d "$db" ]; then
	cp -R "$db" "$stage/terminfo"
fi

mkdir -p "$out"
tar -C "$stage" -cf - . |
	docker run --rm -i --platform linux/amd64 "$image" sh -c '
		mkdir -p /work /out && tar -C /work -xf - &&
		{ python3 -I /work/driver.py "$@" >&2; tar -C /out -cf - .; }' sh "$@" |
	tar -C "$out" -xf -
echo "traces in $out"
