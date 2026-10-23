#!/bin/sh
# Check the linked native fixture and compiler IR, without booting a guest.
set -eu
elf=${1:?native ELF required}
ir=${2:?LLVM IR required}
output=${3:?evidence directory required}
mkdir -p "$output"
go tool nm "$elf" >"$output/symbols.txt"
go tool objdump -s 'goos_g_swap|goos_g_current_task|goos_preempt_entry' "$elf" >"$output/context.txt"

for symbol in goos_g_swap goos_g_entry goos_g_exit goos_g_current_task goos_g_preempt goos_worker_main goos_preempt_entry; do
	grep -Eq " [Tt] $symbol$" "$output/symbols.txt" || {
		printf 'FAIL compiler: missing native runtime symbol %s\n' "$symbol" >&2
		exit 1
	}
done
for hook in 'chanSend' 'chanRecv' 'chanSelect' 'chanClose' 'task.start' 'panicOrGoexit'; do
	grep -Fq "$hook" "$ir" || {
		printf 'FAIL compiler: missing emitted hook %s\n' "$hook" >&2
		exit 1
	}
done
# The assembly task lookup must read the physical M's TCB independently of SP.
grep -Eq 'FS:0x38|FS:56' "$output/context.txt" || {
	printf 'FAIL compiler: no FS-relative current-task lookup\n' >&2
	exit 1
}
# The overflow check must consume a new frame at every level, not an LLVM
# tail-recursion loop that would pass without exercising a guard page.
go tool objdump -s '^main.recurse$' "$elf" >"$output/overflow.txt"
grep -Eq 'CALL[[:space:]]+main\.recurse\(SB\)' "$output/overflow.txt" || {
	printf 'FAIL compiler: overflow fixture has no recursive call\n' >&2
	exit 1
}
printf 'GOCOMPILER_OK symbols=7 hooks=6 tls=fs overflow=recursive fixture=%s\n' "$elf"
