#!/bin/sh
set -eu

log=build/logs/boot.log
reboot_log=build/logs/reboot.log
m7_log=build/logs/m7-reboot.log
corrupt_log=build/logs/corrupt-volume.log
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
	printf 'echo pipeline-early-exit | readline | readline\n'
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
	printf 'ls /system/bin\n'
	sleep 1
	printf 'stat /system/bin/ls\n'
	sleep 1
	printf 'mkdir /system/backdoor\n'
	sleep 1
	printf 'mkdir /home/demo\n'
	sleep 1
	printf 'write /home/demo/message.txt "persistent"\n'
	sleep 1
	printf 'cat /home/demo/message.txt\n'
	sleep 1
	printf 'cat /persist/home/demo/message.txt\n'
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
	printf 'quota\n'
	sleep 1
	printf 'echo arrow\033[Akeys\n'
	sleep 1
	printf 'garbage-before-kill\025echo killed-line\n'
	sleep 1
	printf 'readline\n'
	sleep 1
	printf '\004'
	sleep 1
	printf 'echo after-end-of-input\n'
	sleep 1
	printf 'help cat\n'
	sleep 1
	printf 'ls --help\n'
	sleep 1
	printf 'cp /system/motd\n'
	sleep 1
	printf 'services now\n'
	sleep 1
	printf 'rm /home\n'
	sleep 1
	printf 'rm /tmp/demo\n'
	sleep 1
	printf 'ls /state\n'
	sleep 1
	printf 'cat /state/system.log\n'
	sleep 1
	printf 'config\n'
	sleep 1
	printf 'config set user.name boot-test\n'
	sleep 1
	printf 'config get user.name\n'
	sleep 1
	printf 'config set system.build.release 9.9.9\n'
	sleep 1
	printf 'config set system.log.persist maybe\n'
	sleep 1
	printf 'config get system.missing.key\n'
	sleep 1
	printf 'cat /state/config\n'
	sleep 1
	printf 'config set system.shell.prompt "cfg> "\n'
	sleep 1
	printf 'echo prompt-applied-live\n'
	sleep 1
	printf 'config set system.shell.prompt "goos> "\n'
	sleep 1
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
	printf 'mkdir /persist/long-directory-name\n'
	sleep 2
	printf 'write /persist/long-directory-name/startup-block-regression.txt "a long file name in a long directory"\n'
	sleep 2
	printf 'cat /persist/long-directory-name/startup-block-regression.txt\n'
	sleep 1
	printf 'ls /persist/long-directory-name\n'
	sleep 1
	printf 'ls /persist\n'
	sleep 1
	printf 'sleep 4 &\n'
	sleep 1
	printf 'sleep 30 &\n'
	sleep 1
	printf 'jobs\n'
	sleep 1
	printf 'services\n'
	sleep 1
	printf 'echo foreground while jobs run\n'
	sleep 1
	printf 'kill %%2\n'
	sleep 3
	printf 'sleep 30\n'
	sleep 2
	printf '\003'
	sleep 2
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

for marker in PMM_OK VM_OK NX_WX_OK STACK_GUARDS_OK ADDRESS_SPACE_A_OK ADDRESS_SPACE_B_OK PROCESS_ISOLATION_OK USER_RX_OK USER_STACK_NX_OK USER_COPYOUT_OK USER_COPYIN_OK USER_FAULT_ISOLATION_OK USER_STACK_GUARD_OK; do
	require_marker "memory-security.$marker" "$marker" "$log"
done
pass_group memory-security

for marker in TASK_BLOCK_OK TASK_WAKE_OK TASK_A_OK TASK_B_OK IDLE_TASK_OK SCHEDULER_OK PROCESS_MODEL_OK PROCESS_WAIT_OK PROCESS_HANDLE_WAIT_OK PROCESS_RECLAIM_OK USER_PREEMPT_OK PROCESS_CR3_SCHED_OK; do
	require_marker "scheduler-process.$marker" "$marker" "$log"
done
pass_group scheduler-process

