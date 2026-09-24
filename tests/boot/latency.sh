#!/bin/sh
# Boots one QEMU guest through a FIFO, runs each case under the shell's `time`
# builtin, and writes the median, p90, and minimum to build/reports.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
repeats=${LATENCY_REPEATS:-10}
command_timeout=${COMMAND_TIMEOUT:-30}
case $repeats in '' | *[!0-9]* | 0) echo "LATENCY_REPEATS must be a positive integer" >&2; exit 2 ;; esac
read -r hz diag_build validation_build opt_build variant <build/config/build
# Each build configuration, OPT level included, reports separately, so two
# can be compared.
variant=${variant:-hz$hz}
log=build/logs/latency-$variant.log
report=build/reports/latency-$variant.md
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|CLOCK_CALIBRATION_FAILED'

mkdir -p build/logs build/reports
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-latency.XXXXXX")
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

# stats prints the median, p90 (nearest rank), and minimum of one number per line.
stats() {
	sort -n "$1" | awk '{ v[NR] = $1 } END {
		median = NR % 2 ? v[(NR + 1) / 2] : int((v[NR / 2] + v[NR / 2 + 1]) / 2)
		rank = int((NR * 9 + 9) / 10)
		printf "%d | %d | %d", median, v[rank], v[1]
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

# help cd is a builtin that launches nothing: it bounds the measurement
# overhead. Plain help prints the whole command table and would time the serial
# line instead.
cat >"$work/cases" <<'EOF'
help cd
uname
echo x
ls /system
cat /system/motd
cat /persist/latency.txt
echo y > /persist/latency.txt
echo a | readline | readline
missing-stage
EOF

# The warm-up pass creates the file the persistent cases use and runs each
# case once, so first-use costs do not land in the samples.
run 'echo payload > /persist/latency.txt'
! grep -q "exec:\|exit status:" "$work/output" || fail "creating /persist/latency.txt"
while IFS= read -r command; do
	run "$command"
done <"$work/cases"

table=$work/table
: >"$table"
while IFS= read -r command; do
	: >"$work/samples"
	round=0
	while [ "$round" -lt "$repeats" ]; do
		run "time $command"
		sed -n 's/^time: \([0-9][0-9]*\) us$/\1/p' "$work/output" >>"$work/samples"
		round=$((round + 1))
	done
	[ "$(grep -c '' "$work/samples")" -eq "$repeats" ] || fail "missing time report for: $command"
	printf '| `%s` | %s |\n' "$(printf '%s' "$command" | sed 's/|/\\|/g')" "$(stats "$work/samples")" >>"$table"
done <"$work/cases"

run 'echo payload > /persist/latency.txt'
: >"$work/bench"
for command in 'bench vfs 200' 'bench read /persist/latency.txt 50'; do
	run "$command"
	grep '^bench ' "$work/output" >>"$work/bench" || fail "no result from: $command"
done

if [ "$diag_build" = 1 ]; then
	run diag
	grep -q '^DIAG end' "$work/output" || fail "incomplete diag dump"
	grep '^DIAG clock\|^DIAG launch' "$work/output" >"$work/launch"
fi

{
	printf '# GoOS command latency at %s Hz\n\n' "$hz"
	printf 'Build HZ=%s DIAG=%s OPT=%s, %s runs per case, QEMU %s (TCG) on %s.\n' \
		"$hz" "$diag_build" "${opt_build:-?}" "$repeats" "$(qemu-system-x86_64 --version | sed -n '1s/.*version \([^ ]*\).*/\1/p')" "$(uname -m)"
	printf 'Times are wall-clock microseconds measured by the shell `time` builtin.\n\n'
	printf '| Case | Median us | p90 us | Min us |\n'
	printf '| --- | ---: | ---: | ---: |\n'
	cat "$table"
	printf '\nIn-process VFS round trips (`bench`):\n\n```\n'
	cat "$work/bench"
	printf '```\n'
	if [ "$diag_build" = 1 ]; then
		printf '\nKernel launch stages since boot (`diag`):\n\n```\n'
		cat "$work/launch"
		printf '```\n'
	fi
} >"$report"
cat "$report"
printf 'report: %s\nlog: %s\n' "$report" "$log"
