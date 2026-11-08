#!/bin/sh
# Boots one guest on the default QEMU CPU, which has no RDRAND, and checks
# static PIE load randomization (ADR 0219) with the in-tree pie-probe: the
# kernel generator is seeded from timing jitter alone, so placement,
# AT_RANDOM and getrandom all have to work without a hardware source.
#
# PID 1 launches the probe LAUNCHES times from the shell. Every base must lie
# in the static-PIE window, page-aligned, with the image and its 64 MiB brk
# reservation below the window's end, and the runs must show more than one
# base. A fork child must keep its parent's base, and a chain of execs must
# show more than one base. The probe itself checks auxv, the initial stack,
# its own relocation, TLS, RELRO, brk and getrandom on every run.
#
# usage: linux-aslr.sh ISO OVMF DISK
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
memory=${MEMORY:-512M}
launches=${LAUNCHES:-32}
execs=${EXECS:-16}
command_timeout=${COMMAND_TIMEOUT:-60}
log=build/logs/linux-aslr.log
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|APP_.*FAILED\|INIT_EXEC_.*FAILED\|LINUX_.*_FAILED'

# The window and brk reservation of kernel/compat/linux/elf/mapping.go and
# kernel/compat/linux/memory.go.
window_start=$((0x40000000))
window_end=$((0x80000000))
brk_reserve=$((64 << 20))

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-linux-aslr.XXXXXX")
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

clean() {
	refuse 'exit status:\|killed\|not found\|permission denied\|exec:\|pie check failed' "$1"
}

# legal checks one "pie base=... end=... brk=..." line and appends the base
# to the file named by $2.
legal() {
	line=$1
	base=$(printf '%s\n' "$line" | sed -n 's/^pie base=\(0x[0-9a-f]*\) end=.*/\1/p')
	end=$(printf '%s\n' "$line" | sed -n 's/^pie base=0x[0-9a-f]* end=\(0x[0-9a-f]*\) .*/\1/p')
	[ -n "$base" ] && [ -n "$end" ] || fail "malformed base line: $line"
	base=$((base))
	end=$((end))
	[ "$base" -ge "$window_start" ] || fail "base $base below the window"
	[ $((base % 4096)) -eq 0 ] || fail "base $base not page-aligned"
	[ $((end + brk_reserve)) -le "$window_end" ] || fail "image ending at $end leaves no brk room"
	printf '%s\n' "$base" >>"$2"
	checks=$((checks + 1))
}

# Default CPU: no -cpu option, so QEMU's qemu64 without RDRAND.
qemu-system-x86_64 -machine q35 -accel tcg,thread=multi -m "$memory" \
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

run lscpu
expect '^Flags:' 'CPU flags'
refuse 'rdrand' 'a CPU without RDRAND'

# Repeated launches by PID 1.
: >"$work/launch-bases"
launch=0
while [ "$launch" -lt "$launches" ]; do
	run '/system/linux/pie-probe'
	expect '^pie ok$' "launch $launch"
	clean "launch $launch"
	legal "$(grep '^pie base=' "$work/output" | head -n 1)" "$work/launch-bases"
	launch=$((launch + 1))
done
distinct=$(sort -u "$work/launch-bases" | wc -l | tr -d ' ')
[ "$distinct" -ge 2 ] || fail "$launches launches all loaded at one base"
printf 'launches: %s distinct bases in %s\n' "$distinct" "$launches"

# A fork child keeps its parent's layout.
run '/system/linux/pie-probe fork'
expect '^pie fork ok$' 'fork'
clean 'fork'
parent=$(sed -n 's/^pie base=\(0x[0-9a-f]*\) .*/\1/p' "$work/output" | head -n 1)
child=$(sed -n 's/^pie child base=\(0x[0-9a-f]*\)$/\1/p' "$work/output" | head -n 1)
[ -n "$parent" ] && [ "$((parent))" -eq "$((child))" ] || fail "fork child base $child, parent $parent"
checks=$((checks + 1))

# Each exec of the chain loads a new image at a fresh base.
run "/system/linux/pie-probe exec $execs"
expect '^pie exec done$' 'exec chain'
clean 'exec chain'
: >"$work/exec-bases"
grep '^pie base=' "$work/output" >"$work/exec-lines" || true
[ "$(wc -l <"$work/exec-lines" | tr -d ' ')" -eq $((execs + 1)) ] || fail "exec chain ran $(wc -l <"$work/exec-lines") images, want $((execs + 1))"
while IFS= read -r line; do
	legal "$line" "$work/exec-bases"
done <"$work/exec-lines"
distinct=$(sort -u "$work/exec-bases" | wc -l | tr -d ' ')
[ "$distinct" -ge 2 ] || fail "$((execs + 1)) exec images all loaded at one base"
printf 'execs: %s distinct bases in %s\n' "$distinct" "$((execs + 1))"

# ET_EXEC and the hand-written static PIE still run.
run '/system/linux/hello'
expect '^hello from Linux ABI$' 'ET_EXEC'
clean 'ET_EXEC'
run '/system/linux/hello-pie'
expect '^hello from a static PIE$' 'assembly static PIE'
clean 'assembly static PIE'
run '/system/linux/musl-hello'
expect '^hello from musl$' 'static musl'
clean 'static musl'

! grep -q "$fatal" "$log" || fail "fatal marker in the log"
printf 'PASS linux-aslr (%s checks)\n' "$checks"
