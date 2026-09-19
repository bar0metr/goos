#!/bin/sh
set -eu

log=build/logs/boot.log
reboot_log=build/logs/reboot.log
pid=
checks=0

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		pid=
	fi
}

print_context() {
	file=$1
	anchor=$2
	match=$(grep -n -F "$anchor" "$file" 2>/dev/null | sed -n '1p' || true)
	case "$match" in
	*:*)
		line=${match%%:*}
		start=$((line > 8 ? line - 8 : 1))
		end=$((line + 12))
		sed -n "${start},${end}p" "$file" >&2
		;;
	*) tail -n 40 "$file" >&2 || true ;;
	esac
}

fail_check() {
	name=$1
	expected=$2
	observed=$3
	file=$4
	anchor=$5
	printf 'FAIL %s\n  expected: %s\n  observed: %s\n  log: %s\n' "$name" "$expected" "$observed" "$file" >&2
	printf '  context:\n' >&2
	print_context "$file" "$anchor"
	exit 1
}

require_marker() {
	name=$1
	marker=$2
	file=$3
	anchor=${4-$marker}
	grep -F -q "$marker" "$file" 2>/dev/null || fail_check "$name" "$marker" 'marker not found' "$file" "$anchor"
	checks=$((checks + 1))
}

require_regex() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	grep -q "$pattern" "$file" 2>/dev/null || fail_check "$name" "$expected" 'marker not found' "$file" "$anchor"
	checks=$((checks + 1))
}

forbid_regex() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	if grep -q "$pattern" "$file" 2>/dev/null; then
		observed=$(grep "$pattern" "$file" 2>/dev/null | sed -n '1p')
		fail_check "$name" "$expected" "$observed" "$file" "$anchor"
	fi
	checks=$((checks + 1))
}

require_count() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	observed=$(grep -c "$pattern" "$file" 2>/dev/null || true)
	[ "$observed" -eq "$expected" ] || fail_check "$name" "$expected" "$observed" "$file" "$anchor"
	checks=$((checks + 1))
}

require_min_count() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	observed=$(grep -c "$pattern" "$file" 2>/dev/null || true)
	[ "$observed" -ge "$expected" ] || fail_check "$name" "at least $expected" "$observed" "$file" "$anchor"
	checks=$((checks + 1))
}

wait_for_marker() {
	phase=$1
	marker=$2
	timeout=$3
	file=$4
	elapsed=0
	while [ "$elapsed" -lt "$timeout" ]; do
		grep -q "$marker" "$file" 2>/dev/null && return
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail_check "timeout.$phase" "$marker within ${timeout}s" "timeout after ${elapsed}s" "$file" "$marker"
}

pass_group() { printf 'PASS %s\n' "$1"; }

trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

mkdir -p build/logs

if [ "${1-}" = '--self-test-failure' ]; then
	self_log=build/logs/harness-selftest.log
	printf '%s\n' 'GoOS harness diagnostic fixture' 'goos> echo expected-command' 'unrelated output' >"$self_log"
	require_marker harness.simulated missing-marker "$self_log" 'goos> echo expected-command'
	exit 0
fi

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=build/virtio-blk.img
rm -f "$log" "$reboot_log"

