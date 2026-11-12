#!/bin/sh
# Boots one guest with the installed FAT32 system and three Linux-built ext4
# partitions (ADR 0226) and writes two of them: one mounted read-write from
# the configuration at boot, one from the shell. It checks the mount modes,
# writes through the shell, the refusals of a read-write mount, an instance
# killed with its journal live and recovered by its replacement, a Linux
# program on a writable volume, the VFS, the block service and the driver
# each restarted under both writable volumes, and a clean unmount that a
# read-only mount accepts. The guest is then cut off with the
# configured volume still mounted, and both written partitions are copied to
# OUT for the host checks: the GoOS reader and recovery, and e2fsck.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
out=${3:?output directory required}
memory=${MEMORY:-512M}
command_timeout=${COMMAND_TIMEOUT:-60}
read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'ext4-rw.sh needs a VALIDATION=1 build' >&2; exit 2; }
tree=build/variants/$variant/system
log=build/logs/ext4-rw.log
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|INIT_EXEC_.*FAILED\|VFS_.*FAILED\|BLOCK_.*FAILED'

mkdir -p build/logs "$out"
rm -f "$log" "$out"/*.img
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-ext4-rw.XXXXXX")
fifo=$work/in
mkfifo "$fifo"
pid=

cleanup() {
	exec 3>&- 2>/dev/null || true
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

fail() {
	printf 'FAIL %s: %s\n' "$log" "$1" >&2
	sed -n '1,80p' "$work/output" >&2 2>/dev/null || true
	exit 1
}

printf 'system.mount.1 text ext4 name:goos-rw-cfg /mnt/cfg rw\n' >"$work/config"
go run ./tools/mkfat32 -new -size auto -tree "$tree" -put "$work/config:/state/config" "$work/system.img" >/dev/null
go run ./tools/diskcompose \
	-part "goos-system:data:$work/system.img" \
	-part goos-rw-cfg:linux:userspace/ext4/testdata/canonical.img.gz \
	-part goos-rw-shell:linux:userspace/ext4/testdata/canonical.img.gz \
	-part goos-rw-1k:linux:userspace/ext4/testdata/blockmap-1k.img.gz \
	"$work/disk.img"
rm -f "$work/system.img"

prompts() { grep -c '^goos> ' "$log" 2>/dev/null || true; }

await_prompt() {
	want=$1
	elapsed=0
	while [ "$(prompts)" -lt "$want" ]; do
		grep -q "$fatal" "$log" 2>/dev/null && fail 'fatal marker'
		[ "$elapsed" -lt $((command_timeout * 20)) ] || fail "stall waiting for prompt $want"
		sleep 0.05
		elapsed=$((elapsed + 1))
	done
}

run() {
	start=$(wc -l <"$log")
	next=$(($(prompts) + 1))
	printf '%s\n' "$1" >&3
	await_prompt "$next"
	tail -n "+$((start + 1))" "$log" | tr -d '\r' >"$work/output"
}

expect() { grep -q "$1" "$work/output" || fail "$2"; }
refuse() { ! grep -q "$1" "$work/output" || fail "$2"; }

# await_content PATH TEXT WHAT reads PATH until it holds TEXT: a volume comes
# back only after its replacement replayed the journal and the VFS attached
# it.
await_content() {
	for attempt in $(seq 1 40); do
		run "cat $1"
		grep -q "^$2\$" "$work/output" && return 0
		sleep 1
	done
	fail "$3"
}

# write_marker PATH TEXT writes TEXT to PATH and reads it back.
write_marker() {
	run "echo $2 > $1"
	run "cat $1"
	expect "^$2\$" "a write after recovery to $1"
}

# held_fault FAULT PATH TEXT MARKER holds PATH open in a Linux program,
# injects FAULT, which must print MARKER, and requires the old descriptor to
# fail and a new open to read TEXT once the volume is back.
held_fault() {
	injected=$(grep -c "$4" "$log" || true)
	opened=$(grep -c 'hold opened' "$log" || true)
	settled=$(grep -c 'hold reopened\|hold stale accepted\|hold reopen failed\|hold failed' "$log" || true)
	rejected=$(grep -c 'hold stale rejected' "$log" || true)
	run "/system/linux/abi-probe hold $2 40 &"
	held=
	for attempt in $(seq 1 40); do
		[ "$(grep -c 'hold opened' "$log" || true)" -gt "$opened" ] && held=1 && break
		sleep 0.5
	done
	[ -n "$held" ] || fail "a descriptor held on $2 before fault $1"
	run "fault $1"
	[ "$(grep -c "$4" "$log" || true)" -gt "$injected" ] || fail "fault $1 not injected"
	await_content "$2" "$3" "$2 after fault $1"
	finished=
	for attempt in $(seq 1 120); do
		[ "$(grep -c 'hold reopened\|hold stale accepted\|hold reopen failed\|hold failed' "$log" || true)" -gt "$settled" ] && finished=1 && break
		sleep 0.5
	done
	[ -n "$finished" ] || fail "the held descriptor after fault $1 never finished"
	[ "$(grep -c 'hold stale rejected' "$log" || true)" -gt "$rejected" ] && grep 'hold reopened\|hold stale accepted\|hold reopen failed\|hold failed' "$log" | tail -1 | grep -q 'hold reopened' ||
		fail "a descriptor from before fault $1"
}

# The disk is a throwaway copy written in place: the host checks read what
# the guest left on it.
qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/disk.img" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$fifo" >"$log" 2>&1 &
pid=$!
exec 3>"$fifo"

elapsed=0
until [ "$(prompts)" -ge 1 ]; do
	grep -q "$fatal" "$log" 2>/dev/null && fail 'fatal marker during boot'
	[ "$elapsed" -lt 2400 ] || fail 'boot timeout'
	sleep 0.1
	elapsed=$((elapsed + 1))
done
grep -q 'VOLUME_MOUNT_OK' "$log" || fail 'configured volume not mounted at boot'

run 'mount'
expect '/mnt/cfg.*ext4.*rw' 'configured read-write mount'
run 'echo before-fault > /mnt/cfg/journal.txt'
run 'cat /mnt/cfg/journal.txt'
expect '^before-fault$' 'a file written on the configured volume'

run 'mount -t ext4 -o rw name:goos-rw-shell /mnt/w'
refuse '^mount:' 'read-write mount from the shell'
run 'mount'
expect '/mnt/w.*ext4.*rw' 'shell read-write mount entry'
run 'mkdir /mnt/w/made'
refuse 'mkdir:' 'mkdir on a writable volume'
run 'echo payload > /mnt/w/made/file.txt'
run 'mv /mnt/w/made/file.txt /mnt/w/moved.txt'
refuse '^mv:' 'rename on a writable volume'
run 'cat /mnt/w/moved.txt'
expect '^payload$' 'renamed file content'
run 'rm /mnt/w/hello.txt'
refuse '^rm:' 'unlink on a writable volume'
run 'ls /mnt/w'
refuse 'hello.txt' 'an unlinked name still listed'
expect 'moved.txt' 'the renamed file in its listing'
run 'df'
expect '^ext4 .*/mnt/w .*rw' 'df of a writable volume'

