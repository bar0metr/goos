#!/bin/sh
# Checks, before a guest boots, that each external Linux fixture is a static
# x86_64 ELF with no program interpreter, and prints its SHA-256 for the run's
# record.
#
# usage: tools/linuxfixture/check-static.sh FILE...
set -eu
status=0
for file in "$@"; do
	header=$(od -An -tx1 -N20 "$file" | tr -d ' \n')
	# ELF magic, 64-bit, little endian, and e_machine 0x3e at offset 18.
	case $header in
	7f454c460201*) ;;
	*) echo "$file: not a little-endian ELF64 file" >&2; status=1; continue ;;
	esac
	[ "$(printf %s "$header" | cut -c37-40)" = 3e00 ] || { echo "$file: not x86_64" >&2; status=1; continue; }
	if LC_ALL=C grep -aq '/lib/ld-\|/lib64/ld-' "$file"; then
		echo "$file: names a dynamic loader" >&2
		status=1
		continue
	fi
	printf '%s  %s\n' "$(shasum -a 256 "$file" | cut -d' ' -f1)" "$file"
done
exit "$status"
