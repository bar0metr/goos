#!/bin/sh
# The installed system and recovery boot (ADR 0175). Every boot runs the
# validation build against a disposable disk made from this build's system
# tree; nothing here touches the disk make run uses.
#
#   tests/boot/system.sh ISO OVMF_CODE
#
# The variant, its recovery ISO, its system tree, and its archive inputs are
# found through build/config/build, which make iso writes.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
memory=${MEMORY:-512M}
read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'system.sh needs a VALIDATION=1 build' >&2; exit 2; }
dir=build/variants/$variant
tree=$dir/system
recovery_iso=$dir/goos-x86_64-recovery.iso
version=$(sed -n 's/^[[:space:]]*Release[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' internal/version/version.go)
work=build/system-test
logs=build/logs
rm -rf "$work"
mkdir -p "$work" "$logs"
checks=0
qpid=

cleanup() {
	if [ -n "$qpid" ]; then
		kill "$qpid" 2>/dev/null || true
		wait "$qpid" 2>/dev/null || true
	fi
	exec 3>&- 2>/dev/null || true
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

fail() {
	printf 'FAIL %s\n  expected: %s\n  log: %s\n' "$1" "$2" "$3" >&2
	tail -n 40 "$3" >&2 || true
	exit 1
}

require() { # name pattern log
	grep -q -- "$2" "$3" || fail "$1" "$2" "$3"
	checks=$((checks + 1))
}

forbid() { # name pattern log
	if grep -q -- "$2" "$3"; then
		fail "$1" "no line matching $2" "$3"
	fi
	checks=$((checks + 1))
}

count_at_least() { # name pattern count log
	observed=$(grep -c -- "$2" "$4" || true)
	[ "$observed" -ge "$3" ] || fail "$1" "at least $3 lines matching $2, saw $observed" "$4"
	checks=$((checks + 1))
}

# window prints what a command printed: the lines after the line that shows
# it typed, at the prompt or, when service output split the prompt from it,
# alone, and before the next prompt.
window() { # log command
	awk -v command="$2" 'seen && index($0, "goos> ") == 1 { exit } seen { print } !seen && ($0 == "goos> " command || $0 == command) { seen = 1 }' "$1"
}

require_after() { # name command pattern log
	window "$4" "$2" | grep -q -- "$3" || fail "$1" "$3 after '$2'" "$4"
	checks=$((checks + 1))
}

forbid_after() { # name command pattern log
	if window "$4" "$2" | grep -q -- "$3"; then
		fail "$1" "no $3 after '$2'" "$4"
	fi
	checks=$((checks + 1))
}

wait_for() { # pattern timeout log
	elapsed=0
	while [ "$elapsed" -lt "$2" ]; do
		grep -q -- "$1" "$3" 2>/dev/null && return 0
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail "timeout" "$1 within $2s" "$3"
}

# boot starts QEMU on iso with disk (or none), waits for a prompt, types the
# lines of the script on stdin, powers the machine off, and waits for QEMU to
# end. A line "@sleep N" waits N seconds; "@keys TEXT" sends TEXT, escapes
# expanded, with no newline. Every other line is a command.
boot() { # iso disk log
	image=$1
	disk=$2
	log=$3
	rm -f "$log" "$work/in"
	mkfifo "$work/in"
	set -- -machine q35 -m "$memory" -drive "if=pflash,format=raw,readonly=on,file=$firmware" -serial stdio -display none -no-reboot -cdrom "$image"
	if [ "$disk" != none ]; then
		set -- "$@" -drive "if=none,id=goosdisk,format=raw,file=$disk" -device virtio-blk-pci,drive=goosdisk,disable-modern=on
	fi
	qemu-system-x86_64 "$@" <"$work/in" >"$log" 2>&1 &
	qpid=$!
	exec 3>"$work/in"
	started=$(date +%s)
	wait_for 'goos> ' 90 "$log"
	prompt_seconds=$(($(date +%s) - started))
	# The validation build replaces the storage driver once the system is
	# up; let the session settle before typing.
	sleep 4
	while IFS= read -r line; do
		case $line in
		'@sleep '*) sleep "${line#@sleep }" ;;
		'@keys '*) printf '%b' "${line#@keys }" >&3; sleep 1 ;;
		*) printf '%s\n' "$line" >&3; sleep 1.5 ;;
		esac
	done
	printf 'poweroff\n' >&3
	elapsed=0
	while kill -0 "$qpid" 2>/dev/null && [ "$elapsed" -lt 30 ]; do
		sleep 1
		elapsed=$((elapsed + 1))
	done
	kill "$qpid" 2>/dev/null || true
	wait "$qpid" 2>/dev/null || true
	qpid=
	exec 3>&-
	rm -f "$work/in"
}

