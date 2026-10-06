#!/bin/sh
# The network gate (ADR 0190, 0191). Each scenario boots the validation build
# from its own disposable disk made from this build's system tree:
#
#   1. one guest on QEMU user networking: DHCP, net status, ping to the
#      gateway, a killed and replaced driver, and resource counts that do not
#      grow;
#   2. two guests on one private stream backend with static addresses, each
#      answering the other's echo requests;
#   3. one guest without a network function: storage and the shell work, net
#      status says down, and ping fails within its deadline.
#
#   tests/boot/network.sh ISO OVMF_CODE
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
memory=${MEMORY:-512M}
read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'network.sh needs a VALIDATION=1 build' >&2; exit 2; }
tree=build/variants/$variant/system
work=build/network-test
logs=build/logs
rm -rf "$work"
mkdir -p "$work" "$logs"
checks=0
pids=

cleanup() {
	for pid in $pids; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	exec 3>&- 4>&- 2>/dev/null || true
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

fail() {
	printf 'FAIL %s\n  expected: %s\n  log: %s\n' "$1" "$2" "$3" >&2
	tail -n 60 "$3" >&2 || true
	exit 1
}

wait_for() { # pattern timeout log
	elapsed=0
	while [ "$elapsed" -lt "$2" ]; do
		grep -q -- "$1" "$3" 2>/dev/null && return 0
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail timeout "$1 within $2s" "$3"
}

# window prints what a command printed: the lines after the line that shows
# it typed and before the next prompt. A command typed twice is told apart
# by its trailing marker comment-free twin, so each typed line is unique.
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

require() { # name pattern log
	grep -q -- "$2" "$3" || fail "$1" "$2" "$3"
	checks=$((checks + 1))
}

# disk makes a disposable disk, optionally with network settings stored in
# the configuration file the configuration service restores at boot.
disk() { # name [address]
	rm -rf "$work/tree-$1"
	cp -R "$tree" "$work/tree-$1"
	if [ -n "${2:-}" ]; then
		printf 'system.net.mode text static\nsystem.net.ipv4.address text %s\nsystem.net.ipv4.prefix uint 24\nsystem.net.ipv4.gateway text 0.0.0.0\n' "$2" >"$work/tree-$1/state/config"
	fi
	go run ./tools/mkfat32 -new -tree "$work/tree-$1" "$work/$1.img"
}

# start boots one guest with extra QEMU arguments; its console input is the
# file descriptor fd. It waits for the prompt.
start() { # name fd extra-args...
	name=$1
	fd=$2
	shift 2
	rm -f "$work/$name.in"
	mkfifo "$work/$name.in"
	# shellcheck disable=SC2086
	qemu-system-x86_64 -machine q35 -m "$memory" -drive "if=pflash,format=raw,readonly=on,file=$firmware" \
		-serial stdio -display none -no-reboot -cdrom "$iso" \
		-drive "if=none,id=goosdisk,format=raw,file=$work/$name.img" -device virtio-blk-pci,drive=goosdisk,disable-modern=on \
		"$@" <"$work/$name.in" >"$logs/network-$name.log" 2>&1 &
	pids="$pids $!"
	eval "exec $fd>\"\$work/\$name.in\""
}

type_line() { # fd line
	printf '%s\n' "$2" >&"$1"
	sleep 1.5
}

stop() { # fd log
	type_line "$1" poweroff
	wait_for '^Powering off\.\.\.$' 30 "$2"
}

# held reads the resource counts quota reports for one process name, so two
# readings taken around a workload can be compared exactly.
held() { # log command name
	window "$1" "$2" | awk -v name="$3" '$2 == name { print $3, $4 }'
}

# --- 1. User networking, DHCP, ping, driver replacement ---------------------

disk user
log=$logs/network-user.log
start user 3 -netdev user,id=net0 -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4
wait_for 'goos> ' 90 "$log"
started=$(date +%s)
wait_for 'NETD_READY 10\.0\.2\.' 15 "$log"
printf 'dhcp ready %ss after the prompt\n' "$(($(date +%s) - started))"
sleep 2
type_line 3 'net status'
type_line 3 'quota'
type_line 3 'ping -c 4 -W 1000 10.0.2.2'
sleep 5
type_line 3 'ping -c 4 -W 1000 10.0.2.2 '
sleep 5
type_line 3 'quota '
type_line 3 'echo kept > /home/user/net.txt'
type_line 3 'fault net-driver'
wait_for NET_DRIVER_RESTARTED_OK 20 "$log"
sleep 8
type_line 3 'cat /home/user/net.txt'
type_line 3 'ping -c 4 -W 1000  10.0.2.2'
sleep 5
type_line 3 'quota  '
stop 3 "$log"

require user.pci PCI_NET_DISCOVERY_OK "$log"
require user.driver VIRTIO_NET_DRIVER_OK "$log"
require_after user.state 'net status' '^state: ready$' "$log"
require_after user.link 'net status' '^link: up$' "$log"
require_after user.mac 'net status' '^mac: 52:54:00:12:34:56$' "$log"
require_after user.mode 'net status' '^mode: dhcp$' "$log"
require_after user.address 'net status' '^address: 10\.0\.2\.[0-9]*/24$' "$log"
require_after user.gateway 'net status' '^gateway: 10\.0\.2\.2$' "$log"
require_after user.lease 'net status' '^lease: [0-9]* s$' "$log"
for command in 'ping -c 4 -W 1000 10.0.2.2' 'ping -c 4 -W 1000 10.0.2.2 ' 'ping -c 4 -W 1000  10.0.2.2'; do
	for sequence in 1 2 3 4; do
		require_after "user.reply.$sequence" "$command" "^40 bytes from 10\\.0\\.2\\.2: seq=$sequence time=[0-9]*\\.[0-9][0-9][0-9] ms$" "$log"
	done
	require_after user.summary "$command" '^4 sent, 4 received, 0% lost$' "$log"
	forbid_after user.exit "$command" 'exit status' "$log"
done
require user.fault NET_DRIVER_FAULT_INJECTED "$log"
require user.unbound NETD_DRIVER_UNBOUND "$log"
[ "$(grep -c '^VIRTIO_NET_DRIVER_OK$' "$log")" -ge 2 ] || fail user.replaced 'a second VIRTIO_NET_DRIVER_OK' "$log"
checks=$((checks + 1))
require_after user.file 'cat /home/user/net.txt' '^kept$' "$log"
grep -q 'VIRTIO_BLK_DRIVER_OK' "$log" || fail user.storage 'the storage driver running' "$log"
# The network service's and the driver's resource counts after the second
# round of pings equal those after the first: nothing grows per request.
before=$(held "$log" 'quota' netd)
after=$(held "$log" 'quota ' netd)
[ -n "$before" ] && [ "$before" = "$after" ] || fail user.netd-stable "netd counts '$before' unchanged, saw '$after'" "$log"
replaced=$(held "$log" 'quota  ' netd)
[ "$before" = "$replaced" ] || fail user.netd-after-restart "netd counts '$before' after the driver restart, saw '$replaced'" "$log"
driver_before=$(held "$log" 'quota' virtio-net)
driver_after=$(held "$log" 'quota  ' virtio-net)
[ -n "$driver_before" ] && [ "$driver_before" = "$driver_after" ] || fail user.driver-stable "driver counts '$driver_before' for its replacement, saw '$driver_after'" "$log"
checks=$((checks + 3))
printf 'PASS user networking (%s)\n' "$log"

# --- 2. Two guests on a private stream backend -----------------------------

disk left 192.0.2.10
disk right 192.0.2.11
socket=$work/stream.sock
left=$logs/network-left.log
right=$logs/network-right.log
start left 3 -netdev "stream,id=net0,server=on,addr.type=unix,addr.path=$socket" -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:00:02:10,addr=0x4
sleep 2
start right 4 -netdev "stream,id=net0,server=off,addr.type=unix,addr.path=$socket" -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:00:02:11,addr=0x4
wait_for 'goos> ' 90 "$left"
wait_for 'goos> ' 90 "$right"
wait_for 'NETD_READY 192\.0\.2\.10' 20 "$left"
wait_for 'NETD_READY 192\.0\.2\.11' 20 "$right"
sleep 2
type_line 3 'ping -c 4 192.0.2.11'
type_line 4 'ping -c 4 192.0.2.10'
sleep 8
type_line 3 'net status'
type_line 4 'net status'
stop 3 "$left"
stop 4 "$right"
for sequence in 1 2 3 4; do
	require_after "left.reply.$sequence" 'ping -c 4 192.0.2.11' "^40 bytes from 192\\.0\\.2\\.11: seq=$sequence " "$left"
	require_after "right.reply.$sequence" 'ping -c 4 192.0.2.10' "^40 bytes from 192\\.0\\.2\\.10: seq=$sequence " "$right"
done
require_after left.summary 'ping -c 4 192.0.2.11' '^4 sent, 4 received, 0% lost$' "$left"
require_after right.summary 'ping -c 4 192.0.2.10' '^4 sent, 4 received, 0% lost$' "$right"
require_after left.static 'net status' '^mode: static$' "$left"
require_after left.address 'net status' '^address: 192\.0\.2\.10/24$' "$left"
require_after right.gateway 'net status' '^gateway: none$' "$right"
printf 'PASS stream backend (%s, %s)\n' "$left" "$right"

# --- 3. No network function -----------------------------------------------

disk none
log=$logs/network-none.log
start none 3
wait_for 'goos> ' 90 "$log"
sleep 3
type_line 3 'cat /system/motd'
type_line 3 'net status'
type_line 3 'ping -c 1 -W 500 10.0.2.2'
sleep 3
stop 3 "$log"
require none.absent 'Network: no legacy virtio-net device' "$log"
require none.normal BOOT_MODE_NORMAL "$log"
require_after none.storage 'cat /system/motd' '^Welcome to GoOS$' "$log"
require_after none.down 'net status' '^state: down$' "$log"
require_after none.ping 'ping -c 1 -W 500 10.0.2.2' '^seq=1: ' "$log"
require_after none.ping-summary 'ping -c 1 -W 500 10.0.2.2' '^1 sent, 0 received, 100% lost$' "$log"
require_after none.ping-exit 'ping -c 1 -W 500 10.0.2.2' '^exit status: 1$' "$log"
printf 'PASS no network (%s)\n' "$log"

printf 'PASS network (%d checks)\n' "$checks"
