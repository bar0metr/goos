#!/bin/sh
# Drives one QEMU guest through repeated command rounds, waiting for each
# prompt instead of sleeping, and fails on a stall or a fatal service marker.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
log=${4:?log path required}
rounds=${5:-20}
command_timeout=${COMMAND_TIMEOUT:-30}
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW'

work=$(mktemp -d "${TMPDIR:-/tmp}/goos-stress.XXXXXX")
fifo=$work/in
mkfifo "$fifo"
pid=

cleanup() {
	exec 3>&- 2>/dev/null || true
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

fail() {
	printf 'FAIL %s: %s\n' "$log" "$1" >&2
	grep "$fatal" "$log" 2>/dev/null | sed -n '1,5p' >&2 || true
	printf 'DIAG_FAILURE %s\n' "$1" >>"$log"
	printf 'diag\n' >&3 2>/dev/null || true
	sleep 2
	exit 1
}

prompts() { grep -c '^goos> ' "$log" 2>/dev/null || true; }

await_prompt() {
	want=$1
	elapsed=0
	while [ "$(prompts)" -lt "$want" ]; do
		grep -q "$fatal" "$log" 2>/dev/null && fail "fatal marker"
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "stall waiting for prompt $want"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
	grep -q "$fatal" "$log" 2>/dev/null && fail "fatal marker"
	return 0
}

run() {
	next=$(($(prompts) + 1))
	printf '%s\n' "$1" >&3
	await_prompt "$next"
}

qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

elapsed=0
until grep -q SHELL_VFS_CONNECTED_OK "$log" 2>/dev/null && [ "$(prompts)" -ge 1 ]; do
	grep -q "$fatal" "$log" 2>/dev/null && fail "fatal marker during boot"
	[ "$elapsed" -lt 1200 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done

round=0
while [ "$round" -lt "$rounds" ]; do
	run "cat /system/motd"
	run "ls /persist"
	run "echo stress-$round > /persist/stress.txt"
	run "cat /persist/stress.txt"
	run "echo pipe-$round | readline | readline"
	run "missing-stage | readline | readline"
	run "mkdir /tmp/r$round"
	run "touch /tmp/r$round/f; stat /tmp/r$round/f; rm /tmp/r$round/f"
	run "uname; pwd; echo seq-$round"
	grep -q "^stress-$round\$" "$log" || fail "persistent read-back of round $round"
	grep -q "^pipe-$round\$" "$log" || fail "pipeline output of round $round"
	round=$((round + 1))
done
run "echo stress-complete"
printf 'PASS %s (%s rounds)\n' "$log" "$rounds"
