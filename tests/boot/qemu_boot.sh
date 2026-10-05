#!/bin/sh
set -eu

log=build/logs/boot.log
reboot_log=build/logs/reboot.log
m7_log=build/logs/m7-reboot.log
corrupt_log=build/logs/corrupt-volume.log
fsck_log=build/logs/recovery-fsck.log
partition_log=build/logs/partition-tables.log
stack_log=build/logs/kernel-stack-overflow.log
poweroff_log=build/logs/poweroff.log
poweroff_after_log=build/logs/poweroff-after.log
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
	read -r hz diag validation opt variant <build/config/build
	variant=build/variants/${variant:-hz$hz}
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

wait_for_count() {
	phase=$1
	pattern=$2
	expected=$3
	timeout=$4
	file=$5
	elapsed=0
	while [ "$elapsed" -lt "$timeout" ]; do
		observed=$(grep -c "$pattern" "$file" 2>/dev/null || true)
		[ "$observed" -ge "$expected" ] && return
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail_check "timeout.$phase" "$expected matches within ${timeout}s" "$observed matches after ${elapsed}s" "$file" "$pattern"
}

pass_group() { printf 'PASS %s\n' "$1"; }

# await_ready waits until the system writing log has settled, at most two
# minutes: within the Nth boot in log, a prompt has been printed and the
# validation build's storage driver fault at the end of the boot has been
# recovered from. The shell is read from the volume (ADR 0175) and can start
# again after that fault, so it then waits three seconds more for such a
# restart to finish. On a timeout it returns, and the marker the caller waits
# for next reports the failure.
await_ready() {
	waited=0
	while [ "$waited" -lt 240 ]; do
		if awk -v want="${2:-1}" '/BOOT_SERVICES_EXITED/ { boot++ } boot == want && /goos> / { prompt = 1 } boot == want && /BLOCK_DRIVER_REPLACED_OK/ { driver = 1 } END { exit !(prompt && driver) }' "$1" 2>/dev/null; then
			sleep 3
			return 0
		fi
		sleep 0.5
		waited=$((waited + 1))
	done
}

# await_count waits, for at most a minute, until the file has at least count
# lines matching pattern. A guest step that takes longer than usual, as on
# several emulated processors, then does not receive the next command early.
await_count() { # file pattern count
	waited=0
	while [ "$(grep -c "$2" "$1" 2>/dev/null || true)" -lt "$3" ] && [ "$waited" -lt 600 ]; do
		sleep 0.1
		waited=$((waited + 1))
	done
}

# guest_memory_mib prints the size a QEMU -m value names, in MiB. It accepts a
# whole number with an optional M or G suffix and fails on anything else.
guest_memory_mib() {
	number=${1%[MmGg]}
	case $number in '' | *[!0-9]*) return 1 ;; esac
	case $1 in
	*[Gg]) echo $((number * 1024)) ;;
	*) echo "$number" ;;
	esac
}

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
# MEMORY is the guest RAM, in QEMU -m syntax; the Makefile passes it.
memory=${MEMORY:-512M}
# SMP is the guest's processor count, as a QEMU -smp argument.
smp=${SMP:-1}
# An executable far larger than anything the build produces (ADR 0106): the
# validation build's vmprobe with three MiB of read-only data it never
# touches, put on the volume the guest mounts, so launching it proves a large
# program starts without being copied whole.
variant=$(cut -d' ' -f5 build/config/build)
# Every disk this script makes for itself is an installed one, made from the
# build's system tree as make test-disk makes the main one (ADR 0175).
system_tree=build/variants/$variant/system
go run ./tools/padimage "build/variants/$variant/userspace/bin/vmprobe" build/bigprobe 3145728
# The padded program exercises a source much larger than the pages it touches.
# Launch must not read the padding before the process starts.
go run ./tools/padimage "build/variants/$variant/userspace/bin/vmprobe" build/hugeprobe 12582912
go run ./tools/mkfat32 -put build/bigprobe:/home/tools/bigprobe -put build/hugeprobe:/home/tools/hugeprobe "$disk"
rm -f "$log" "$reboot_log"

