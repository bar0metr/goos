#!/bin/sh
# Boots one validation guest with several processors and checks that they
# all run tasks at once: the kernel's start-up markers, lscpu, the Linux
# probe's smp mode, which proves overlapping execution on distinct
# processors, its kernel mode, which proves kernel code running on several
# processors at once, and the thread, fork, exec, pipe, and signal probes, which now
# run with their tasks spread over the processors. Boot and command handling
# follow tests/boot/linux.sh.
#
# usage: smp.sh ISO OVMF DISK
#
# SMP is the QEMU -smp argument, four processors in two cores of two threads
# by default; CPUS is the count lscpu must report online.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
smp=${SMP:-4,sockets=1,cores=2,threads=2}
cpus=${CPUS:-4}
command_timeout=${COMMAND_TIMEOUT:-120}
log=build/logs/smp-$cpus.log
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|LINUX_.*_FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-smp.XXXXXX")
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

expect() {
	grep -q "$1" "$work/output" || fail "$2: no line matches $1"
	checks=$((checks + 1))
}

refuse() {
	! grep -q "$1" "$work/output" || fail "$2: a line matches $1"
	checks=$((checks + 1))
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

tr -d '\r' <"$log" >"$work/output"
expect "^SMP: $cpus of $cpus processors online\$" 'every processor joined the scheduler'
if [ "$cpus" -gt 1 ]; then
	expect '^SMP_AP_START_OK$' 'every application processor started'
fi
expect '^PID1_INIT_OK$' 'PID 1 started'

last=$((cpus - 1))
online=0
[ "$last" -eq 0 ] || online=0-$last
run lscpu
expect "^CPU(s): *$cpus\$" 'lscpu CPU count'
expect "^On-line CPU(s) list: *$online\$" 'lscpu online list'
refuse '^Off-line CPU(s) list:' 'lscpu offline list'

# Threads and processes whose calls share no lock run kernel code on several
# processors at once. The kernel prints each new peak of processors inside
# calls together, and of one process's tasks inside calls together; only a
# program with several threads raises the second, so it rises here.
peaks() {
	tr -d '\r' <"$log" | grep -o "$1 [0-9]*" | sed 's/.* //' | sort -n | tail -n 1
}
before=$(peaks KERNEL_THREAD_PEAK)
run '/system/linux/abi-probe kernel'
expect '^kernel ok$' 'kernel: calls beside each other'
# Housekeeping prints the peaks on its next tick; one more command gives it
# that tick.
run uptime
threads=$(peaks KERNEL_THREAD_PEAK)
peak=$(peaks KERNEL_PARALLEL_PEAK)
if [ "$cpus" -gt 1 ]; then
	[ "${threads:-0}" -ge 2 ] && [ "${threads:-0}" -gt "${before:-0}" ] ||
		fail "kernel-thread-peak is ${threads:-0} after the kernel probe, ${before:-0} before; want at least 2, raised by the probe"
	[ "${peak:-0}" -ge 2 ] || fail "kernel-parallel-peak is ${peak:-0}, want at least 2"
	checks=$((checks + 2))
fi
printf '  kernel-parallel-peak: %s, kernel-thread-peak: %s (before the probe %s)\n' "${peak:-0}" "${threads:-0}" "${before:-0}"

run '/system/linux/abi-probe smp'
expect "^smp cpus: $cpus online\$" 'smp: sched_getaffinity'
expect '^smp workers: ' 'smp: workers ran'
expect '^smp futex: ' 'smp: contended mutex'
expect '^smp vm: ' 'smp: discards and faults'
expect '^smp speedup: ' 'smp: speedup measured'
expect '^smp lifecycle: ' 'smp: fork beside threads'
expect '^smp exec: ' 'smp: exec beside threads'
expect '^smp affinity: ' 'smp: sched_setaffinity holds and widens'
expect '^smp pinned: ' 'smp: one pinned worker per processor'
expect '^smp deadlines: ' 'smp: timeouts and wakes'
expect '^smp ok$' 'smp probe'
sed -n -e 's/^smp workers: /  workers: /p' -e 's/^smp speedup: /  speedup: /p' -e 's/^smp affinity: /  affinity: /p' "$work/output"

# The cpu command lists every processor online, each having switched tasks.
run cpu
rows=$(awk '$2 == "online" && $6 + 0 > 0' "$work/output" | wc -l | tr -d ' ')
[ "$rows" -eq "$cpus" ] || fail "cpu listed $rows online processors with switches, want $cpus"
checks=$((checks + 1))
run 'cpu -r'
expect '^CPU *STATE *TICKS *IDLE *TASK *SWITCHES *STEALS *MIGRATIONS *RESCHED *SHOOTDOWN$' 'cpu -r header'



for mode in threads fpstate vfork wait pipe signal kill; do
	run "/system/linux/abi-probe $mode"
	expect "^$mode ok\$" "$mode probe"
done
run '/system/linux/abi-probe fork'
expect '^fork child ok$' 'fork probe child'
expect '^fork parent ok$' 'fork probe parent'

# Native processes in a pipeline exchange their bytes through streams while
# the probes above left every processor busy in turn.
run 'echo smp-pipeline | readline | readline'
expect '^smp-pipeline$' 'native pipeline'
run 'uname; uptime'
expect 'GoOS' 'uname'

printf 'PASS %s (%d checks, %d processors)\n' "$log" "$checks" "$cpus"
