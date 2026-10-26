#!/bin/sh
# Boots one validation guest with several processors and checks native
# threads (ADR 0203): threads of one process run on several processors at
# once, IDs, joins, exit codes and the task quota behave, threads allocate
# and collect on one heap without losing an object, the roots of threads
# that run, sleep, or allocate in a region survive another's collection,
# and a fault in a thread ends only its process. Writes build/reports/threads.txt.
#
# SUITE picks the checks: native (threads, lifecycle, faults), gc (roots and
# collections, region transitions held during collections included), region
# (only the region transitions), allocator (thread-owned slabs), or all, the
# default. SUITE=bench
# runs no check: it times allocation with the heap lock alone and with
# thread-owned slabs on 1, 2, 4, and 8 threads, BENCH_RUNS times each with
# BENCH_OPS allocations per thread, and writes
# build/reports/thread-allocator.md.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
cpus=${SMP:-4}
suite=${SUITE:-all}
log=build/logs/threads${SUITE:+-$SUITE}.log
report=build/reports/threads${SUITE:+-$SUITE}.txt
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

# field NAME LINE-PATTERN prints the last value of NAME=... on the lines
# matching LINE-PATTERN in the last command's output: a profile wraps one
# record over several lines.
field() {
	grep "$2" "$work/output" | tr ' ' '\n' | sed -n "s/^$1=//p" | tail -n 1
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


if wants native; then
	# One process's threads on several processors at once.
	for count in 2 4 7; do
		run "threadprobe smoke $count"
		require '^THREADPROBE_SMOKE_OK$' "smoke with $count threads"
		grep '^THREADPROBE_SMOKE ' "$work/output" >>"$work/summary"
	done

	run 'threadprobe lifecycle'
	require '^THREADPROBE_LIFECYCLE_OK$' 'thread lifecycle'
	grep '^THREADPROBE_LIFECYCLE ' "$work/output" >>"$work/summary"

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
fi

if wants gc; then
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
fi

if wants gc || [ "$suite" = region ]; then
	# A thread stopped inside region Enter and Leave while the main thread
	# or another worker collects: every collection gives up until it is
	# released, the next one runs, and no list is lost.
	for check in 1:16 4:24; do
		run "threadprobe region-window ${check%:*} ${check#*:}"
		require '^THREADPROBE_REGION_WINDOW_OK rounds=' "region transitions held during collections with ${check%:*} threads"
		grep '^THREADPROBE_REGION_WINDOW[_ ]' "$work/output" >>"$work/summary"
	done
fi

if [ "$suite" = bench ]; then
	runs=${BENCH_RUNS:-3}
	ops=${BENCH_OPS:-50000}
	for threads in 1 2 4 8; do
		for mode in lock cache big; do
			run=0
			while [ "$run" -lt "$runs" ]; do
				run "threadprobe bench $threads $ops $mode"
				require '^THREADPROBE_BENCH_OK$' "bench $threads $mode"
				printf '%s %s %s %s %s %s %s %s\n' "$threads" "$mode" "$(field ns '^THREADPROBE_BENCH ')" \
					"$(field heap_contended '^ALLOC_PROFILE threadprobe threads ')" \
					"$(field refills '^ALLOC_PROFILE threadprobe threads ')" \
					"$(field pause_max '^ALLOC_PROFILE threadprobe threads ')" \
					"$(field pause_total '^ALLOC_PROFILE threadprobe threads ')" \
					"$(field collections '^THREADPROBE_BENCH ')" >>"$work/bench"
				run=$((run + 1))
			done
		done
	done
	# The median run of each thread count and mode: throughput, scaling
	# against one thread of the same mode, and the shared-heap and
	# collection work behind it.
	{
		printf '# Thread allocator benchmark\n\n'
		printf '%s processors, %s allocations per thread, median of %s runs. ' "$cpus" "$ops" "$runs"
		printf 'Accelerator: %s.\n\n' "${ACCEL:-tcg}"
		printf '| Mode | Threads | ms | Mallocs/s | ns/alloc | Scaling | Collections | Contended | Refills/kalloc | Pause share | Longest pause us |\n'
		printf '|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n'
		for mode in lock cache big; do
			one=
			for threads in 1 2 4 8; do
				median=$(awk -v t="$threads" -v m="$mode" '$1 == t && $2 == m' "$work/bench" | sort -k3 -n | awk '{a[NR]=$0} END {print a[int((NR+1)/2)]}')
				[ -n "$one" ] || one=$(echo "$median" | awk '{print $3}')
				echo "$median $ops $one" | awk '{
					total = $1 * $9; rate = total / $3 * 1e9; single = $9 / $10 * 1e9
					printf "| %s | %d | %.1f | %.0f | %.0f | %.0f%% | %d | %d | %.2f | %.1f%% | %.0f |\n", $2, $1, $3 / 1e6,
						rate, $3 / total, rate / ($1 * single) * 100, $8, $4, $5 * 1000 / total, $7 / $3 * 100, $6 / 1e3
				}'
			done
		done
	} >"$work/bench.md"
	mkdir -p build/reports
	cp "$work/bench.md" build/reports/thread-allocator.md
	cat "$work/bench" >>"$work/summary"
fi

