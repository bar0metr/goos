#!/bin/sh
# Boots one validation guest and runs applets of a static x86_64 BusyBox the
# caller supplied, which the build placed under /system/linux. The applets
# are the release's interoperability set: echo, pwd, cat, ls, mkdir, rm, and
# a non-interactive sh that starts programs, connects pipelines, redirects,
# and runs a script. When an applet fails, the kernel's record of
# unsupported calls is printed, which names what the applet missed.
#
# usage: linux_busybox.sh ISO OVMF DISK NAME
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
name=${4:?program name under /system/linux required}
busybox=/system/linux/$name
memory=${MEMORY:-512M}
# SMP is the guest's processor count, as a QEMU -smp argument.
smp=${SMP:-1}
command_timeout=${COMMAND_TIMEOUT:-60}
log=build/logs/linux-busybox.log
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|LINUX_.*_FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-busybox.XXXXXX")
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
	# A DIAG=1 build has the diag command; its "DIAG linux" line lists the
	# call numbers the kernel refused most recently.
	printf 'diag\n' >&3 2>/dev/null || true
	sleep 2
	grep -a 'DIAG linux' "$log" | tail -1 >&2 || true
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
	[ "$elapsed" -lt 1200 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done

run "$busybox echo hello from busybox"
expect '^hello from busybox$' 'busybox echo'
clean 'busybox echo'

run 'cd /tmp'
run "$busybox pwd"
expect '^/tmp$' 'busybox pwd'
clean 'busybox pwd'

run 'write /persist/busy.txt read-by-busybox'
run "$busybox cat /persist/busy.txt"
expect '^read-by-busybox$' 'busybox cat'
clean 'busybox cat'

run "$busybox ls /persist"
expect '^busy.txt$' 'busybox ls'
clean 'busybox ls'

run "$busybox mkdir /tmp/linux-test"
clean 'busybox mkdir'
run 'write /tmp/linux-test/file doomed; ls /tmp/linux-test'
expect '^file$' 'a file in the directory busybox made'

run "$busybox rm /tmp/linux-test/file"
clean 'busybox rm'
run 'ls /tmp/linux-test; echo listed'
refuse '^file$' 'busybox rm'
expect '^listed$' 'busybox rm'

# A non-interactive shell starts other Linux programs, by path and through
# PATH, and sees their exit status.
run "$busybox sh -c '/system/linux/musl-hello one two'"
expect '^argc=3$' 'busybox sh runs a program'
expect '^hello from musl$' 'busybox sh runs a program'
clean 'busybox sh runs a program'

run "$busybox sh -c '$busybox false || $busybox echo recovered'"
expect '^recovered$' 'busybox sh sees a failing status'
clean 'busybox sh sees a failing status'

run "$busybox sh -c '$busybox false; echo status=\$?; exit 3'"
expect '^status=1$' 'busybox sh reports a child status'
expect '^exit status: 3$' 'busybox sh exit status reaches the native shell'

run "$busybox sh -c 'busybox echo through-path'"
expect '^through-path$' 'busybox sh searches PATH'
clean 'busybox sh searches PATH'

# Pipelines of two and three stages.
run "$busybox sh -c '$busybox echo hello | $busybox cat'"
expect '^hello$' 'two-stage busybox pipeline'
clean 'two-stage busybox pipeline'

run "$busybox sh -c '$busybox echo A | $busybox cat | $busybox cat'"
expect '^A$' 'three-stage busybox pipeline'
clean 'three-stage busybox pipeline'

# Redirection, then a child reading the file.
run "$busybox sh -c '$busybox echo data >/tmp/sh-file; $busybox cat /tmp/sh-file'"
expect '^data$' 'busybox sh redirection and a child'
clean 'busybox sh redirection and a child'

# A script passed by name.
run "write /persist/test.sh '$busybox echo from-script; $busybox echo second-line'"
run "$busybox sh /persist/test.sh"
expect '^from-script$' 'busybox sh script'
expect '^second-line$' 'busybox sh script'
clean 'busybox sh script'

# A background job and wait.
run "$busybox sh -c '$busybox sleep 1 & wait; echo waited'"
expect '^waited$' 'busybox sh background job and wait'
clean 'busybox sh background job and wait'

# Applets outside the release set are observed, not required: the report
# says which of them run, so the supported set can grow from measurements.
observe() {
	run "$busybox $1"
	if grep -q 'exit status:\|killed\|applet not found' "$work/output"; then
		verdict=fails
	else
		verdict=runs
	fi
	printf 'INFO busybox %s: %s\n' "$1" "$verdict"
}
run 'write /tmp/observe.txt observed'
for applet in 'uname -a' 'id' 'date' 'sleep 1' 'touch /tmp/touched' 'cp /tmp/observe.txt /tmp/copied' \
	'mv /tmp/copied /tmp/moved' 'head -n 1 /tmp/observe.txt' 'wc -c /tmp/observe.txt' 'stat /tmp/observe.txt' \
	'ls -l /tmp' 'rmdir /tmp/linux-test' 'true' 'basename /tmp/observe.txt' 'env' 'df /tmp' 'sh -c true' \
	'stat /dev/null' 'dd if=/dev/zero of=/tmp/zeros bs=512 count=2' 'tail -c 4 /tmp/observe.txt' \
	'md5sum /tmp/observe.txt' 'sh -c "echo discarded > /dev/null"'; do
	observe "$applet"
done

# A DIAG=1 build has the diag command. Its "DIAG linux" line counts the calls
# the kernel refused and lists the most recent numbers.
printf 'diag\n' >&3
elapsed=0
until grep -a -q 'DIAG end\|diag: unavailable' "$log" || [ "$elapsed" -ge 600 ]; do
	sleep 0.05
	elapsed=$((elapsed + 1))
done
grep -a 'DIAG linux' "$log" | tail -1 | tr -d '\r' | sed 's/^/INFO /' || true

! grep -q "$fatal" "$log" || fail "fatal marker in the log"
printf 'PASS linux-busybox (%s checks)\n' "$checks"
