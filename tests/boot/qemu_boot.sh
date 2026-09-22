#!/bin/sh
set -eu

log=build/logs/boot.log
reboot_log=build/logs/reboot.log
m7_log=build/logs/m7-reboot.log
pid=
checks=0
monitor=${TMPDIR:-/tmp}/goos-boot-monitor.$$
heap_first=

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		pid=
	fi
	rm -f "$monitor"
}

# heap_symbol prints the address of runtime.heapptr in the running kernel.
heap_symbol() {
	read -r hz diag validation <build/config/build
	variant=build/variants/hz$hz
	if [ "$diag" = 1 ]; then
		variant=$variant-diag
	fi
	if [ "$validation" = 1 ]; then
		variant=$variant-validation
	fi
	awk '$3 == "runtime.heapptr" { print $1; exit }' "$variant/kernel/goos.sym" 2>/dev/null
}

# heap_pointer reads that address through the QEMU monitor. The kernel leaks
# every allocation (ADR 0065), so this must not move once the system is up.
heap_pointer() {
	[ -n "${1:-}" ] || return 0
	(printf 'x/1gx 0x%s\n' "$1"; sleep 0.4) | nc -U "$monitor" 2>/dev/null |
		tr -d '\r' | sed -n 's/^[0-9a-f][0-9a-f]*: \(0x[0-9a-f][0-9a-f]*\)$/\1/p' | tail -n 1
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

# window prints the log lines after the first line containing $2 and before the
# next line containing $3, so checks can be tied to a single command's output.
window() {
	awk -v start="$2" -v stop="$3" 'seen && index($0, stop) { exit } seen { print } !seen && index($0, start) { seen = 1 }' "$1"
}

require_window() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	start=$5
	stop=$6
	window "$file" "$start" "$stop" | grep -q "$pattern" || fail_check "$name" "$expected" 'marker not found' "$file" "$start"
	checks=$((checks + 1))
}

forbid_window() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	start=$5
	stop=$6
	observed=$(window "$file" "$start" "$stop" | grep "$pattern" | sed -n '1p' || true)
	[ -z "$observed" ] || fail_check "$name" "$expected" "$observed" "$file" "$start"
	checks=$((checks + 1))
}

count_window() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	start=$5
	stop=$6
	observed=$(window "$file" "$start" "$stop" | grep -c "$pattern" || true)
	[ "$observed" -eq "$expected" ] || fail_check "$name" "$expected" "$observed" "$file" "$start"
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
disk=${3:?test disk path required}
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
	printf 'echo input-redirection-data > /tmp/input.txt\n'
	sleep 1
	printf 'readline < /tmp/input.txt > /tmp/input-copy.txt\n'
	sleep 1
	printf 'cat /tmp/input-copy.txt\n'
	sleep 1
	printf 'echo stderr-probe 2> /tmp/stderr.txt\n'
	sleep 1
	printf 'cat /tmp/stderr.txt\n'
	sleep 1
	printf 'echo stderr-append 2>> /tmp/stderr.txt\n'
	sleep 1
	printf 'cat /tmp/stderr.txt\n'
	sleep 1
	printf 'echo stderr-sink-failure 2> /missing-dir/err.txt; echo stderr-sink-failure-recovered\n'
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
	printf 'cd /persist\n'
	sleep 1
	printf 'cat state.txt\n'
	sleep 1
	printf 'echo interactive-persist-data > tt.txt\n'
	sleep 1
	printf 'cat tt.txt\n'
	sleep 1
	printf 'cd /system\n'
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
	printf 'services\n'
	sleep 1
	printf 'caps\n'
	sleep 1
	printf 'handles\n'
	sleep 1
	printf 'log\n'
	sleep 1
	printf 'write /persist/startup.txt "a startup block larger than the former 128-byte limit"\n'
	sleep 2
	printf 'cat /persist/startup.txt\n'
	sleep 1
	printf 'sleep 500 &\n'
	sleep 1
	printf 'sleep 500 &\n'
	sleep 1
	printf 'services\n'
	sleep 1
	printf 'echo foreground while jobs run\n'
	sleep 5
	deep=/persist
	for dir in aaaaaaaa bbbbbbbb cccccccc dddddddd eeeeeeee; do
		deep=$deep/$dir
		printf 'mkdir %s\n' "$deep"
		sleep 2
	done
	printf 'cd /persist/aaaaaaaa/bbbbbbbb/cccccccc/dddddddd/eeeeeeee\n'
	sleep 1
	printf 'echo "pipeline request larger than one ipc message 0123456789abcdefghijklmnopqrstuvwxyz" | readline | readline | readline\n'
	sleep 3
	printf 'cd /system\n'
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
	-monitor "unix:$monitor,server,nowait" \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$log" 2>&1 &
pid=$!

heap_address=$(heap_symbol || true)
(
	waited=0
	while [ "$waited" -lt 600 ] && ! grep -q '^goos> ' "$log" 2>/dev/null; do
		sleep 0.2
		waited=$((waited + 1))
	done
	heap_pointer "$heap_address" >"$monitor.heap" 2>/dev/null || true
) &

wait_for_marker boot '^arena-stress-complete$' 180 "$log"
heap_last=$(heap_pointer "$heap_address" 2>/dev/null || true)
heap_first=$(cat "$monitor.heap" 2>/dev/null || true)
rm -f "$monitor.heap"
cleanup