# disk_from makes a disposable disk from a copy of the system tree, after
# running an edit of the copy.
disk_from() { # name edit-command...
	name=$1
	shift
	rm -rf "$work/tree-$name"
	cp -R "$tree" "$work/tree-$name"
	(cd "$work/tree-$name" && "$@")
	go run ./tools/mkfat32 -new -tree "$work/tree-$name" "$work/$name.img"
}

# --- Archive contents and the default disk (proof 8) ------------------------

# The production archive is built from the same inputs without the
# validation probes; it must hold the recovery commands and nothing else of
# the distribution.
inputs="$dir/userspace/init $dir/userspace/fault-probe $dir/userspace/vfs $dir/userspace/shell $dir/userspace/block $dir/userspace/virtio-blk $dir/userspace/tty $dir/userspace/fat32 $dir/userspace/bin $dir/userspace/commands"
# shellcheck disable=SC2086
go run ./tools/initramfs $inputs "$work/production.tar"
tar -tf "$work/production.tar" >"$work/production.list"
for member in system/init system/vfs system/shell system/tty system/fat32 system/commands system/bin/ls system/bin/cat system/bin/edit system/bin/fsck-fat system/bin/write; do
	grep -qx "$member" "$work/production.list" || fail archive.member "$member in the production archive" "$work/production.list"
	checks=$((checks + 1))
done
for excluded in system/bin/gfm system/bin/bench system/bin/uname system/bin/args system/linux/ system/linux/abi-probe system/linux/raw-probe system/fault-probe system/config system/log system/force-recovery; do
	if grep -qx "$excluded" "$work/production.list"; then
		fail archive.exclusion "no $excluded in the production archive" "$work/production.list"
	fi
	checks=$((checks + 1))
done
printf 'PASS archive (production holds %s members)\n' "$(wc -l <"$work/production.list" | tr -d ' ')"

# The default run never changes a disk that exists, and the explicit
# installation refuses one.
go run ./tools/mkfat32 -new -tree "$tree" "$work/kept.img"
before=$(cksum <"$work/kept.img")
make --no-print-directory run-disk DISK="$work/kept.img" VALIDATION=1 >"$logs/system-run-disk.log" 2>&1
if make --no-print-directory disk DISK="$work/kept.img" VALIDATION=1 >>"$logs/system-run-disk.log" 2>&1; then
	fail disk.refuse 'make disk refuses an existing disk' "$logs/system-run-disk.log"
fi
[ "$(cksum <"$work/kept.img")" = "$before" ] || fail disk.unchanged 'an existing disk left as it was' "$logs/system-run-disk.log"
checks=$((checks + 3))
printf 'PASS default-disk\n'

# --- Normal boot from the installed disk (proofs 1, 4, 6, 7) ----------------

go run ./tools/mkfat32 -new -tree "$tree" "$work/installed.img"
log=$logs/system-normal.log
boot "$iso" "$work/installed.img" "$log" <<'EOF'
services
uname
args one
echo persisted-data > /home/user/data.txt
cp /system/bin/uname /home/user/bin/myuname
cp /system/bin/fsck-fat /home/user/bin/fsck-fat
echo x > /system/motd
rm /persist/system/motd
mv /persist/SYSTEM/release /home/user/release
touch /persist/System/bin/new
mkdir /system/extra
cp /home/user/data.txt /persist/system/commands
mv /home/user/data.txt /system/bin/data
mkdir /tmp/fake
mv /tmp/fake /persist/system/fake
/system/linux/fsprobe /system/motd
/system/linux/fsprobe /persist/SYSTEM/linux-made
cat /system/motd
cat /system/release
stat /persist/system/bin/new
stat /persist/system/fake
/home/user/bin/fsck-fat --check auto
fsck-fat --check auto
cat /home/user/data.txt
echo pipe-data | readline | readline
echo redirected-data > /tmp/redirected.txt
readline < /tmp/redirected.txt
/system/linux/musl-hello one two
EOF
require normal.mode '^\[MODE\] normal' "$log"
require normal.published SYSTEM_PUBLISHED_OK "$log"
require normal.protected SYSTEM_PROTECTED_OK "$log"
for service in config log shell; do
	require "normal.service-$service" "SERVICE_FROM_DISK_OK $service" "$log"
	require_after "normal.running-$service" services "^$service  *running" "$log"