for marker in HANDLE_TABLE_OK KCALL_ABI_OK IPC_WIRE_OK IPC_REQUEST_REPLY_OK IPC_HANDLE_TRANSFER_OK IPC_BOUNDED_OK IPC_PEER_DEATH_OK SHMEM_OBJECT_OK SHMEM_ZERO_COPY_OK SHMEM_RIGHTS_OK STREAM_ENDPOINT_OK STREAM_SHARED_ENDPOINT_OK TTY_CAPABILITY_OK; do
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
# The validation build kills the storage driver once, after the system is up.
# A driver is the component most likely to fail, so the path it takes when it
# does is exercised on every boot rather than only when hardware misbehaves.
require_marker storage.fault-injected BLOCK_DRIVER_FAULT_INJECTED "$log"
# Whether the block service observes the loss depends on whether a request was
# in flight when the driver died, so the replacement is asserted and the loss is
# not: the replacement is what recovery means.
require_marker storage.driver-replaced BLOCK_DRIVER_REPLACED_OK "$log"
require_count storage.driver-started '^VIRTIO_BLK_DRIVER_OK$' 2 "$log" BLOCK_DRIVER_FAULT_INJECTED
# The services either side of the driver stay up: a crash in one is not a
# cascade through the storage stack.
require_count storage.block-service-stable '^BLOCK_SERVICE_OK$' 1 "$log" BLOCK_SERVICE_OK
forbid_regex storage.no-block-restart 'service exited service=block' 'the block service did not restart' "$log" BLOCK_DRIVER_LOST_OK
pass_group storage

for marker in RAMFS_PROTOCOL_OK RAMFS_METADATA_OK RAMFS_IO_OK RAMFS_BOUNDS_OK VFS_NAMESPACE_OK VFS_MOUNT_OK VFS_PATH_OK VFS_CACHE_OK VFS_PROTOCOL_OK VFS_SERVICE_OK FAT32_MOUNT_OK; do
	require_marker "filesystem.$marker" "$marker" "$log"
done
require_marker filesystem.persisted-content 'Welcome to GoOS' "$log" 'cat /persist/state.txt'
require_marker filesystem.system-image-mounted SYSTEM_IMAGE_MOUNT_OK "$log"
# The namespace must name every mount, so the layout is discoverable by walking
# it rather than only by knowing a path in advance.
for entry in system tmp persist home state; do
	require_regex "filesystem.root-lists-$entry" "^$entry/\$" "$entry/ in the root listing" "$log" 'ls /'
done
# /system is the boot image itself: the executables the launch path resolves are
# the ones a listing shows, and nothing can write to them.
require_regex filesystem.system-lists-bin '^bin/$' 'bin/ under /system' "$log" 'ls system'
for entry in ls cat write; do
	require_regex "filesystem.system-bin-lists-$entry" "^$entry\$" "$entry in /system/bin" "$log" 'ls /system/bin'
done
# Commands are separate images described by a generated index; there is no
# multicall image behind them any more (ADR 0080).
forbid_regex filesystem.no-multicall-image '^coreutils$' 'no coreutils image' "$log" 'ls /system/bin'
require_regex filesystem.command-index '^commands$' 'the command index in /system' "$log" 'ls system'
require_marker filesystem.pid1-read-index INIT_COMMAND_INDEX_OK "$log"
require_regex filesystem.system-image-immutable '^mkdir: /system/backdoor: read-only filesystem$' 'a refused write to the system image' "$log" 'mkdir /system/backdoor'
# User data has its own mount on the persistent volume and is the same bytes
# under the whole-volume name.
require_regex filesystem.home-content '^persistent$' 'persistent through /home' "$log" 'cat /home/demo/message.txt'
require_regex filesystem.home-alias '^persistent$' 'persistent through /persist/home' "$log" 'cat /persist/home/demo/message.txt'
pass_group filesystem

for marker in SHELL_PROCESS_OK SHELL_VFS_CONNECTED_OK SHELL_HELP_OK ECHO_PROCESS_OK ARGS_PROCESS_OK ARGV_ENV_PROCESS_OK STDIN_HANDLE_OK STDERR_HANDLE_OK STDIN_READ_OK SHELL_CLEAR_OK UPTIME_PROCESS_OK PWD_PROCESS_OK LS_PROCESS_OK SHELL_CD_OK CAT_PROCESS_OK MKDIR_PROCESS_OK TOUCH_PROCESS_OK STAT_PROCESS_OK CP_PROCESS_OK MV_PROCESS_OK RM_PROCESS_OK UNAME_PROCESS_OK PS_PROCESS_OK MEM_PROCESS_OK SLEEP_PROCESS_OK EXIT_STATUS_PROPAGATION_OK; do
	require_marker "shell.$marker" "$marker" "$log"
