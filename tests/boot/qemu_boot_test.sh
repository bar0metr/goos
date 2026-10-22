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
