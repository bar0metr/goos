#!/bin/sh
set -eu

missing=0
for tool in go tinygo make qemu-system-x86_64 hdiutil nm; do
	if command -v "$tool" >/dev/null 2>&1; then
		printf '[ OK ] %s\n' "$tool"
	else
		printf '[MISS] %s\n' "$tool" >&2
		missing=1
	fi
done

ovmf=${OVMF_CODE:-/opt/local/share/qemu/edk2-x86_64-code.fd}
if [ -f "$ovmf" ]; then
	printf '[ OK ] OVMF: %s\n' "$ovmf"
else
	printf '[MISS] OVMF firmware: %s\n' "$ovmf" >&2
	printf '       MacPorts: sudo port install qemu\n' >&2
	missing=1
fi

if [ "$missing" -ne 0 ]; then
	printf 'Toolchain incomplete. No packages were installed.\n' >&2
	exit 1
fi

printf 'GoOS toolchain is ready.\n'