done
require_after normal.disk-command uname "^GoOS $version x86_64" "$log"
require_after normal.disk-source uname SYSTEM_COMMAND_FROM_DISK_OK "$log"
require_after normal.path 'args one' 'PATH=/system/bin:/home/user/bin' "$log"
for command in 'echo x > /system/motd' 'rm /persist/system/motd' 'mv /persist/SYSTEM/release /home/user/release' 'touch /persist/System/bin/new' \
	'mkdir /system/extra' 'cp /home/user/data.txt /persist/system/commands' 'mv /home/user/data.txt /system/bin/data' 'mv /tmp/fake /persist/system/fake'; do
	require_after "normal.refused: $command" "$command" 'read-only filesystem\|permission denied\|busy' "$log"
done
require_after normal.linux-refused '/system/linux/fsprobe /system/motd' 'write failed: errno' "$log"
forbid_after normal.linux-not-written '/system/linux/fsprobe /system/motd' 'write ok' "$log"
require_after normal.linux-alias-refused '/system/linux/fsprobe /persist/SYSTEM/linux-made' 'write failed: errno' "$log"
require_after normal.motd-intact 'cat /system/motd' '^Welcome to GoOS$' "$log"
require_after normal.record-intact 'cat /system/release' "^release $version$" "$log"
require_after normal.no-new 'stat /persist/system/bin/new' 'no such file' "$log"
require_after normal.no-fake 'stat /persist/system/fake' 'no such file' "$log"
# A copy of fsck-fat under the user's directory is an ordinary program: no
# index describes it, so it gets no storage slice and nothing is unmounted.
forbid_after normal.copy-no-authority '/home/user/bin/fsck-fat --check auto' 'FSCK_FAT_CHECK_OK\|volume in use' "$log"
require_after normal.busy 'fsck-fat --check auto' 'volume in use by the running system' "$log"
forbid normal.busy-no-unmount FAT32_RESTARTED_OK "$log"
require_after normal.still-mounted 'cat /home/user/data.txt' '^persisted-data$' "$log"
require_after normal.pipeline 'echo pipe-data | readline | readline' '^pipe-data$' "$log"
require_after normal.redirection 'readline < /tmp/redirected.txt' '^redirected-data$' "$log"
require_after normal.linux '/system/linux/musl-hello one two' 'hello from musl' "$log"
require normal.flushed 'Powering off' "$log"
printf 'PASS normal (prompt after %ss)\n' "$prompt_seconds"

# --- Reboot of the same disk, then storage faults (proofs 2, 5) -------------

log=$logs/system-restart.log
boot "$iso" "$work/installed.img" "$log" <<'EOF'
cat /home/user/data.txt
myuname
fault fat32
@sleep 4
uname
echo x > /system/motd
fault vfs
@sleep 6
uname
rm /persist/system/motd
fault driver
@sleep 4
uname
fault storage
@sleep 6
echo after-storage
services
ls /bootstrap/bin
EOF
require_after restart.persisted 'cat /home/user/data.txt' '^persisted-data$' "$log"
require_after restart.user-command myuname "^GoOS $version x86_64" "$log"
require_after restart.user-command-file myuname FILE_EXEC_OK "$log"
require restart.fat32 FAT32_RESTARTED_OK "$log"
require restart.vfs VFS_RESTARTED_OK "$log"
require restart.driver BLOCK_DRIVER_REPLACED_OK "$log"
count_at_least restart.reattached SYSTEM_REATTACHED_OK 2 "$log"
count_at_least restart.disk-commands '^GoOS .* x86_64$' 4 "$log"
require_after restart.protected-after-fat32 'echo x > /system/motd' 'read-only filesystem' "$log"
require_after restart.protected-after-vfs 'rm /persist/system/motd' 'read-only filesystem\|permission denied' "$log"
require restart.storage-lost STORAGE_LOSS_INJECTED "$log"
require restart.recovery '^\[MODE\] recovery: stage=runtime' "$log"
require restart.recovery-sessions RECOVERY_SESSIONS_OK "$log"
require_after restart.recovery-prompt 'echo after-storage' '^after-storage$' "$log"
require_after restart.fat32-failed services '^fat32  *failed' "$log"
require_after restart.recovery-commands 'ls /bootstrap/bin' '^fsck-fat$' "$log"
count_after=$(window "$log" services | grep -c '^shell  *running' || true)
[ "$count_after" -eq 1 ] || fail restart.one-shell 'one serial shell after the transition' "$log"
checks=$((checks + 1))
printf 'PASS restart\n'

