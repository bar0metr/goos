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
go build -o "$work/allocreport" ./tools/allocreport || fail 'cannot build tools/allocreport'
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
# The report needs a VFS allocator profile taken after the shell rounds. The
# service prints one every 16384 requests and its request count every 1024
# (userspace/cmd/vfs/memory.go), so how many requests the runs above made
# decides whether one followed them. If none did, drive just enough requests
# to pass the next multiple of 16384, from the last count it reported, which
# trails its true count. memreclaim service makes three requests per count.
# Only a complete profile counts: tools/allocreport count checks that every
# line its end line announces arrived and that every counter is there, so a
# profile that lost a line to the console is not taken. A line printed as the
# shell shows its prompt follows "goos> ", so no pattern is anchored.
vfs_period=16384
vfs_profiles() { "$work/allocreport" count -sizes -name vfs "$log"; }
profiles_before=$(vfs_profiles)
attempt=1
while [ "$(vfs_profiles)" -le "$profiles_before" ]; do
	[ "$attempt" -le 3 ] || fail "no VFS allocator profile after the shell rounds in $((attempt - 1)) extra runs"
	served=$(tr -d '\r' <"$log" | sed -n 's/.*VFS_MEMORY requests=\([0-9][0-9]*\).*/\1/p' | tail -n 1)
	[ -n "$served" ] || fail 'the VFS service reported no request count'
	needed=$(((served / vfs_period + 1) * vfs_period - served))
	count=$((needed / 3 + 2))
	[ "$count" -ge 100 ] || count=100
	check "memreclaim service $count" '^MEMRECLAIM_SERVICE_OK$'
	attempt=$((attempt + 1))
done
tr -d '\r' <"$log" | grep -o 'VFS_MEMORY .*\|ALLOC_PROFILE vfs .*' >>"$work/all"

{
	printf 'ALLOCATOR_RUN label=%s revision=%s memory=%s\n' "$label" "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" "$memory"
	grep '^ALLOC_PROFILE \|^MEMRECLAIM_\|^VFS_MEMORY ' "$work/all"
	cat "$work/rounds"
} >"$report"
[ "$("$work/allocreport" count -sizes -name vfs "$report")" -ge 1 ] || fail "no complete VFS allocator profile in $report"
printf 'PASS allocator profile: %s\n' "$report"
