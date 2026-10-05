#!/bin/sh
# Runs a static 7-Zip the caller supplies on a diagnostic guest with several
# processors. 7-Zip archives /system/linux, which holds it and the committed
# Linux fixtures, tests the archive, extracts it to /tmp, and hashes both
# trees; every file must come back with the same SHA-256. The diag lines
# before and after give the most threads of one process that ran on distinct
# processors at once: nothing before 7-Zip may reach two, and 7-Zip must.
# The report goes to build/reports/linux-7zip.md.
#
# usage: linux_7zip.sh ISO OVMF DISK 7ZZS
#
# SMP is the QEMU -smp argument, four processors by default.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?disk path required}
binary=${4:?7zzs path required}
memory=${MEMORY:-1G}
smp=${SMP:-4}
command_timeout=${COMMAND_TIMEOUT:-1800}
log=build/logs/linux-7zip.log
report=build/reports/linux-7zip.md
name=$(basename "$binary")
seven=/system/linux/$name
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|LIFECYCLE_QUEUE_OVERFLOW'

mkdir -p build/logs build/reports
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-7zip.XXXXXX")
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
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

expect() {
	grep -q "$1" "$work/output" || fail "$2: no line matches $1"
}

# peak prints the parallel-peak field of the last diag command.
peak() {
	sed -n 's/^DIAG sched .* parallel-peak=\([0-9]*\).*/\1/p' "$work/output" | tail -n 1
}

# hashes keeps the name and SHA-256 of every file 7-Zip hashed, sorted.
hashes() {
	awk 'NF == 3 && length($1) == 64 && $1 ~ /^[0-9a-f]+$/ { print $3, $1 }' "$work/output" | sort
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

run diag
before=$(peak)
[ -n "$before" ] || fail 'diag printed no parallel-peak'
[ "$before" -lt 2 ] || fail "parallel-peak is $before before 7-Zip ran"

run "$seven i"
expect '^7-Zip' 'version banner'
version=$(grep -m 1 '^7-Zip' "$work/output")
threads=$(sed -n 's/.*Threads:\([0-9]*\).*/\1/p' "$work/output" | head -n 1)

run "$seven a /tmp/linux.7z /system/linux"
expect '^Everything is Ok$' 'create'
run "$seven t /tmp/linux.7z"
expect '^Everything is Ok$' 'test'
run "$seven x -y -o/tmp/out /tmp/linux.7z"
expect '^Everything is Ok$' 'extract'

run "$seven h -scrcSHA256 /system/linux"
expect '^Everything is Ok$' 'hash the source'
hashes >"$work/source"
run "$seven h -scrcSHA256 /tmp/out/linux"
expect '^Everything is Ok$' 'hash the extracted tree'
hashes >"$work/extracted"
files=$(wc -l <"$work/source" | tr -d ' ')
[ "$files" -ge 2 ] || fail "only $files files hashed"
cmp -s "$work/source" "$work/extracted" || {
	diff "$work/source" "$work/extracted" >&2 || true
	fail 'extracted files differ from the source'
}
expected=$(shasum -a 256 "$binary" | cut -d ' ' -f 1)
grep -q "^linux/$name $expected\$" "$work/source" || fail "$name in the guest does not hash as on the host"

run diag
after=$(peak)
[ -n "$after" ] && [ "$after" -ge 2 ] || fail "parallel-peak is ${after:-missing} after 7-Zip, want at least 2"
refused=$(sed -n 's/^DIAG linux .*refused=\([^ ]*\).*/\1/p' "$work/output" | tail -n 1)

{
	printf '# 7-Zip on GoOS\n\n'
	printf '| Item | Value |\n|---|---|\n'
	printf '| Version | %s |\n' "$version"
	printf '| Binary SHA-256 | `%s` |\n' "$expected"
	printf '| QEMU -smp | %s |\n' "$smp"
	printf '| Threads reported | %s |\n' "${threads:-unknown}"
	printf '| Create, test, extract | Everything is Ok |\n'
	printf '| Files compared | %s, all SHA-256 equal |\n' "$files"
	printf '| parallel-peak before, after | %s, %s |\n' "$before" "$after"
	printf '| Refused Linux calls | %s |\n\n' "${refused:-none}"
	printf '## SHA-256 of the extracted files\n\n```\n'
	cat "$work/extracted"
	printf '```\n'
} >"$report"

printf 'PASS %s (%s files, parallel-peak %s, threads %s)\n  report: %s\n' "$log" "$files" "$after" "${threads:-?}" "$report"