# A command line of about 3.5 KiB with 700 arguments, and one argument of
# 3000 bytes: more than a message, the former 128-byte line, or the former
# eight-argument startup block could carry (ADR 0109).
many_arguments=$(awk 'BEGIN { for (i = 0; i < 700; i++) printf "%sw%d", (i ? " " : ""), i }')
long_argument=$(awk 'BEGIN { for (i = 0; i < 3000; i++) printf "x" }')
# A 255-byte ASCII name, the FAT32 unit limit, and a 200-byte directory name, so two
# levels make a path longer than a message (ADR 0110).
long_name=$(awk 'BEGIN { for (i = 0; i < 255; i++) printf "n" }')
# One byte past the native name limit, NameMax (ADR 0114).
too_long_name=$(awk 'BEGIN { for (i = 0; i < 1025; i++) printf "n" }')
long_directory=$(awk 'BEGIN { for (i = 0; i < 200; i++) printf "d" }')
( await_ready "$log"
  printf 'help\n'
  sleep 1
	printf 'echo "hello structured world"\n'
	sleep 1
	printf 'args "alpha beta" escaped\\ value\n'
	sleep 1
	printf 'args %s\n' "$many_arguments"
	sleep 2
	printf 'echo %s > /tmp/long-argument; stat /tmp/long-argument\n' "$long_argument"
	sleep 2
	printf 'touch /tmp/m1 /tmp/m2 /tmp/m3; rm /tmp/m1 /tmp/m2 /tmp/m3; echo many-paths-done\n'
	sleep 1
	printf 'mkdir /tmp/%s /tmp/%s/%s; echo deep-ramfs > /tmp/%s/%s/%s; cat /tmp/%s/%s/%s\n' "$long_directory" "$long_directory" "$long_directory" "$long_directory" "$long_directory" "$long_name" "$long_directory" "$long_directory" "$long_name"
	sleep 2
	printf 'mkdir /home/%s; echo deep-fat32 > /home/%s/%s; ls /home/%s; mv /home/%s/%s /home/%s/renamed-%s; cat /home/%s/renamed-%s\n' "$long_directory" "$long_directory" "$long_name" "$long_directory" "$long_directory" "$long_name" "$long_directory" "$long_directory" "$long_directory" "$long_directory"
	sleep 3
	printf 'touch /tmp/%s\n' "$too_long_name"
	sleep 1
	# Seven background pipelines at once, two VFS sessions each: more jobs
	# than the former four exec slots allowed, more sessions than the VFS's
	# former twelve, and more channels than the kernel's former 24 (ADR 0111).
	for job in 1 2 3 4 5 6 7; do
		printf 'sleep 4 | cat /system/motd | write /tmp/bg%s &\n' "$job"
		sleep 0.3
	done
	sleep 6
	printf 'cat /tmp/bg1 /tmp/bg4 /tmp/bg7\n'
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
	printf 'mount\n'
	sleep 1
	printf 'df\n'
	sleep 1
	printf 'gview /system/motd\n'
	sleep 1
	# PageDown, PageUp, Home, End, then F10 through the COM1 raw session.
	printf '\033[6~\033[5~\033[H\033[F\033[21~'
	sleep 1
	printf 'echo after-gview-serial\n'
	sleep 1
	# gfm runs gview and edit on the selected file through PID 1 (ADR 0130):
	# Home and Down select the file, F3 views it, PageDown and F10 return, F4
	# edits it, X is typed at its start, F2 saves through a replacement
	# (ADR 0129), and F10 twice leaves the editor and the manager.
	printf 'mkdir /home/fm\n'
	sleep 1
	printf 'write /home/fm/note.txt saved text\n'
	sleep 1
	printf 'cd /home/fm\n'
	sleep 1
	printf 'gfm\n'
	sleep 2
	printf '\033[H\033[B'
	sleep 1
	printf '\033[13~'
	sleep 2
	printf '\033[6~\033[21~'
	sleep 2
	printf '\033[14~'
	sleep 2
	printf 'X'
	sleep 1
	printf '\033[12~'
	sleep 2
	printf '\033[21~'
	sleep 2
	printf '\033[21~'
	sleep 2
	printf 'cat /home/fm/note.txt\n'
	sleep 1
	printf 'ls /home/fm\n'
	sleep 1
	printf 'cd /\n'
	sleep 1
	printf 'stat /system/bin/ls\n'
	sleep 1
	printf 'stat /system/bin\n'
	sleep 1
	printf 'stat -m /system\n'
	sleep 1
	printf 'stat -b /system/motd\n'
	sleep 1
	printf 'stat -x /system\n'
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
	# A program on the persistent volume runs from there: PID 1 reads it into
	# an executable object and the kernel launches the object (ADR 0097).
	printf 'cp /system/bin/args /home/demo/args\n'
	sleep 3
	printf '/home/demo/args file-exec "from the volume"\n'
	sleep 3
	printf 'cd /home/demo; ./args relative-file-exec; cd /\n'
	sleep 3
	printf '/home/demo/args piped-file-exec | readline\n'
	sleep 3
	printf '/home/tools/bigprobe\n'
	sleep 25
	# Bulk I/O (ADR 0107): a MiB through a shared buffer in sixteen
	# exchanges, to the volatile root and to the volume.
	bench_count=$(grep -c '^BENCH_PROCESS_OK$' "$log" 2>/dev/null || true)
	printf 'bench io /tmp/bulk.bin 1M\n'
	bench_count=$((bench_count + 1))
	wait_for_count bulk.tmp '^BENCH_PROCESS_OK$' "$bench_count" 60 "$log"
	printf 'bench io /home/bulk.bin 1M\n'
	bench_count=$((bench_count + 1))
	# FAT32 I/O duration depends on host load. Synchronize on completion instead
	# of queuing the next command after an assumed fixed delay.
	wait_for_count bulk.home '^BENCH_PROCESS_OK$' "$bench_count" 60 "$log"
	printf 'cp /home/tools/bigprobe /tmp/bigprobe; stat /tmp/bigprobe\n'
	sleep 5
	printf '/home/tools/hugeprobe &\n'
	sleep 0.5
	printf 'echo responsive-while-loading\n'
	sleep 15
	printf '/home/demo/message.txt\n'
	sleep 2
	printf '/home/demo/missing\n'
	sleep 2
	printf '/system/vfs\n'
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
	# The terminal service and the shell restart independently (ADR 0089):
	# each is killed once, and the other has to carry on with its replacement.
	printf 'fault tty\n'
	sleep 2
	printf 'echo after-terminal-restart\n'
	sleep 1
	# A full-screen program on the restarted terminal service gets raw mode
	# from the new session and leaves the shell usable.
	printf 'gview /system/motd\n'
	sleep 2
	printf '\033[21~'
	sleep 2
	printf 'echo after-restarted-terminal-gview\n'
	sleep 1
	printf 'sleep 30 &\n'
	sleep 1
	printf 'fault shell\n'
	sleep 3
	printf 'echo after-shell-restart; jobs; ps\n'
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
	# Phase 4: the running system's programs are read from the volume, so
	# PID 1 refuses an exclusive check of it and unmounts nothing (ADR 0175).
	# The check and the repair run in the recovery boot below.
	printf 'fsck-fat --check auto\n'
	sleep 2
	printf 'cat /home/demo/message.txt; echo after-fsck-busy\n'
	sleep 2
	# The FAT32 service parses the volume in a process of its own (ADR 0096).
	# Killing it leaves the rest of the namespace working, and its restart
	# serves the same volume under the mounts and handles the VFS kept.
	printf 'fault fat32\n'
	sleep 3
	printf 'ls /tmp; echo after-fat32-fault\n'
	sleep 1
	printf 'cat /home/demo/message.txt; echo after-fat32-restart\n'
	sleep 2
	# The validation VFS answers one name late. A Linux program that opens
	# it waits out the bridge's eight-second deadline, gets ETIMEDOUT, and on
	# its next request must discard the late reply and carry on.
	printf '/system/linux/abi-probe session-timeout\n'
	sleep 11
	# The VFS restarts too (ADR 0099): its clients are handed new sessions,
	# the FAT32 service restarts behind it, and the shell keeps its jobs.
	printf 'sleep 25 &\n'
	sleep 1
	# A Linux program keeps the session it was launched with. It waits here
	# for its input to end, which happens after the VFS has died, and must
	# then find that session dead instead of reconnected. The terminal
	# redraws its prompt in front of a background job's output, so the
	# checks below match the text anywhere in a line.
	printf 'sleep 4 | /system/linux/abi-probe session-loss &\n'
	sleep 1
	printf 'fault vfs\n'
	sleep 5
	printf 'ls /tmp; echo after-vfs-fault\n'
	sleep 1
	printf 'cat /home/demo/message.txt; echo after-vfs-restart\n'
	sleep 2
	printf 'jobs\n'
	sleep 1
	printf 'echo before-vfs-ps; ps; echo after-vfs-ps\n'
	sleep 2
	# After the VFS restart gfm and the viewer it starts get sessions from the
	# new VFS (ADR 0130). Ctrl-C while gfm waits for the editor stops both,
	# and the shell reads lines again.
	printf 'cd /home/fm\n'
	sleep 1
	printf 'gfm\n'
	sleep 2
	printf '\033[H\033[B\033[13~'
	sleep 2
	printf '\033[21~'
	sleep 2
	printf '\033[14~'
	sleep 2
	printf '\003'
	sleep 2
	printf 'echo after-nested-interrupt; ps\n'
	sleep 2
	printf 'cd /\n'
	sleep 1
	# The configuration and log services restart on their own (ADR 0100):
	# each gets the sessions its first launch consumed, and the shell and the
	# log service are handed new configuration sessions.
	printf 'fault config\n'
	sleep 3
	printf 'config get user.name\n'
	sleep 1
	printf 'config set user.name boot-test\n'
	sleep 1
	printf 'fault log\n'
	sleep 3
	printf 'echo after-log-restart\n'
	sleep 2
	# The block service restarts too (ADR 0101): the driver and the FAT32
	# service start again behind it, and the volume is read and written.
	printf 'fault block\n'
	sleep 5
	printf 'cat /home/demo/message.txt; echo after-block-restart\n'
	sleep 2
	printf 'write /home/demo/after-block.txt block-restart-write; cat /home/demo/after-block.txt\n'
	sleep 3
	printf 'cd /system\n'
	sleep 1
	printf 'uptime\n'
	sleep 1
	# Native virtual memory (ADR 0104): the checks, then one process per
	# fault it must not survive while the system does.
	printf 'vmprobe\n'
	sleep 8
	for fault in read-only guard unmapped quota; do
		printf 'vmprobe %s\n' "$fault"
		sleep 3
	done
	printf 'echo after-vm-faults\n'
	sleep 1
	# Run the same workload five times and count the pages in use after each.
	# The first two rounds let lazy caches and allocator high-water marks settle;
	# the last three must agree. A path that does not free a channel, stream,
	# executable object, shared-memory mapping, startup object, RAMFS page, or
	# path page then shows up as sustained growth (GoOS 0.1.3 §11).
	for round in 1 2 3 4 5; do
		printf 'echo leak-round-%s\n' "$round"
		sleep 1
		printf 'cp /home/tools/bigprobe /tmp/leak.bin; rm /tmp/leak.bin\n'
		sleep 2
		printf '/home/demo/args leak-check | readline\n'
		sleep 1
		printf 'bench io /tmp/leak.io 256K; rm /tmp/leak.io\n'
		sleep 2
		printf 'touch /tmp/%s; ls /tmp > /tmp/leak.ls; rm /tmp/%s /tmp/leak.ls\n' "$long_name" "$long_name"
		sleep 1
		printf 'sleep 1 | cat /system/motd | write /tmp/leak.pipe; rm /tmp/leak.pipe\n'
		sleep 3
		printf 'mem -p\n'
		sleep 1
	done
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
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
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

# The scripted session sleeps about 345 seconds between its commands. Its
# commands are read from the installed volume (ADR 0175), which takes longer
# than launching them from the archive did, and the guest falls behind the
# script; the deadline leaves room for that and for a loaded host.
wait_for_marker boot '^arena-stress-complete$' 900 "$log"
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

# The expected release is read from the one place it is written, so changing
# the version needs no edit here.
release=$(sed -n 's/^[[:space:]]*Release[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/version/version.go)
[ -n "$release" ] || fail_check version.source 'Release in internal/version/version.go' 'not found' "$log" 'GoOS'
release_pattern=$(printf '%s' "$release" | sed 's/[].[*^$\\]/\\&/g')
require_regex version.banner "^GoOS $release_pattern[[:cntrl:]]*\$" "GoOS $release" "$log" GoOS
require_regex version.uname "^GoOS $release_pattern x86_64[[:cntrl:]]*\$" "GoOS $release x86_64" "$log" uname
pass_group version

for marker in PMM_OK VM_OK NX_WX_OK PAT_CONFIGURED FRAMEBUFFER_MAPPING_OK FRAMEBUFFER_USER_ISOLATION_OK FRAMEBUFFER_PRIMITIVES_OK FRAMEBUFFER_TERMINAL_OK FRAMEBUFFER_VALIDATION_PATTERN_OK PS2_KEYBOARD_READY STACK_GUARDS_OK ADDRESS_SPACE_A_OK ADDRESS_SPACE_B_OK PROCESS_ISOLATION_OK USER_RX_OK USER_STACK_NX_OK USER_COPYOUT_OK USER_COPYIN_OK USER_FAULT_ISOLATION_OK KERNEL_HIGH_ACCESS_FAULT_OK KERNEL_HIGH_ISOLATION_OK USER_ENTRY_STACK_BOUNDED_OK USER_LOW_ADDRESS_EXEC_OK USER_STACK_GUARD_OK BOOT_STACK_GUARD_OK; do
	require_marker "memory-security.$marker" "$marker" "$log"
done
pass_group memory-security

# The page allocator manages memory above the first GiB (ADR 0103). The
# validation kernel exercises one high page through the direct map and a
# process mapping whenever the machine has any. q35 places RAM above 4 GiB
# once the guest has more than 2.75 GiB.
memory_mib=$(guest_memory_mib "$memory") ||
	fail_check physical-memory.size 'MEMORY as a whole number with an optional M or G suffix' "$memory" "$log" PMM_OK
require_regex physical-memory.managed '^Memory: [1-9][0-9]* MiB managed' 'managed memory in the boot banner' "$log" 'Memory:'
if [ "$memory_mib" -ge 2048 ]; then
	require_regex physical-memory.high-managed '^Physical pages above 1 GiB: [1-9]' 'managed pages above 1 GiB' "$log" PMM_OK
	require_marker physical-memory.high-page PMM_HIGH_OK "$log"
elif [ "$memory_mib" -le 1024 ]; then
	require_marker physical-memory.no-high-memory PMM_HIGH_NONE "$log"
fi
if [ "$memory_mib" -ge 3072 ]; then
	require_regex physical-memory.above-4gib '^PMM high page: 0x[1-9a-f][0-9a-f]\{8\}[[:cntrl:]]*$' 'a high page above 4 GiB' "$log" 'PMM high page'
fi
pass_group physical-memory

for marker in TASK_BLOCK_OK TASK_WAKE_OK TASK_A_OK TASK_B_OK IDLE_TASK_OK SCHEDULER_OK PROCESS_MODEL_OK PROCESS_WAIT_OK PROCESS_HANDLE_WAIT_OK PROCESS_RECLAIM_OK USER_PREEMPT_OK PROCESS_CR3_SCHED_OK THREAD_CREATION_ROLLBACK_OK; do
	require_marker "scheduler-process.$marker" "$marker" "$log"
done
pass_group scheduler-process

for marker in HANDLE_TABLE_OK KCALL_ABI_OK IPC_WIRE_OK IPC_REQUEST_REPLY_OK IPC_HANDLE_TRANSFER_OK IPC_BOUNDED_OK IPC_PEER_DEATH_OK SHMEM_OBJECT_OK SHMEM_ZERO_COPY_OK SHMEM_RIGHTS_OK STREAM_ENDPOINT_OK STREAM_SHARED_ENDPOINT_OK TTY_CAPABILITY_OK; do
	require_marker "ipc-capabilities.$marker" "$marker" "$log"
done
pass_group ipc-capabilities

for marker in INIT_PROCESS_OK INITRAMFS_DISCOVERY_OK PID1_INIT_OK PROCESS_EXIT_EVENT_OK SUPERVISOR_CRASH_EVENT_OK INIT_LIFECYCLE_IPC_OK INIT_SERVICE_LAUNCH_OK INIT_SERVICE_STARTED_OK USERSPACE_SUPERVISOR_OK LIFECYCLE_IPC_OK INIT_EXECUTABLE_LOOKUP_OK LAUNCH_AUTHORITY_OK INIT_CAPABILITY_MANIFEST_OK NATIVE_STARTUP_ABI_OK PID1_SERVICE_LAUNCH_OK LINUX_PID1_LAUNCH_OK LINUX_MUSL_OK LINUX_VFS_SESSION_OK LINUX_PATH_OK LINUX_FILE_IO_OK LINUX_STAT_OK LINUX_DIRENT_OK LINUX_SYSTEM_OK USER_CONSOLE_CAP_OK TTY_SERVICE_OK TTY_RIGHTS_OK USER_EXIT_OK USER_RING3_OK NATIVE_PROCESS_OK; do
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
count_window storage.driver-started '^VIRTIO_BLK_DRIVER_OK$' 2 "$log" 'Starting services' 'goos> fault block'
# The services either side of the driver stay up: a crash in one is not a
# cascade through the storage stack.
count_window storage.block-service-stable '^BLOCK_SERVICE_OK$' 1 "$log" 'Starting services' 'goos> fault block'
# The persistent volume is served by a filesystem service of its own, and a
# crash in it is recovered from like the driver's (ADR 0096).
require_window storage.fat32-process ' fat32$' 'fat32 listed by ps as a process of its own' "$log" 'goos> echo after-shell-restart' 'goos> mkdir'
require_min_count storage.fat32-started '^FAT32_SERVICE_OK$' 2 "$log" FAT32_SERVICE_OK
require_marker storage.fat32-fault FAT32_FAULT_INJECTED "$log" 'fault fat32'
require_marker storage.fat32-block-renewed BLOCK_FILESYSTEM_REPLACED_OK "$log" 'fault fat32'
require_marker storage.fat32-reattached FAT32_REATTACHED_OK "$log" 'fault fat32'
require_marker storage.fat32-restarted FAT32_RESTARTED_OK "$log" 'fault fat32'
require_regex storage.fsck-busy 'volume in use by the running system' 'the exclusive check refused in normal mode' "$log" 'fsck-fat --check auto'
forbid_regex storage.fsck-not-run '^FSCK_FAT_CHECK_OK$' 'no check against the live volume' "$log" 'fsck-fat --check auto'
require_window storage.fsck-busy-mounted '^persistent$' 'the volume still mounted after the refusal' "$log" 'goos> cat /home/demo/message.txt; echo after-fsck-busy' 'after-fsck-busy'
require_regex storage.fat32-rest-of-namespace '^after-fat32-fault$' 'the shell and /tmp working after the fault' "$log" 'fault fat32'
require_window storage.fat32-volume-served-again '^persistent$' 'the volume read through the restarted service' "$log" 'goos> cat /home/demo/message.txt; echo after-fat32-restart' 'after-fat32-restart'
# File data moves between the VFS and the FAT32 service through the file cache
# they share, not through messages (ADR 0098).
require_marker storage.file-cache FILE_CACHE_OK "$log"
# The development disk has no partition table: the filesystem service is
# given the whole disk (ADR 0115). A restarted filesystem service has the
# volume chosen again from the table.
require_min_count storage.whole-disk-volume '^BLOCK_VOLUME_WHOLE_OK$' 2 "$log" BLOCK_VOLUME_WHOLE_OK
count_window storage.vfs-not-restarted '^VFS_SERVICE_OK$' 0 "$log" 'goos> fault fat32' 'goos> fault vfs'
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
for entry in ls cat write gview edit gfm; do
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

for marker in SHELL_PROCESS_OK SHELL_VFS_CONNECTED_OK SHELL_HELP_OK ECHO_PROCESS_OK ARGS_PROCESS_OK ARGV_ENV_PROCESS_OK STDIN_HANDLE_OK STDERR_HANDLE_OK STDIN_READ_OK SHELL_CLEAR_OK UPTIME_PROCESS_OK PWD_PROCESS_OK LS_PROCESS_OK MOUNT_PROCESS_OK DF_PROCESS_OK GVIEW_EXIT_OK SHELL_CD_OK CAT_PROCESS_OK MKDIR_PROCESS_OK TOUCH_PROCESS_OK STAT_PROCESS_OK CP_PROCESS_OK MV_PROCESS_OK RM_PROCESS_OK UNAME_PROCESS_OK PS_PROCESS_OK MEM_PROCESS_OK SLEEP_PROCESS_OK EXIT_STATUS_PROPAGATION_OK; do
	require_marker "shell.$marker" "$marker" "$log"
done
require_marker shell.echo-output 'hello structured world' "$log" 'echo "hello structured world"'
require_marker shell.argv-output 'alpha beta escaped value' "$log" 'args "alpha beta"'
require_marker shell.path-environment 'PATH=/system/bin' "$log" 'args "alpha beta"'
require_regex startup.many-arguments '^w0 w1 w2 .* w698 w699$' '700 arguments reach the command in order' "$log" 'goos> args w0'
require_regex startup.long-argument '^file 2\.93 KiB (3001 bytes)$' 'a 3000-byte argument written whole' "$log" 'goos> echo xxxx'
require_window startup.many-paths '^many-paths-done$' 'touch and rm take several paths' "$log" 'goos> touch /tmp/m1' 'goos> readline'
forbid_window startup.many-paths-errors 'not found\|usage:' 'no path was refused' "$log" 'goos> touch /tmp/m1' 'goos> readline'
require_regex names.ramfs-long-path '^deep-ramfs$' 'a 255-byte name under a 405-byte path in /tmp' "$log" 'goos> mkdir /tmp/ddd'
require_regex names.fat32-long-name "^$long_name\$" 'a 255-byte name listed on the FAT32 volume' "$log" 'goos> mkdir /home/ddd'
require_regex names.fat32-rename '^deep-fat32$' 'a long name renamed on the FAT32 volume' "$log" 'goos> mkdir /home/ddd'
require_regex names.too-long ': file name too long$' 'a 1025-byte name refused as too long' "$log" 'goos> touch /tmp/nnn'
count_window registries.background-jobs '\[[1-7]\] started$' 7 "$log" 'goos> sleep 4 | cat /system/motd | write /tmp/bg1 &' 'goos> cat /tmp/bg1 /tmp/bg4 /tmp/bg7'
forbid_window registries.no-exhaustion 'resource limit\|too many background jobs\|cannot start' 'seven pipelines ran without running out of anything' "$log" 'goos> sleep 4 | cat /system/motd | write /tmp/bg1 &' 'goos> readline'
count_window registries.background-output 'Welcome to GoOS' 3 "$log" 'goos> cat /tmp/bg1 /tmp/bg4 /tmp/bg7' 'goos> readline'
# A line typed while a command runs is that command's input: it is echoed once
# and printed once more by readline, and never reaches the shell as a command.
require_count shell.stdin-output '^bounded stdin payload$' 2 "$log" 'goos> readline'
forbid_regex shell.stdin-not-a-command '^bounded: command not found$' 'typed input reached the command, not the shell' "$log" 'goos> readline'
require_regex shell.cwd '^/system$' '/system on its own line' "$log" 'cd "/system"'
require_regex stat.file-kib '^file [0-9]*\.[0-9][0-9] KiB ([1-9][0-9]* bytes)$' 'a file size in KiB by default' "$log" 'goos> stat /system/bin/ls'
require_regex stat.directory-size '^directory [1-9][0-9]*\.[0-9][0-9] KiB ([1-9][0-9]* bytes), [1-9][0-9]* files, 0 directories$' 'a directory sized by its contents' "$log" 'goos> stat /system/bin'
require_regex stat.directory-mib '^directory [0-9]*\.[0-9][0-9] MiB ([1-9][0-9]* bytes), [1-9][0-9]* files, 3 directories$' 'stat -m counts nested directories in MiB' "$log" 'goos> stat -m /system'
require_regex stat.bytes '^file 16 bytes$' 'stat -b prints exact bytes' "$log" 'goos> stat -b /system/motd'
require_regex stat.bad-flag '^usage: stat \[-b|-k|-m|-g\] PATH$' 'an unknown unit flag is a usage error' "$log" 'goos> stat -x /system'
require_window mount.system '^/bootstrap  *image  *ro$' 'boot archive in mount output' "$log" 'goos> mount' 'goos> df'
require_window mount.installed '^/system  *fat32  *ro$' 'protected installed system in mount output' "$log" 'goos> mount' 'goos> df'
require_window mount.persist '^/persist  *fat32  *rw$' 'persistent volume in mount output' "$log" 'goos> mount' 'goos> df'
require_window df.header '^Filesystem  *Mount  *Size  *Used  *Free  *Flags$' 'df column header' "$log" 'goos> df' 'goos> stat /system/bin/ls'
require_window df.system '^image  */bootstrap  *[0-9][0-9]*\.[0-9]M  *[0-9][0-9]*\.[0-9]M  *0B  *ro$' 'read-only archive capacity' "$log" 'goos> df' 'goos> stat /system/bin/ls'
require_window df.persist '^fat32  */persist  *[0-9][0-9]*\.[0-9]M  *[0-9][0-9]*\.[0-9]M  *[0-9][0-9]*\.[0-9]M  *rw$' 'persistent capacity' "$log" 'goos> df' 'goos> stat /system/bin/ls'
require_regex gview.serial-return '^after-gview-serial$' 'shell input after gview on COM1' "$log" 'gview /system/motd'
require_marker launch.gfm GFM_LAUNCH_OK "$log"
require_min_count launch.viewer-after-vfs-restart 'GVIEW_EXIT_OK' 4 "$log" 'echo after-nested-interrupt'
require_regex launch.terminal-restart '^after-restarted-terminal-gview$' 'shell input after gview on a restarted terminal service' "$log" 'gview /system/motd'
require_regex launch.nested-interrupt '^after-nested-interrupt$' 'shell input after Ctrl-C stopped gfm and its editor' "$log" 'echo after-nested-interrupt'
forbid_window launch.nested-stopped ' \(gfm\|edit\)$' 'gfm and its editor stopped by Ctrl-C' "$log" 'goos> echo after-nested-interrupt' 'goos> cd /'

require_marker launch.edit-exit EDIT_EXIT_OK "$log"
require_marker launch.edit-save EDIT_SAVE_OK "$log"
require_min_count launch.viewer-returns 'GVIEW_EXIT_OK' 2 "$log" 'gview /system/motd'
require_regex launch.saved-contents '^Xsaved text$' 'the edit saved through a replacement' "$log" 'cat /home/fm/note.txt'
forbid_window launch.no-temporary '\.note\.txt\.~save' 'no temporary file left by the save' "$log" 'goos> ls /home/fm' 'goos> cd /'
pass_group shell

require_regex help.list '^Commands in /system/bin:$' 'help lists the executables' "$log" 'goos> help'
require_marker help.command SHELL_HELP_COMMAND_OK "$log" 'help cat'
require_regex help.command-usage '^usage: cat FILE\.\.\.$' 'help cat prints the usage' "$log" 'help cat'
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
# A marker a command prints can follow the next prompt on the same line when
# the guest is behind the script, so the counts accept a prefix.
require_min_count process-reuse.cat 'CAT_PROCESS_OK$' 31 "$log" CAT_PROCESS_OK
require_min_count process-reuse.exec 'SHELL_EXEC_OK$' 66 "$log" SHELL_EXEC_OK
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

require_marker vm.probe VM_PROBE_OK "$log" 'goos> vmprobe'
require_count vm.large-executable 'vm: all checks passed$' 3 "$log" 'goos> /home/tools/bigprobe'
require_regex exec.file-backed-responsive '^responsive-while-loading$' 'the shell answered after launching a 12 MiB file-backed program' "$log" 'goos> /home/tools/hugeprobe &'
require_window bulk.tmp-write '^io write, 1048576 bytes, 16 exchanges' 'volatile bulk write completed' "$log" 'goos> bench io /tmp/bulk.bin 1M' 'goos> bench io /home/bulk.bin 1M'
require_window bulk.tmp-read '^io read, 1048576 bytes, 16 exchanges' 'volatile bulk read completed' "$log" 'goos> bench io /tmp/bulk.bin 1M' 'goos> bench io /home/bulk.bin 1M'
require_window bulk.home-write '^io write, 1048576 bytes, 16 exchanges' 'FAT32 bulk write completed' "$log" 'goos> bench io /home/bulk.bin 1M' 'goos> cp /home/tools/bigprobe /tmp/bigprobe'
require_window bulk.home-read '^io read, 1048576 bytes, 16 exchanges' 'FAT32 bulk read completed' "$log" 'goos> bench io /home/bulk.bin 1M' 'goos> cp /home/tools/bigprobe /tmp/bigprobe'
require_regex bulk.copy '^file 3[01][0-9][0-9]\.[0-9]* KiB (3[0-9]\{6\} bytes)' 'a 3 MiB copy from the volume' "$log" 'goos> cp /home/tools/bigprobe /tmp/bigprobe'
for check in 'demand-zero pages' 'unmap returns the pages' 'discard reads as zero again' 'writable executable memory refused' 'overlapping fixed reservation refused' 'image unmap refused' 'stack guard below the reservation' 'heap grows past its old 128 KiB' 'large BSS starts zero and is writable' 'a 512 KiB stack fits the default reservation'; do
	require_regex "vm.$check" "^vm: $check\$" "$check" "$log" 'goos> vmprobe'
done
require_regex vm.read-only 'user fault: access not permitted' 'a write to a read-only page retired' "$log" 'goos> vmprobe read-only'
require_regex vm.guard 'user fault: guard page' 'a stack overflow stopped at the guard' "$log" 'goos> vmprobe guard'
require_regex vm.unmapped 'user fault: no mapping' 'an unmapped access retired' "$log" 'goos> vmprobe unmapped'
require_regex vm.quota 'user fault: page quota exhausted' 'a process past its page quota retired' "$log" 'goos> vmprobe quota'
require_regex vm.system-survives '^after-vm-faults$' 'the system carries on after the faults' "$log" 'goos> echo after-vm-faults'
leak_counts=$(sed -n '/goos> echo leak-round-3/,$p' "$log" | grep -a '^pages used: ' | sed -n '1,3s/^pages used: //p')
leak_third=$(printf '%s\n' "$leak_counts" | sed -n 1p)
leak_fourth=$(printf '%s\n' "$leak_counts" | sed -n 2p)
leak_fifth=$(printf '%s\n' "$leak_counts" | sed -n 3p)
if [ -n "$leak_third" ] && [ "$leak_third" = "$leak_fourth" ] && [ "$leak_third" = "$leak_fifth" ]; then
	checks=$((checks + 1))
else
	fail_check leaks.steady-state 'the same pages in use after rounds 3, 4, and 5' "${leak_third:-none}, ${leak_fourth:-none}, ${leak_fifth:-none}" "$log" 'goos> echo leak-round-3'
fi
count_window leaks.rounds '^pages used: ' 5 "$log" 'goos> echo leak-round-1' 'goos> cat /missing'
pass_group vm

# PID 1's storage probe writes a pattern across sectors 7 and 8 and puts the
# original bytes back (ADR 0115): the pattern must be gone from the disk, and
# the backup FSInfo trail signature it crossed intact.
value=$(dd if="$disk" bs=1 skip=4092 count=7 2>/dev/null | od -An -tx1 | tr -d ' \n')
[ "$value" = 000055aa000000 ] || fail_check persistence.block-bytes 'the bytes the probe crossed restored (000055aa000000)' "$value" "$log" BLOCK_SERVICE_IO_OK
checks=$((checks + 1))
grep -a -q 'Welcome to GoOS' "$disk" || fail_check persistence.fat32-image 'Welcome to GoOS in disk image' 'content not found' "$log" 'cat /persist/state.txt'
checks=$((checks + 1))

( await_ready "$reboot_log"
	printf 'cat /persist/state.txt\n'
	sleep 2
	printf '/home/demo/args after-reboot\n'
	sleep 3
	printf 'cat /state/system.log.1\n'
	sleep 2
	printf 'ls /state\n'
	sleep 2
	printf 'config get user.name\n'
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
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
require_window persistence.reboot-log '^\[[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]\.[0-9]*Z\] \(INFO\|WARN\|ERROR\) ' 'a UTC calendar record from the previous session' "$reboot_log" 'goos> cat /state/system.log.1' 'goos> ls /state'
# The value set in the previous session is restored over the schema default.
require_regex persistence.reboot-file-exec '^after-reboot$' 'a program on the volume run after the reset' "$reboot_log" 'goos> /home/demo/args after-reboot'
require_window persistence.reboot-config '^boot-test$' 'a setting restored from the previous session' "$reboot_log" 'goos> config get user.name' 'NO_SUCH_LINE'
pass_group persistence

# A VFS that dies is restarted (ADR 0099). Its clients are handed new
# sessions instead of being restarted: the shell keeps its jobs, the config
# and log services their state. The FAT32 service restarts behind it and is
# attached to the new VFS, so the volume is served again; /tmp, which lived in
# the old VFS, starts empty.
require_marker vfs-restart.injected VFS_FAULT_INJECTED "$log" 'fault vfs'
require_marker vfs-restart.restarted VFS_RESTARTED_OK "$log" 'fault vfs'
require_count vfs-restart.service-started-twice '^VFS_SERVICE_OK$' 2 "$log" 'fault vfs'
require_marker vfs-restart.config-session CONFIG_SESSION_RENEWED_OK "$log" 'fault vfs'
require_marker vfs-restart.log-session LOG_SESSION_RENEWED_OK "$log" 'fault vfs'
require_min_count vfs-restart.fat32-restarted '^FAT32_RESTARTED_OK$' 2 "$log" 'fault vfs'
require_regex vfs-restart.namespace '^after-vfs-fault$' 'the shell using the namespace after the restart' "$log" 'goos> ls /tmp; echo after-vfs-fault'
forbid_window vfs-restart.tmp-empty '^redirected.txt$' 'an empty /tmp in the new VFS' "$log" 'goos> ls /tmp; echo after-vfs-fault' 'after-vfs-fault'
require_window vfs-restart.volume '^persistent$' 'the volume read through the new VFS' "$log" 'goos> cat /home/demo/message.txt; echo after-vfs-restart' 'after-vfs-restart'
require_regex vfs-restart.linux-timeout 'session timeout: ETIMEDOUT, then ok$' 'a Linux open gives up on a silent VFS and the session survives' "$log" 'goos> /system/linux/abi-probe session-timeout'
forbid_regex vfs-restart.linux-no-reconnect 'session kept: ' 'a Linux process was not given a new session' "$log" 'goos> fault vfs'
# The shell is read from the volume (ADR 0175). A program whose source is
# served by the VFS that died can end with it when it next touches a page it
# has not read yet (ADR 0121). Every check below looks only at what happened
# from this fault to the end of its scenario, never at an earlier fault.
#
# Either the shell survived: it was handed a new session, kept its jobs, and
# its Linux job found the session it was launched with dead.
# Or it ended: it started again from the volume exactly once, its jobs were
# stopped with it, and none of their processes is left.
vfs_window() { window "$log" 'VFS_FAULT_INJECTED' 'cd /home/fm'; }
shell_starts=$(vfs_window | grep -c 'SERVICE_FROM_DISK_OK shell$' || true)
if vfs_window | grep -q 'SHELL_VFS_RENEWED_OK$'; then
	[ "$shell_starts" -eq 0 ] || fail_check vfs-restart.shell-kept 'no shell start when the shell kept its session' "$shell_starts" "$log" 'VFS_FAULT_INJECTED'
	vfs_window | grep -q '^\[[0-9]\] running  sleep 25$' || fail_check vfs-restart.jobs-kept 'the shell kept its job across the restart' 'job not listed' "$log" 'VFS_FAULT_INJECTED'
	vfs_window | grep -q 'session lost: EIO$' || fail_check vfs-restart.linux-session-dead 'a Linux process keeps a dead session after the VFS restarts' 'no EIO' "$log" 'VFS_FAULT_INJECTED'
else
	[ "$shell_starts" -eq 1 ] || fail_check vfs-restart.shell-started 'one shell start from the volume' "$shell_starts" "$log" 'VFS_FAULT_INJECTED'
	vfs_window | grep -q 'SESSION_JOBS_STOPPED_OK$' || fail_check vfs-restart.jobs-ended 'the jobs of the ended shell stopped with it' 'marker not found' "$log" 'VFS_FAULT_INJECTED'
	leftover=$(window "$log" 'before-vfs-ps' 'after-vfs-ps' | grep ' sleep$\| abi-probe$' | sed -n '1p' || true)
	[ -z "$leftover" ] || fail_check vfs-restart.jobs-gone 'no process of the ended shell left' "$leftover" "$log" 'before-vfs-ps'
	window "$log" 'before-vfs-ps' 'after-vfs-ps' | grep -q ' ps$' || fail_check vfs-restart.ps-ran 'ps listed the processes after the restart' 'no ps output' "$log" 'before-vfs-ps'
fi
checks=$((checks + 3))
pass_group vfs-restart

# The configuration and log services restart on their own (ADR 0100). The
# restarted configuration service is back with the stored settings, and the
# shell reads and writes them over the session it was handed; the restarted
# log service resumes after the last record its predecessor wrote.
require_marker service-restart.config-injected CONFIG_FAULT_INJECTED "$log" 'fault config'
require_marker service-restart.config-restarted CONFIG_RESTARTED_OK "$log" 'fault config'
require_count service-restart.config-started-twice '^CONFIG_SERVICE_OK$' 2 "$log" 'fault config'
require_marker service-restart.shell-config SHELL_CONFIG_RENEWED_OK "$log" 'fault config'
require_marker service-restart.log-config LOG_CONFIG_RENEWED_OK "$log" 'fault config'
require_window service-restart.config-read '^boot-test$' 'a setting read back from the restarted service' "$log" 'goos> fault config' 'goos> fault log'
require_window service-restart.config-write '^CONFIG_SET_OK$' 'a setting written through the new session' "$log" 'goos> fault config' 'goos> fault log'
forbid_window service-restart.config-errors 'config:' 'no configuration error after the restart' "$log" 'goos> fault config' 'goos> fault log'
require_marker service-restart.log-injected LOG_FAULT_INJECTED "$log" 'fault log'
require_marker service-restart.log-restarted LOG_RESTARTED_OK "$log" 'fault log'
require_count service-restart.log-started-twice '^LOG_SERVICE_OK$' 2 "$log" 'fault log'
require_marker service-restart.log-resumed LOG_RESUMED_OK "$log" 'fault log'
pass_group service-restart

# The block service restarts (ADR 0101). The driver and the FAT32 service see
# it gone, exit, and start again after it; each hands the new block service
# its endpoint, and the volume is mounted, read and written again.
require_marker block-restart.injected BLOCK_FAULT_INJECTED "$log" 'fault block'
require_marker block-restart.restarted BLOCK_RESTARTED_OK "$log" 'fault block'
# The restarted service may print its marker on the line where the shell
# has just redrawn its prompt, so the count matches the marker at a line's end.
require_count block-restart.service-started-twice 'BLOCK_SERVICE_OK$' 2 "$log" 'fault block'
require_window block-restart.driver-again '^VIRTIO_BLK_DRIVER_OK$' 'a driver started for the new block service' "$log" 'goos> fault block' 'after-block-restart'
require_window block-restart.driver-installed '^BLOCK_DRIVER_REPLACED_OK$' 'the new driver handed to the block service' "$log" 'goos> fault block' 'after-block-restart'
require_window block-restart.filesystem-installed '^BLOCK_FILESYSTEM_REPLACED_OK$' 'the new FAT32 service handed to the block service' "$log" 'goos> fault block' 'after-block-restart'
require_window block-restart.fat32-restarted '^FAT32_RESTARTED_OK$' 'the FAT32 service attached again' "$log" 'goos> fault block' 'after-block-restart'
require_window block-restart.volume-read '^persistent$' 'the volume read through the new block service' "$log" 'cat /home/demo/message.txt; echo after-block-restart' 'after-block-restart'
require_regex block-restart.volume-written '^block-restart-write$' 'a file written and read back through the new block service' "$log" 'after-block.txt'
pass_group block-restart

# Programs outside /system run from executable objects PID 1 fills (ADR 0097).
# The kernel proves the object's bounds and sealing at boot, the shell proves
# it cannot create one, and PID 1 proves partial, unsealed, closed and
# non-program objects are refused.
require_marker file-exec.kernel-object EXECUTABLE_OBJECT_OK "$log"
require_marker file-exec.shell-denied EXECUTABLE_CREATE_DENIED_OK "$log"
require_marker file-exec.launched FILE_EXEC_OK "$log" '/home/demo/args'
require_window file-exec.absolute '^file-exec from the volume$' 'arguments printed by the copy on /home' "$log" 'goos> /home/demo/args file-exec' 'goos> cd /home/demo'
require_window file-exec.environment '^PATH=/system/bin:/home/user/bin$' 'the shell environment given to the copy' "$log" 'goos> /home/demo/args file-exec' 'goos> cd /home/demo'
require_regex file-exec.relative '^relative-file-exec$' 'a program path relative to the working directory' "$log" './args relative-file-exec'
require_regex file-exec.pipeline '^piped-file-exec$' 'a program from the volume as a pipeline stage' "$log" 'piped-file-exec | readline'
require_regex file-exec.not-a-program '^/home/demo/message.txt: not a valid executable$' 'a data file refused by the validator' "$log" 'goos> /home/demo/message.txt'
require_regex file-exec.missing '^/home/demo/missing: command not found$' 'a missing program named by its path' "$log" 'goos> /home/demo/missing'
require_regex file-exec.service-refused '^/system/vfs: permission denied$' 'a service of the image is not a command' "$log" 'goos> /system/vfs'
pass_group file-exec

# The persistent log is a service, not a kernel feature: it drains the bounded
# kernel ring onto the volume so a session's records outlive its reset.
require_marker log.started LOG_SERVICE_OK "$log"
require_regex log.service-reported '^\[ OK \] log$' 'the log service reported started' "$log" 'Starting services'
require_regex log.file-listed '^system\.log$' 'system.log on the state volume' "$log" 'goos> ls /state'
require_regex log.rotated '^system\.log\.1$' 'a rotated log beside it' "$log" 'goos> ls /state'
require_window log.persisted-record '^\[[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]\.[0-9]*Z\] \(INFO\|WARN\|ERROR\) ' 'a UTC calendar record read back from the file' "$log" 'goos> cat /state/system.log' 'goos> config'
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
require_regex quota.shell-streams '^[0-9][0-9]*  *shell  *strm  *[0-9]*/16$' 'the shell holding an ordinary stream share' "$log" 'goos> quota'
require_regex quota.init-streams '^1  *init  *strm  *[0-9]*/64$' 'PID 1 holding the system share' "$log" 'goos> quota'
require_regex quota.command-streams '^[0-9][0-9]*  *quota  *strm  *[0-9]*/8$' 'a command running with the tiny profile' "$log" 'goos> quota'
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
require_regex release.log 'INFO  init\[1\]: service started service=[a-z-]* pid=' 'structured service start event' "$log" 'goos> log'
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
# Killing the terminal service restarts it on a fresh session, which PID 1
# hands to the running shell; the shell's next prompt and command go through
# the replacement.
require_marker session.tty-fault TTY_FAULT_INJECTED "$log" 'fault tty'
require_marker session.tty-restarted TTY_RESTARTED_OK "$log" 'fault tty'
require_count session.tty-started 'TTY_TERMINAL_OK' 2 "$log" 'fault tty'
require_marker session.tty-session-offered TTY_SESSION_OFFERED_OK "$log" 'fault tty'
require_marker session.shell-took-session SHELL_TERMINAL_REPLACED_OK "$log" 'fault tty'
require_regex session.shell-survived-terminal '^after-terminal-restart$' 'the shell working through the new terminal' "$log" 'after-terminal-restart'
# Killing the shell restarts it; the running terminal service takes the new
# session over its control channel. The job the old shell left running is
# stopped with its session rather than orphaned.
require_marker session.shell-fault SHELL_FAULT_INJECTED "$log" 'fault shell'
require_marker session.shell-restarted SHELL_RESTARTED_OK "$log" 'fault shell'
# The shell also starts again after the storage faults of this session,
# because it is read from the volume (ADR 0175); the shell fault itself
# starts exactly one.
count_window session.shell-started 'SHELL_PROCESS_OK' 1 "$log" 'SHELL_FAULT_INJECTED' 'after-shell-restart'
require_marker session.shell-session-offered SHELL_SESSION_OFFERED_OK "$log" 'fault shell'
require_marker session.terminal-took-session TTY_SESSION_REPLACED_OK "$log" 'fault shell'
require_marker session.jobs-stopped SESSION_JOBS_STOPPED_OK "$log" 'fault shell'
require_regex session.terminal-survived-shell '^after-shell-restart$' 'the terminal serving the new shell' "$log" 'after-shell-restart'
require_window session.no-inherited-jobs '^no background jobs$' 'the new shell starts with no jobs' "$log" 'goos> echo after-shell-restart' 'goos> mkdir'
forbid_window session.jobs-ended '  *sleep$' 'the old shell'"'"'s job was stopped' "$log" 'goos> echo after-shell-restart' 'goos> mkdir'
require_regex exec.large-pipeline '^pipeline request larger than one ipc message 0123456789abcdefghijklmnopqrstuvwxyz$' 'pipeline request over 256 bytes' "$log" 'readline | readline | readline'
pass_group release-shell

# The M7 acceptance runs inside one VM: write, reboot from the shell, read back.
# It uses /home because that is where the specification puts user data.
rm -f "$m7_log"
( await_ready "$m7_log"
	printf 'mkdir /home/m7\n'
	sleep 3
	printf 'write /home/m7/hello.txt "Hello from GoOS"\n'
	sleep 3
	printf 'cat /home/m7/hello.txt\n'
	sleep 3
	printf 'reboot\n'
	await_ready "$m7_log" 2
	printf 'cat /home/m7/hello.txt\n'
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
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
# prompt is where the volume gets looked at. Both the boot sector and its
# backup at sector 6 are damaged: with an intact backup the volume would be
# mounted read-only from it instead (ADR 0112).
rm -f "$corrupt_log"
corrupt_disk=build/corrupt-virtio-blk.img
rm -f "$corrupt_disk"
go run ./tools/mkfat32 -tree "$system_tree" "$corrupt_disk"
for boot_sector in 0 6; do
	printf '\000\000\000\040' | dd of="$corrupt_disk" bs=1 seek=$((boot_sector * 512 + 32)) count=4 conv=notrunc 2>/dev/null
done
( await_ready "$corrupt_log"
	printf 'ls /\n'
	sleep 2
	printf 'echo volatile-root-still-works\n'
	sleep 2
	printf 'ls /bootstrap/bin\n'
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$corrupt_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$corrupt_log" 2>&1 &
pid=$!

wait_for_marker corrupt '^LS_PROCESS_OK$' 60 "$corrupt_log"
wait_for_count corrupt '^LS_PROCESS_OK$' 2 30 "$corrupt_log"
cleanup
require_marker corrupt.boot BOOT_OK "$corrupt_log"
require_marker corrupt.shell SHELL_VFS_CONNECTED_OK "$corrupt_log"
require_regex corrupt.reported '^\[WARN\] no persistent storage$' 'the refused volume reported' "$corrupt_log" 'Starting services'
forbid_regex corrupt.no-mount-marker '^FAT32_MOUNT_OK$' 'no mount claimed for a refused volume' "$corrupt_log" 'Starting services'
# The namespace tells the truth about what is there. Without the volume the
# installed system is unreachable, so the machine runs in recovery mode from
# the archive (ADR 0175).
require_regex corrupt.recovery '^\[MODE\] recovery: stage=storage' 'recovery mode for a refused volume' "$corrupt_log" 'Starting services'
require_window corrupt.root-listed '^bootstrap/$' 'the boot archive still mounted' "$corrupt_log" 'ls /' 'LS_PROCESS_OK'
forbid_window corrupt.persist-absent '^persist/$' 'no persistent mounts listed' "$corrupt_log" 'ls /' 'LS_PROCESS_OK'
forbid_window corrupt.system-absent '^system/$' 'no installed system listed' "$corrupt_log" 'ls /' 'LS_PROCESS_OK'
require_regex corrupt.usable '^volatile-root-still-works$' 'the system usable without storage' "$corrupt_log" 'goos> echo volatile-root-still-works'
require_window corrupt.recovery-commands '^fsck-fat$' 'the recovery commands' "$corrupt_log" 'goos> ls /bootstrap/bin' 'LS_PROCESS_OK'
pass_group corrupt-volume

# The exclusive check runs in recovery mode (ADR 0175). The recovery ISO boots
# the main test disk: PID 1 cleanly unmounts the volume, hands fsck-fat the
# exclusive block slice, then supervision mounts it again after the checker
# closes its endpoint. Repair is explicit, gets the same exclusive slice,
# reports an idempotent zero-change pass on the clean image, and remounts it
# again.
rm -f "$fsck_log"
recovery_iso=build/variants/$variant/goos-x86_64-recovery.iso
( await_ready "$fsck_log"
	printf 'fsck-fat --check auto\n'
	await_count "$fsck_log" '^FAT32_RESTARTED_OK$' 1
	sleep 2
	printf 'cat /home/demo/message.txt; echo after-fsck\n'
	sleep 2
	printf 'fsck-fat --repair auto\n'
	await_count "$fsck_log" '^FAT32_RESTARTED_OK$' 2
	sleep 2
	printf 'cat /home/demo/message.txt; echo after-fsck-repair\n'
	sleep 2
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$recovery_iso" -display none -serial stdio -no-reboot -no-shutdown >"$fsck_log" 2>&1 &
pid=$!
wait_for_marker fsck '^after-fsck-repair$' 90 "$fsck_log"
cleanup
require_regex storage.fsck-recovery '^\[MODE\] recovery: stage=forced' 'forced recovery mode' "$fsck_log" 'Starting services'
require_marker storage.fsck-check FSCK_FAT_CHECK_OK "$fsck_log" 'fsck-fat --check auto'
require_regex storage.fsck-summary '^files=[0-9][0-9]* directories=[1-9][0-9]* used-clusters=[0-9][0-9]* free-clusters=[0-9][0-9]* errors=0$' 'clean fsck summary' "$fsck_log" 'fsck-fat --check auto'
require_window storage.fsck-remounted '^persistent$' 'persistent volume remounted after fsck' "$fsck_log" 'FSCK_FAT_CHECK_OK' 'ECHO_PROCESS_OK'
require_marker storage.fsck-repair FSCK_FAT_REPAIR_OK "$fsck_log" 'fsck-fat --repair auto'
require_regex storage.fsck-repair-summary '^files=[0-9][0-9]* directories=[1-9][0-9]* used-clusters=[0-9][0-9]* free-clusters=[0-9][0-9]* errors=0 repaired=0$' 'idempotent clean repair summary' "$fsck_log" 'fsck-fat --repair auto'
require_window storage.fsck-repair-remounted '^persistent$' 'persistent volume remounted after repair' "$fsck_log" 'FSCK_FAT_REPAIR_OK' 'ECHO_PROCESS_OK'
pass_group recovery-fsck

# A volume inside a partition: the block service reads the MBR or GPT and
# gives the filesystem service a slice of the disk holding the partition, and
# nothing else (ADR 0115). A file written in the guest must come back out of
# the partition on the host, through a table the host still finds valid:
# nothing — not the filesystem, not PID 1's storage probe — may have written
# outside the slice or over the table.
for table_kind in mbr gpt; do
	table_upper=$(printf '%s' "$table_kind" | tr '[:lower:]' '[:upper:]')
	partition_disk=build/partition-$table_kind-virtio-blk.img
	rm -f "$partition_log" "$partition_disk" "build/partition-$table_kind.out"
	go run ./tools/mkfat32 -size 128 -table "$table_kind" -tree "$system_tree" "$partition_disk"
	( await_ready "$partition_log"
		printf 'write /persist/partition.txt "inside-%s-partition"\n' "$table_kind"
		sleep 2
		printf 'cat /persist/partition.txt\n'
		sleep 2
		printf 'config get system.shell.prompt\n'
	) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
		-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
		-drive "if=none,id=goosdisk,format=raw,file=$partition_disk" \
		-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
		-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$partition_log" 2>&1 &
	pid=$!
	wait_for_marker "partition.$table_kind" '^CONFIG_GET_OK$' 60 "$partition_log"
	cleanup
	require_marker "partition.$table_kind.chosen" "BLOCK_VOLUME_${table_upper}_OK" "$partition_log"
	require_marker "partition.$table_kind.mounted" FAT32_MOUNT_OK "$partition_log"
	require_marker "partition.$table_kind.probe" BLOCK_SERVICE_IO_OK "$partition_log"
	require_regex "partition.$table_kind.read" "^inside-$table_kind-partition\$" 'the file read back in the guest' "$partition_log" 'goos> cat /persist/partition.txt'
	go run ./tools/mkfat32 -get "partition.txt:build/partition-$table_kind.out" "$partition_disk"
	if [ "$(cat "build/partition-$table_kind.out")" != "inside-$table_kind-partition" ]; then
		fail_check "partition.$table_kind.host" "the guest's file inside the $table_kind partition" "$(cat "build/partition-$table_kind.out")" "$partition_log"
	fi
done
pass_group partition-tables

# A kernel stack that overflows must stop at its guard and be reported, not
# run into the memory below it or reset the machine without a word. PID 1's
# validation-only native call recurses on its own kernel-entry stack; the
# kernel prints that stack's guard range first, and the double fault, taken on
# its own stack, prints the address of the fault that could not be delivered.
# That address has to lie in the guard (ADR 0091).
rm -f "$stack_log"
stack_disk=build/stack-virtio-blk.img
rm -f "$stack_disk"
go run ./tools/mkfat32 -tree "$system_tree" "$stack_disk"
( await_ready "$stack_log"
	printf 'fault kernel-stack\n'
	sleep 10
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$stack_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$stack_log" 2>&1 &
pid=$!

wait_for_marker kernel-stack '^PANIC: double fault' 60 "$stack_log"
cleanup
require_marker kernel-stack.injected KERNEL_STACK_FAULT_INJECTED "$stack_log"
guard=$(tr -d '\r' <"$stack_log" | sed -n 's/^KERNEL_STACK_GUARD \(0x[0-9a-f]*\) \(0x[0-9a-f]*\)$/\1 \2/p' | head -n 1)
fault=$(tr -d '\r' <"$stack_log" | sed -n 's/^PANIC: double fault.*: 0x\([0-9a-f]\{16\}\)$/0x\1/p' | head -n 1)
[ -n "$guard" ] && [ -n "$fault" ] ||
	fail_check kernel-stack.report 'the guard range and the double-fault address' "guard '$guard', fault '$fault'" "$stack_log" 'KERNEL_STACK'
checks=$((checks + 1))
guard_low=${guard% *}
guard_high=${guard#* }
[ $((fault)) -ge $((guard_low)) ] && [ $((fault)) -lt $((guard_high)) ] ||
	fail_check kernel-stack.in-guard "a fault address in [$guard_low, $guard_high)" "$fault" "$stack_log" 'PANIC: double fault'
checks=$((checks + 1))
require_count kernel-stack.one-boot 'BOOT_SERVICES_EXITED' 1 "$stack_log" 'PANIC: double fault'
pass_group kernel-stack-overflow

# poweroff flushes storage and switches the machine off through ACPI S5 (ADR
# 0092). QEMU runs without -no-shutdown here, so it exits by itself when the
# guest powers off; a second boot from the same disk reads the file back.
rm -f "$poweroff_log" "$poweroff_after_log"
poweroff_disk=build/poweroff-virtio-blk.img
rm -f "$poweroff_disk"
go run ./tools/mkfat32 -tree "$system_tree" "$poweroff_disk"
( await_ready "$poweroff_log"
	printf 'write /home/poweroff.txt "kept across poweroff"\n'
	sleep 2
	printf 'poweroff\n'
	sleep 30
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$poweroff_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot >"$poweroff_log" 2>&1 &
pid=$!
elapsed=0
while kill -0 "$pid" 2>/dev/null && ! grep -q 'Powering off' "$poweroff_log" 2>/dev/null && [ "$elapsed" -lt 60 ]; do
	sleep 1
	elapsed=$((elapsed + 1))
done
# From the message on, the machine has ten seconds to switch itself off.
elapsed=0
while kill -0 "$pid" 2>/dev/null && [ "$elapsed" -lt 10 ]; do
	sleep 1
	elapsed=$((elapsed + 1))
done
qemu_running=0
kill -0 "$pid" 2>/dev/null && qemu_running=1
cleanup
require_marker poweroff.acpi ACPI_POWEROFF_OK "$poweroff_log"
require_regex poweroff.message '^Powering off\.\.\.' 'the power-off announced' "$poweroff_log" 'goos> poweroff'
[ "$qemu_running" -eq 0 ] ||
	fail_check poweroff.machine-off 'QEMU exited when the guest powered off' 'QEMU still running 10s after the message' "$poweroff_log" 'Powering off'
checks=$((checks + 1))
forbid_regex poweroff.no-panic 'PANIC' 'no panic on the way down' "$poweroff_log" 'Powering off'

( await_ready "$poweroff_after_log"
	printf 'cat /home/poweroff.txt\n'
	sleep 3
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$poweroff_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$poweroff_after_log" 2>&1 &
pid=$!
wait_for_marker poweroff-after '^kept across poweroff' 60 "$poweroff_after_log"
cleanup
require_regex poweroff.data-kept '^kept across poweroff' 'the file written before poweroff' "$poweroff_after_log" 'goos> cat /home/poweroff.txt'
pass_group poweroff

printf '\nPASS qemu-boot (%s checks)\n' "$checks"
