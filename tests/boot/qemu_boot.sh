#!/bin/sh
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
log=build/logs/boot.log
mkdir -p build/logs
rm -f "$log"

qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-cdrom "$iso" -display none -serial "file:$log" -no-reboot -no-shutdown &
pid=$!

elapsed=0
while [ "$elapsed" -lt 20 ]; do
	if grep -q 'BOOT_OK' "$log" 2>/dev/null; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		printf 'boot test passed\n'
		exit 0
	fi
	sleep 1
	elapsed=$((elapsed + 1))
done

kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
printf 'boot test timed out; serial output follows:\n' >&2
cat "$log" >&2
exit 1
