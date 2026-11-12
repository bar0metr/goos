#!/bin/sh
# Boots one guest with the installed FAT32 system and three Linux-built ext4
# partitions (ADR 0226) and writes two of them: one mounted read-write from
# the configuration at boot, one from the shell. It checks the mount modes,
# writes through the shell, the refusals of a read-write mount, an instance
# killed with its journal live and recovered by its replacement, and a clean
# unmount that a read-only mount accepts. The guest is then cut off with the
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
