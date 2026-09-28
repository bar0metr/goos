#!/bin/sh
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?test disk path required}
memory=${MEMORY:-512M}
log=build/logs/console-gui.log
dump=build/reports/console-gui.ppm
monitor=${TMPDIR:-/tmp}/goos-console-gui-monitor.$$
pid=

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -f "$monitor"
}

fail() {
	printf 'FAIL console-gui: %s\n' "$1" >&2
	tail -n 40 "$log" >&2 2>/dev/null || true
	exit 1
}

trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM
mkdir -p build/logs build/reports
rm -f "$log" "$dump" "$monitor"

qemu-system-x86_64 -machine q35 -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-device VGA -display none -serial "file:$log" \
	-monitor "unix:$monitor,server,nowait" -no-reboot -cdrom "$iso" &
pid=$!

elapsed=0
while [ ! -S "$monitor" ] && [ "$elapsed" -lt 100 ]; do
	kill -0 "$pid" 2>/dev/null || fail "QEMU exited before monitor startup"
	sleep 0.1
	elapsed=$((elapsed + 1))
done
[ -S "$monitor" ] || fail "QEMU monitor socket was not created"

elapsed=0
while ! grep -q 'FRAMEBUFFER_VALIDATION_PATTERN_OK' "$log" 2>/dev/null; do
	kill -0 "$pid" 2>/dev/null || fail "QEMU exited before framebuffer marker"
	[ "$elapsed" -lt 90 ] || fail "framebuffer marker timeout"
	sleep 1
	elapsed=$((elapsed + 1))
done

(printf 'stop\nscreendump %s\n' "$dump"; sleep 0.2) | nc -U "$monitor" >/dev/null 2>&1 || fail "QEMU screendump command failed"
elapsed=0
while [ ! -s "$dump" ] && [ "$elapsed" -lt 50 ]; do
	sleep 0.1
	elapsed=$((elapsed + 1))
done
[ -s "$dump" ] || fail "QEMU did not create the screendump"

GOTOOLCHAIN=local GOCACHE="$(pwd)/build/cache/go" XDG_CACHE_HOME="$(pwd)/build/cache" \
	go run ./tools/ppmcheck "$dump" || fail "validation pixels did not match"
printf 'PASS console-gui\n'
