#!/bin/sh
# Host test of tests/boot/leak_round.sh: a round against canned command
# output must pass when every step did its work, and must fail, before the
# round counts, when any step printed an error, lacked its success line, made
# the wrong thing, or left a file behind. No guest is booted.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
. "$here/leak_round.sh"

work=$(mktemp -d "${TMPDIR:-/tmp}/goos-leak-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

copy_bytes=73216
motd_bytes=412
# broken names the fault to inject: "COMMAND=OUTPUT" replaces the output of
# that command line in every round.
broken=

fail() {
	printf '%s\n' "$1" >"$work/failure"
	exit 1
}

# answer prints what a working guest prints for a command line.
answer() {
	printf 'goos> %s\n' "$1"
	case $1 in
	'stat -b /system/bin/vmprobe' | 'stat -b /tmp/leak.bin') printf 'file %s bytes\nmodified=2026-10-19T00:00:00Z\n' "$copy_bytes" ;;
	'wc -c /system/motd' | 'wc -c /tmp/leak.pipe') printf '%s\n' "$motd_bytes" ;;
	'bench io /tmp/leak.io 256K')
		printf 'io write, 262144 bytes, 4 exchanges, 5051 us, 50681 KiB/s\n'
		printf 'io read, 262144 bytes, 4 exchanges, 4266 us, 60008 KiB/s\n'
		printf 'VFS_MEMORY requests=1024 live=1 objects=1 collections=1 pages=1\n'
		;;
	'stat -b /tmp/leak.io') printf 'file 262144 bytes\n' ;;
	'grep vmprobe /tmp/leak.ls') printf 'vmprobe\n' ;;
	'cat /tmp/leak.r') printf 'random: available\n' ;;
	'stat -b /tmp/'*) printf 'file 1 bytes\n' ;;
	*) printf 'SHELL_TERMINAL_LINE_OK\n' ;;
	esac
}

# A file removed once reads as gone; the fault "keep" leaves it.
run() {
	case $broken in
	"$1="*)
		printf 'goos> %s\n%s\n' "$1" "${broken#*=}" >"$work/output"
		return 0
		;;
	esac
	case $1 in
	'stat -b /tmp/'*)
		if [ -e "$work/made${1#stat -b }" ]; then
			answer "$1" >"$work/output"
			return 0
		fi
		printf 'goos> %s\nstat: %s: no such file or directory\n' "$1" "${1#stat -b }" >"$work/output"
		return 0
		;;
	'cp '*' /tmp/'* | *'> /tmp/'* | 'bench io /tmp/'* | *'write /tmp/'*)
		target=$(printf '%s\n' "$1" | grep -o '/tmp/[a-z.]*' | tail -n 1)
		mkdir -p "$work/made/tmp"
		: >"$work/made$target"
		;;
	'rm /tmp/'*)
		[ "$broken" = keep ] || rm -f "$work/made${1#rm }"
		;;
	esac
	answer "$1" >"$work/output"
}

# attempt runs prepare and two rounds in a subshell and reports whether they
# passed, leaving the failure message in "$work/failure".
attempt() {
	broken=$1
	rm -rf "$work/made" "$work/failure"
	(
		leak_prepare
		leak_round 1
		leak_round 2
	)
}

attempt '' || {
	cat "$work/failure" >&2
	echo 'leak round test: a working round failed' >&2
	exit 1
}

for fault in \
	'cp /system/bin/vmprobe /tmp/leak.bin=cp: /tmp/leak.bin: no space left on device' \
	'stat -b /tmp/leak.bin=file 1024 bytes' \
	'bench io /tmp/leak.io 256K=io write, 262144 bytes, 4 exchanges, 5051 us, 50681 KiB/s' \
	'stat -b /tmp/leak.io=stat: /tmp/leak.io: no such file or directory' \
	'grep vmprobe /tmp/leak.ls=SHELL_TERMINAL_LINE_OK' \
	'sleep 1 | cat /system/motd | write /tmp/leak.pipe=write: /tmp/leak.pipe: read-only filesystem' \
	'wc -c /tmp/leak.pipe=0' \
	'/system/linux/abi-probe random > /tmp/leak.r=exit status: 1' \
	'cat /tmp/leak.r=random: unavailable' \
	'rm /tmp/leak.ls=rm: /tmp/leak.ls: directory not empty' \
	'ls /system/bin > /tmp/leak.ls=ls: command not found' \
	keep; do
	if attempt "$fault"; then
		echo "leak round test: a round passed with the fault: $fault" >&2
		exit 1
	fi
	grep -q '^leak round [12]: ' "$work/failure" || {
		echo "leak round test: the failure for '$fault' names no round:" >&2
		cat "$work/failure" >&2
		exit 1
	}
done
echo 'PASS leak round checks'
