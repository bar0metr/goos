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

monitor_command() {
	(printf '%s\n' "$1"; sleep 0.1) | nc -U "$monitor" >/dev/null 2>&1
}

send_key() {
	monitor_command "sendkey $1" || fail "QEMU rejected key $1"
}

wait_for_log() {
	pattern=$1
	description=$2
	limit=${3:-90}
	elapsed=0
	while ! grep -q "$pattern" "$log" 2>/dev/null; do
		kill -0 "$pid" 2>/dev/null || fail "QEMU exited before $description"
		[ "$elapsed" -lt "$limit" ] || fail "$description timeout"
		sleep 1
		elapsed=$((elapsed + 1))
	done
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

wait_for_log 'FRAMEBUFFER_VALIDATION_PATTERN_OK' 'framebuffer marker'
wait_for_log '^goos> ' 'shell prompt' 180
wait_for_log '^BOOT_OK' 'completed validation boot' 180

# Validation output can scroll the overlay between BOOT_OK and the next kernel
# service pass. Sample the live display until that bounded pass restores it.
pixels_ok=false
attempt=0
while [ "$attempt" -lt 20 ]; do
	rm -f "$dump"
	monitor_command "screendump $dump" || fail "QEMU screendump command failed"
	elapsed=0
	while [ ! -s "$dump" ] && [ "$elapsed" -lt 50 ]; do
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	[ -s "$dump" ] || fail "QEMU did not create the screendump"
	if GOTOOLCHAIN=local GOCACHE="$(pwd)/build/cache/go" XDG_CACHE_HOME="$(pwd)/build/cache" \
		go run ./tools/ppmcheck "$dump" >/dev/null 2>&1; then
		pixels_ok=true
		break
	fi
	sleep 0.25
	attempt=$((attempt + 1))
done
if [ "$pixels_ok" != true ]; then
	GOTOOLCHAIN=local GOCACHE="$(pwd)/build/cache/go" XDG_CACHE_HOME="$(pwd)/build/cache" \
		go run ./tools/ppmcheck "$dump" || true
	fail "validation pixels did not match"
fi
printf 'PASS console-gui\n'

# Exercise editing and modifier paths before entering the mandatory command.
# COM1 is a file sink, so all input below can only arrive through IRQ1.
send_key shift-x
send_key backspace
for key in d i s c a r d; do
	send_key "$key"
done
send_key ctrl-u
send_key ctrl-l
send_key up
for key in e c h o spc g r a p h i c a l minus k e y b o a r d minus o k; do
	send_key "$key"
done
send_key ret

wait_for_log '^PS2_KEYBOARD_OK' 'PS/2 input marker'
wait_for_log '^graphical-keyboard-ok' 'graphical keyboard command output'
printf 'PASS console-keyboard\n'
