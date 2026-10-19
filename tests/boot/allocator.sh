#!/bin/sh
# Boots one validation guest and records what the native allocator does under
# representative work, for the allocator report (tools/allocreport). It is a
# measurement, not a gate: the checks of the reclaimable heap are
# tests/boot/memory.sh. The guest runs memreclaim's checks, a VFS request run,
# a short burst followed by idle time, a timed mixed workload, and rounds of
# shell work, and prints the allocator profile of each process that reports
# one. Writes build/reports/allocator-guest-$LABEL.txt.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
label=${LABEL:-current}
memory=${MEMORY:-512M}
requests=${SERVICE_REQUESTS:-3000}
workload=${WORKLOAD_ROUNDS:-2000}
rounds=${ROUNDS:-6}
log=build/logs/allocator-$label.log
report=build/reports/allocator-guest-$label.txt
command_timeout=${COMMAND_TIMEOUT:-240}
fatal='PANIC\|INIT_EXEC_.*FAILED\|VFS_OUT_OF_MEMORY\|SHELL_OUT_OF_MEMORY'

mkdir -p build/logs build/reports
rm -f "$log" "$report"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-allocator.XXXXXX")
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
	sed -n '1,200p' "$work/output" >&2 2>/dev/null || true
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

# check runs a command and requires its success marker, so a measurement is
# never taken from a command that failed.
check() {
	run "$1"
	grep -q "$2" "$work/output" || fail "$1: no $2"
	# The VFS lines are taken from the whole log at the end.
	grep -v '^VFS_MEMORY \|^ALLOC_PROFILE vfs ' "$work/output" >>"$work/all" || true
}

qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi -smp 1 \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"
: >"$work/all"
: >"$work/rounds"

elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	grep -q "$fatal" "$log" 2>/dev/null && fail 'fatal marker during boot'
	[ "$elapsed" -lt 1200 ] || fail 'boot timeout'
	sleep 0.1
	elapsed=$((elapsed + 1))
done

check memreclaim '^MEMRECLAIM_OK$'
check "memreclaim service $requests" '^MEMRECLAIM_SERVICE_OK$'
check 'memreclaim burst' '^MEMRECLAIM_BURST_OK$'
check "memreclaim workload $workload" '^MEMRECLAIM_WORKLOAD_OK$'
# The same workload with set numbers of free pages retained after each
# collection, each in a fresh process.
for retained in 0 16 64 256; do
	check "memreclaim workload $workload $retained" '^MEMRECLAIM_WORKLOAD_OK$'
done

# Shell work, every step checked (tests/boot/leak_round.sh).
. "$(dirname "$0")/leak_round.sh"
leak_prepare
round=1
while [ "$round" -le "$rounds" ]; do
	leak_round "$round"
	run 'sleep 2'
	run 'mem -p'
	kernel=$(sed -n 's/^pages in the kernel: //p' "$work/output")
	processes=$(sed -n 's/^pages in processes: //p' "$work/output")
	[ -n "$kernel" ] && [ -n "$processes" ] || fail "no page counts in round $round"
	printf 'ALLOCATOR_ROUND round=%s kernel=%s processes=%s\n' "$round" "$kernel" "$processes" | tee -a "$work/rounds"
	round=$((round + 1))
done
# The VFS reports its profile every 16384 requests; the shell rounds above
# add to its count, and this run makes sure one more report follows them.
check "memreclaim service 1500" '^MEMRECLAIM_SERVICE_OK$'
grep -h '^VFS_MEMORY \|^ALLOC_PROFILE vfs ' "$log" | tr -d '\r' >>"$work/all"

{
	printf 'ALLOCATOR_RUN label=%s revision=%s memory=%s\n' "$label" "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" "$memory"
	grep '^ALLOC_PROFILE \|^MEMRECLAIM_\|^VFS_MEMORY ' "$work/all"
	cat "$work/rounds"
} >"$report"
printf 'PASS allocator profile: %s\n' "$report"
