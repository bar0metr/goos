#!/bin/sh
# Checks that no big kernel lock comes back (ADR 0184). Kernel calls, faults,
# and housekeeping take domain locks in level order; a lock that serializes
# the whole object layer would undo that. The check refuses the names the
# removed lock had and any new lock level or entry lock of that shape:
#
#   KernelLock, KernelUnlock, HoldsKernelLock, kernelLock, LevelKernel,
#   and a goos_kernel_enter that takes any lock but the scheduler's.
#
# Usage: no_kernel_lock.sh [root]   (default: the repository root)
#        no_kernel_lock.sh --self-test
set -eu

pattern='\bKernelLock\b|\bKernelUnlock\b|\bHoldsKernelLock\b|\bkernelLock\b|\bLevelKernel\b|\bbigLock\b|\bLevelBig\b'

check() {
	root=$1
	status=0
	found=$(cd "$root" && grep -R -nE --include='*.go' --include='*.S' "$pattern" kernel internal 2>/dev/null || true)
	if [ -n "$found" ]; then
		printf '%s\n' "$found" | sed 's/^/kernel lock reintroduced: /' >&2
		status=1
	fi
	# The entry path checks whether the task is stopping and clears its
	# stale wake under the scheduler lock; it takes no other lock.
	enter=$(cd "$root" && find kernel -name '*.go' -exec awk '/^func kernelEnter\(/,/^}/' {} + 2>/dev/null || true)
	if printf '%s\n' "$enter" | grep -E '\.(Lock|TryLock|LockUnless|RLock)\(' | grep -qv 'schedulerLock\.Lock()'; then
		echo "kernel lock reintroduced: kernelEnter takes a lock" >&2
		status=1
	fi
	return $status
}

if [ "${1:-}" = "--self-test" ]; then
	scratch=$(mktemp -d)
	trap 'rm -rf "$scratch"' EXIT
	mkdir -p "$scratch/kernel/arch" "$scratch/internal"
	printf 'package arch\n\nfunc kernelEnter() uint64 { return 1 }\n' > "$scratch/kernel/arch/enter.go"
	check "$scratch" 2>/dev/null || { echo "kernel lock self-test: a clean tree was refused" >&2; exit 1; }
	for planted in 'var kernelLock int' 'func KernelLock() {}' 'const LevelKernel = 1'; do
		printf 'package arch\n\n%s\n' "$planted" > "$scratch/kernel/arch/planted.go"
		if check "$scratch" 2>/dev/null; then
			echo "kernel lock self-test: missed $planted" >&2
			exit 1
		fi
	done
	rm "$scratch/kernel/arch/planted.go"
	printf 'package arch\n\nfunc kernelEnter() uint64 {\n\tglobal.Lock()\n\treturn 1\n}\n' > "$scratch/kernel/arch/enter.go"
	if check "$scratch" 2>/dev/null; then
		echo "kernel lock self-test: missed a lock in kernelEnter" >&2
		exit 1
	fi
	echo "PASS kernel lock self-test"
	exit 0
fi

check "${1:-$(dirname "$0")/../..}"