run 'mount -o rw name:goos-rw-1k /mnt/k'
expect 'refused' 'a read-write mount of 1 KiB blocks'
run 'mount -o rwx name:goos-rw-1k /mnt/k'
expect 'invalid mount mode' 'an unknown mount mode'
run 'mount name:goos-rw-1k /mnt/k'
refuse '^mount:' 'the same volume read-only'
run 'mkdir /mnt/k/new'
expect 'mkdir:' 'a write to the read-only volume'
run 'umount /mnt/k'

# Kill the configured volume's instance. Its replacement gets a new
# writable grant, replays the journal, and serves what the old one wrote.
run 'fault volume'
grep -q 'VOLUME_FAULT_INJECTED' "$log" || fail 'volume fault not injected'
run 'cat /mnt/w/moved.txt'
expect '^payload$' 'the other writable volume during the replacement'
recovered=
for attempt in 1 2 3 4 5 6 7 8 9 10; do
	run 'cat /mnt/cfg/journal.txt'
	if grep -q '^before-fault$' "$work/output"; then
		recovered=1
		break
	fi
	sleep 1
done
[ -n "$recovered" ] || fail 'replacement does not serve what its predecessor wrote'
run 'mount'
expect '/mnt/cfg.*ext4.*rw' 'replacement mounted read-write'
run 'echo after-fault > /mnt/cfg/second.txt'
run 'cat /mnt/cfg/second.txt'
expect '^after-fault$' 'a write to the replacement'

