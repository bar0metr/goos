#!/bin/sh
# Profiles the kernel's locks under the workloads that reach its wide
# domains (ADR 0184, ADR 0187). A diagnostic guest with SMP processors runs
# each workload in turn and prints its counters after each one; the report
# gives, per workload and per lock level, the acquisitions, the ones that
# waited, their total and longest spin in TSC cycles, and the kernel peaks.
#
# usage: lock_profile.sh ISO OVMF DISK [7ZZS-NAME]
#
# The workloads: the probe's smp and kernel modes; three copies of its
# domains mode at once, each with file, pipe, and fork threads; two native
# copies of one file at once; and, when a 7-Zip is on the disk, an archive of
# /system/linux. The report goes to build/reports/lock-profile.md, or to
# PROFILE_REPORT.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
sevenzip=${4:-}
memory=${MEMORY:-1G}
smp=${SMP:-4}
command_timeout=${COMMAND_TIMEOUT:-1800}
log=build/logs/lock-profile.log
report=${PROFILE_REPORT:-build/reports/lock-profile.md}
fatal='PANIC\|LOCK_STUCK\|OUT_OF_MEMORY\|exec: no reply'

mkdir -p build/logs "$(dirname "$report")"
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-profile.XXXXXX")
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
	printf 'FAIL %s\n  log: %s\n' "$1" "$log" >&2
	[ ! -s "$work/output" ] || sed -n '1,40p' "$work/output" >&2
	exit 1
}

prompts() { grep -c '^goos> ' "$log" 2>/dev/null || true; }

run() {
	command=$1
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$command" >&3
	elapsed=0
	while [ "$(prompts)" -lt "$next" ]; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker after: $command"
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "no prompt after: $command"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	tr -d '\r' <"$log" | tail -n "+$((start + 1))" >"$work/output"
}

# await waits for a line a background job prints.
await() {
	elapsed=0
	until tr -d '\r' <"$log" | grep -q "$1"; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker while waiting for $1"
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "no line matches $1"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
}

expect() {
	grep -q "$1" "$work/output" || fail "$2: no line matches $1"
}

# snapshot runs diag and keeps its lock and sched lines as $work/$1.
snapshot() {
	run diag
	grep '^DIAG lock \|^DIAG sched ' "$work/output" >"$work/$1" || fail "diag printed no lock lines"
}

qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

command=boot
elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker during boot"
	[ "$elapsed" -lt 1800 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done

steps="boot"
snapshot boot

run '/system/linux/abi-probe smp'
expect '^smp ok$' 'smp'
snapshot smp
steps="$steps smp"

run '/system/linux/abi-probe kernel'
expect '^kernel ok$' 'kernel'
snapshot kernel
steps="$steps kernel"

run '/system/linux/abi-probe domains a &'
run '/system/linux/abi-probe domains b &'
run '/system/linux/abi-probe domains c'
expect '^domains c ok$' 'domains c'
await '^domains a ok$'
await '^domains b ok$'
snapshot domains
steps="$steps domains"

source=/system/linux/abi-probe
[ -z "$sevenzip" ] || source=/system/linux/$sevenzip
run "cat $source > /tmp/copy-a &"
run "cat $source > /tmp/copy-b"
run 'ls /tmp'
expect '^copy-a$' 'native copy a'
expect '^copy-b$' 'native copy b'
snapshot copies
steps="$steps copies"

if [ -n "$sevenzip" ]; then
	run "/system/linux/$sevenzip a /tmp/profile.7z /system/linux"
	expect '^Everything is Ok$' '7-Zip'
	snapshot 7zip
	steps="$steps 7zip"
fi

# delta prints, for each lock level, the counters of snapshot $2 minus $1;
# the longest spin is the most since boot.
delta() {
	awk -v previous="$work/$1" '
		function field(line, name,   pattern, start) {
			pattern = " " name "="
			start = index(line, pattern)
			if (start == 0) return 0
			line = substr(line, start + length(pattern))
			sub(/ .*/, "", line)
			return line + 0
		}
		BEGIN {
			while ((getline line < previous) > 0) {
				if (line !~ /^DIAG lock /) continue
				level = line; sub(/.*level=/, "", level); sub(/ .*/, "", level)
				acquired[level] = field(line, "acquired"); contended[level] = field(line, "contended"); spin[level] = field(line, "spin")
			}
		}
		/^DIAG lock / {
			level = $0; sub(/.*level=/, "", level); sub(/ .*/, "", level)
			a = field($0, "acquired") - acquired[level]
			c = field($0, "contended") - contended[level]
			if (a == 0) next
			printf "| %s | %d | %d | %.2f%% | %d | %d |\n", level, a, c, 100 * c / a, field($0, "spin") - spin[level], field($0, "longest")
		}
		/^DIAG sched / {
			peaks = sprintf("kernel-parallel-peak %d, kernel-thread-peak %d", field($0, "kernel-parallel-peak"), field($0, "kernel-thread-peak"))
		}
		END { if (peaks != "") printf "\nPeaks since boot: %s.\n", peaks }
	' "$work/$2"
}

{
	printf '# Lock profile\n\n'
	printf 'QEMU -smp %s, TCG. Counts are per workload; spins are TSC cycles; the longest spin is the most since boot.\n' "$smp"
	previous=boot
	for step in $steps; do
		[ "$step" != boot ] || continue
		printf '\n## %s\n\n' "$step"
		printf '| Level | Acquired | Contended | Share | Spin | Longest |\n|---|---:|---:|---:|---:|---:|\n'
		delta "$previous" "$step"
		previous=$step
	done
} >"$report"

printf 'PASS %s (%s)\n  report: %s\n' "$log" "$steps" "$report"