done
require_marker shell.echo-output 'hello structured world' "$log" 'echo "hello structured world"'
require_marker shell.argv-output 'alpha beta escaped value' "$log" 'args "alpha beta"'
require_marker shell.path-environment 'PATH=/system/bin' "$log" 'args "alpha beta"'
# A line typed while a command runs is that command's input: it is echoed once
# and printed once more by readline, and never reaches the shell as a command.
require_count shell.stdin-output '^bounded stdin payload$' 2 "$log" 'goos> readline'
forbid_regex shell.stdin-not-a-command '^bounded: command not found$' 'typed input reached the command, not the shell' "$log" 'goos> readline'
require_regex shell.cwd '^/system$' '/system on its own line' "$log" 'cd "/system"'
pass_group shell

require_regex help.list '^Commands in /system/bin:$' 'help lists the executables' "$log" 'goos> help'
require_marker help.command SHELL_HELP_COMMAND_OK "$log" 'help cat'
require_regex help.command-usage '^usage: cat FILE$' 'help cat prints the usage' "$log" 'help cat'
require_regex help.utility-flag '^usage: ls \[PATH\]$' 'ls --help prints the usage' "$log" 'ls --help'
require_regex help.utility-arity '^usage: cp SOURCE TARGET$' 'cp with one path prints the usage' "$log" 'cp /system/motd'
require_regex help.builtin-arity '^usage: services$' 'a builtin with arguments prints the usage' "$log" 'services now'
require_regex errors.missing-file '^cat: /missing: no such file or directory$' 'a missing file named with its reason' "$log" 'cat /missing'
require_regex errors.busy-mount '^rm: /home: a mount depends on it$' 'a mount point refused as busy' "$log" 'rm /home'
require_regex errors.not-empty '^rm: /tmp/demo: directory not empty$' 'a non-empty directory refused as such' "$log" 'rm /tmp/demo'
require_regex format.uptime '^up [0-9][0-9]*:[0-9][0-9]:[0-9][0-9]$' 'uptime as hours, minutes and seconds' "$log" uptime
require_regex format.mem '^total: [0-9][0-9]*\.[0-9] [KMG]iB$' 'memory in binary units' "$log" 'goos> mem'
pass_group help

# The terminal service edits the line (ADR 0082): an arrow key's escape
# sequence is not typed as text, Ctrl-U discards the line, and Ctrl-D ends a
# command's input instead of reaching it as a byte.
require_marker terminal.service TTY_TERMINAL_OK "$log"
require_marker terminal.line TTY_LINE_OK "$log"
require_marker terminal.shell-line SHELL_TERMINAL_LINE_OK "$log"
# Output goes through the terminal service too (ADR 0089): it relays what the
# shell and every command write, and the shell cannot read that stream back.
require_marker terminal.output-relayed TTY_OUTPUT_OK "$log"
require_marker terminal.output-write-only TERMINAL_STREAM_READ_DENIED_OK "$log"
require_regex terminal.escape-skipped '^arrowkeys$' 'an arrow key typed no text' "$log" 'echo arrow'
require_regex terminal.kill-line '^killed-line$' 'the line after Ctrl-U runs alone' "$log" 'killed-line'
forbid_regex terminal.kill-line-discarded 'garbage-before-kill.*not found' 'the killed text never ran' "$log" 'killed-line'
require_regex terminal.end-of-input '^after-end-of-input$' 'the shell continues after Ctrl-D ended a command' "$log" 'after-end-of-input'
pass_group terminal

# Relaunching an image maps its code from the shared cache (ADR 0081).
require_marker boot.image-pages-shared IMAGE_PAGES_SHARED_OK "$log"
require_marker memory-security.image-pages-isolated IMAGE_PAGES_ISOLATED_OK "$log"
require_marker pipeline.exec PIPELINE_EXEC_OK "$log"
require_marker pipeline.data piped-native-data "$log" 'echo piped-native-data'
# Every stage holds a copy of the terminal's writing end for its diagnostics;
# the first stage exiting must not close it for the stage that prints last.
require_regex pipeline.early-exit '^pipeline-early-exit$' 'the last stage printed after the first exited' "$log" 'echo pipeline-early-exit'
for marker in sequence-first sequence-pipe sequence-last sequence-after-failure 'exec: invalid command sequence' 'quoted;separator' 'escaped;separator' pipeline-four-data pipeline-last-failure-recovered pipeline-middle-failure-recovered pipeline-first-failure-recovered; do
	require_regex "pipeline.$marker" "^${marker}$" "$marker on its own line" "$log" "$marker"
