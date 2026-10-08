#!/bin/sh
# The network gate (ADR 0190, 0191). Each scenario boots the validation build
# from its own disposable disk made from this build's system tree:
#
#   1. one guest on QEMU user networking: DHCP, net status, ping to the
#      gateway, a killed and replaced driver, and resource counts that do not
#      grow;
#   2. two guests on one private stream backend with static addresses, each
#      answering the other's echo requests, DNS and generic UDP against the
#      test peer (ADR 0195), and a reload of tuning settings that applies a
#      valid candidate and refuses an invalid one by key;
#   3. one guest without a network function and with an invalid stored
#      setting: storage and the shell work, net status says down and reports
#      the built-in defaults, ping fails within its deadline, and UDP over lo
#      still works.
#
#   tests/boot/network.sh ISO OVMF_CODE
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
memory=${MEMORY:-512M}
# SMP is each guest's processor count, as a QEMU -smp argument.
smp=${SMP:-1}
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

require_count_after() { # name command pattern count log
	count=$(window "$5" "$2" | grep -c -- "$3" || true)
	[ "$count" = "$4" ] || fail "$1" "$4 occurrences of $3 after '$2', saw $count" "$5"
	checks=$((checks + 1))
}

wait_command() { # command timeout log
	elapsed=0
	while [ "$elapsed" -lt "$2" ]; do
		window "$3" "$1" | grep -q '^SHELL_EXEC_OK$' && return 0
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail timeout "'$1' completion within $2s" "$3"
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
# the configuration file the configuration service restores at boot: a
# static address, and one more stored line.
disk() { # name [address] [line]
	rm -rf "$work/tree-$1"
	cp -R "$tree" "$work/tree-$1"
	if [ -n "${2:-}" ]; then
		printf 'system.net.mode text static\nsystem.net.ipv4.address text %s\nsystem.net.ipv4.prefix uint 24\nsystem.net.ipv4.gateway text 0.0.0.0\n' "$2" >"$work/tree-$1/state/config"
	fi
	if [ -n "${3:-}" ]; then
		printf '%s\n' "$3" >>"$work/tree-$1/state/config"
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
	qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" -drive "if=pflash,format=raw,readonly=on,file=$firmware" \
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

# NETWORK_SCENARIOS limits a development run to some of the scenarios.
scenarios=${NETWORK_SCENARIOS:-user stream none}
wants() { case " $scenarios " in *" $1 "*) return 0 ;; esac; return 1; }

# --- 1. User networking, DHCP, ping, driver replacement ---------------------

if wants user; then

disk user
log=$logs/network-user.log
start user 3 -netdev user,id=net0 -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4
wait_for 'goos> ' 90 "$log"
started=$(date +%s)
wait_for 'NETD_READY 10\.0\.2\.' 15 "$log"
printf 'dhcp ready %ss after the prompt\n' "$(($(date +%s) - started))"
sleep 2
type_line 3 'net status'
type_line 3 'resolve status'
type_line 3 'ping -c 1 localhost'
type_line 3 'ping -c 4 -W 1000 10.0.2.2'
sleep 5
type_line 3 'quota'
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
type_line 3 'fault net-driver'
sleep 12
type_line 3 'ping -c 4 -W 1000   10.0.2.2'
sleep 5
type_line 3 'quota   '
stop 3 "$log"

require user.pci PCI_NET_DISCOVERY_OK "$log"
require user.driver VIRTIO_NET_DRIVER_OK "$log"
require_after user.state 'net status' '^  state: ready$' "$log"
require_after user.link 'net status' '^  link: up$' "$log"
require_after user.mac 'net status' '^  mac: 52:54:00:12:34:56$' "$log"
require_after user.mode 'net status' '^  mode: dhcp$' "$log"
require_after user.address 'net status' '^  address: 10\.0\.2\.[0-9]*/24$' "$log"
require_after user.gateway 'net status' '^  gateway: 10\.0\.2\.2$' "$log"
require_after user.lease 'net status' '^  lease: [0-9]* s$' "$log"
require_after user.mtu 'net status' '^  mtu: 1500$' "$log"
forbid_after user.settings 'net status' '^  settings:' "$log"
require_after user.lo 'net status' '^  address: 127\.0\.0\.1/8$' "$log"
require_after user.dns-server 'resolve status' '^servers: 10\.0\.2\.3 (dhcp)$' "$log"
require_after user.localhost 'ping -c 1 localhost' '^40 bytes from 127\.0\.0\.1: seq=1 ' "$log"
for command in 'ping -c 4 -W 1000 10.0.2.2' 'ping -c 4 -W 1000 10.0.2.2 ' 'ping -c 4 -W 1000  10.0.2.2' 'ping -c 4 -W 1000   10.0.2.2'; do
	for sequence in 1 2 3 4; do
		require_after "user.reply.$sequence" "$command" "^40 bytes from 10\\.0\\.2\\.2: seq=$sequence time=[0-9]*\\.[0-9][0-9][0-9] ms$" "$log"
	done
	require_after user.summary "$command" '^4 sent, 4 received, 0% lost$' "$log"
	forbid_after user.exit "$command" 'exit status' "$log"
done
require user.fault NET_DRIVER_FAULT_INJECTED "$log"
require user.unbound NETD_DRIVER_UNBOUND "$log"
[ "$(grep -c '^VIRTIO_NET_DRIVER_OK$' "$log")" -ge 3 ] || fail user.replaced 'three VIRTIO_NET_DRIVER_OK lines, one per driver' "$log"
checks=$((checks + 1))
require_after user.file 'cat /home/user/net.txt' '^kept$' "$log"
require user.storage VIRTIO_BLK_DRIVER_OK "$log"
# Exact per-process counts: a round of pings changes nothing once the first
# round has warmed the service up, and a second driver replacement leaves
# PID 1, the service and the new driver where the first one did.
compare() { # name first second process
	first=$(held "$log" "$2" "$4")
	second=$(held "$log" "$3" "$4")
	[ -n "$first" ] && [ "$first" = "$second" ] || fail "$1" "$4 counts after '$2' equal to those after '$3'" "$log"
	checks=$((checks + 1))
}
compare user.netd-pings 'quota' 'quota ' netd
compare user.driver-pings 'quota' 'quota ' virtio-net
compare user.init-pings 'quota' 'quota ' init
compare user.netd-restart 'quota  ' 'quota   ' netd
compare user.driver-restart 'quota  ' 'quota   ' virtio-net
compare user.init-restart 'quota  ' 'quota   ' init
printf 'PASS user networking (%s)\n' "$log"
fi

# --- 2. Two guests and the test peer on a private segment ----------------

if wants stream; then

# netpeer joins both guests as a hub and is the segment's DHCP and DNS server
# at 192.0.2.53. Its query log is how the test sees what reached DNS.
peer=$work/netpeer
socket=$work/hub.sock
control=$work/netpeer.control
queries=$logs/network-netpeer.log
rm -f "$queries"
cat >"$work/zone" <<'ZONE'
left.test 60 A 192.0.2.10
right.test 60 A 192.0.2.11
alias.test 60 CNAME right.test
gone.test NXDOMAIN
fallback.test 60 A 192.0.2.12
slow.test 60 SLOW 192.0.2.13
tcp-fallback.test 60 TCP 192.0.2.14
tcp-fallback-reset.test TCPRESET
ZONE
go build -o "$peer" ./tools/netpeer
"$peer" -socket "$socket" -control "$control" -zone "$work/zone" -log "$queries" \
	-lease 52:54:00:00:02:10=192.0.2.10 -lease-time 40 >"$work/netpeer.out" 2>&1 &
pids="$pids $!"
wait_for 'netpeer: listening' 10 "$work/netpeer.out"
peer_command() { printf '%s\n' "$1" >"$control"; }
queried() { grep -c "^query $1 1 " "$queries" || true; }
# listening waits for the left guest's next udp listen to have bound its
# ports, so the peer's datagrams cannot arrive first.
listens=0
listening() {
	listens=$((listens + 1))
	elapsed=0
	while [ "$(grep -c '^UDP_LISTENING' "$left" || true)" -lt "$listens" ]; do
		[ "$elapsed" -lt 20 ] || fail udp.listen "udp listen number $listens bound" "$left"
		sleep 1
		elapsed=$((elapsed + 1))
	done
}
tcp_listens=0
tcp_listening() {
	tcp_listens=$((tcp_listens + 1))
	elapsed=0
	while [ "$(grep -c '^TCP_LISTENING' "$left" || true)" -lt "$tcp_listens" ]; do
		[ "$elapsed" -lt 20 ] || fail tcp.listen "tcp listen number $tcp_listens bound" "$left"
		sleep 1
		elapsed=$((elapsed + 1))
	done
}

disk left
disk right 192.0.2.11 'system.dns.server1 text 192.0.2.53'
left=$logs/network-left.log
right=$logs/network-right.log
start left 3 -netdev "stream,id=net0,server=off,addr.type=unix,addr.path=$socket" -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:00:02:10,addr=0x4
start right 4 -netdev "stream,id=net0,server=off,addr.type=unix,addr.path=$socket" -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:00:02:11,addr=0x4
wait_for 'goos> ' 90 "$left"
wait_for 'goos> ' 90 "$right"
wait_for 'NETD_READY 192\.0\.2\.10' 30 "$left"
wait_for 'NETD_READY 192\.0\.2\.11' 30 "$right"
sleep 2
type_line 3 'ping -c 4 192.0.2.11'
type_line 4 'ping -c 4 192.0.2.10'
sleep 8
type_line 3 'resolve status'
type_line 4 'resolve status'
type_line 3 'resolve right.test'
type_line 3 'resolve RIGHT.test.'
first_count=$(queried right.test)
type_line 3 'resolve flush'
type_line 3 'resolve right.test '
second_count=$(queried right.test)
type_line 3 'resolve alias.test'
type_line 3 'resolve gone.test'
type_line 3 'resolve localhost'
type_line 3 'ping -c 2 right.test'
sleep 3
type_line 4 'resolve left.test'
type_line 4 'hosts set 192.0.2.10 leftie'
type_line 4 'resolve leftie'
type_line 4 'ping -c 1 leftie'
sleep 2
type_line 3 'config set system.net.ipv4.default-ttl 18446744073709551616'
type_line 3 'config set system.net.ipv4.default-ttl 128'
type_line 3 'config set system.net.arp.resolve-attempts 5'
type_line 3 'net reload'
type_line 3 'config set system.net.dhcp.initial-retry-ms 30000'
type_line 3 'config set system.net.dhcp.max-retry-ms 10000'
type_line 3 'net reload '
type_line 3 'ping -c 2 192.0.2.11'
sleep 3
type_line 3 'net status'
type_line 3 'resolve right.test  '
# Generic UDP on the left guest against the peer's echo service on port 7
# and datagrams the peer sends on command.
# The first datagram comes a second after the listener's receive went out,
# so netd holds the RecvFrom and completes it when the datagram arrives.
type_line 3 'udp listen -c 1 -W 8000 9000'
listening
sleep 1
peer_command 'udp-send 192.0.2.10 9000 4000 hello guest'
sleep 2
type_line 3 'udp send -r 192.0.2.53 7 echo me'
type_line 3 'udp listen -c 2 -W 8000 9001 9002'
listening
peer_command 'udp-send 192.0.2.10 9002 4002 two'
peer_command 'udp-send 192.0.2.10 9001 4001 one'
sleep 2
type_line 3 'udp send -r 192.0.2.53 7 decoy check'
type_line 3 'udp listen -c 3 -W 8000 9003'
listening
peer_command 'udp-burst 192.0.2.10 9003 4003 3'
sleep 2
type_line 3 'udp self 127.0.0.1 9100 loop'
type_line 3 'udp self 127.0.0.2 9101 other'
type_line 3 'udp self 192.0.2.10 9102 own'
type_line 3 'udp self -s 0 127.0.0.1 9103'
type_line 3 'udp listen -c 1 -W 8000 9004'
listening
peer_command 'udp-fill 192.0.2.10 9004 4004 0'
sleep 2
type_line 3 'udp send -s 1472 -r 192.0.2.53 7'
type_line 3 'udp send -s 1473 192.0.2.53 7'
type_line 3 'udp listen -c 1 -W 8000 9005'
listening
peer_command 'udp-bad 192.0.2.10 9005 4005 bad'
peer_command 'udp-send 192.0.2.10 9005 4005 good'
sleep 2
peer_command 'udp-send 192.0.2.10 9999 4006 nobody'
sleep 1
type_line 3 'net status '
# Generic TCP: remote echo with peer MSS segmentation, deferred operations,
# retransmission after a dropped data segment, reset/refusal, local delivery,
# and a guest listener whose Accept waits before the other guest connects.
type_line 3 'tcp connect -W 8000 192.0.2.53 7 echo tcp'
wait_for '^8 bytes: echo tcp$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 echo tcp' 12 "$left"
type_line 3 'tcp connect -s 4096 -W 8000 192.0.2.53 7'
# Guest reads may join segments that arrive together, so only the total is
# fixed here; the segment sizes are checked at the peer below.
wait_for '^tcp-echo .* 496$' 12 "$queries"
wait_command 'tcp connect -s 4096 -W 8000 192.0.2.53 7' 12 "$left"
echoed=$(window "$left" 'tcp connect -s 4096 -W 8000 192.0.2.53 7' | sed -n 's/^\([0-9]*\) bytes:.*/\1/p' | awk '{sum += $1} END {print sum + 0}')
[ "$echoed" = 4096 ] || fail tcp.stream "4096 echoed bytes, got $echoed" "$left"
checks=$((checks + 1))
peer_command 'tcp-delay-synack 1200'
type_line 3 'tcp connect -W 8000 192.0.2.53 7 delayed'
wait_for '^7 bytes: delayed$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 delayed' 12 "$left"
peer_command 'tcp-drop-next data'
type_line 3 'tcp connect -W 8000 192.0.2.53 7 retry'
wait_for '^5 bytes: retry$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 retry' 12 "$left"
type_line 3 'tcp connect -W 8000 192.0.2.53 7 reorder'
wait_for '^7 bytes: reorder$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 reorder' 12 "$left"
type_line 3 'tcp connect -W 8000 192.0.2.53 7 duplicate'
wait_for '^9 bytes: duplicate$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 duplicate' 12 "$left"
peer_command 'tcp-zero-next 2200'
type_line 3 'tcp connect -W 8000 192.0.2.53 7 zero'
wait_for '^4 bytes: zero$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 zero' 12 "$left"
type_line 3 'tcp connect -W 8000 192.0.2.53 7 reset'
wait_for '^tcp: reset$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 7 reset' 12 "$left"
type_line 3 'tcp connect -W 8000 192.0.2.53 9 closed'
wait_for '^tcp: connect: connection refused$' 12 "$left"
wait_command 'tcp connect -W 8000 192.0.2.53 9 closed' 12 "$left"
type_line 3 'tcp self 127.0.0.1 9300 loop'
wait_for '^4 bytes: loop$' 12 "$left"
wait_command 'tcp self 127.0.0.1 9300 loop' 12 "$left"
type_line 3 'tcp self 127.0.0.2 9301 other'
wait_for '^5 bytes: other$' 12 "$left"
wait_command 'tcp self 127.0.0.2 9301 other' 12 "$left"
type_line 3 'tcp self 192.0.2.10 9302 own'
wait_for '^3 bytes: own$' 12 "$left"
wait_command 'tcp self 192.0.2.10 9302 own' 12 "$left"
type_line 3 'tcp listen -W 8000 -c 1 -b 192.0.2.10 9303'
tcp_listening
type_line 4 'tcp connect -W 8000 192.0.2.10 9303 guest stream'
wait_for '^12 bytes: guest stream$' 12 "$right"
sleep 4
peer_command 'edns off'
type_line 3 'resolve fallback.test'
peer_command 'edns on'
# The peer holds this answer for a second, inside the resolver's first wait:
# the query's RecvFrom waits in netd and resolved takes the answer from it.
type_line 3 'resolve slow.test'
sleep 1
# DNS over TCP: the peer answers these names with TC=1 over UDP. The full
# answer over TCP is larger than one GTCP1 slot, and the second lookup comes
# from the cache. The reset name's TCP answer is cut short by a reset.
type_line 3 'net status   '
type_line 3 'resolve tcp-fallback.test'
wait_command 'resolve tcp-fallback.test' 12 "$left"
type_line 3 'resolve tcp-fallback.test '
type_line 3 'resolve tcp-fallback-reset.test'
wait_command 'resolve tcp-fallback-reset.test' 12 "$left"
sleep 2
type_line 3 'resolve status '
type_line 3 'net status    '
# UDP limits reload with the rest of system.net.*: the DHCP values refused
# above are put back first, so the candidate fails or applies on UDP alone.
type_line 3 'config set system.net.dhcp.max-retry-ms 64000'
type_line 3 'config set system.net.udp.max-payload 1300'
type_line 3 'net reload  '
type_line 3 'udp self -s 1301 127.0.0.1 9200'
type_line 3 'udp self -s 1300 127.0.0.1 9201'
type_line 3 'config set system.net.udp.ephemeral-port-min 60000'
type_line 3 'config set system.net.udp.ephemeral-port-max 50000'
type_line 3 'net reload   '
type_line 3 'config set system.net.udp.ephemeral-port-min 49152'
type_line 3 'config set system.net.udp.ephemeral-port-max 65535'
type_line 3 'config set system.net.tcp.rto-min-ms 2000'
type_line 3 'net reload    '
type_line 3 'config set system.net.tcp.rto-max-ms 1000'
type_line 3 'net reload     '
type_line 3 'config set system.net.tcp.rto-min-ms 1000'
type_line 3 'config set system.net.tcp.rto-max-ms 60000'
type_line 3 'net status  '
# Address conflicts on the static guest: one claim is defended, a second
# inside the defend interval makes it give the address up, and a probe the
# peer answers keeps a new address from going live. lo carries on.
peer_command 'claim 192.0.2.11'
sleep 2
type_line 4 'net status'
peer_command 'claim 192.0.2.11'
sleep 2
type_line 4 'net status '
peer_command 'defend-probe 192.0.2.12'
type_line 4 'config set system.net.ipv4.address 192.0.2.12'
type_line 4 'net reload'
sleep 8
type_line 4 'net status  '
type_line 4 'ping -c 1 127.0.0.1'
peer_command quit
stop 3 "$left"
stop 4 "$right"
for sequence in 1 2 3 4; do
	require_after "left.reply.$sequence" 'ping -c 4 192.0.2.11' "^40 bytes from 192\\.0\\.2\\.11: seq=$sequence " "$left"
	require_after "right.reply.$sequence" 'ping -c 4 192.0.2.10' "^40 bytes from 192\\.0\\.2\\.10: seq=$sequence " "$right"
done
require_after left.summary 'ping -c 4 192.0.2.11' '^4 sent, 4 received, 0% lost$' "$left"
require_after right.summary 'ping -c 4 192.0.2.10' '^4 sent, 4 received, 0% lost$' "$right"
require_after left.dhcp-dns 'resolve status' '^servers: 192\.0\.2\.53 (dhcp)$' "$left"
require_after right.config-dns 'resolve status' '^servers: 192\.0\.2\.53 (config)$' "$right"
require_after left.resolve 'resolve right.test' '^right\.test: 192\.0\.2\.11 (dns, ttl 60 s)$' "$left"
require_after left.cached 'resolve RIGHT.test.' '^right\.test: 192\.0\.2\.11 (cache, ttl ' "$left"
[ "$first_count" = 1 ] || fail left.cache-hit "one upstream query for right.test before the flush, saw $first_count" "$queries"
[ "$second_count" = 2 ] || fail left.flush "a second upstream query after resolve flush, saw $second_count" "$queries"
checks=$((checks + 2))
require_after left.flushed 'resolve flush' '^cache flushed$' "$left"
require_after left.cname 'resolve alias.test' '^alias\.test: 192\.0\.2\.11 (dns' "$left"
require_after left.nxdomain 'resolve gone.test' 'name not found' "$left"
require_after left.localhost 'resolve localhost' '^localhost: 127\.0\.0\.1 (localhost)$' "$left"
grep -q '^query localhost' "$queries" && fail left.localhost-local 'no DNS query for localhost' "$queries"
checks=$((checks + 1))
require_after left.ping-name 'ping -c 2 right.test' '^PING right\.test (192\.0\.2\.11)$' "$left"
require_after left.ping-name-summary 'ping -c 2 right.test' '^2 sent, 2 received, 0% lost$' "$left"
require_after right.resolve 'resolve left.test' '^left\.test: 192\.0\.2\.10 (dns' "$right"
require_after right.hosts 'resolve leftie' '^leftie: 192\.0\.2\.10 (hosts)$' "$right"
require_after right.hosts-ping 'ping -c 1 leftie' '^1 sent, 1 received, 0% lost$' "$right"
require_after left.dhcp 'net status' '^  mode: dhcp$' "$left"
require_after left.address 'net status' '^  address: 192\.0\.2\.10/24$' "$left"
require_after left.overflow 'config set system.net.ipv4.default-ttl 18446744073709551616' '^config: value does not match the declared type$' "$left"
require_after left.reload 'net reload' '^network configuration applied$' "$left"
require_after left.refused 'net reload ' 'invalid setting system\.net\.dhcp\.max-retry-ms; previous configuration kept' "$left"
require_after left.after-reload 'ping -c 2 192.0.2.11' '^2 sent, 2 received, 0% lost$' "$left"
require_after left.mode-kept 'net status' '^  state: ready$' "$left"
require_after left.cache-after-reload 'resolve right.test  ' '^right\.test: 192\.0\.2\.11 (cache' "$left"
require_after left.udp-listen 'udp listen -c 1 -W 8000 9000' '^11 bytes from 192\.0\.2\.53:4000 to 192\.0\.2\.10: hello guest$' "$left"
require_after left.udp-sent 'udp send -r 192.0.2.53 7 echo me' '^7 bytes to 192\.0\.2\.53:7 from 192\.0\.2\.10:[0-9]*$' "$left"
require_after left.udp-echo 'udp send -r 192.0.2.53 7 echo me' '^7 bytes from 192\.0\.2\.53:7 to 192\.0\.2\.10: echo me$' "$left"
require_after left.udp-port-one 'udp listen -c 2 -W 8000 9001 9002' '^3 bytes from 192\.0\.2\.53:4001 to 192\.0\.2\.10: one$' "$left"
require_after left.udp-port-two 'udp listen -c 2 -W 8000 9001 9002' '^3 bytes from 192\.0\.2\.53:4002 to 192\.0\.2\.10: two$' "$left"
require_after left.udp-connected 'udp send -r 192.0.2.53 7 decoy check' '^11 bytes from 192\.0\.2\.53:7 to 192\.0\.2\.10: decoy check$' "$left"
forbid_after left.udp-decoy 'udp send -r 192.0.2.53 7 decoy check' 'from 192\.0\.2\.53:8 ' "$left"
order=$(window "$left" 'udp listen -c 3 -W 8000 9003' | sed -n 's/^7 bytes from 192\.0\.2\.53:4003 to 192\.0\.2\.10: \(burst [0-9]\)$/\1/p' | tr '\n' ,)
[ "$order" = 'burst 1,burst 2,burst 3,' ] || fail left.udp-order "bursts in order, saw '$order'" "$left"
checks=$((checks + 1))
require_after left.udp-loopback 'udp self 127.0.0.1 9100 loop' '^4 bytes from 127\.0\.0\.1:[0-9]* to 127\.0\.0\.1: loop$' "$left"
require_after left.udp-loopback-other 'udp self 127.0.0.2 9101 other' '^5 bytes from 127\.0\.0\.1:[0-9]* to 127\.0\.0\.2: other$' "$left"
require_after left.udp-own 'udp self 192.0.2.10 9102 own' '^3 bytes from 192\.0\.2\.10:[0-9]* to 192\.0\.2\.10: own$' "$left"
require_after left.udp-zero-local 'udp self -s 0 127.0.0.1 9103' '^0 bytes from 127\.0\.0\.1:[0-9]* to 127\.0\.0\.1$' "$left"
require_after left.udp-zero 'udp listen -c 1 -W 8000 9004' '^0 bytes from 192\.0\.2\.53:4004 to 192\.0\.2\.10$' "$left"
require_after left.udp-largest 'udp send -s 1472 -r 192.0.2.53 7' '^1472 bytes from 192\.0\.2\.53:7 to 192\.0\.2\.10$' "$left"
require_after left.udp-too-large 'udp send -s 1473 192.0.2.53 7' '^udp: send: invalid argument$' "$left"
require_after left.udp-checksum 'udp listen -c 1 -W 8000 9005' '^4 bytes from 192\.0\.2\.53:4005 to 192\.0\.2\.10: good$' "$left"
forbid_after left.udp-corrupt 'udp listen -c 1 -W 8000 9005' ': bad$' "$left"
require_after left.udp-counters 'net status ' '^  dropped: no-endpoint [1-9][0-9]* queue-full 0 checksum 1 too-long 0$' "$left"
require_after left.udp-limits 'net status ' '^  limits: ports 49152-65535 queue 8 payload 4096$' "$left"
require_after left.tcp-echo 'tcp connect -W 8000 192.0.2.53 7 echo tcp' '^8 bytes: echo tcp$' "$left"
require_after left.tcp-eof 'tcp connect -W 8000 192.0.2.53 7 echo tcp' '^eof$' "$left"
# The peer echoes the 4096 bytes as three 1200-byte segments and a 496-byte
# tail on one connection. Guest reads may join them, so count at the peer.
large=$(sed -n 's/^tcp-echo \(.*\) 496$/\1/p' "$queries" | head -n 1)
segments=$(grep -c "^tcp-echo $large 1200\$" "$queries" || true)
[ -n "$large" ] && [ "$segments" = 3 ] || fail left.tcp-large-full "3 echoed 1200-byte segments and a 496-byte tail, saw $segments to '$large'" "$queries"
checks=$((checks + 2))
require_after left.tcp-delay 'tcp connect -W 8000 192.0.2.53 7 delayed' '^7 bytes: delayed$' "$left"
require_after left.tcp-retransmit 'tcp connect -W 8000 192.0.2.53 7 retry' '^5 bytes: retry$' "$left"
require_after left.tcp-reorder 'tcp connect -W 8000 192.0.2.53 7 reorder' '^7 bytes: reorder$' "$left"
require_count_after left.tcp-duplicate 'tcp connect -W 8000 192.0.2.53 7 duplicate' '^9 bytes: duplicate$' 1 "$left"
require_after left.tcp-zero-window 'tcp connect -W 8000 192.0.2.53 7 zero' '^4 bytes: zero$' "$left"
require left.tcp-persist '^tcp-zero-probe ' "$queries"
require_after left.tcp-reset 'tcp connect -W 8000 192.0.2.53 7 reset' '^tcp: reset$' "$left"
require_after left.tcp-refused 'tcp connect -W 8000 192.0.2.53 9 closed' '^tcp: connect: connection refused$' "$left"
require_after left.tcp-loopback 'tcp self 127.0.0.1 9300 loop' '^4 bytes: loop$' "$left"
require_after left.tcp-loopback-other 'tcp self 127.0.0.2 9301 other' '^5 bytes: other$' "$left"
require_after left.tcp-own 'tcp self 192.0.2.10 9302 own' '^3 bytes: own$' "$left"
require_after left.tcp-listener 'tcp listen -W 8000 -c 1 -b 192.0.2.10 9303' '^12 bytes: guest stream$' "$left"
require_after right.tcp-client 'tcp connect -W 8000 192.0.2.10 9303 guest stream' '^12 bytes: guest stream$' "$right"
require_after left.tcp-status 'net status  ' '^tcp:$' "$left"
require_after left.tcp-limits 'net status  ' '^  limits: ports 49152-65535 backlog 8 buffers 16384/16384$' "$left"
require_after left.dns-fallback 'resolve fallback.test' '^fallback\.test: 192\.0\.2\.12 (dns, ttl 60 s)$' "$left"
[ "$(queried fallback.test)" = 2 ] || fail left.edns-fallback "two queries for fallback.test, one with EDNS and one without" "$queries"
checks=$((checks + 1))
require left.resolver-tcp '^RESOLVED_TCP_BOUND_OK$' "$left"
require_after left.dns-tcp 'resolve tcp-fallback.test' '^tcp-fallback\.test: 192\.0\.2\.14 (dns, ttl 60 s)$' "$left"
require_after left.dns-tcp-cached 'resolve tcp-fallback.test ' '^tcp-fallback\.test: 192\.0\.2\.14 (cache, ttl ' "$left"
for marker in 'query tcp-fallback\.test 1 ' 'dns-tcp-query tcp-fallback\.test 1 ' 'dns-tcp-answer tcp-fallback\.test '; do
	[ "$(grep -c "^$marker" "$queries" || true)" = 1 ] || fail left.dns-tcp-once "one '$marker' line, the cached lookup asking nothing" "$queries"
	checks=$((checks + 1))
done
[ "$(grep -c '^dns-tcp-accept 192\.0\.2\.10:' "$queries" || true)" = 2 ] || fail left.dns-tcp-connections 'one TCP connection per fallback' "$queries"
checks=$((checks + 1))
require_after left.dns-tcp-reset 'resolve tcp-fallback-reset.test' 'temporary failure' "$left"
require peer.dns-tcp-reset '^dns-tcp-reset tcp-fallback-reset\.test ' "$queries"
require_after left.dns-tcp-counters 'resolve status ' '^tcp: fallbacks 2 answers 1$' "$left"
require_after left.dns-tcp-errors 'resolve status ' '^tcp-errors: failures 1 timeouts 0$' "$left"
# The fallback connections are gone once the lookups end.
before=$(window "$left" 'net status   ' | grep '^  endpoints: [0-9]*/32 listeners')
after=$(window "$left" 'net status    ' | grep '^  endpoints: [0-9]*/32 listeners')
[ -n "$before" ] && [ "$before" = "$after" ] || fail left.dns-tcp-endpoints "TCP endpoints after the fallbacks as before, '$before' then '$after'" "$left"
checks=$((checks + 1))
require_after left.dns-deferred 'resolve slow.test' '^slow\.test: 192\.0\.2\.13 (dns, ttl 60 s)$' "$left"
require peer.slow '^slow slow\.test 1s$' "$queries"
[ "$(queried slow.test)" = 1 ] || fail left.dns-deferred-once "one query for slow.test, answered while its receive waited" "$queries"
checks=$((checks + 1))
# Every query from the left guest left from its own ephemeral port.
ports=$(sed -n 's/^query .* 192\.0\.2\.10:\([0-9]*\) id=.*/\1/p' "$queries")
total=$(printf '%s\n' "$ports" | grep -c . || true)
distinct=$(printf '%s\n' "$ports" | sort -u | grep -c . || true)
low=$(printf '%s\n' "$ports" | awk '$1 < 49152' | grep -c . || true)
[ "$total" -ge 4 ] && [ "$total" = "$distinct" ] && [ "$low" = 0 ] || fail left.dns-ports "a fresh ephemeral port for each of $total queries" "$queries"
checks=$((checks + 1))
[ "$(grep -c '^dhcp REQUEST 52:54:00:00:02:10 ' "$queries" || true)" -ge 2 ] || fail left.dhcp-renew 'a lease renewal REQUEST' "$queries"
checks=$((checks + 1))
require_after left.udp-reload 'net reload  ' '^network configuration applied$' "$left"
require_after left.udp-payload-limit 'udp self -s 1301 127.0.0.1 9200' '^udp: send: invalid argument$' "$left"
require_after left.udp-payload-kept 'udp self -s 1300 127.0.0.1 9201' '^1300 bytes from 127\.0\.0\.1:' "$left"
require_after left.udp-cross-field 'net reload   ' 'invalid setting system\.net\.udp\.ephemeral-port-max; previous configuration kept' "$left"
require_after left.tcp-reload 'net reload    ' '^network configuration applied$' "$left"
require_after left.tcp-cross-field 'net reload     ' 'invalid setting system\.net\.tcp\.rto-max-ms; previous configuration kept' "$left"
require_after left.udp-limits-kept 'net status  ' '^  limits: ports 49152-65535 queue 8 payload 1300$' "$left"
require_after right.defended 'net status' '^  conflicts: probe 0 active 1 defended 1 relinquished 0' "$right"
require_after right.defended-ready 'net status' '^  state: ready$' "$right"
require_after right.relinquished 'net status ' '^  conflicts: probe 0 active 2 defended 1 relinquished 1' "$right"
require_after right.failed 'net status ' '^  state: failed$' "$right"
require_after right.probe-conflict 'net status  ' '^  conflicts: probe 1 ' "$right"
require_after right.probe-failed 'net status  ' '^  address: none$' "$right"
require_after right.loopback 'ping -c 1 127.0.0.1' '^1 sent, 1 received, 0% lost$' "$right"
require 'peer.claims' '^defend 192\.0\.2\.12$' "$queries"
printf 'PASS private segment (%s, %s)\n' "$left" "$right"
fi

# --- 3. No network function -----------------------------------------------

if wants none; then

disk none '' 'system.net.ipv4.probe-count uint 0'
log=$logs/network-none.log
start none 3
wait_for 'goos> ' 90 "$log"
sleep 3
type_line 3 'cat /system/motd'
type_line 3 'net status'
type_line 3 'net reload'
type_line 3 'ping -c 1 -W 500 10.0.2.2'
sleep 3
type_line 3 'ping -c 1 127.0.0.1'
type_line 3 'ping -c 1 localhost'
type_line 3 'resolve localhost'
type_line 3 'hosts get localhost'
type_line 3 'resolve example.test'
type_line 3 'udp self 127.0.0.2 9100 lonely'
type_line 3 'tcp self 127.0.0.2 9300 lonely'
stop 3 "$log"
require none.absent 'Network: no legacy virtio-net device' "$log"
require none.normal BOOT_MODE_NORMAL "$log"
require_after none.storage 'cat /system/motd' '^Welcome to GoOS$' "$log"
require_after none.down 'net status' '^  state: down$' "$log"
require_after none.lo 'net status' '^lo:$' "$log"
require_after none.fallback 'net status' '^  settings: built-in defaults' "$log"
require_after none.refused 'net reload' 'invalid setting system\.net\.ipv4\.probe-count; previous configuration kept' "$log"
require_after none.ping 'ping -c 1 -W 500 10.0.2.2' '^seq=1: ' "$log"
require_after none.ping-summary 'ping -c 1 -W 500 10.0.2.2' '^1 sent, 0 received, 100% lost$' "$log"
require_after none.ping-exit 'ping -c 1 -W 500 10.0.2.2' '^exit status: 1$' "$log"
require_after none.loopback 'ping -c 1 127.0.0.1' '^40 bytes from 127\.0\.0\.1: seq=1 time=0\.000 ms$' "$log"
require_after none.localhost 'ping -c 1 localhost' '^1 sent, 1 received, 0% lost$' "$log"
require_after none.resolve 'resolve localhost' '^localhost: 127\.0\.0\.1 (localhost)$' "$log"
require_after none.hosts 'hosts get localhost' '^127\.0\.0\.1 localhost$' "$log"
require_after none.no-servers 'resolve example.test' 'no DNS servers' "$log"
require_after none.udp-loopback 'udp self 127.0.0.2 9100 lonely' '^6 bytes from 127\.0\.0\.1:[0-9]* to 127\.0\.0\.2: lonely$' "$log"
require_after none.tcp-loopback 'tcp self 127.0.0.2 9300 lonely' '^6 bytes: lonely$' "$log"
printf 'PASS no network (%s)\n' "$log"
fi

printf 'PASS network (%d checks)\n' "$checks"