( sleep 4
  printf 'help\n'
  sleep 1
	printf 'echo "hello structured world"\n'
	sleep 1
	printf 'args "alpha beta" escaped\\ value\n'
	sleep 1
	printf 'readline\n'
	sleep 1
	printf 'bounded stdin payload\n'
	sleep 1
	printf 'echo piped-native-data | readline\n'
	sleep 1
  printf 'echo sequence-first; echo sequence-pipe | readline; echo sequence-last\n'
  sleep 1
  printf 'cd "/system"; pwd; cd /\n'
  sleep 1
  printf 'cat /missing; echo sequence-after-failure\n'
  sleep 1
  printf 'echo sequence-must-not-run; echo "unterminated\n'
  sleep 1
  printf 'echo "quoted;separator"; echo escaped\\;separator\n'
  sleep 1
  printf 'echo pipeline-four-data | readline | readline | readline\n'
  sleep 1
  printf 'readline | missing-stage; echo pipeline-last-failure-recovered\n'
  sleep 1
  printf 'echo pipeline-must-not-run | missing-stage | readline; echo pipeline-middle-failure-recovered\n'
  sleep 1
  printf 'missing-stage | readline | readline; echo pipeline-first-failure-recovered\n'
  sleep 1
  count=0
  while [ "$count" -lt 20 ]; do
    printf 'echo pipeline-reuse | readline | readline | readline\n'
    sleep 0.3
    printf 'missing-stage | readline | readline\n'
    sleep 0.3
    count=$((count + 1))
  done
  printf 'echo pipeline-stress-complete\n'
  sleep 1
	printf 'echo redirected-native-data > /tmp/redirected.txt\n'
	sleep 1
	printf 'cat /tmp/redirected.txt\n'
	sleep 1
	printf 'echo append-first > /tmp/appended.txt\n'
	sleep 1
	printf 'echo append-second >> /tmp/appended.txt\n'
	sleep 1
	printf 'cat /tmp/appended.txt\n'
	sleep 1
  printf 'clear\n'
  sleep 1
  printf 'uptime\n'
	sleep 1
	printf 'pwd\n'
	sleep 1
	printf 'ls /\n'
	sleep 1
	printf 'ls system\n'
	sleep 1
	printf 'cd /system\n'
	sleep 1
	printf 'ls\n'
	sleep 1
	printf 'pwd\n'
	sleep 1
	printf 'cat motd\n'
	sleep 1
	printf 'cp /system/motd /persist/state.txt\n'
	sleep 1
	printf 'cat /persist/state.txt\n'
	sleep 1
	printf 'mkdir /tmp/demo\n'
	sleep 1
	printf 'touch /tmp/demo/file\n'
	sleep 1
	printf 'stat /tmp/demo/file\n'
	sleep 1
	printf 'cp /system/motd /tmp/demo/copy\n'
	sleep 1
	printf 'cat /tmp/demo/copy\n'
	sleep 1
	printf 'mv /tmp/demo/file /tmp/demo/moved\n'
	sleep 1
	printf 'rm /tmp/demo/moved\n'
	sleep 1
	printf 'uname\n'
	sleep 1
	printf 'ps\n'
	sleep 1
	printf 'mem\n'
	sleep 1
	printf 'uptime\n'
	sleep 1
	printf 'sleep 1\n'
	sleep 1
	printf 'cat /missing\n'
	count=0
	while [ "$count" -lt 30 ]; do
		sleep 0.2
		printf 'ls\n'
		sleep 0.2
		printf 'cat motd\n'
		count=$((count + 1))
	done
	sleep 1
	printf 'echo arena-stress-complete\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$log" 2>&1 &
pid=$!

wait_for_marker boot '^arena-stress-complete$' 120 "$log"
cleanup

for marker in BOOT_SERVICES_EXITED EXCEPTION_OK GDT_OK IDT_OK TIMER_OK NATIVE_IMAGE_LOADER_OK BOOT_OK; do
	require_marker "boot.$marker" "$marker" "$log"
done
pass_group boot

for marker in PMM_OK VM_OK NX_WX_OK STACK_GUARDS_OK ADDRESS_SPACE_A_OK ADDRESS_SPACE_B_OK PROCESS_ISOLATION_OK USER_RX_OK USER_STACK_NX_OK USER_COPYOUT_OK USER_COPYIN_OK USER_FAULT_ISOLATION_OK; do
	require_marker "memory-security.$marker" "$marker" "$log"
done
pass_group memory-security

for marker in TASK_BLOCK_OK TASK_WAKE_OK TASK_A_OK TASK_B_OK IDLE_TASK_OK SCHEDULER_OK PROCESS_MODEL_OK PROCESS_WAIT_OK PROCESS_HANDLE_WAIT_OK PROCESS_RECLAIM_OK USER_PREEMPT_OK PROCESS_CR3_SCHED_OK; do
	require_marker "scheduler-process.$marker" "$marker" "$log"
done
pass_group scheduler-process

for marker in HANDLE_TABLE_OK KCALL_ABI_OK IPC_WIRE_OK IPC_REQUEST_REPLY_OK IPC_HANDLE_TRANSFER_OK IPC_BOUNDED_OK IPC_PEER_DEATH_OK SHMEM_OBJECT_OK SHMEM_ZERO_COPY_OK SHMEM_RIGHTS_OK STREAM_ENDPOINT_OK TTY_CAPABILITY_OK; do
	require_marker "ipc-capabilities.$marker" "$marker" "$log"
done
pass_group ipc-capabilities

for marker in INIT_PROCESS_OK INITRAMFS_DISCOVERY_OK PID1_INIT_OK PROCESS_EXIT_EVENT_OK SUPERVISOR_CRASH_EVENT_OK INIT_LIFECYCLE_IPC_OK INIT_SERVICE_LAUNCH_OK INIT_SERVICE_STARTED_OK USERSPACE_SUPERVISOR_OK LIFECYCLE_IPC_OK INIT_EXECUTABLE_LOOKUP_OK INIT_CAPABILITY_MANIFEST_OK NATIVE_STARTUP_ABI_OK PID1_SERVICE_LAUNCH_OK USER_DEBUG_WRITE_OK TTY_SERVICE_OK TTY_RIGHTS_OK USER_EXIT_OK USER_RING3_OK NATIVE_PROCESS_OK; do
	require_marker "userspace.$marker" "$marker" "$log"
done
pass_group userspace

for marker in BLOCK_SERVICE_OK BLOCK_SERVICE_IO_OK PCI_DISCOVERY_OK VIRTIO_BLK_CAPABILITIES_OK VIRTIO_BLK_DRIVER_OK VIRTIO_BLK_IRQ_OK; do
	require_marker "storage.$marker" "$marker" "$log"
done
pass_group storage

for marker in RAMFS_PROTOCOL_OK RAMFS_METADATA_OK RAMFS_IO_OK RAMFS_BOUNDS_OK VFS_NAMESPACE_OK VFS_MOUNT_OK VFS_PATH_OK VFS_CACHE_OK VFS_PROTOCOL_OK VFS_SERVICE_OK FAT32_MOUNT_OK; do
	require_marker "filesystem.$marker" "$marker" "$log"
done
require_marker filesystem.persisted-content 'Welcome to GoOS' "$log" 'cat /persist/state.txt'
pass_group filesystem

for marker in SHELL_PROCESS_OK SHELL_VFS_CONNECTED_OK SHELL_HELP_OK ECHO_PROCESS_OK ARGS_PROCESS_OK ARGV_ENV_PROCESS_OK STDIN_HANDLE_OK STDERR_HANDLE_OK STDIN_READ_OK SHELL_CLEAR_OK UPTIME_PROCESS_OK PWD_PROCESS_OK LS_PROCESS_OK SHELL_CD_OK CAT_PROCESS_OK MKDIR_PROCESS_OK TOUCH_PROCESS_OK STAT_PROCESS_OK CP_PROCESS_OK MV_PROCESS_OK RM_PROCESS_OK UNAME_PROCESS_OK PS_PROCESS_OK MEM_PROCESS_OK SLEEP_PROCESS_OK EXIT_STATUS_PROPAGATION_OK; do
	require_marker "shell.$marker" "$marker" "$log"
done
require_marker shell.echo-output 'hello structured world' "$log" 'echo "hello structured world"'
require_marker shell.argv-output 'alpha beta escaped value' "$log" 'args "alpha beta"'
require_marker shell.path-environment 'PATH=/system/bin' "$log" 'args "alpha beta"'
require_marker shell.stdin-output 'bounded stdin payload' "$log" readline
require_regex shell.cwd '^/system$' '/system on its own line' "$log" 'cd "/system"'
pass_group shell

require_marker pipeline.exec PIPELINE_EXEC_OK "$log"
require_marker pipeline.data piped-native-data "$log" 'echo piped-native-data'
for marker in sequence-first sequence-pipe sequence-last sequence-after-failure 'exec: invalid command sequence' 'quoted;separator' 'escaped;separator' pipeline-four-data pipeline-last-failure-recovered pipeline-middle-failure-recovered pipeline-first-failure-recovered; do
	require_regex "pipeline.$marker" "^${marker}$" "$marker on its own line" "$log" "$marker"
done
forbid_regex pipeline.rejected-sequence '^sequence-must-not-run$' 'sequence-must-not-run absent' "$log" sequence-must-not-run
forbid_regex pipeline.failed-stage-output '^pipeline-must-not-run$' 'pipeline-must-not-run absent' "$log" pipeline-must-not-run
pass_group pipeline

require_count pipeline-stress.reuse '^pipeline-reuse$' 20 "$log" pipeline-reuse
require_count pipeline-stress.expected-failures '^exec: launch failed: 2$' 23 "$log" 'exec: launch failed: 2'
forbid_regex pipeline-stress.launch-errors 'APP_LAUNCH_.*FAILED' 'no APP_LAUNCH_* failures' "$log" APP_LAUNCH_
require_regex pipeline-stress.complete '^pipeline-stress-complete$' 'pipeline-stress-complete on its own line' "$log" pipeline-stress-complete
require_min_count process-reuse.cat '^CAT_PROCESS_OK$' 31 "$log" CAT_PROCESS_OK
require_min_count process-reuse.exec '^SHELL_EXEC_OK$' 66 "$log" SHELL_EXEC_OK
require_marker process-reuse.arena arena-stress-complete "$log"
pass_group pipeline-stress

require_regex redirection.data '^redirected-native-data$' 'redirected-native-data on its own line' "$log" 'cat /tmp/redirected.txt'
require_regex redirection.append-first '^append-first$' 'append-first on its own line' "$log" 'cat /tmp/appended.txt'
require_regex redirection.append-second '^append-second$' 'append-second on its own line' "$log" 'cat /tmp/appended.txt'
require_marker redirection.writer WRITE_PROCESS_OK "$log" 'echo redirected-native-data'
require_marker redirection.shell REDIRECTION_EXEC_OK "$log" 'echo redirected-native-data'
pass_group redirection

value=$(dd if="$disk" bs=1 skip=4093 count=6 2>/dev/null)
[ "$value" = GBLKIO ] || fail_check persistence.block-bytes GBLKIO "$value" "$log" BLOCK_SERVICE_IO_OK
checks=$((checks + 1))
grep -a -q 'Welcome to GoOS' "$disk" || fail_check persistence.fat32-image 'Welcome to GoOS in disk image' 'content not found' "$log" 'cat /persist/state.txt'
checks=$((checks + 1))

( sleep 4
	printf 'cat /persist/state.txt\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$reboot_log" 2>&1 &
pid=$!

wait_for_marker reboot '^CAT_PROCESS_OK$' 60 "$reboot_log"
cleanup
require_marker persistence.reboot-boot BOOT_OK "$reboot_log"
require_marker persistence.reboot-mount FAT32_MOUNT_OK "$reboot_log"
require_marker persistence.reboot-shell SHELL_VFS_CONNECTED_OK "$reboot_log"
require_marker persistence.reboot-content 'Welcome to GoOS' "$reboot_log" 'cat /persist/state.txt'
require_marker persistence.reboot-cat CAT_PROCESS_OK "$reboot_log" 'cat /persist/state.txt'
forbid_regex persistence.reboot-no-copy CP_PROCESS_OK 'CP_PROCESS_OK absent on reboot' "$reboot_log" CP_PROCESS_OK
pass_group persistence

printf '\nPASS qemu-boot (%s checks)\n' "$checks"
