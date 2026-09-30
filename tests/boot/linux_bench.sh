#!/bin/sh
# Boots one guest and times four Linux calls with the conformance probe's
# bench mode, beside the native bench cases that use the same kernel paths.
# It writes the results to build/reports. The numbers describe this host and
# this QEMU; they are a baseline to compare a change against and no gate.
#
# usage: linux_bench.sh ISO OVMF DISK
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
command_timeout=${COMMAND_TIMEOUT:-60}
read -r hz diag_build validation_build opt_build variant <build/config/build
variant=${variant:-hz$hz}
log=build/logs/linux-bench-$variant.log
report=build/reports/linux-abi-$variant.md
repeats=${LINUX_BENCH_REPEATS:-3}
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|LINUX_.*_FAILED'

mkdir -p build/logs build/reports
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-linux-bench.XXXXXX")
fifo=$work/in
mkfifo "$fifo"
pid=
checks=0

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
	[ ! -s "$work/output" ] || sed -n '1,25p' "$work/output" >&2
	exit 1
}

prompts() { grep -c '^goos> ' "$log" 2>/dev/null || true; }

await_prompt() {
	want=$1
	elapsed=0
	while [ "$(prompts)" -lt "$want" ]; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker in the log"
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "no prompt after: $command"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
}

# run sends one command line and leaves its output in $work/output.
run() {
	command=$1
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$command" >&3
	await_prompt "$next"
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

# interrupt sends a command, waits for the line that shows it is blocked, and
# presses Ctrl-C.
interrupt() {
	command=$1
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$command" >&3
	elapsed=0
	until tail -n "+$((start + 1))" "$log" | grep -q "$2"; do
		[ "$elapsed" -lt 400 ] || fail "$command never printed: $2"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
	sleep 1
	printf '\003' >&3
	await_prompt "$next"
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

# expect requires a line of the last command's output to match.
expect() {
	grep -q "$1" "$work/output" || fail "$2: no line matches $1"
	checks=$((checks + 1))
}

# refuse requires that no line of the last command's output matches.
refuse() {
	! grep -q "$1" "$work/output" || fail "$2: a line matches $1"
	checks=$((checks + 1))
}

# clean requires a run that ended with status zero and no shell complaint.
clean() {
	refuse 'exit status:\|killed\|not found\|permission denied\|exec:' "$1"
}

qemu-system-x86_64 -machine q35 -m "$memory" \
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
	[ "$elapsed" -lt 1200 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done

# The probe writes its four results to standard error and 2000 lines to
# standard output, which a native consumer takes.
linux='/system/linux/abi-probe bench | write /tmp/bench.out'
run "$linux"
grep -q '^linux-bench ' "$work/output" || fail "no result from the Linux bench"

: >"$work/linux.lines"
: >"$work/native.lines"
round=0
while [ "$round" -lt "$repeats" ]; do
	run "$linux"
	grep '^linux-bench ' "$work/output" >>"$work/linux.lines" || fail "no result from the Linux bench"
	for native in 'bench vfs 1000' 'bench stream 5000 64' 'bench read /system/linux/abi-probe 50'; do
		run "$native"
		grep '^bench ' "$work/output" | sed "s|^|$native: |" >>"$work/native.lines" || fail "no result from: $native"
	done
	round=$((round + 1))
done

# lowest prints the smallest value a case reported over the runs.
lowest() { sed -n "s/^linux-bench $1 \([0-9][0-9]*\) ns.call\$/\1/p" "$work/linux.lines" | sort -n | sed -n '1p'; }

{
	printf '# Linux ABI call times at %s Hz\n\n' "$hz"
	printf 'Build HZ=%s DIAG=%s OPT=%s, %s runs after one warm-up run, QEMU %s (TCG) on %s.\n' \
		"$hz" "$diag_build" "${opt_build:-?}" "$repeats" "$(qemu-system-x86_64 --version | sed -n '1s/.*version \([^ ]*\).*/\1/p')" "$(uname -m)"
	printf 'Times come from CLOCK_MONOTONIC inside the guest. Each row is the lowest\n'
	printf 'average over the runs.\n\n'
	printf '| Linux call | ns per call | What it costs |\n| --- | ---: | --- |\n'
	printf '| `getpid` | %s | SYSCALL entry, dispatch, IRETQ return |\n' "$(lowest getpid)"
	printf '| `clock_gettime` | %s | the same plus a TSC read and a 16-byte copy-out |\n' "$(lowest clock_gettime)"
	printf '| `write` of 64 bytes to a pipe | %s | a stream write; the reader is a native program |\n' "$(lowest stdout-write-64)"
	printf '| `read` of 4 KiB from a file | %s | one VFS exchange through the I/O memory object |\n' "$(lowest vfs-read-4k)"
	printf '\nNative cases from the same boot, for scale (`bench`, ADR 0085):\n\n```\n'
	cat "$work/native.lines"
	printf '```\n\nRaw Linux results:\n\n```\n'
	cat "$work/linux.lines"
	printf '```\n'
} >"$report"

! grep -q "$fatal" "$log" || fail "fatal marker in the log"
cat "$report"
printf 'PASS linux-bench (%s)\n' "$report"
