#!/bin/sh
# Boots one guest and checks the reclaimable native heap (ADR 0201): the
# memreclaim checks, a double free and heap exhaustion that must stop only
# their own process, a burst whose pages must go back once the process is
# idle, a VFS service that must hold its memory steady over
# thousands of requests, and a long leak run of mixed workloads. Writes
# build/reports/memory.txt.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
requests=${SERVICE_REQUESTS:-3000}
log=build/logs/memory.log
report=build/reports/memory.txt
command_timeout=${COMMAND_TIMEOUT:-240}
fatal='PANIC\|INIT_EXEC_.*FAILED\|VFS_OUT_OF_MEMORY\|SHELL_OUT_OF_MEMORY'

mkdir -p build/logs build/reports
rm -f "$log" "$report"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-memory.XXXXXX")
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

qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi -smp 1 \
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

run memreclaim
for marker in LAYOUT CONTAINERS STACK_ROOT GLOBAL_ROOT INTERIOR_ROOT CYCLE DEEP_GRAPH ZERO_REUSE BOUNDARY CLASSES FREE REGION PAGES 1000_CYCLES AUTO_GC; do
	require "^MEMRECLAIM_${marker}_OK\$" "memreclaim $marker"
done
require '^MEMRECLAIM_OK$' 'memreclaim did not finish'
reserved=$(field reserved '^MEMRECLAIM_STATS reserved=')
post_live=$(field live '^MEMRECLAIM_STATS reserved=')
peak_live=$(field peak_live '^MEMRECLAIM_STATS peak_live=')
mallocs=$(field mallocs '^MEMRECLAIM_STATS peak_live=')
frees=$(field frees '^MEMRECLAIM_STATS peak_live=')
collections=$(field collections '^MEMRECLAIM_STATS collections=')
pause=$(field longest_ns '^MEMRECLAIM_PAUSE')
pages_before=$(sed -n 's/^MEMRECLAIM_PAGES_BEFORE=//p' "$work/output")
pages_peak=$(sed -n 's/^MEMRECLAIM_PAGES_PEAK=//p' "$work/output")
pages_after=$(sed -n 's/^MEMRECLAIM_PAGES_AFTER=//p' "$work/output")
auto=$(grep '^MEMRECLAIM_AUTO ' "$work/output")

# A double free stops its process and nothing else.
run 'memreclaim double-free'
require '^MEMRECLAIM_DOUBLE_FREE_ATTEMPT$' 'double free not attempted'
grep -q '^MEMRECLAIM_DOUBLE_FREE_MISSED$' "$work/output" && fail 'double free not detected'

# Exhaustion: the heap collects, retries, and only then reports.
run 'memreclaim oom'
require '^MEMRECLAIM_OOM_OK$' 'out-of-memory path'
oom=$(grep '^MEMRECLAIM_OOM ' "$work/output")

# The VFS service under a long run of requests.
run "memreclaim service $requests"
require '^MEMRECLAIM_SERVICE_OK$' 'VFS memory band'
grep '^VFS_MEMORY ' "$work/output" >"$work/vfs" || fail 'no VFS memory reports'
samples=$(wc -l <"$work/vfs")
[ "$samples" -ge 4 ] || fail "only $samples VFS memory reports"
vfs_collections=$(tail -n 1 "$work/vfs" | tr ' ' '\n' | sed -n 's/^collections=//p')
[ "$vfs_collections" -gt 0 ] || fail 'the VFS service never collected'
# The band is set by the reports after the first two; later ones may not
# exceed it by more than 32 pages or by more than 1 MiB of live heap.
band_pages=$(sed -n '2,3p' "$work/vfs" | tr ' ' '\n' | sed -n 's/^pages=//p' | sort -n | tail -n 1)
band_live=$(sed -n '2,3p' "$work/vfs" | tr ' ' '\n' | sed -n 's/^live=//p' | sort -n | tail -n 1)
last_pages=$(tail -n 1 "$work/vfs" | tr ' ' '\n' | sed -n 's/^pages=//p')
last_live=$(tail -n 1 "$work/vfs" | tr ' ' '\n' | sed -n 's/^live=//p')
[ "$last_pages" -le $((band_pages + 32)) ] || fail "VFS pages grew from $band_pages to $last_pages"
[ "$last_live" -le $((band_live + 1048576)) ] || fail "VFS live heap grew from $band_live to $last_live"
service=$(grep '^MEMRECLAIM_SERVICE ' "$work/output" | tail -n 1)