done
forbid_regex pipeline.rejected-sequence '^sequence-must-not-run$' 'sequence-must-not-run absent' "$log" sequence-must-not-run
forbid_regex pipeline.failed-stage-output '^pipeline-must-not-run$' 'pipeline-must-not-run absent' "$log" pipeline-must-not-run
pass_group pipeline

require_count pipeline-stress.reuse '^pipeline-reuse$' 20 "$log" pipeline-reuse
require_count pipeline-stress.expected-failures '^missing-stage: command not found$' 23 "$log" 'missing-stage: command not found'
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
	sleep 2
	printf 'cat /state/system.log.1\n'
	sleep 2
	printf 'ls /state\n'
	sleep 2
	printf 'config get user.name\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$reboot_log" 2>&1 &
pid=$!

wait_for_marker reboot '^boot-test$' 60 "$reboot_log"
cleanup
require_marker persistence.reboot-boot BOOT_OK "$reboot_log"
require_marker persistence.reboot-mount FAT32_MOUNT_OK "$reboot_log"
require_marker persistence.reboot-shell SHELL_VFS_CONNECTED_OK "$reboot_log"
require_marker persistence.reboot-content 'Welcome to GoOS' "$reboot_log" 'cat /persist/state.txt'
require_marker persistence.reboot-cat CAT_PROCESS_OK "$reboot_log" 'cat /persist/state.txt'
forbid_regex persistence.reboot-no-copy CP_PROCESS_OK 'CP_PROCESS_OK absent on reboot' "$reboot_log" CP_PROCESS_OK
# The rotated file can only have been written before this boot, so reading a
# record out of it proves the log survived the reset.
require_window persistence.reboot-log '^\[ *[0-9][0-9]*\.[0-9]*\] \(INFO\|WARN\|ERROR\) ' 'a formatted record from the previous session' "$reboot_log" 'goos> cat /state/system.log.1' 'goos> ls /state'
# The value set in the previous session is restored over the schema default.
require_window persistence.reboot-config '^boot-test$' 'a setting restored from the previous session' "$reboot_log" 'goos> config get user.name' 'NO_SUCH_LINE'
pass_group persistence

# The persistent log is a service, not a kernel feature: it drains the bounded
# kernel ring onto the volume so a session's records outlive its reset.
require_marker log.started LOG_SERVICE_OK "$log"
require_regex log.service-reported '^\[ OK \] log$' 'the log service reported started' "$log" 'Starting services'
require_regex log.file-listed '^system\.log$' 'system.log on the state volume' "$log" 'goos> ls /state'
require_regex log.rotated '^system\.log\.1$' 'a rotated log beside it' "$log" 'goos> ls /state'
require_window log.persisted-record '^\[ *[0-9][0-9]*\.[0-9]*\] \(INFO\|WARN\|ERROR\) ' 'a formatted record read back from the file' "$log" 'goos> cat /state/system.log' 'goos> config'
pass_group log

# Configuration is typed and scoped: a value is checked against the declaration
# its key carries, a key the schema does not declare is not a new setting, and
# what the running image publishes cannot be rewritten.
require_marker config.started CONFIG_SERVICE_OK "$log"
require_regex config.service-reported '^\[ OK \] config$' 'the config service reported started' "$log" 'Starting services'
require_regex config.read-only-listed '^system\.build\.release .* (read-only)$' 'the published release marked read-only' "$log" 'goos> config'
require_regex config.typed-listing '^system\.log\.persist true$' 'a typed value in the listing' "$log" 'goos> config'
require_window config.set-read-back '^boot-test$' 'a value read back after being set' "$log" 'goos> config get user.name' 'goos> config set system.build.release'
require_regex config.read-only-refused '^config: setting is read-only$' 'a write to a published value refused' "$log" 'goos> config set system.build.release 9.9.9'
require_regex config.type-refused '^config: value does not match the declared type$' 'a wrongly typed value refused' "$log" 'goos> config set system.log.persist maybe'
require_regex config.unknown-refused '^config: no such setting$' 'an undeclared key refused' "$log" 'goos> config get system.missing.key'
require_window config.exported '^user\.name text boot-test$' 'the exported form on the state volume' "$log" 'goos> cat /state/config' 'goos> config set system.shell.prompt'
forbid_window config.read-only-not-exported '^system\.build\.release' 'published values absent from the exported file' "$log" 'goos> cat /state/config' 'goos> config set system.shell.prompt'
# A setting is not configuration until something reads it. The shell owns the
# prompt, so changing it applies at once rather than at the next boot.
require_regex config.prompt-applied '^cfg> echo prompt-applied-live$' 'the new prompt in use' "$log" 'config set system.shell.prompt'
require_regex config.prompt-restored '^cfg> config set system.shell.prompt "goos> "$' 'the prompt set back' "$log" 'prompt-applied-live'
# Sessions are granted with different scopes and rights: the shell administers
# configuration, the log service only reads the one namespace that governs it.
require_regex config.session-admin 'config\[[0-9]*\]: session added rights=3' 'a read-write session' "$log" 'goos> log'
require_regex config.session-confined 'config\[[0-9]*\]: session added rights=1' 'a read-only session' "$log" 'goos> log'
pass_group config

