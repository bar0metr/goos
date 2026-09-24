#!/bin/sh
# Boots one QEMU guest through a FIFO, runs the in-guest `bench` microbenchmarks
# MICROBENCH_REPEATS times each, and writes the median and minimum to
# build/reports. Boot and command handling follow tests/boot/latency.sh.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
repeats=${MICROBENCH_REPEATS:-5}
command_timeout=${COMMAND_TIMEOUT:-30}
case $repeats in '' | *[!0-9]* | 0) echo "MICROBENCH_REPEATS must be a positive integer" >&2; exit 2 ;; esac
read -r hz diag_build validation_build opt_build variant <build/config/build
# Each build configuration, OPT level included, reports separately, so two
# can be compared.
variant=${variant:-hz$hz}
log=build/logs/microbench-$variant.log
report=build/reports/microbench-$variant.md
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|CLOCK_CALIBRATION_FAILED'

mkdir -p build/logs build/reports
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-microbench.XXXXXX")
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

# run sends one command line and leaves its output in $work/output.
run() {
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$1" >&3
	await_prompt "$next"
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

# field prints the number before " <unit>" in a bench result line.
field() { sed -n "s|.* \([0-9][0-9]*\) $2.*|\1|p" "$1"; }

# stats prints the median and minimum of one number per line.
stats() {
	sort -n "$1" | awk '{ v[NR] = $1 } END {
		median = NR % 2 ? v[(NR + 1) / 2] : int((v[NR / 2] + v[NR / 2 + 1]) / 2)
		printf "%d %d", median, v[1]
	}'
}

qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	grep -q "$fatal" "$log" 2>/dev/null && fail "fatal marker during boot"
	[ "$elapsed" -lt 1200 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done

# The uncached read uses a copy of a system image on the FAT32 volume. At
# 10 KiB it spans 20 sectors, more than the four the FAT32 sector cache holds,
# so every pass reads its data from the virtio-blk device again.
run 'cat /system/bin/uname > /persist/uname.bin'
! grep -q "exec:\|exit status:\|cat:\|write:" "$work/output" || fail "copying /system/bin/uname"

# id|command. Each command prints one `bench` line. The handoff case is short so
# that most of its chunks are round trips; see userspace/bin/bench.
cat >"$work/cases" <<'EOF'
ipc-vfs|bench vfs 1000
stream-1|bench stream 5000 1
handoff-1|bench send 200 1 | bench recv 200 1
pipe-1|bench send 5000 1 | bench recv 5000 1
stream-256|bench stream 5000 256
pipe-256|bench send 5000 256 | bench recv 5000 256
read-image|bench read /system/bin/uname 50
read-fat32|bench read /persist/uname.bin 20
meta-ramfs|bench meta /tmp/bench.tmp 500
EOF

# The warm-up pass runs each case once, so first-use costs such as TCG's
# translation of new code do not land in the samples.
while IFS='|' read -r id command; do
	run "$command"
	grep -q '^bench ' "$work/output" || fail "no result from: $command"
done <"$work/cases"

while IFS='|' read -r id command; do
	: >"$work/$id.lines"
	round=0
	while [ "$round" -lt "$repeats" ]; do
		run "$command"
		grep '^bench ' "$work/output" >>"$work/$id.lines" || fail "no result from: $command"
		round=$((round + 1))
	done
	[ "$(grep -c 'ns/op' "$work/$id.lines")" -eq "$repeats" ] || fail "malformed result from: $command"
	for unit in ns/op KiB/s waits sampled 'ns median'; do
		field "$work/$id.lines" "$unit" >"$work/$id.$(printf '%s' "$unit" | tr -c 'a-zA-Z' _)"
	done
done <"$work/cases"

# value prints the median over the runs of one reported unit, or - if the case
# does not report it.
value() {
	file=$work/$1.$(printf '%s' "$2" | tr -c 'a-zA-Z' _)
	if [ -s "$file" ]; then stats "$file" | cut -d' ' -f1; else printf -- -; fi
}

# row prints one table row from the medians over the runs of each unit and
# the minimum average.
row() {
	printf '| %s | `%s` | %s | %s | %s | %s | %s | %s |\n' "$2" \
		"$(grep "^$1|" "$work/cases" | cut -d'|' -f2- | sed 's/|/\\|/g')" \
		"$(value "$1" ns/op)" "$(stats "$work/$1.ns_op" | cut -d' ' -f2)" \
		"$(value "$1" KiB/s)" "$(value "$1" waits)" "$(value "$1" sampled)" "$(value "$1" 'ns median')"
}

{
	printf '# GoOS in-guest microbenchmarks at %s Hz\n\n' "$hz"
	printf 'Build HZ=%s DIAG=%s OPT=%s, %s runs per case after one warm-up run, QEMU %s (TCG) on %s.\n' \
		"$hz" "$diag_build" "${opt_build:-?}" "$repeats" "$(qemu-system-x86_64 --version | sed -n '1s/.*version \([^ ]*\).*/\1/p')" "$(uname -m)"
	printf 'Times come from the TSC clock inside the guest. "Avg" is the average over one\n'
	printf 'run; "op median" is the median interval of the operations a run sampled (for\n'
	printf '`recv`, the chunks that followed exactly one wait, which are round trips).\n'
	printf 'Each column gives the median over the runs; "min avg" is the lowest average.\n\n'
	printf '| Benchmark | Command | Avg ns/op | Min avg | KiB/s | Waits | Sampled | Op median ns |\n'
	printf '| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |\n'
	row ipc-vfs 'IPC ping-pong: client to VFS service and back'
	row stream-1 'Stream calls in one process, 1 B (no switch)'
	row handoff-1 'Stream handoff between two processes, 1 B'
	row pipe-1 'IPC throughput between two processes, 1 B'
	row stream-256 'Stream calls in one process, 256 B (no switch)'
	row pipe-256 'IPC throughput between two processes, 256 B'
	row read-image 'VFS read, memory-resident system image'
	row read-fat32 'VFS read, FAT32 file beyond the sector cache'
	row meta-ramfs 'RAMFS create and unlink through the VFS'
	handoff=$(value handoff-1 'ns median') baseline=$(value stream-1 'ns median')
	if [ "$(value handoff-1 sampled)" -gt 0 ]; then
		printf '\nContext switch, derived as (handoff - one-process stream calls) / 2 from the\n'
		printf 'op medians: %s ns. It is one switch between two address spaces plus half of\n' $(((handoff - baseline) / 2))
		printf 'a blocking wait, its wake, and the retried wait.\n'
	else
		printf '\nContext switch: no chunk of the handoff case was a round trip.\n'
	fi
	printf '\nRaw results:\n\n```\n'
	while IFS='|' read -r id command; do
		cat "$work/$id.lines"
	done <"$work/cases"
	printf '```\n'
} >"$report"
cat "$report"
printf 'report: %s\nlog: %s\n' "$report" "$log"

