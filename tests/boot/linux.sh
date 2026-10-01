#!/bin/sh
# Boots one validation guest and runs the Linux ABI end-to-end sequence from
# the shell: the release proofs, the hardening modes of the conformance probe,
# two Linux processes at once, and Ctrl-C on a blocked Linux process. The
# boot itself runs PID 1's Linux probes, whose markers are checked first.
# Boot and command handling follow tests/boot/microbench.sh.
#
# The guest CPU has RDRAND, so this run covers getrandom and AT_RANDOM with a
# source behind them. The main boot test uses the default CPU, which has
# none, and covers the refusal.
#
# usage: linux.sh ISO OVMF DISK
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
command_timeout=${COMMAND_TIMEOUT:-60}
log=build/logs/linux-abi.log
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|LINUX_.*_FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-linux.XXXXXX")
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

qemu-system-x86_64 -machine q35 -cpu qemu64,+rdrand -m "$memory" \
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

# PID 1 ran the raw probes and the conformance probe before the shell started.
tr -d '\r' <"$log" >"$work/output"
for marker in LINUX_PID1_LAUNCH_OK LINUX_RAW_SYSCALL_OK LINUX_MEMORY_TLS_OK LINUX_MUSL_OK \
	LINUX_VFS_SESSION_OK LINUX_PATH_OK LINUX_FILE_IO_OK LINUX_STAT_OK LINUX_DIRENT_OK LINUX_SYSTEM_OK; do
	expect "^$marker\$" "PID 1 Linux probes"
done

# The release proofs: a program with no libc, a static musl program, files
# through the VFS on the volatile root and on FAT32, and a directory listing.
run '/system/linux/hello'
expect '^hello from Linux ABI$' 'minimal ELF'
clean 'minimal ELF'

# A static position-independent image. The program checks the addresses the
# kernel reported against where it runs, and prints only when they agree.
run '/system/linux/hello-pie'
expect '^hello from a static PIE$' 'static PIE'
clean 'static PIE'

run '/system/linux/musl-hello alpha beta'
expect '^argc=3$' 'static musl arguments'
expect '^hello from musl$' 'static musl'
clean 'static musl'

for path in /tmp/linux-file /persist/linux-file; do
	run "/system/linux/fsprobe $path"
	expect '^write ok$' "fsprobe $path"
	expect '^read ok$' "fsprobe $path"
	expect '^stat ok$' "fsprobe $path"
	clean "fsprobe $path"
	# A native program reads what the Linux program wrote.
	run "cat $path"
	expect '^written by a Linux program$' "native cat of $path"
done

run 'mkdir /tmp/listed; write /tmp/listed/first one; write /tmp/listed/second two; mkdir /tmp/listed/nested'
run '/system/linux/dirprobe /tmp/listed'
expect '^first$' 'dirprobe file'
expect '^second$' 'dirprobe file'
expect '^nested/$' 'dirprobe directory'
expect '^dirprobe ok: 3 entries$' 'dirprobe count'
clean 'dirprobe'

run '/system/linux/dirprobe /persist'
expect '^linux-file$' 'dirprobe on FAT32'
expect '^dirprobe ok: ' 'dirprobe on FAT32'
clean 'dirprobe on FAT32'

# A Linux program on a writable volume. Its pages come from the file through
# the VFS as they are touched, so a call whose argument lies in a page not
# yet loaded has to wait for that page, not fail.
run 'cp /system/linux/musl-hello /persist/musl-hello'
run '/persist/musl-hello from disk'
expect '^argc=3$' 'Linux program from FAT32'
expect '^hello from musl$' 'Linux program from FAT32'
clean 'Linux program from FAT32'
run 'cp /system/linux/abi-probe /persist/abi-probe'
run '/persist/abi-probe hostile'
expect '^hostile ok$' 'conformance probe from FAT32'
refuse 'failed' 'conformance probe from FAT32'

# A Linux program in a native pipeline, on either side.
run '/system/linux/musl-hello | write /tmp/piped; cat /tmp/piped'
expect '^hello from musl$' 'Linux program writing to a native one'
run 'cat /tmp/listed/first | /system/linux/abi-probe pair solo drain'
expect '^pair solo ok pid [0-9]*$' 'Linux program reading from a native one'
expect '^one$' 'Linux program reading from a native one'

# Two Linux processes at once: separate descriptors, working directories,
# TLS, heaps, and VFS sessions.
run '/system/linux/abi-probe pair left | /system/linux/abi-probe pair right drain'
expect '^pair left ok pid [0-9]*$' 'concurrent Linux processes'
expect '^pair right ok pid [0-9]*$' 'concurrent Linux processes'
refuse 'failed' 'concurrent Linux processes'
left=$(sed -n 's/^pair left ok pid //p' "$work/output")
right=$(sed -n 's/^pair right ok pid //p' "$work/output")
[ -n "$left" ] && [ "$left" != "$right" ] || fail "the two processes share PID $left"
checks=$((checks + 1))

# Hostile arguments, exhausted resources, and the native call gate.
run '/system/linux/abi-probe hostile'
expect '^hostile ok$' 'hostile pointers and arguments'
refuse 'failed' 'hostile pointers and arguments'
clean 'hostile pointers and arguments'

run '/system/linux/abi-probe exhaust'
expect '^descriptor table ok$' 'descriptor table exhaustion'
expect '^page quota ok: [0-9]* MiB mapped before ENOMEM$' 'page quota exhaustion'
expect '^exhaust ok$' 'resource exhaustion'
clean 'resource exhaustion'

run '/system/linux/abi-probe isolation'
expect '^native gate ok$' 'native call gate'
expect '^handle guesses ok$' 'native handles as descriptors'
expect '^isolation ok$' 'isolation'
clean 'isolation'

