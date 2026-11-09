#!/bin/sh
# Boots one guest whose disk holds the installed FAT32 system and three more
# partitions: two Linux-built ext4 volumes and one with no filesystem
# (ADR 0222). It checks the configured mount at boot, mount and umount from
# the shell, every refusal the mount policy names, a crashed instance being
# replaced while the other volumes keep serving, and a slot being reused.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
memory=${MEMORY:-512M}
command_timeout=${COMMAND_TIMEOUT:-60}
read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'volumes.sh needs a VALIDATION=1 build' >&2; exit 2; }
tree=build/variants/$variant/system
log=build/logs/volumes.log
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|INIT_EXEC_.*FAILED\|VFS_.*FAILED\|BLOCK_.*FAILED'

mkdir -p build/logs
rm -f "$log"
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-volumes.XXXXXX")
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

# The configured volume mounts at boot; the configuration file is part of
# the installed volume's state.
printf 'system.mount.1 text auto name:goos-ext4-b /mnt/cfg\n' >"$work/config"
go run ./tools/mkfat32 -new -size auto -tree "$tree" -put "$work/config:/state/config" "$work/system.img" >/dev/null
go run ./tools/diskcompose \
	-part "goos-system:data:$work/system.img" \
	-part goos-ext4-a:linux:userspace/ext4/testdata/canonical.img.gz \
	-part goos-ext4-b:linux:userspace/ext4/testdata/blockmap-1k.img.gz \
	-part goos-junk:linux:zero:4 \
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

qemu-system-x86_64 -machine q35 -m "$memory" -accel tcg,thread=multi \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/disk.img,snapshot=on" \
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

run 'cat /mnt/cfg/hello.txt'
expect '^hello ext3$' 'configured ext3-style volume content'

run 'mount -t ext4 name:goos-ext4-a /mnt/a'
refuse '^mount:' 'mount of the second volume'
run 'cat /mnt/a/hello.txt'
expect '^hello ext4$' 'ext4 volume content'
run 'ls /mnt/a/dir/sub'
expect 'deep.txt' 'ext4 directory listing'
run 'mount'
expect '/mnt/a.*ext4.*ro' 'mount table entry'
run 'ls /persist'
expect 'system' 'persistent volume beside the additional ones'

run 'mount name:goos-junk /mnt/j'
expect 'unsupported filesystem' 'a partition with no filesystem'
run 'mount part:2 /mnt/x'
expect 'already mounted' 'a second instance on a mounted partition'
run 'mount part:1 /mnt/x'
expect 'already mounted' 'an instance on the persistent volume'
run 'mount name:goos-missing /mnt/x'
expect 'no such partition' 'a missing partition'
run 'mount name:goos-ext4-a /system'
expect 'invalid mount path' 'a path outside /mnt'
run 'mount -t fat32 name:goos-junk /mnt/x'
expect 'unsupported filesystem\|another filesystem type' 'a forced type'
run 'mkdir /mnt/a/new'
expect 'mkdir:' 'a write to a read-only volume'

run 'fault volume'
grep -q 'VOLUME_FAULT_INJECTED' "$log" || fail 'volume fault not injected'
run 'cat /mnt/a/hello.txt'
expect '^hello ext4$' 'other volume during a replacement'
recovered=
for attempt in 1 2 3 4 5 6 7 8 9 10; do
	run 'cat /mnt/cfg/hello.txt'
	if grep -q '^hello ext3$' "$work/output"; then
		recovered=1
		break
	fi
	sleep 1
done
[ -n "$recovered" ] || fail 'replaced instance does not serve its volume'

run 'umount /mnt/a'
refuse '^umount:' 'unmount'
run 'cat /mnt/a/hello.txt'
refuse '^hello ext4$' 'unmounted volume still served'
grep -q 'VOLUME_UNMOUNT_OK' "$log" || fail 'unmount marker'
run 'mount name:goos-ext4-a /mnt/again'
refuse '^mount:' 'remount in a released slot'
run 'cat /mnt/again/hello.txt'
expect '^hello ext4$' 'remounted volume content'
run 'umount /mnt/nothing'
expect 'nothing is mounted there' 'unmount of an unknown path'

printf 'PASS %s\n' "$log"