require_regex release.ps-header '^PID   TYPE     STATE      MEM  CPU   COMMAND$' 'ps table header' "$log" 'goos> ps'
# Memory and CPU are accounted per process, so init must report pages of its own.
require_regex release.ps-accounting '^1     native   [a-z]*  *[1-9][0-9]*  *[0-9]' 'init with accounted pages' "$log" 'goos> ps'
require_marker quota.enforced QUOTA_OK "$log"
require_regex quota.header '^PID   COMMAND           RESOURCE  HELD/LIMIT$' 'quota table header' "$log" 'goos> quota'
# The shell is an ordinary process: its stream share must be a part of the
# table rather than all of it, or the quota bounds nothing.
require_regex quota.shell-streams '^[0-9][0-9]*  *shell  *strm  *[0-9]*/4$' 'the shell holding an ordinary stream share' "$log" 'goos> quota'
require_regex quota.init-streams '^1  *init  *strm  *[0-9]*/24$' 'PID 1 holding the system share' "$log" 'goos> quota'
require_regex release.ps-init '^1     native   [a-z]* *[0-9][0-9]*  *[0-9][0-9]*  *init$' 'PID 1 listed as init' "$log" 'goos> ps'
# The shell waits for input instead of retrying the read, so after a session of
# this length its CPU total stays in three digits. Before it was over seven
# thousand: the accounting is what makes the regression visible.
require_regex release.shell-idle '^[0-9][0-9]*  *native  *[a-z]*  *[0-9][0-9]*  *[0-9][0-9]\{0,2\}  *shell$' 'the shell idle rather than polling' "$log" 'goos> ps'
forbid_regex release.console-overrun 'console input dropped' 'no console input lost' "$log" 'goos> ps'
require_regex release.services 'vfs         running     [0-9]' 'vfs listed as running' "$log" 'goos> services'
# The shell holds no console capability at all: typed lines reach it from the
# terminal service (ADR 0082), what it prints goes back through that service on
# a write-only stream (ADR 0089), and reading the log is a capability of its own.
require_regex release.caps '^stdout    stream    write,wait$' 'shell terminal output, write only' "$log" 'goos> caps'
forbid_window release.caps-no-console '^console ' 'no console capability in the shell' "$log" 'goos> caps' 'goos> handles'
require_regex release.caps-terminal '^tty       channel   read,write,wait$' 'shell terminal session' "$log" 'goos> caps'
require_regex release.caps-log '^log       device    read$' 'shell log capability' "$log" 'goos> caps'
require_regex release.handles '^0x0000000100000001  stream' 'shell handle table' "$log" 'goos> handles'
require_regex release.log 'INFO  init\[1\]: service started service=vfs pid=' 'structured service start event' "$log" 'goos> log'
require_regex release.large-startup '^a startup block larger than the former 128-byte limit$' 'write with a startup block over 128 bytes' "$log" 'goos> cat /persist/startup.txt'
require_regex release.long-name-read '^a long file name in a long directory$' 'long name created and read back' "$log" 'goos> cat /persist/long-directory-name/startup-block-regression.txt'
require_window release.long-name-listed 'startup-block-regression\.txt' 'long file name in ls output' "$log" 'goos> ls /persist/long-directory-name' 'goos> ls /persist'
require_window release.long-directory-listed 'long-directory-name' 'long directory name in ls output' "$log" 'goos> ls /persist' 'goos> sleep 4 &'
require_regex jobs.started '^\[1\] started$' 'first background job accepted' "$log" 'goos> sleep 4 &'
require_regex jobs.second '^\[2\] started$' 'second background job accepted' "$log" 'goos> sleep 30 &'
require_regex jobs.list '^\[2\] running  sleep 30$' 'jobs lists a running background job' "$log" 'goos> jobs'
require_window jobs.foreground '^foreground while jobs run$' 'foreground command runs while a job is active' "$log" 'goos> echo foreground while jobs run' '[2] killed'
# A job's report arrives while the prompt is shown, so the terminal service
# erases the prompt line first: the report ends a row that began with it.
require_regex jobs.killed '\[2\] killed$' 'kill stops a background job' "$log" 'goos> kill %2'
require_regex jobs.done-1 '\[1\] done$' 'first background job reported' "$log" 'goos> sleep 4 &'
require_regex jobs.interrupt '^\^C$' 'Ctrl-C stops the foreground command' "$log" 'goos> sleep 30'
require_regex exec.large-pipeline '^pipeline request larger than one ipc message 0123456789abcdefghijklmnopqrstuvwxyz$' 'pipeline request over 256 bytes' "$log" 'readline | readline | readline'
pass_group release-shell

