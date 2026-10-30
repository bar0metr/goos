#!/bin/sh
# Boots one validation guest and drives Linux programs on their terminal
# through the serial console: the repository's tty-probe, and when the build
# placed them under /system/linux, an interactive BusyBox sh and GNU nano.
# Keys go in as a terminal sends them; checks read the console output, escape
# sequences and all.
#
# usage: linux_tty.sh ISO OVMF DISK [BUSYBOX_NAME [NANO_NAME]]
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
busybox_name=${4:-}
nano_name=${5:-}
memory=${MEMORY:-512M}
smp=${SMP:-1}
command_timeout=${COMMAND_TIMEOUT:-60}
log=build/logs/linux-tty.log
fatal='PANIC\|_WAIT_PERMISSION_DENIED\|_WAIT_NOT_FOUND\|_WAIT_FAILED\|_RECEIVE_FAILED\|_SEND_FAILED\|_CONTROL_FAILED\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LIFECYCLE_QUEUE_OVERFLOW\|LINUX_.*_FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-tty.XXXXXX")
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
	tail -c 2000 "$log" | tr -d '\r' | sed -n '1,40p' >&2
	printf '\003' >&3 2>/dev/null || true
	sleep 1
	printf 'diag\n' >&3 2>/dev/null || true
	sleep 2
	grep -a 'DIAG linux' "$log" | tail -3 >&2 || true
	exit 1
}

prompts() { grep -ac 'goos> ' "$log" 2>/dev/null || true; }

# mark records where the output of the next step starts.
mark() { start=$(wc -c <"$log"); }

# since prints the output since the last mark, without carriage returns.
since() { tail -c "+$((start + 1))" "$log" | tr -d '\r'; }

# await waits until the output since the mark matches pattern.
await() {
	elapsed=0
	until since | grep -aq "$1"; do
		! grep -aq "$fatal" "$log" 2>/dev/null || fail "fatal marker in the log"
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "$2: never printed $1"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
	checks=$((checks + 1))
}

# await_prompt waits for the native prompt after the last mark.
await_prompt() {
	want=$(($(prompts) + 0))
	elapsed=0
	until since | grep -aq 'goos> '; do
		! grep -aq "$fatal" "$log" 2>/dev/null || fail "fatal marker in the log"
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "$1: no native prompt"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
}

# keys sends bytes as typed, a little apart so they arrive as keystrokes.
keys() {
	printf "$1" >&3
	sleep 0.3
}

qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

start=0
elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	! grep -aq "$fatal" "$log" 2>/dev/null || fail "fatal marker during boot"
	[ "$elapsed" -lt 1200 ] || fail "boot timeout"
	sleep 0.1
	elapsed=$((elapsed + 1))
done
sleep 1

# Descriptor identity, /dev/tty, termios, geometry, sessions and groups,
# stop and continue, and fsync, each checked by the probe itself.
mark
keys '/system/linux/tty-probe identity\n'
await '^tty identity ok$' 'tty-probe identity'
await_prompt 'tty-probe identity'

# Canonical input: the discipline edits the line and echoes it.
mark
keys '/system/linux/tty-probe canon\n'
await '^type a line$' 'tty-probe canon starts'
keys 'helo\177lo wxx\027world\r'
await '^line=\[hello world\] size=12$' 'canonical editing'
await_prompt 'tty-probe canon'

# Non-canonical input: bytes arrive as typed, a carriage return untranslated,
# and Ctrl-C as a byte, not a signal. The terminal service reports Ctrl-C out
# of band, ahead of bytes still queued, so it is typed apart.
mark
keys '/system/linux/tty-probe raw\n'
await '^raw ready$' 'tty-probe raw starts'
keys 'q\r'
keys '\003'
await '^raw bytes 3: 71 0d 03$' 'non-canonical input'
await_prompt 'tty-probe raw'

if [ -n "$busybox_name" ]; then
	busybox=/system/linux/$busybox_name
	mark
	keys "$busybox sh\n"
	await '# \|\$ ' 'interactive busybox sh prompt'
	mark
	keys 'echo sum=$((40+2))\n'
	await 'sum=42' 'busybox sh runs a command'
	# A foreground child gets the terminal; Ctrl-C ends it and the rest of
	# the command list, not the shell.
	mark
	keys "$busybox sleep 30; echo after-sleep\n"
	sleep 2
	keys '\003'
	await '\^C' 'the terminal echoes Ctrl-C'
	mark
	keys 'echo status=$?\n'
	await 'status=130' 'Ctrl-C ends the foreground child'
	mark
	keys 'cd /tmp && pwd\n'
	await '^/tmp$' 'the shell survives Ctrl-C'
	if [ -n "$nano_name" ]; then
		nano=/system/linux/$nano_name
		mark
		keys "printf 'first line\\\\n' > /persist/nano.txt; $nano -I /persist/nano.txt\n"
		await 'GNU nano' 'nano draws from busybox sh'
		await 'first line' 'nano shows the file'
		keys 'from nano '
		keys '\017'
		await 'Write to File:' 'nano asks where to write'
		keys '\r'
		await 'Wrote 1 line' 'nano saves'
		keys '\030'
		mark
		keys "$busybox cat /persist/nano.txt; echo cat=\$?\n"
		await 'from nano first line' 'the saved bytes are right'
		await 'cat=0' 'cat after nano'
		# nano 9.2 suspends on ^T^Z with kill(0, SIGSTOP); the shell sees
		# the stop through wait4(WUNTRACED) and takes the terminal back,
		# and fg continues nano, which draws again.
		mark
		keys "$nano -I /persist/nano.txt\n"
		await 'from nano first line' 'nano opens again'
		mark
		keys '\024'
		keys '\032'
		await 'Stopped' 'the shell reports nano stopped'
		mark
		keys 'echo shell-back=$((6*7))\n'
		await 'shell-back=42' 'the shell has the terminal while nano is stopped'
		mark
		keys 'fg\n'
		await 'from nano first line' 'nano draws again after fg'
		keys '\030'
		mark
		keys 'echo after-nano=$?\n'
		await 'after-nano=0' 'the shell after nano exits'
	fi
	mark
	keys 'exit\n'
	await_prompt 'exit busybox sh'
fi

if [ -n "$nano_name" ]; then
	mark
	keys "/system/linux/$nano_name -I /persist/nano.txt\n"
	await 'from nano first line' 'nano launched from the native shell reopens the file'
	keys '\030'
	await_prompt 'nano exits to the native shell'
	mark
	keys 'cat /persist/nano.txt\n'
	await 'from nano first line' 'the native shell reads the file nano saved'
	await_prompt 'cat'
fi

printf 'PASS linux-tty (%s checks)\n' "$checks"
