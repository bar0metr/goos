#!/bin/sh
# Boots one four-vCPU guest and validates native lscpu: every processor the
# firmware lists has started and runs tasks.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
log=build/logs/lscpu.log
command_timeout=${COMMAND_TIMEOUT:-30}
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-lscpu.XXXXXX")
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
	sed -n '1,160p' "$work/output" >&2 2>/dev/null || true
	exit 1
}

prompts() { grep -c '^goos> ' "$log" 2>/dev/null || true; }

await_prompt() {
	want=$1
	elapsed=0
	while [ "$(prompts)" -lt "$want" ]; do
		grep -q "$fatal" "$log" 2>/dev/null && fail 'fatal marker'
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "stall waiting for prompt $want"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
}

run() {
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$1" >&3
	await_prompt "$next"
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi \
	-smp 4,sockets=1,cores=2,threads=2 \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	grep -q "$fatal" "$log" 2>/dev/null && fail 'fatal marker during boot'
	[ "$elapsed" -lt 1200 ] || fail 'boot timeout'
	sleep 0.1
	elapsed=$((elapsed + 1))
done

run lscpu
grep -q '^Architecture:             x86_64$' "$work/output" || fail 'architecture'
grep -q '^Byte Order:               Little Endian$' "$work/output" || fail 'byte order'
grep -q '^CPU(s):                   4$' "$work/output" || fail 'CPU count'
grep -q '^On-line CPU(s) list:      0-3$' "$work/output" || fail 'online list'
! grep -q '^Off-line CPU(s) list:' "$work/output" || fail 'offline list'
grep -q '^Thread(s) per core:       2$' "$work/output" || fail 'threads per core'
grep -q '^Core(s) per socket:       2$' "$work/output" || fail 'cores per socket'
grep -q '^Socket(s):                1$' "$work/output" || fail 'socket count'
grep -q '^Vendor ID:' "$work/output" || fail 'vendor'
grep -q '^Flags:' "$work/output" || fail 'flags'

run 'lscpu -e=CPU,ONLINE,SOCKET,CORE,THREAD,APICID'
grep -q '^CPU  ONLINE  SOCKET  CORE  THREAD  APICID$' "$work/output" || fail 'extended header'

run 'lscpu -J'
grep -q '^{"lscpu":\[' "$work/output" || fail 'JSON output'

printf 'PASS %s\n' "$log"
