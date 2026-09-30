#!/bin/sh
# Runs the conformance probe on a real Linux kernel booted in QEMU, with no
# user-mode emulator between the program and the kernel. This is the strict
# reference: the probe runs with ABI_PROBE_STRICT set and accepts only the
# exact Linux result for every check, the hostile ones included.
#
# The caller supplies the kernel and a static BusyBox, which is the guest's
# /init shell and nothing more. Neither is kept in the repository. An Alpine
# "virt" kernel works as it comes:
#
#   https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/x86_64/netboot/vmlinuz-virt
#
# usage: userspace/linux/run-on-linux-kernel.sh KERNEL BUSYBOX [fixture]
set -eu

here=$(cd "$(dirname "$0")" && pwd)
kernel=${1:?path to a Linux x86_64 kernel image required}
busybox=${2:?path to a static x86_64 BusyBox required}
fixture=${3:-abi-probe}
timeout=${TIMEOUT:-120}

work=$(mktemp -d "${TMPDIR:-/tmp}/goos-linux-kernel.XXXXXX")
pid=
cleanup() {
	[ -z "$pid" ] || kill "$pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

root=$work/root
mkdir -p "$root/bin" "$root/dev" "$root/tmp" "$root/system/linux"
cp "$busybox" "$root/bin/busybox"
cp "$here/prebuilt/$fixture" "$root/system/linux/$fixture"
chmod 755 "$root/bin/busybox" "$root/system/linux/$fixture"
cat > "$root/init" <<INIT
#!/bin/busybox sh
/bin/busybox mount -t devtmpfs devtmpfs /dev
cd /system/linux
export ABI_PROBE_STRICT=1 GOOS_LINUX_PROBE=musl
echo "== default"
/system/linux/$fixture one two < /dev/null | /bin/busybox cat
echo "== hostile"
/system/linux/$fixture hostile < /dev/null | /bin/busybox cat
echo "== process"
/system/linux/$fixture fork < /dev/null | /bin/busybox cat
/system/linux/$fixture vfork < /dev/null | /bin/busybox cat
/system/linux/$fixture orphan < /dev/null | /bin/busybox cat
echo "== done"
/bin/busybox poweroff -f
INIT
chmod 755 "$root/init"
(cd "$root" && find . | LC_ALL=C sort | cpio -o -H newc 2>/dev/null | gzip -9 > "$work/initramfs.cpio.gz")

log=$work/serial.log
: > "$log"
qemu-system-x86_64 -machine q35 -m 256M -kernel "$kernel" -initrd "$work/initramfs.cpio.gz" \
	-append "console=ttyS0 rdinit=/init panic=-1 quiet" \
	-display none -serial "file:$log" -no-reboot >/dev/null 2>&1 &
pid=$!

elapsed=0
until grep -q '^== done' "$log" 2>/dev/null; do
	if ! kill -0 "$pid" 2>/dev/null || [ "$elapsed" -ge $((timeout * 10)) ]; then
		tr -d '\r' < "$log"
		echo "FAIL the guest ended or timed out before the probe finished" >&2
		exit 1
	fi
	sleep 0.1
	elapsed=$((elapsed + 1))
done

tr -d '\r' < "$log" | sed -n '/^== default/,/^== done/p'
status=0
tr -d '\r' < "$log" | grep -q '^abi-probe ok$' || { echo "FAIL default mode" >&2; status=1; }
tr -d '\r' < "$log" | grep -q '^hostile ok$' || { echo "FAIL hostile mode" >&2; status=1; }
if [ "$fixture" = abi-probe ]; then
	for line in 'fork parent ok' 'vfork ok' 'clone ok' 'orphan ok'; do
		tr -d '\r' < "$log" | grep -q "^$line\$" || { echo "FAIL process mode: no $line" >&2; status=1; }
	done
fi
[ "$status" != 0 ] || echo "PASS linux-kernel-reference ($(tr -d '\r' < "$log" | grep -c ' ok$') lines ok)"
exit "$status"
