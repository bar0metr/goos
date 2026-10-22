#!/bin/sh
# Check the boot script's window assertions without booting a guest.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-boot-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

# Use the harness helpers and its actual assertion, without its guest setup.
sed '/^trap cleanup EXIT/,$d' "$here/qemu_boot.sh" >"$work/helpers"
grep '^[[:space:]]*require_window storage.fat32-volume-after-stall ' "$here/qemu_boot.sh" >"$work/assertion"
[ "$(wc -l <"$work/assertion" | tr -d ' ')" -eq 1 ]

attempt() {
	(
		. "$work/helpers"
		stall_log=$work/serial
		. "$work/assertion"
	) >"$work/result" 2>&1
}

command='cat /home/.validation-fat32-recovery; echo after-fat32-stall'
for prompt in 'goos> ' ''; do
	printf '%s%s\r\npersistent\r\nCAT_PROCESS_OK\r\nafter-fat32-stall\r\n' "$prompt" "$command" >"$work/serial"
	if ! attempt; then
		cat "$work/result" >&2
		echo 'FAIL boot recovery assertion: successful read rejected' >&2
		exit 1
	fi
done

# Earlier reads and later output cannot substitute for this command's data.
for output in 'cat: storage did not answer in time' ''; do
	printf 'persistent\r\n%s\r\n%s\r\nafter-fat32-stall\r\npersistent\r\n' "$command" "$output" >"$work/serial"
	if attempt; then
		echo 'FAIL boot recovery assertion: missing read accepted' >&2
		exit 1
	fi
	grep -q '^FAIL storage.fat32-volume-after-stall$' "$work/result"
done

printf 'persistent\r\nafter-fat32-stall\r\n' >"$work/serial"
if attempt; then
	echo 'FAIL boot recovery assertion: missing command accepted' >&2
	exit 1
fi

# A command the guest never ran leaves its window open; a later read must not
# close it.
printf '%s\r\nls /tmp\r\npersistent\r\n' "$command" >"$work/serial"
if attempt; then
	echo 'FAIL boot recovery assertion: open window accepted' >&2
	exit 1
fi

echo 'PASS boot recovery assertion'

# Child output and prompt redraws can arrive before PID 1's completion reply.
# Drive the real helper with deterministic log events instead of a guest or
# sleeps: only a fresh shell acknowledgement may release the next command.
(
	. "$work/helpers"
	log=$work/ready-serial
	: >"$log"
	shell_ready_limit=5
	shell_ready_sequence=1
	ready_step=0
	sleep() {
		ready_step=$((ready_step + 1))
		case $ready_step in
		1) printf 'after-command\r\n' >>"$log" ;;
		2) printf 'goos> \r\nSHELL_READY token=1\r\n' >>"$log" ;;
		3) printf 'SHELL_READY token=2\r\n' >>"$log" ;;
		*) exit 1 ;;
		esac
	}
	send_after_done 'echo after-command' '^after-command$' >"$work/sent"
	[ "$ready_step" -eq 3 ] || exit 1
	[ "$(cat "$work/sent")" = 'echo after-command; __boot_ready 2' ] || exit 1
	# The same output pattern is valid again, but neither the old output nor
	# the previous acknowledgement may release this second command.
	ready_step=0
	sleep() {
		ready_step=$((ready_step + 1))
		case $ready_step in
		1) printf 'SHELL_READY token=3\r\n' >>"$log" ;;
		2) printf 'after-command\r\n' >>"$log" ;;
		*) exit 1 ;;
		esac
	}
	send_after_done 'echo after-command' '^after-command$' >"$work/sent"
	[ "$ready_step" -eq 2 ] || exit 1
	[ "$(cat "$work/sent")" = 'echo after-command; __boot_ready 3' ] || exit 1
) || { echo 'FAIL boot shell-ready acknowledgement' >&2; exit 1; }

# A missing completion, missing expected output, stale output, or a redraw
# must fail within the bound and must not allow the next command to be sent.
for missing in completion output fresh-output; do
	if (
		. "$work/helpers"
		log=$work/ready-serial
		printf 'goos> \r\nSHELL_READY token=1\r\n' >"$log"
		if [ "$missing" = fresh-output ]; then
			printf 'after-command\r\n' >>"$log"
		fi
		shell_ready_limit=3
		shell_ready_sequence=1
		ready_step=0
		sleep() {
			ready_step=$((ready_step + 1))
			[ "$ready_step" -le "$shell_ready_limit" ] || exit 1
			if [ "$ready_step" -eq 1 ]; then
				case $missing in
				completion) printf 'after-command\r\ngoos> \r\n' >>"$log" ;;
				*) printf 'SHELL_READY token=2\r\n' >>"$log" ;;
				esac
			fi
		}
		send_after_done 'echo after-command' '^after-command$'
		printf 'next-command-must-not-be-sent\n'
	) >"$work/result" 2>&1; then
		echo "FAIL boot shell-ready: missing $missing accepted" >&2
		exit 1
	fi
	grep -q '^FAIL timeout.shell-ready$' "$work/result"
	grep -q 'after 3 polls' "$work/result"
	if grep -q 'next-command-must-not-be-sent' "$work/result"; then
		echo 'FAIL boot shell-ready: continued after timeout' >&2
		exit 1
	fi
done

echo 'PASS boot shell-ready acknowledgement'