# --- Forced recovery checks the installed volume (proof 6) ------------------

log=$logs/system-forced.log
boot "$recovery_iso" "$work/installed.img" "$log" <<'EOF'
fsck-fat --check auto
@sleep 6
cat /home/user/data.txt
EOF
require forced.mode '^\[MODE\] recovery: stage=forced' "$log"
forbid forced.no-installed-services 'SERVICE_FROM_DISK_OK' "$log"
require forced.check FSCK_FAT_CHECK_OK "$log"
require forced.clean 'errors=0' "$log"
require forced.remounted FAT32_RESTARTED_OK "$log"
require_after forced.data 'cat /home/user/data.txt' '^persisted-data$' "$log"
printf 'PASS forced-recovery\n'

# --- Recovery for each way the installed system can be unusable (proof 3) ---

recovery_case() { # name disk stage reason
	log=$logs/system-recovery-$1.log
	boot "$iso" "$2" "$log" <<'EOF'
ls /bootstrap/bin
cat /bootstrap/motd
df
mount
EOF
	require "recovery.$1.mode" "^\[MODE\] recovery: stage=$3 .*reason=$4" "$log"
	forbid "recovery.$1.no-installed-services" 'SERVICE_FROM_DISK_OK' "$log"
	require_after "recovery.$1.commands" 'ls /bootstrap/bin' '^edit$' "$log"
	require_after "recovery.$1.motd" 'cat /bootstrap/motd' '^Welcome to GoOS$' "$log"
	[ "$prompt_seconds" -le 60 ] || fail "recovery.$1.bounded" 'a prompt within 60 seconds' "$log"
	checks=$((checks + 1))
	printf 'PASS recovery %s (prompt after %ss)\n' "$1" "$prompt_seconds"
}

recovery_case missing-disk none storage 'no usable volume'

go run ./tools/mkfat32 -new "$work/blank.img"
recovery_case blank "$work/blank.img" record 'no installed system'

disk_from invalid-record sh -c 'printf "garbage\n" >system/release'
recovery_case invalid-record "$work/invalid-record.img" record 'record malformed'

disk_from old-release sh -c 'sed "s/^release .*/release 0.0.0/" system/release >r && mv r system/release'
recovery_case old-release "$work/old-release.img" record "release mismatch: installed 0.0.0, boot $version"

disk_from no-shell rm system/services/shell
recovery_case no-shell "$work/no-shell.img" services missing

disk_from bad-index sh -c 'printf "bad line\n" >>system/commands'
recovery_case bad-index "$work/bad-index.img" index 'malformed index'

# Recovery is usable: edit a file and check the volume it boots without.
log=$logs/system-recovery-tools.log
boot "$iso" "$work/blank.img" "$log" <<'EOF'
write /home/note.txt recovered text
edit /home/note.txt
@sleep 2
@keys X
@keys \033[12~
@sleep 1
@keys \033[21~
@sleep 1
cat /home/note.txt
stat /home/note.txt
fsck-fat --check auto
@sleep 6
ls /home
EOF
require tools.edit-save EDIT_SAVE_OK "$log"
require tools.edit-exit EDIT_EXIT_OK "$log"
require_after tools.edited 'cat /home/note.txt' '^Xrecovered text$' "$log"
require_after tools.stat 'stat /home/note.txt' '^file' "$log"
require tools.check FSCK_FAT_CHECK_OK "$log"
require_after tools.after-check 'ls /home' '^note.txt$' "$log"
printf 'PASS recovery tools\n'

printf 'PASS system boot (%d checks)\n' "$checks"
