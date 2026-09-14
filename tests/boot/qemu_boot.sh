#!/bin/sh
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
log=build/logs/boot.log
mkdir -p build/logs
rm -f "$log"

qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-cdrom "$iso" -display none -serial "file:$log" -no-reboot -no-shutdown &
pid=$!

elapsed=0
while [ "$elapsed" -lt 20 ]; do
	if grep -q 'BOOT_SERVICES_EXITED' "$log" 2>/dev/null && \
		grep -q 'PMM_OK' "$log" 2>/dev/null && \
		grep -q 'NX_WX_OK' "$log" 2>/dev/null && \
		grep -q 'VM_OK' "$log" 2>/dev/null && \
		grep -q 'EXCEPTION_OK' "$log" 2>/dev/null && \
		grep -q 'GDT_OK' "$log" 2>/dev/null && \
		grep -q 'IDT_OK' "$log" 2>/dev/null && \
		grep -q 'TIMER_OK' "$log" 2>/dev/null && \
		grep -q 'STACK_GUARDS_OK' "$log" 2>/dev/null && \
		grep -q 'TASK_BLOCK_OK' "$log" 2>/dev/null && \
		grep -q 'TASK_WAKE_OK' "$log" 2>/dev/null && \
		grep -q 'TASK_A_OK' "$log" 2>/dev/null && \
		grep -q 'TASK_B_OK' "$log" 2>/dev/null && \
		grep -q 'IDLE_TASK_OK' "$log" 2>/dev/null && \
		grep -q 'SCHEDULER_OK' "$log" 2>/dev/null && \
		grep -q 'HANDLE_TABLE_OK' "$log" 2>/dev/null && \
		grep -q 'PROCESS_MODEL_OK' "$log" 2>/dev/null && \
		grep -q 'ADDRESS_SPACE_A_OK' "$log" 2>/dev/null && \
		grep -q 'ADDRESS_SPACE_B_OK' "$log" 2>/dev/null && \
		grep -q 'PROCESS_ISOLATION_OK' "$log" 2>/dev/null && \
		grep -q 'USER_RX_OK' "$log" 2>/dev/null && \
		grep -q 'USER_STACK_NX_OK' "$log" 2>/dev/null && \
		grep -q 'USER_RING3_OK' "$log" 2>/dev/null && \
		grep -q 'NATIVE_PROCESS_OK' "$log" 2>/dev/null && \
		grep -q 'BOOT_OK' "$log" 2>/dev/null; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		printf 'boot test passed\n'
		exit 0
	fi
	sleep 1
	elapsed=$((elapsed + 1))
done

kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
printf 'boot test timed out; serial output follows:\n' >&2
cat "$log" >&2
exit 1