# The CPU of this guest has a generator, so the kernel must hand out bytes.
run '/system/linux/abi-probe random'
expect '^random: hardware$' 'random source'
clean 'random source'

run '/system/linux/abi-probe exec'
expect '^exec ok pid [0-9]*$' 'in-place Linux image replacement'
clean 'in-place Linux image replacement'

run '/system/linux/abi-probe fork'
expect '^fork child ok$' 'forked Linux child'
expect '^fork parent ok$' 'forked Linux parent'
clean 'forked Linux parent'

run '/system/linux/abi-probe vfork'
expect '^vfork ok$' 'vfork suspends the parent until the child exits'
expect '^clone ok$' 'clone process form and thread refusal'
clean 'clone process form and thread refusal'

run '/system/linux/abi-probe orphan'
expect '^orphan ok$' 'an orphaned Linux process is reparented to PID 1'
clean 'an orphaned Linux process is reparented to PID 1'

run '/system/linux/abi-probe wait'
expect '^wait ok$' 'wait4 reaps exact and any children, with and without WNOHANG'
clean 'wait4 reaps exact and any children, with and without WNOHANG'

run '/system/linux/abi-probe pipe'
expect '^pipe ok$' 'Linux pipes: direction, EOF, EPIPE, lifetime, PIPE_BUF'
clean 'Linux pipes: direction, EOF, EPIPE, lifetime, PIPE_BUF'

run '/system/linux/abi-probe signal'
expect '^signal ok$' 'Linux signal delivery, handlers, masks, and rt_sigreturn'
clean 'Linux signal delivery, handlers, masks, and rt_sigreturn'

run '/system/linux/abi-probe kill'
expect '^kill ok$' 'kill, tkill, and tgkill on the process tree'
clean 'kill, tkill, and tgkill on the process tree'

run '/system/linux/abi-probe eintr'
expect '^eintr ok$' 'signals interrupt blocked calls; SA_RESTART restarts them'
clean 'signals interrupt blocked calls; SA_RESTART restarts them'

run '/system/linux/abi-probe poll'
expect '^poll ok$' 'poll readiness, hangup, error, timeout, and wake-up'
clean 'poll readiness, hangup, error, timeout, and wake-up'

run '/system/linux/abi-probe ppoll'
expect '^ppoll ok$' 'ppoll timespec timeout and temporary signal mask'
clean 'ppoll timespec timeout and temporary signal mask'

run '/system/linux/abi-probe run /system/linux/musl-hello alpha beta'
expect '^argc=3$' 'a Linux program replaces itself with another'
expect '^hello from musl$' 'a Linux program replaces itself with another'
clean 'a Linux program replaces itself with another'

run '/system/linux/abi-probe hardening'
expect '^hardening ok$' 'failed exec, fork memory, descriptors, pipes, and signal frames'
clean 'failed exec, fork memory, descriptors, pipes, and signal frames'

run '/system/linux/abi-probe limits'
expect '^fork bomb bounded at 15$' 'the family budget bounds a fork bomb'
expect '^limits ok$' 'zombies count against the family budget'
clean 'zombies count against the family budget'

run '/system/linux/abi-probe path'
expect '^path /system/linux$' 'a Linux program searches the Linux fixture directory'

# Running off the end of the stack reaches the guard below it. That is a
# fault: the process dies and the system goes on.
run '/system/linux/abi-probe stack-overflow; echo after-stack-overflow'
expect '^stack-overflow: recursing$' 'stack overflow'
refuse 'stack-overflow survived' 'stack overflow'
expect 'user fault: guard page' 'stack overflow reaches the guard'
expect '^after-stack-overflow$' 'shell after a stack overflow'
run '/system/linux/musl-hello'
expect '^hello from musl$' 'Linux program after a stack overflow'

# An I/O port access is a fault: the process dies and the system goes on.
run '/system/linux/abi-probe port-io; echo after-port-io'
expect '^port-io: writing to an I/O port$' 'port access'
refuse 'port-io survived' 'port access'
expect '^after-port-io$' 'shell after a faulted Linux process'

# Ctrl-C ends a Linux process blocked on a stream, in a sleep, and in a wait
# for the VFS, and the shell reads the next line each time.
interrupt '/system/linux/abi-probe block' 'blocking on standard input'
refuse 'standard input ended' 'Ctrl-C on a blocked read'
run 'echo after-blocked-read'
expect '^after-blocked-read$' 'shell after Ctrl-C on a blocked read'

# Ctrl-C on a Linux launch root ends every descendant it forked.
interrupt '/system/linux/abi-probe family' '^family blocking with child'
run 'ps'
refuse 'abi-probe' 'Ctrl-C on a Linux root ends its family'

interrupt '/system/linux/abi-probe sleep' '^sleeping$'
refuse 'sleep ended' 'Ctrl-C on a sleep'
run 'echo after-sleep'
expect '^after-sleep$' 'shell after Ctrl-C on a sleep'

interrupt '/system/linux/abi-probe session-timeout' 'abi-probe session-timeout'
refuse 'session timeout:\|stalled open\|open after timeout' 'Ctrl-C on a VFS wait'
run '/system/linux/musl-hello'
expect '^hello from musl$' 'Linux program after Ctrl-C on a VFS wait'

# The killed processes left nothing behind: the same checks pass again, and
# the kernel counted no VFS translation failure it should not have.
run '/system/linux/abi-probe hostile'
expect '^hostile ok$' 'hostile mode after the kills'
run '/system/linux/abi-probe pair again | write /tmp/paired; cat /tmp/paired'
expect '^pair again ok pid [0-9]*$' 'pairing after the kills'

! grep -q "$fatal" "$log" || fail "fatal marker in the log"
printf 'PASS linux-abi (%s checks)\n' "$checks"
