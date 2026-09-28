#!/bin/sh
# Checks that unsafe code stays inside its audited packages (ADR 0095).
#
#   kernel, internal: "unsafe" only under the uefi, vm, amd64, and framebuffer
#   directories.
#   userspace: "unsafe", //go:linkname, and //go:extern only under
#   userspace/runtime, the layer that owns the native ABI and the allocator.
#
# Usage: unsafe_boundary.sh [root]   (default: the repository root)
#        unsafe_boundary.sh --self-test
set -eu

check() {
	root=$1
	status=0
	kernel=$(cd "$root" && grep -R --include='*.go' -ln '"unsafe"' kernel internal \
		--exclude-dir=uefi --exclude-dir=vm --exclude-dir=amd64 --exclude-dir=framebuffer 2>/dev/null || true)
	userspace=$(cd "$root" && grep -R --include='*.go' -lE '"unsafe"|^//go:linkname|^//go:extern' userspace 2>/dev/null |
		grep -v '^userspace/runtime/' || true)
	for file in $kernel $userspace; do
		echo "unsafe outside its audited packages: $file" >&2
		status=1
	done
	return $status
}

if [ "${1:-}" = "--self-test" ]; then
	scratch=$(mktemp -d)
	trap 'rm -rf "$scratch"' EXIT
	mkdir -p "$scratch/kernel/ok" "$scratch/kernel/framebuffer" "$scratch/userspace/runtime/sys" "$scratch/userspace/cmd/app" "$scratch/internal"
	printf 'package framebuffer\n\nimport "unsafe"\n' > "$scratch/kernel/framebuffer/framebuffer.go"
	printf 'package sys\n\nimport "unsafe"\n' > "$scratch/userspace/runtime/sys/sys.go"
	printf 'package main\n' > "$scratch/userspace/cmd/app/main.go"
	check "$scratch" 2>/dev/null || { echo "unsafe boundary self-test: a clean tree was refused" >&2; exit 1; }
	for planted in 'import "unsafe"' '//go:linkname f goos_f' '//go:extern _heap_start'; do
		printf 'package main\n\n%s\n' "$planted" > "$scratch/userspace/cmd/app/main.go"
		if check "$scratch" 2>/dev/null; then
			echo "unsafe boundary self-test: missed $planted" >&2
			exit 1
		fi
	done
	printf 'package main\n' > "$scratch/userspace/cmd/app/main.go"
	printf 'package ok\n\nimport "unsafe"\n' > "$scratch/kernel/ok/ok.go"
	if check "$scratch" 2>/dev/null; then
		echo "unsafe boundary self-test: missed kernel unsafe" >&2
		exit 1
	fi
	echo "PASS unsafe boundary self-test"
	exit 0
fi

check "${1:-$(dirname "$0")/../..}"