# A Linux program renames onto another link of the same inode, writes a
# file whose last name went, and syncs files and directories.
run '/system/linux/abi-probe ext4-rw /mnt/w'
expect '^ext4 rw ok$' 'a Linux program on a writable volume'

# A restarted VFS has no volumes; each instance ends with it, and its
# replacement replays the journal and is attached again at its path.
write_marker /mnt/cfg/vfs.txt vfs-marker
restarts=$(grep -c 'VFS_RESTARTED_OK' "$log" || true)
run 'fault vfs'
restarted=
for attempt in $(seq 1 60); do
	[ "$(grep -c 'VFS_RESTARTED_OK' "$log" || true)" -gt "$restarts" ] && restarted=1 && break
	sleep 0.5
done
[ -n "$restarted" ] || fail 'the VFS was not restarted'
await_content /mnt/cfg/vfs.txt vfs-marker 'the configured volume after a VFS restart'
await_content /mnt/w/moved.txt payload 'the shell volume after a VFS restart'
write_marker /mnt/cfg/after-vfs.txt after-vfs

# The block service and the driver: every instance gets a new writable
# grant from the storage that came back, and a descriptor opened before
# stays dead.
held_fault block /mnt/w/moved.txt payload BLOCK_FAULT_INJECTED
await_content /mnt/cfg/after-vfs.txt after-vfs 'the configured volume after a block restart'
write_marker /mnt/cfg/after-block.txt after-block
held_fault driver /mnt/cfg/journal.txt before-fault BLOCK_DRIVER_FAULT_INJECTED
await_content /mnt/w/moved.txt payload 'the shell volume after a driver restart'
write_marker /mnt/cfg/after-driver.txt after-driver
write_marker /mnt/w/after-driver.txt shell-after-driver
run 'mount'
expect '/mnt/cfg.*ext4.*rw' 'the configured volume read-write after the restarts'
expect '/mnt/w.*ext4.*rw' 'the shell volume read-write after the restarts'

# The storage restarts charged no restart budget: the configured volume
# still survives as many crashes of its own as the budget allows.
for crash in 1 2 3; do
	injected=$(grep -c 'VOLUME_FAULT_INJECTED' "$log" || true)
	run 'fault volume'
	[ "$(grep -c 'VOLUME_FAULT_INJECTED' "$log" || true)" -gt "$injected" ] || fail "volume crash $crash not injected"
	await_content /mnt/cfg/after-driver.txt after-driver "the configured volume after crash $crash past the storage restarts"
	write_marker /mnt/cfg/crash-$crash.txt crash-$crash
done
grep -q 'VOLUME_OFFLINE' "$log" && fail 'a volume went offline'

# A clean unmount leaves a volume the read-only mount accepts.
run 'umount /mnt/w'
refuse '^umount:' 'unmount of a writable volume'
run 'mount name:goos-rw-shell /mnt/w'
refuse '^mount:' 'read-only mount after a clean unmount'
run 'cat /mnt/w/moved.txt'
expect '^payload$' 'content after remount'
run 'mkdir /mnt/w/x'
expect 'mkdir:' 'a write after a read-only remount'
run 'umount /mnt/w'

# Cut the guest off with /mnt/cfg mounted: its journal is live.
kill "$pid"
wait "$pid" 2>/dev/null || true
pid=
go run ./tools/diskcompose -extract goos-rw-shell "$work/disk.img" "$out/clean.img"
go run ./tools/diskcompose -extract goos-rw-cfg "$work/disk.img" "$out/dirty.img"
printf 'PASS %s\n' "$log"
