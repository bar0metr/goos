#!/bin/sh
# Boots one guest and checks the reclaimable native heap (ADR 0201): the
# memreclaim checks, a double free and heap exhaustion that must stop only
# their own process, and a VFS service that must hold its memory steady over
# thousands of requests. Writes build/reports/memory.txt.
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
for marker in LAYOUT STACK_ROOT GLOBAL_ROOT INTERIOR_ROOT CYCLE DEEP_GRAPH ZERO_REUSE BOUNDARY FREE REGION PAGES 1000_CYCLES AUTO_GC; do
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

run 'echo after-memory'
require '^after-memory$' 'the system did not survive the memory checks'

{
	printf 'heap reservation bytes: %s\n' "$reserved"
	printf 'peak live bytes: %s\n' "$peak_live"
	printf 'peak committed pages (page test): %s\n' "$pages_peak"
	printf 'post-GC live bytes: %s\n' "$post_live"
	printf 'pages before / after the page test: %s / %s\n' "$pages_before" "$pages_after"
	printf 'allocations: %s\n' "$mallocs"
	printf 'frees: %s\n' "$frees"
	printf 'collections: %s\n' "$collections"
	printf 'longest pause ns (TCG): %s\n' "$pause"
	printf 'automatic collection: %s\n' "$auto"
	printf 'exhaustion: %s\n' "$oom"
	printf 'VFS service: %s, %s reports, band %s pages, last %s pages, last live %s bytes, %s collections\n' \
		"$service" "$samples" "$band_pages" "$last_pages" "$last_live" "$vfs_collections"
} >"$report"
cat "$report"
printf 'PASS memory\n'
