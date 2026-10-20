#!/bin/sh
# Boots one validation guest with several processors and checks goroutines
# (ADR 0204) with goprobe: the worker pool, goroutines on several
# processors at once, channels and select, the sync package, timers and
# contexts, fairness on one permit, panics, the goroutine limit, the roots
# of goroutines in every waiting state, handle waits, thread.Start threads
# with goroutines, and the faults that end the process. Writes
# build/reports/goroutines-SUITE.txt.
#
# SUITE picks the checks: runtime (scheduling, channels, sync, timers,
# fairness, panics, limit, threads), gc, io, faults, or all, the default.
# SUITE=bench runs goprobe bench BENCH_RUNS times and writes
# build/reports/goroutines-bench.txt. CHECKS, when set, replaces the suite
# with a space-separated list of goprobe modes, for a quick look at one.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
cpus=${SMP:-4}
suite=${SUITE:-all}
log=build/logs/goroutines-$suite.log
report=build/reports/goroutines-$suite.txt
command_timeout=${COMMAND_TIMEOUT:-240}
fatal='^PANIC\|^\[PANIC\]\|INIT_EXEC_.*FAILED\|VFS_OUT_OF_MEMORY\|SHELL_OUT_OF_MEMORY'

mkdir -p build/logs build/reports
rm -f "$log" "$report"
work=$(go run ./tools/scratch goos-goroutines.)
fifo=$work/in
mkfifo "$fifo"
pid=
: >"$work/summary"

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

# probe MODE checks one goprobe mode that ends with GOPROBE_<NAME>_OK.
probe() {
	run "goprobe $1"
	name=$(printf '%s' "${1%% *}" | tr '[:lower:]' '[:upper:]')
	case $name in
	GC) marker=GC ;;
	*) marker=$name ;;
	esac
	! grep -q '^GOPROBE_FAIL' "$work/output" || fail "goprobe $1: $(grep '^GOPROBE_FAIL' "$work/output")"
	require "^GOPROBE_${marker}_OK\$" "goprobe $1"
	grep '^GOPROBE_' "$work/output" | grep -v '_OK$' >>"$work/summary"
}

wants() { [ "$suite" = all ] || [ "$suite" = "$1" ]; }

qemu-system-x86_64 -machine q35 -m "$memory" -accel "${ACCEL:-tcg,thread=multi}" -smp "$cpus" \
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

if [ -n "${CHECKS:-}" ]; then
	for check in $CHECKS; do
		probe "$(printf '%s' "$check" | tr ':' ' ')"
	done
else
	if wants runtime; then
		probe pool
		counts=1
		if [ "$cpus" -ge 2 ]; then counts="1 2"; fi
		if [ "$cpus" -gt 2 ]; then counts="$counts $cpus"; fi
		for count in $counts; do
			probe "smoke $count"
		done
		probe chan
		probe sync
		probe timers
		probe fair
		probe panic
		probe limit
		probe threads
	fi
	if wants gc; then
		for count in 1 $cpus; do
			probe "gc $count"
		done
	fi
	if wants io; then
		probe io
		probe waitsets
	fi
	if wants faults; then
		# A goroutine that overflows its stack hits the guard below it and
		# ends its process; every goroutine asleep ends the process with 2.
		run 'goprobe overflow'
		require '^GOPROBE_OVERFLOW_STARTED$' 'overflow goroutine started'
		require 'user fault: guard page' 'the overflow was reported'
		! grep -q '^GOPROBE_OVERFLOW_SURVIVED$' "$work/output" || fail 'the process outlived its overflow'
		run 'goprobe deadlock'
		require '^GOPROBE_DEADLOCK_STARTED$' 'deadlock check started'
		require 'all goroutines are asleep - deadlock' 'the deadlock was reported'
		printf 'faults: overflow and deadlock ended their processes\n' >>"$work/summary"
		# The system and goroutines carry on.
		probe pool
	fi
	if [ "$suite" = bench ]; then
		runs=${BENCH_RUNS:-3}
		count=0
		while [ "$count" -lt "$runs" ]; do
			probe bench
			count=$((count + 1))
		done
	fi
fi

if [ "${LEGACY_THREADS:-0}" = 1 ]; then
	for check in 'smoke 3:SMOKE' 'lifecycle:LIFECYCLE' 'gc 3 10:GC' 'roots:ROOTS' 'storm 3 10:STORM'; do
		run "threadprobe ${check%:*}"
		require "^THREADPROBE_${check##*:}_OK\$" "legacy threadprobe ${check%:*}"
		grep '^THREADPROBE_' "$work/output" >>"$work/summary"
	done
fi

run 'echo after-goroutines'
require '^after-goroutines$' 'the system carries on after the goroutine checks'

{
	printf 'processors: %s suite: %s accelerator: %s\n' "$cpus" "$suite" "${ACCEL:-tcg}"
	cat "$work/summary"
} >"$report"
cat "$report"
printf 'PASS goroutines %s\n' "$suite"