# The M7 acceptance runs inside one VM: write, reboot from the shell, read back.
# It uses /home because that is where the specification puts user data.
rm -f "$m7_log"
( sleep 6
	printf 'mkdir /home/m7\n'
	sleep 2
	printf 'write /home/m7/hello.txt "Hello from GoOS"\n'
	sleep 2
	printf 'cat /home/m7/hello.txt\n'
	sleep 2
	printf 'reboot\n'
	sleep 12
	printf 'cat /home/m7/hello.txt\n'
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
require_count m7.read-before-and-after '^Hello from GoOS$' 2 "$m7_log" 'cat /home/m7/hello.txt'
require_window m7.read-after-reboot '^Hello from GoOS$' 'file read back after the in-guest reboot' "$m7_log" 'Rebooting...' 'NO_SUCH_LINE'
pass_group m7-reboot

# A volume the filesystem refuses must not take the system with it. The image
# claims far more sectors than the device holds, which is the first geometry
# check a mount makes, and the machine has to come up to a prompt anyway: that
# prompt is where the volume gets looked at.
rm -f "$corrupt_log"
corrupt_disk=build/corrupt-virtio-blk.img
go run ./tools/mkfat32 "$corrupt_disk"
printf '\000\000\000\040' | dd of="$corrupt_disk" bs=1 seek=32 count=4 conv=notrunc 2>/dev/null
( sleep 6
	printf 'ls /\n'
	sleep 2
	printf 'echo volatile-root-still-works\n'
	sleep 2
	printf 'config get system.shell.prompt\n'
) | qemu-system-x86_64 -machine q35 -m 512M \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$corrupt_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$corrupt_log" 2>&1 &
pid=$!

wait_for_marker corrupt '^CONFIG_GET_OK$' 60 "$corrupt_log"
cleanup
require_marker corrupt.boot BOOT_OK "$corrupt_log"
require_marker corrupt.shell SHELL_VFS_CONNECTED_OK "$corrupt_log"
require_regex corrupt.reported '^\[WARN\] no persistent storage$' 'the refused volume reported' "$corrupt_log" 'Starting services'
forbid_regex corrupt.no-mount-marker '^FAT32_MOUNT_OK$' 'no mount claimed for a refused volume' "$corrupt_log" 'Starting services'
# The namespace tells the truth about what is there.
require_window corrupt.root-listed '^system/$' 'the system image still mounted' "$corrupt_log" 'ls /' 'LS_PROCESS_OK'
forbid_window corrupt.persist-absent '^persist/$' 'no persistent mounts listed' "$corrupt_log" 'ls /' 'LS_PROCESS_OK'
require_regex corrupt.usable '^volatile-root-still-works$' 'the system usable without storage' "$corrupt_log" 'goos> echo volatile-root-still-works'
require_marker corrupt.config CONFIG_GET_OK "$corrupt_log" 'config get system.shell.prompt'
pass_group corrupt-volume

printf '\nPASS qemu-boot (%s checks)\n' "$checks"