# A short burst of garbage, then idle time: the pages the burst took must go
# back once the process waits, and stay back while it waits on.
run 'memreclaim burst'
require '^MEMRECLAIM_BURST_OK$' 'idle process gives its burst pages back'
burst=$(grep '^MEMRECLAIM_BURST ' "$work/output")

# A long leak run (ADR 0201). The boot test's leak rounds allow a small band
# for process heaps; this run looks for a slow leak behind that band. Each
# round copies a large file, streams through the VFS, lists a directory into a
# file, runs a pipeline and a Linux program, then waits two seconds, far past
# the 100 ms after which every idle service collects its heap, and counts
# pages. From round 3 the kernel's pages may dip by a page or two when a
# process of the previous command is still being torn down as mem counts, but
# the second half may never exceed the first half's highest; the process
# pages of the second half may not exceed the first half's highest by more
# than 4 pages.
rounds=${LEAK_ROUNDS:-20}
round=1
: >"$work/leak"
while [ "$round" -le "$rounds" ]; do
	run 'cp /system/bin/vmprobe /tmp/leak.bin; rm /tmp/leak.bin'
	run 'bench io /tmp/leak.io 256K; rm /tmp/leak.io'
	run 'ls /system/bin > /tmp/leak.ls; rm /tmp/leak.ls'
	run 'sleep 1 | cat /system/motd | write /tmp/leak.pipe; rm /tmp/leak.pipe'
	run '/system/linux/abi-probe random > /tmp/leak.r; rm /tmp/leak.r'
	run 'sleep 2'
	run 'mem -p'
	kernel=$(sed -n 's/^pages in the kernel: //p' "$work/output")
	processes=$(sed -n 's/^pages in processes: //p' "$work/output")
	[ -n "$kernel" ] && [ -n "$processes" ] || fail "no page counts in round $round"
	printf '%s %s %s\n' "$round" "$kernel" "$processes" >>"$work/leak"
	round=$((round + 1))
done
half=$(( (rounds + 3) / 2 ))
kernel_first=$(awk -v h="$half" '$1 >= 3 && $1 < h { print $2 }' "$work/leak" | sort -n | tail -n 1)
kernel_second=$(awk -v h="$half" '$1 >= h { print $2 }' "$work/leak" | sort -n | tail -n 1)
kernel_lowest=$(awk '$1 >= 3 { print $2 }' "$work/leak" | sort -n | head -n 1)
[ "$kernel_second" -le "$kernel_first" ] && [ $((kernel_first - kernel_lowest)) -le 4 ] ||
	fail "kernel pages moved: $(awk '$1 >= 3 { printf "%s ", $2 }' "$work/leak")"
first_high=$(awk -v h="$half" '$1 >= 3 && $1 < h { print $3 }' "$work/leak" | sort -n | tail -n 1)
second_high=$(awk -v h="$half" '$1 >= h { print $3 }' "$work/leak" | sort -n | tail -n 1)
lowest=$(awk '$1 >= 3 { print $3 }' "$work/leak" | sort -n | head -n 1)
[ "$second_high" -le $((first_high + 4)) ] || fail "process pages rose from $first_high to $second_high"
[ $((second_high - lowest)) -le 16 ] || fail "process pages spread from $lowest to $second_high"
leak_kernel=$(awk '$1 >= 3 { printf "%s ", $2 }' "$work/leak")
leak_processes=$(awk '{ printf "%s ", $3 }' "$work/leak")

run 'echo after-memory'
require '^after-memory$' 'the system did not survive the memory checks'

{
	printf 'heap reservation bytes: %s\n' "$reserved"
	printf 'peak live bytes: %s\n' "$peak_live"
	printf 'peak pages charged by the kernel (page test): %s\n' "$pages_peak"
	printf 'post-GC live bytes: %s\n' "$post_live"
	printf 'pages before / after the page test: %s / %s\n' "$pages_before" "$pages_after"
	printf 'allocations: %s\n' "$mallocs"
	printf 'frees: %s\n' "$frees"
	printf 'collections: %s\n' "$collections"
	printf 'longest pause ns (TCG): %s\n' "$pause"
	printf 'automatic collection: %s\n' "$auto"
	printf 'exhaustion: %s\n' "$oom"
	printf 'burst and idle: %s\n' "$burst"
	printf 'VFS service: %s, %s reports, band %s pages, last %s pages, last live %s bytes, %s collections\n' \
		"$service" "$samples" "$band_pages" "$last_pages" "$last_live" "$vfs_collections"
	printf 'leak run: %s rounds, kernel pages %s, process pages by round: %s\n' "$rounds" "$leak_kernel" "$leak_processes"
} >"$report"
cat "$report"
printf 'PASS memory\n'