if [ -n "$heap_first" ] && [ -n "$heap_last" ]; then
	[ "$heap_first" = "$heap_last" ] ||
		fail_check kernel-heap.stable "$heap_first (no kernel allocation after boot)" "$heap_last" "$log" 'goos> '
	checks=$((checks + 1))
else
	printf 'SKIP kernel-heap.stable (QEMU monitor unavailable)\n'
fi

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
require_regex redirection.input '^input-redirection-data$' 'input-redirection-data on its own line' "$log" 'cat /tmp/input-copy.txt'
require_regex redirection.persist-relative '^interactive-persist-data$' 'interactive-persist-data on its own line' "$log" 'cat tt.txt'
forbid_window redirection.stderr-diverted '^ECHO_PROCESS_OK$' 'echo diagnostics absent from console' "$log" 'echo stderr-probe 2> /tmp/stderr.txt' 'cat /tmp/stderr.txt'
require_window redirection.stderr-stdout-kept '^stderr-probe$' 'stdout still reaches console' "$log" 'echo stderr-probe 2> /tmp/stderr.txt' 'cat /tmp/stderr.txt'
require_window redirection.stderr-file '^ECHO_PROCESS_OK$' 'diagnostic read back from file' "$log" 'cat /tmp/stderr.txt' 'echo stderr-append'
forbid_window redirection.stderr-append-diverted '^ECHO_PROCESS_OK$' 'append diagnostics absent from console' "$log" 'echo stderr-append 2>> /tmp/stderr.txt' 'cat /tmp/stderr.txt'
count_window redirection.stderr-append-preserved '^ECHO_PROCESS_OK$' 2 "$log" 'echo stderr-append 2>> /tmp/stderr.txt' 'echo stderr-sink-failure'
require_regex redirection.stderr-sink-failure '^stderr-sink-failure-recovered$' 'shell recovers after failed stderr sink' "$log" 'echo stderr-sink-failure'
require_marker redirection.writer WRITE_PROCESS_OK "$log" 'echo redirected-native-data'
require_marker redirection.shell REDIRECTION_EXEC_OK "$log" 'echo redirected-native-data'
forbid_regex redirection.driver-oom 'VIRTIO_BLK_OUT_OF_MEMORY' 'no virtio-blk heap exhaustion' "$log" VIRTIO_BLK_OUT_OF_MEMORY
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

require_regex release.ps-header '^PID   TYPE     STATE      COMMAND$' 'ps table header' "$log" 'goos> ps'
require_regex release.ps-init '^1     native   [a-z]* *init$' 'PID 1 listed as init' "$log" 'goos> ps'
require_regex release.services 'vfs         running     [0-9]' 'vfs listed as running' "$log" 'goos> services'
require_regex release.caps '^console   device    read,write$' 'shell console capability' "$log" 'goos> caps'
require_regex release.handles '^0x0000000100000001  device' 'shell handle table' "$log" 'goos> handles'
require_regex release.log 'INFO  init\[1\]: service started service=vfs pid=' 'structured service start event' "$log" 'goos> log'
require_regex release.large-startup '^a startup block larger than the former 128-byte limit$' 'write with a startup block over 128 bytes' "$log" 'goos> cat /persist/startup.txt'
require_regex jobs.started '^\[1\] started$' 'first background job accepted' "$log" 'goos> sleep 500 &'
require_regex jobs.second '^\[2\] started$' 'second background job accepted' "$log" 'goos> sleep 500 &'
require_window jobs.foreground '^foreground while jobs run$' 'foreground command runs while jobs are active' "$log" 'goos> echo foreground while jobs run' '[1] done'
require_regex jobs.done-1 '^\[1\] done$' 'first background job reported' "$log" 'goos> sleep 500 &'
require_regex jobs.done-2 '^\[2\] done$' 'second background job reported' "$log" 'goos> sleep 500 &'
require_regex exec.large-pipeline '^pipeline request larger than one ipc message 0123456789abcdefghijklmnopqrstuvwxyz$' 'pipeline request over 256 bytes' "$log" 'readline | readline | readline'
pass_group release-shell

# The M7 acceptance runs inside one VM: write, reboot from the shell, read back.
rm -f "$m7_log"
( sleep 6
	printf 'mkdir /persist/demo\n'
	sleep 2
	printf 'write /persist/demo/hello.txt "Hello from GoOS"\n'
	sleep 2
	printf 'cat /persist/demo/hello.txt\n'
	sleep 2
	printf 'reboot\n'
	sleep 12
	printf 'cat /persist/demo/hello.txt\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-shutdown >"$m7_log" 2>&1 &
pid=$!

wait_for_marker m7 '^Rebooting\.\.\.$' 60 "$m7_log"
elapsed=0
until [ "$(grep -c '^Hello from GoOS$' "$m7_log" 2>/dev/null || true)" -ge 2 ] || [ "$elapsed" -ge 60 ]; do
	sleep 1
	elapsed=$((elapsed + 1))
done
cleanup
require_count m7.boots 'Kernel initialized' 2 "$m7_log" 'Rebooting...'
require_count m7.read-before-and-after '^Hello from GoOS$' 2 "$m7_log" 'cat /persist/demo/hello.txt'
require_window m7.read-after-reboot '^Hello from GoOS$' 'file read back after the in-guest reboot' "$m7_log" 'Rebooting...' 'NO_SUCH_LINE'
pass_group m7-reboot

printf '\nPASS qemu-boot (%s checks)\n' "$checks"
