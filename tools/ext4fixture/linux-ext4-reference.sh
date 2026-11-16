#!/bin/sh
# Runs fsbench on Linux under KVM on a lab node, on a volume that
# make-bench-volume.sh made, so the numbers sit beside those of
# make test-pve-ext4. The guest is the Alpine "virt" kernel with BusyBox as
# /init; ext4 and virtio-blk load as modules from the same Alpine release.
#
# usage: tools/ext4fixture/linux-ext4-reference.sh KERNEL BUSYBOX MODULES OUT
#
# KERNEL   vmlinuz-virt from dl-cdn.alpinelinux.org/alpine/v3.22/releases/x86_64/netboot/
# BUSYBOX  a static x86_64 BusyBox
# MODULES  a tar of crc16.ko, mbcache.ko, jbd2.ko and ext4.ko from that
#          release's modloop-virt, plus virtio_blk.ko and crc32c_generic.ko
#          from its initramfs-virt, all of one kernel version
# OUT      where the bench lines go
#
# NODE (user@host) and NODE_PORT name the lab node; the command runs qemu
# there with sudo -n. ROUNDS (default 3) and SIZE in MiB (default 1024)
# match the GoOS scenario. The volume and the initramfs live in a scratch
# directory on the node that the script removes.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
kernel=${1:?kernel required}
busybox=${2:?busybox required}
modules=${3:?modules tar required}
out=${4:?output file required}
node=${NODE:?NODE=user@host required}
port=${NODE_PORT:-22}
rounds=${ROUNDS:-3}
size=${SIZE:-1024}

work=$(mktemp -d "${TMPDIR:-/tmp}/goos-ext4-ref.XXXXXX")
trap 'rm -rf "$work"' EXIT
root=$work/root
mkdir -p "$root/bin" "$root/dev" "$root/proc" "$root/sys" "$root/mnt/bench" "$root/system/linux" "$root/modules"
cp "$busybox" "$root/bin/busybox"
cp "$here/../../userspace/linux/prebuilt/fsbench" "$root/system/linux/fsbench"
tar xf "$modules" -C "$root/modules"
find "$root/modules" -name '*.ko' -exec mv {} "$root/modules/" \;
chmod 755 "$root/bin/busybox" "$root/system/linux/fsbench"
cat >"$root/init" <<INIT
#!/bin/busybox sh
bb=/bin/busybox
\$bb mount -t devtmpfs devtmpfs /dev
\$bb mount -t proc proc /proc
\$bb mount -t sysfs sysfs /sys
for module in crc16 crc32c_generic mbcache jbd2 ext4 virtio_blk; do
	\$bb insmod /modules/\$module.ko || echo "insmod \$module failed"
done
for attempt in 1 2 3 4 5 6 7 8 9 10; do
	[ -b /dev/vda ] && break
	\$bb sleep 0.5
done
\$bb mount -t ext4 /dev/vda /mnt/bench || echo 'mount failed'
\$bb grep /mnt/bench /proc/mounts
round=1
while [ \$round -le $rounds ]; do
	/system/linux/fsbench /mnt/bench </dev/null | \$bb sed "s/^/round=\$round /"
	round=\$((round + 1))
done
\$bb umount /mnt/bench
echo 'reference done'
\$bb poweroff -f
INIT
chmod 755 "$root/init"
(cd "$root" && find . | cpio -o -H newc 2>/dev/null | gzip -9) >"$work/initramfs.gz"
DOCKER_FLAGS=${DOCKER_FLAGS:-} "$here/make-bench-volume.sh" "$work/bench.img" "$size" >/dev/null
gzip -1 "$work/bench.img"

remote=$(ssh -p "$port" "$node" 'mktemp -d /var/tmp/goos-ext4-ref.XXXXXX')
cleanup_remote() { ssh -p "$port" "$node" "rm -rf '$remote'" || true; }
trap 'cleanup_remote; rm -rf "$work"' EXIT
scp -q -P "$port" "$kernel" "$work/initramfs.gz" "$work/bench.img.gz" "$node:$remote/"
ssh -p "$port" "$node" "gzip -d '$remote/bench.img.gz' && sudo -n timeout 1800 qemu-system-x86_64 -enable-kvm -cpu host -smp 4 -m 1024 \
	-kernel '$remote/$(basename "$kernel")' -initrd '$remote/initramfs.gz' \
	-append 'console=ttyS0 quiet panic=-1' \
	-drive file='$remote/bench.img',if=virtio,format=raw,cache=none,aio=native \
	-nographic -no-reboot" | tr -d '\r' >"$work/console"
grep -q 'reference done' "$work/console" || {
	echo 'the reference run did not finish:' >&2
	tail -n 40 "$work/console" >&2
	exit 1
}
grep '^round=[0-9]* bench ' "$work/console" | grep -v ' bench ok$' >"$out"
printf 'reference bench lines in %s\n' "$out"
