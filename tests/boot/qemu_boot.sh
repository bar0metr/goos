#!/bin/sh
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
log=build/logs/boot.log
mkdir -p build/logs
rm -f "$log"

( sleep 4
  printf 'help\n'
  sleep 1
  printf 'echo hello\n'
  sleep 1
  printf 'clear\n'
  sleep 1
  printf 'uptime\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$log" 2>&1 &
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
		grep -q 'IPC_WIRE_OK' "$log" 2>/dev/null && \
		grep -q 'IPC_REQUEST_REPLY_OK' "$log" 2>/dev/null && \
		grep -q 'IPC_HANDLE_TRANSFER_OK' "$log" 2>/dev/null && \
		grep -q 'TTY_CAPABILITY_OK' "$log" 2>/dev/null && \
		grep -q 'RAMFS_PROTOCOL_OK' "$log" 2>/dev/null && \
		grep -q 'RAMFS_METADATA_OK' "$log" 2>/dev/null && \
		grep -q 'RAMFS_IO_OK' "$log" 2>/dev/null && \
		grep -q 'RAMFS_BOUNDS_OK' "$log" 2>/dev/null && \
		grep -q 'VFS_NAMESPACE_OK' "$log" 2>/dev/null && \
		grep -q 'VFS_MOUNT_OK' "$log" 2>/dev/null && \
		grep -q 'VFS_PATH_OK' "$log" 2>/dev/null && \
		grep -q 'VFS_CACHE_OK' "$log" 2>/dev/null && \
		grep -q 'VFS_PROTOCOL_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_PROCESS_OK' "$log" 2>/dev/null && \
		grep -q 'INITRAMFS_DISCOVERY_OK' "$log" 2>/dev/null && \
		grep -q 'PID1_INIT_OK' "$log" 2>/dev/null && \
		grep -q 'PROCESS_EXIT_EVENT_OK' "$log" 2>/dev/null && \
		grep -q 'SUPERVISOR_CRASH_EVENT_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_LIFECYCLE_IPC_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_SERVICE_LAUNCH_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_SERVICE_STARTED_OK' "$log" 2>/dev/null && \
		grep -q 'USERSPACE_SUPERVISOR_OK' "$log" 2>/dev/null && \
		grep -q 'LIFECYCLE_IPC_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_EXECUTABLE_LOOKUP_OK' "$log" 2>/dev/null && \
		grep -q 'INIT_CAPABILITY_MANIFEST_OK' "$log" 2>/dev/null && \
		grep -q 'PID1_SERVICE_LAUNCH_OK' "$log" 2>/dev/null && \
		grep -q 'SHELL_PROCESS_OK' "$log" 2>/dev/null && \
		grep -q 'SHELL_HELP_OK' "$log" 2>/dev/null && \
		grep -q 'hello' "$log" 2>/dev/null && \
		grep -q 'SHELL_ECHO_OK' "$log" 2>/dev/null && \
		grep -q 'SHELL_CLEAR_OK' "$log" 2>/dev/null && \
		grep -q 'SHELL_UPTIME_OK' "$log" 2>/dev/null && \
		grep -q 'IPC_BOUNDED_OK' "$log" 2>/dev/null && \
		grep -q 'IPC_PEER_DEATH_OK' "$log" 2>/dev/null && \
		grep -q 'SHMEM_OBJECT_OK' "$log" 2>/dev/null && \
		grep -q 'SHMEM_ZERO_COPY_OK' "$log" 2>/dev/null && \
		grep -q 'SHMEM_RIGHTS_OK' "$log" 2>/dev/null && \
		grep -q 'ADDRESS_SPACE_A_OK' "$log" 2>/dev/null && \
		grep -q 'ADDRESS_SPACE_B_OK' "$log" 2>/dev/null && \
		grep -q 'PROCESS_ISOLATION_OK' "$log" 2>/dev/null && \
		grep -q 'USER_RX_OK' "$log" 2>/dev/null && \
		grep -q 'USER_STACK_NX_OK' "$log" 2>/dev/null && \
		grep -q 'USER_COPYOUT_OK' "$log" 2>/dev/null && \
		grep -q 'KCALL_ABI_OK' "$log" 2>/dev/null && \
		grep -q 'NATIVE_IMAGE_LOADER_OK' "$log" 2>/dev/null && \
		grep -q 'USER_COPYIN_OK' "$log" 2>/dev/null && \
		grep -q 'USER_DEBUG_WRITE_OK' "$log" 2>/dev/null && \
		grep -q 'TTY_SERVICE_OK' "$log" 2>/dev/null && \
		grep -q 'TTY_RIGHTS_OK' "$log" 2>/dev/null && \
		grep -q 'USER_EXIT_OK' "$log" 2>/dev/null && \
		grep -q 'USER_PREEMPT_OK' "$log" 2>/dev/null && \
		grep -q 'PROCESS_CR3_SCHED_OK' "$log" 2>/dev/null && \
		grep -q 'USER_FAULT_ISOLATION_OK' "$log" 2>/dev/null && \
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
