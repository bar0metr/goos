#!/bin/sh
# Boots one validation guest with several processors and checks native
# threads (ADR 0203): threads of one process run on several processors at
# once, IDs, joins, exit codes and the task quota behave, threads allocate
# and collect on one heap without losing an object, the roots of threads
# that run, sleep, or allocate in a region survive another's collection,
# and a fault in a thread ends only its process. Writes build/reports/threads.txt.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
cpus=${SMP:-4}
log=build/logs/threads.log
report=build/reports/threads.txt
command_timeout=${COMMAND_TIMEOUT:-240}
fatal='PANIC\|INIT_EXEC_.*FAILED\|VFS_OUT_OF_MEMORY\|SHELL_OUT_OF_MEMORY'

mkdir -p build/logs build/reports
rm -f "$log" "$report"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-threads.XXXXXX")
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

require() {
	grep -q "$1" "$work/output" || fail "$2"
}

# field NAME LINE-PATTERN prints the value of NAME=... on the last line
# matching LINE-PATTERN in the last command's output.
field() {
	grep "$2" "$work/output" | tail -n 1 | tr ' ' '\n' | sed -n "s/^$1=//p"
}

qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi -smp "$cpus" \
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


# One process's threads on several processors at once.
for count in 2 4 7; do
	run "threadprobe smoke $count"
	require '^THREADPROBE_SMOKE_OK$' "smoke with $count threads"
	grep '^THREADPROBE_SMOKE ' "$work/output" >>"$work/summary"
done

run 'threadprobe lifecycle'
require '^THREADPROBE_LIFECYCLE_OK$' 'thread lifecycle'
grep '^THREADPROBE_LIFECYCLE ' "$work/output" >>"$work/summary"

for count in 1 3 7; do
	run "threadprobe gc $count 40"
	require '^THREADPROBE_GC_OK$' "collection with $count threads"
	grep '^THREADPROBE_GC ' "$work/output" >>"$work/summary"
done

run 'threadprobe roots'
require '^THREADPROBE_ROOTS_OK$' 'roots of running, sleeping, and region-allocating threads'
grep '^THREADPROBE_ROOTS ' "$work/output" >>"$work/summary"

for count in 2 7; do
	run "threadprobe storm $count 20"
	require '^THREADPROBE_STORM_OK$' "collection requests from $count threads at once"
	grep '^THREADPROBE_STORM ' "$work/output" >>"$work/summary"
done

# A fault in a worker ends its whole process and nothing else.
for mode in fault:'user fault: no mapping' overflow:'user fault: guard page'; do
	run "threadprobe ${mode%%:*}"
	require '^THREADPROBE_CRASH_STARTED$' "${mode%%:*} worker started"
	require "${mode#*:}" "a worker's ${mode%%:*} was reported"
	! grep -q '^THREADPROBE_CRASH_SURVIVED$' "$work/output" || fail "the process outlived its worker's ${mode%%:*}"
	printf '%s: process ended\n' "${mode%%:*}" >>"$work/summary"
done

# The ended processes left their slots and stacks: threads start again.
run 'threadprobe lifecycle'
require '^THREADPROBE_LIFECYCLE_OK$' 'thread lifecycle after the faults'

run 'echo after-threads'
require '^after-threads$' 'the system carries on after the thread checks'

{
	printf 'processors: %s\n' "$cpus"
	cat "$work/summary"
} >"$report"
cat "$report"
printf 'PASS threads\n'