if wants allocator; then
	# With the heap lock alone no allocation comes from an owned slab; with
	# thread caches nearly all do, and the profile is complete.
	for mode in lock cache big; do
		run "threadprobe bench 4 20000 $mode"
		require '^THREADPROBE_BENCH_OK$' "allocation with $mode"
		require '^ALLOC_PROFILE threadprobe threads ' "the $mode profile has its threads line"
		require '^ALLOC_PROFILE threadprobe gc ' "the $mode profile has its gc line"
		require '^ALLOC_PROFILE threadprobe end lines=' "the $mode profile ended"
		local_allocs=$(field local_allocs '^ALLOC_PROFILE threadprobe threads ')
		refills=$(field refills '^ALLOC_PROFILE threadprobe threads ')
		ops=$(field ops '^THREADPROBE_BENCH ')
		if [ "$mode" = lock ]; then
			[ "$local_allocs" -eq 0 ] || fail "the heap lock alone allocated $local_allocs from owned slabs"
		else
			[ "$local_allocs" -ge $((ops * 9 / 10)) ] || fail "thread caches served $local_allocs of $ops allocations"
			[ "$refills" -gt 0 ] && [ "$refills" -lt $((ops / 8)) ] || fail "thread caches refilled $refills times for $ops allocations"
			# Several threads allocated, so the goal counted them and an
			# automatic collection kept their slabs.
			allocators=$(field max_allocators '^ALLOC_PROFILE threadprobe gc ')
			[ "$allocators" -ge 2 ] || fail "the goal counted $allocators allocating threads"
			preserved=$(field preserved '^ALLOC_PROFILE threadprobe cache ')
			[ "$preserved" -gt 0 ] || fail 'no automatic collection kept a thread cache'
		fi
		if [ "$mode" = big ]; then
			batches=$(field batches '^ALLOC_PROFILE threadprobe cache ')
			slabs=$(field batch_slabs '^ALLOC_PROFILE threadprobe cache ')
			[ "$slabs" -gt "$batches" ] || fail "refills of large classes took $slabs slabs in $batches batches"
		fi
		grep '^THREADPROBE_BENCH \|^ALLOC_PROFILE threadprobe threads \|^ALLOC_PROFILE threadprobe cache \|^ALLOC_PROFILE threadprobe gc ' "$work/output" >>"$work/summary"
	done
	run 'threadprobe gc 7 40'
	require '^THREADPROBE_GC_OK$' 'collection over owned slabs'
	run 'threadprobe storm 7 20'
	require '^THREADPROBE_STORM_OK$' 'collection requests over owned slabs'

	# Objects freed by the other thread of a pair, one way and both ways,
	# across automatic collections: none is handed out twice.
	for check in 2:one 4:two; do
		run "threadprobe remote ${check%:*} 20000 ${check#*:}"
		require '^THREADPROBE_REMOTE_OK$' "remote frees ${check#*:} way on ${check%:*} threads"
		require '^THREADPROBE_REMOTE_CHECK bad=0$' "remote frees ${check#*:} way kept every tag"
		remote_frees=$(field remote_frees '^ALLOC_PROFILE threadprobe threads ')
		[ "$remote_frees" -gt 0 ] || fail "no remote free in the ${check#*:} way run"
		grep '^THREADPROBE_REMOTE ' "$work/output" >>"$work/summary"
	done

	# Most of the heap stays live: the goal is twice the live heap, not
	# the floor the allocating threads give.
	run 'threadprobe live 4 40000 8192'
	require '^THREADPROBE_LIVE_OK$' 'high-live allocation'
	live=$(field live '^ALLOC_PROFILE threadprobe gc ')
	goal=$(field goal '^ALLOC_PROFILE threadprobe gc ')
	floor=$(field floor '^ALLOC_PROFILE threadprobe gc ')
	[ "$live" -gt "$floor" ] || fail "the live heap $live stayed under the floor $floor"
	[ "$goal" -ge $((live * 2)) ] || fail "goal $goal under twice the live heap $live"
	grep '^THREADPROBE_LIVE \|^ALLOC_PROFILE threadprobe gc ' "$work/output" >>"$work/summary"

	# Bursts on four threads, each followed by an idle wait on all of
	# them: idle collections run, and the process's pages do not climb.
	run 'threadprobe idle 4 8'
	require '^THREADPROBE_IDLE_OK$' 'bursts and idle waits'
	idle=$(field idle '^ALLOC_PROFILE threadprobe gc ')
	[ "$idle" -gt 0 ] || fail 'no idle collection ran while every thread waited'
	grep '^THREADPROBE_IDLE round=' "$work/output" | tr ' ' '\n' | sed -n 's/^pages=//p' >"$work/pages"
	first=$(head -n 3 "$work/pages" | sort -n | tail -n 1)
	last=$(tail -n 3 "$work/pages" | sort -n | tail -n 1)
	[ "$last" -le $((first + 64)) ] || fail "pages climbed from $first to $last over the idle rounds"
	grep '^THREADPROBE_IDLE' "$work/output" >>"$work/summary"

	# Four threads hold a third of the page quota, drop it, and take it
	# again: the collector keeps the process inside its quota.
	run 'threadprobe quota 4 3'
	require '^THREADPROBE_QUOTA_OK$' 'allocation near the page quota'
	grep '^THREADPROBE_QUOTA' "$work/output" >>"$work/summary"
fi

run 'echo after-threads'
require '^after-threads$' 'the system carries on after the thread checks'

{
	printf 'processors: %s suite: %s\n' "$cpus" "$suite"
	cat "$work/summary"
} >"$report"
cat "$report"
printf 'PASS threads %s\n' "$suite"
