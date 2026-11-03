#!/bin/sh
# The Linux sockets gate (ADR 0210). One validation guest on a private
# stream backend runs static Linux programs against tools/netpeer, which is
# the segment's DHCP server, its DNS server at 192.0.2.53, an HTTP server
# with a large file, TCP and UDP echo on port 7, a port that drops SYNs, and
# a TCP client that fetches from servers inside the guest. Every byte the
# Linux programs move goes through their own socket calls: the gate never
# runs fetch, tcp, udp, or resolve on their behalf.
#
# The scenarios, each selectable through LINUX_NETWORK_SCENARIOS:
#
#   loopback  netprobe's loopback groups
#   dns       /etc, libc-style lookups through the stub at 127.0.0.1:53, a
#             truncated answer that resolved repeats over TCP, NXDOMAIN
#   client    numeric and named HTTP downloads with SHA-256, nonblocking
#             connect, refusal, reset, a silent port, a receive timeout
#   udp       echo with the peer: unconnected, nonblocking, connected,
#             truncation
#   inherit   a socket shared with a forked child and an execve'd image
#   server    netprobe serve fetched by the peer one at a time and at once,
#             an abrupt client reset, and a restart on the same port
#   apps      BusyBox httpd, nc and wget, and curl
#   faults    fault net-driver, fault netd, fault linuxsockd
#
# usage: linux_network.sh ISO OVMF BUSYBOX CURL
#
# BUSYBOX and CURL are the host paths of the static binaries the build put
# under /system/linux; their SHA-256 must match BUSYBOX_SHA256 and
# CURL_SHA256, which default to the pinned releases.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
busybox_host=${3:?BusyBox path required}
curl_host=${4:?curl path required}
memory=${MEMORY:-512M}
smp=${SMP:-1}
command_timeout=${COMMAND_TIMEOUT:-90}
scenarios=${LINUX_NETWORK_SCENARIOS:-loopback dns client udp inherit server apps faults}
busybox_pin=${BUSYBOX_SHA256:-9209cc9efa5840f06e0e61f78c1ca4b9a374eb6508ac0bd831cf53181a86f2db}
curl_pin=${CURL_SHA256:-5f8fa1c76781b051939f808bee042bdfb3998fff9dbee060a27d8c4613c8c692}

read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'linux_network.sh needs a VALIDATION=1 build' >&2; exit 2; }
tree=build/variants/$variant/system
work=build/linux-network-test
log=build/logs/linux-network.log
queries=build/logs/linux-network-netpeer.log
probe=/system/linux/netprobe
bb=/system/linux/$(basename "$busybox_host")
curl=/system/linux/$(basename "$curl_host")
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|LIFECYCLE_QUEUE_OVERFLOW\|_RECEIVE_FAILED\|_SEND_FAILED'

sha() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
[ "$(sha "$busybox_host")" = "$busybox_pin" ] || { echo "$busybox_host is not the pinned BusyBox ($busybox_pin)" >&2; exit 2; }
[ "$(sha "$curl_host")" = "$curl_pin" ] || { echo "$curl_host is not the pinned curl ($curl_pin)" >&2; exit 2; }
[ -f "$tree$bb" ] && [ -f "$tree$curl" ] && [ -f "$tree$probe" ] || { echo "$tree lacks netprobe, BusyBox or curl" >&2; exit 2; }

rm -rf "$work"
mkdir -p "$work" build/logs
rm -f "$log" "$queries"
pids=
checks=0
marks=0

cleanup() {
	exec 3>&- 2>/dev/null || true
	for pid in $pids; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
	rm -rf "$work/guest.img" "$work/tree" "$work/big.bin" 2>/dev/null || true
}
trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

fail() {
	printf 'FAIL %s\n  log: %s\n  peer: %s\n' "$1" "$log" "$queries" >&2
	[ ! -s "$work/output" ] || sed -n '1,40p' "$work/output" >&2
	tail -n 20 "$queries" >&2 2>/dev/null || true
	exit 1
}

serial() { tr -d '\r' <"$log"; }

# send types one line at the guest's prompt and returns at once.
send() { printf '%s\n' "$1" >&3; }

# finish waits for the marker echoed after the last command and keeps
# what was printed since $start in $work/output.
finish() {
	elapsed=0
	until serial | grep -q "^gate-mark-$marks\$"; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker after: $command"
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "no end after: $command"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	serial | tail -n "+$((start + 1))" | sed "/^gate-mark-$marks\$/q" >"$work/output"
}

# begin notes where a command's output starts. The prompt the command is
# typed at is already in the log, without its newline, and counts in
# prompts; the next prompt makes the count grow by one.
begin() {
	command=$1
	start=$(serial | wc -l)
	prompts=$(serial | grep -c '^goos> ' || true)
	marks=$((marks + 1))
}

# run types a command followed by a marker and waits for the marker. A
# marker line, not the prompt, ends the output, so a background job that
# makes the terminal redraw its prompt cannot end a window early.
run() {
	begin "$1"
	send "$1; echo gate-mark-$marks"
	finish
}

# interactive types a command without a marker, so another step can act
# while it runs; done_interactive ends it, with Ctrl-C when asked.
interactive() {
	begin "$1"
	send "$1"
}

# done_interactive waits for the program to end and the prompt to come
# back, so the marker goes to the shell and not to the program's input.
done_interactive() {
	[ "${1:-}" != interrupt ] || { sleep 1; printf '\003' >&3; }
	elapsed=0
	until [ "$(serial | grep -c '^goos> ' || true)" -gt "$prompts" ]; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker after: $command"
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "no prompt after: $command"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	send "echo gate-mark-$marks"
	finish
}

# background starts a job with a trailing '&', which takes no marker of
# its own.
background() {
	begin "$1"
	send "$1"
	sleep 1
	send "echo gate-mark-$marks"
	finish
	expect '\[[0-9]*\] started$' "background job: $1"
}

# await_guest waits for a line in the guest's log since the current command.
await_guest() {
	elapsed=0
	until serial | tail -n "+$((start + 1))" | grep -q -- "$1"; do
		! grep -q "$fatal" "$log" 2>/dev/null || fail "fatal marker while waiting for $1"
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "guest never printed $1 after: $command"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
}

expect() {
	grep -q -- "$1" "$work/output" || fail "$2: no line matches $1"
	checks=$((checks + 1))
}

refuse() {
	! grep -q -- "$1" "$work/output" || fail "$2: a line matches $1"
	checks=$((checks + 1))
}

peer_command() { printf '%s\n' "$1" >"$work/netpeer.control"; }

# peer_wait waits for a peer log line and prints it.
peer_wait() {
	elapsed=0
	until grep -q -- "$1" "$queries" 2>/dev/null; do
		[ "$elapsed" -lt $((command_timeout * 10)) ] || fail "the peer never logged $1"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	grep -- "$1" "$queries" | tail -n 1
}

peer_expect() {
	grep -q -- "$1" "$queries" || fail "$2: the peer logged no $1"
	checks=$((checks + 1))
}

# results matches the line a client command ends with.
results='\(http-got\|tcp-got\|client-abort\|client-refused\|client-reset\|client-timeout\)'

# peer_fetch has the peer fetch PATH from a guest server; a refused attempt
# is tried again while the server starts. It prints the result line.
peer_fetch() { # tag port path [command]
	verb=${4:-http-get}
	attempt=1
	while :; do
		tag=$1-$attempt
		peer_command "$verb $tag 192.0.2.10 $2 $3"
		line=$(peer_wait "^$results $tag ")
		case $line in
		client-refused*) [ "$attempt" -lt 10 ] || fail "the guest refused $1 ten times"; sleep 1; attempt=$((attempt + 1)) ;;
		*) printf '%s\n' "$line"; return 0 ;;
		esac
	done
}

# await_file runs cat on a guest file until a line matches pattern.
await_file() { # file pattern seconds
	tries=0
	while :; do
		run "cat $1"
		grep -q -- "$2" "$work/output" && return 0
		[ "$tries" -lt "$3" ] || fail "$1 never held $2"
		sleep 1
		tries=$((tries + 1))
	done
}

wants() { case " $scenarios " in *" $1 "*) return 0 ;; esac; return 1; }

# --- host side --------------------------------------------------------

head -c 300000 /dev/urandom >"$work/big.bin"
big_sum=$(sha "$work/big.bin")
abi_sum=$(sha userspace/linux/prebuilt/abi-probe)
abi_size=$(wc -c <userspace/linux/prebuilt/abi-probe | tr -d ' ')
cat >"$work/zone" <<'ZONE'
plain.test 60 A 192.0.2.53
example.test 60 TCP 192.0.2.53
gone.test NXDOMAIN
ZONE
go build -o "$work/netpeer" ./tools/netpeer
"$work/netpeer" -socket "$work/hub.sock" -control "$work/netpeer.control" -zone "$work/zone" -log "$queries" \
	-lease 52:54:00:00:02:10=192.0.2.10 -http-file "big=$work/big.bin" >"$work/netpeer.out" 2>&1 &
pids="$pids $!"
elapsed=0
until grep -q 'netpeer: listening' "$work/netpeer.out" 2>/dev/null; do
	[ "$elapsed" -lt 100 ] || { cat "$work/netpeer.out" >&2; echo 'FAIL netpeer did not start' >&2; exit 1; }
	sleep 0.1
	elapsed=$((elapsed + 1))
done

cp -R "$tree" "$work/tree"
go run ./tools/mkfat32 -new -tree "$work/tree" "$work/guest.img"
rm -rf "$work/tree"
mkfifo "$work/in"
qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/guest.img" -device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-netdev "stream,id=net0,server=off,addr.type=unix,addr.path=$work/hub.sock" \
	-device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:00:02:10,addr=0x4 \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown <"$work/in" >"$log" 2>&1 &
pids="$pids $!"
exec 3>"$work/in"

command=boot
start=0
elapsed=0
until serial | grep -q 'NETD_READY 192\.0\.2\.10' && serial | grep -q '^goos> '; do
	! grep -q "$fatal" "$log" 2>/dev/null || fail 'fatal marker during boot'
	[ "$elapsed" -lt 1800 ] || fail 'no prompt and DHCP lease within 180 s'
	sleep 0.1
	elapsed=$((elapsed + 1))
done
sleep 2

# --- loopback ---------------------------------------------------------

if wants loopback; then
	run "$probe"
	for group in create sockaddr bind listen connect stream shutdown sigpipe poll select udp dup exec eintr options file; do
		expect "^$group ok\$" "loopback $group"
	done
	expect '^netprobe ok$' 'loopback groups'
fi

# --- dns --------------------------------------------------------------

if wants dns; then
	run "$bb cat /etc/resolv.conf"
	expect '^nameserver 127\.0\.0\.1$' '/etc/resolv.conf'
	run "$bb cat /etc/nsswitch.conf"
	expect '^hosts: *files dns$' '/etc/nsswitch.conf'
	run "$bb cat /etc/hosts"
	expect '^127\.0\.0\.1[[:space:]]*localhost' '/etc/hosts'
	run "$bb touch /etc/resolv.conf"
	expect 'Read-only file system\|Permission denied' '/etc is read-only'
	run "$probe resolve localhost"
	expect '^resolve localhost 127\.0\.0\.1 via hosts$' 'localhost from /etc/hosts'
	run "$probe resolve plain.test"
	expect '^resolve plain\.test 192\.0\.2\.53 via udp$' 'A query to the stub over UDP'
	peer_expect '^query plain\.test 1 192\.0\.2\.10:' 'resolved asked the peer for plain.test'
	run "$probe resolve example.test"
	expect '^resolve example\.test 192\.0\.2\.53 via \(udp\|tcp\)$' 'a truncated upstream answer'
	peer_expect '^query example\.test 1 192\.0\.2\.10:' 'resolved asked over UDP'
	peer_expect '^dns-tcp-query example\.test 1 192\.0\.2\.10:' 'resolved repeated the query over TCP'
	run "$probe resolve gone.test"
	expect '^resolve gone\.test failed: name not found$' 'NXDOMAIN'
fi

# --- client -----------------------------------------------------------

if wants client; then
	run "$probe fetch 192.0.2.53 8080 /big"
	expect "^fetch status=200 bytes=300000 sha256=$big_sum\$" 'numeric download'
	run "$probe fetch example.test 8080 /big"
	expect "^fetch status=200 bytes=300000 sha256=$big_sum\$" 'download by name'
	run "$probe fetch -n 192.0.2.53 80 /big"
	expect "^fetch status=200 bytes=300000 sha256=$big_sum\$" 'download after a nonblocking connect'
	run "$probe connect 192.0.2.53 9 5000"
	expect '^connect result=ECONNREFUSED pollerr=1 cleared=1 blocking=ECONNREFUSED$' 'refusal'
	run "$probe connect 192.0.2.53 81 3000"
	expect '^connect result=timeout waited=ok$' 'a silent port'
	peer_expect '^tcp-silent 192\.0\.2\.10:' 'the peer dropped the SYN'
	run "$probe reset 192.0.2.53 7"
	expect '^reset read=ECONNRESET write=E' 'reset'
	run "$probe silent 192.0.2.53 7 1500"
	expect '^silent read=EAGAIN elapsed=ok$' 'receive timeout from a silent peer'
fi

# --- udp and inherit --------------------------------------------------

if wants udp; then
	run "$probe udp-echo 192.0.2.53 7"
	expect '^udp-echo ok$' 'UDP echo'
	peer_expect '^udp-echo 192\.0\.2\.10:[0-9]* 99$' 'the peer echoed the long datagram'
fi

if wants inherit; then
	run "$probe inherit 192.0.2.53 7"
	expect '^inherit ok$' 'socket across fork and execve'
fi

# --- server -----------------------------------------------------------

check_fetch() { # line name
	case $1 in
	"http-got "*" status=200 body=$abi_size sha256=$abi_sum") checks=$((checks + 1)) ;;
	*) fail "$2: $1" ;;
	esac
}

if wants server; then
	interactive "$probe serve 8081 /system/linux/abi-probe 6"
	await_guest '^serve listening 8081$'
	check_fetch "$(peer_fetch s1 8081 /abi-probe)" 'first sequential fetch'
	check_fetch "$(peer_fetch s2 8081 /abi-probe)" 'second sequential fetch'
	peer_command 'http-get c1 192.0.2.10 8081 /abi-probe'
	peer_command 'http-get c2 192.0.2.10 8081 /abi-probe'
	peer_command 'http-get c3 192.0.2.10 8081 /abi-probe'
	for tag in c1 c2 c3; do
		check_fetch "$(peer_wait "^$results $tag ")" "concurrent fetch $tag"
	done
	case $(peer_fetch a1 8081 /abi-probe http-abort) in
	client-abort*) checks=$((checks + 1)) ;;
	*) fail 'abrupt client close' ;;
	esac
	await_guest '^serve done '
	done_interactive
	expect '^serve done 5 of 6$' 'five answers and one abort'
	expect '^serve [0-9] aborted writing errno=\(EPIPE\|ECONNRESET\)' 'the aborted write fails without SIGPIPE'
	refuse 'killed\|exit status' 'the server survived the abort'
	# The server closed first, so its connections wait in TIME_WAIT; the
	# restarted server binds the port again with SO_REUSEADDR.
	interactive "$probe serve 8081 /system/linux/abi-probe 1"
	await_guest '^serve listening 8081$'
	check_fetch "$(peer_fetch r1 8081 /abi-probe)" 'fetch after the restart'
	await_guest '^serve done '
	done_interactive
	expect '^serve done 1 of 1$' 'restarted server'
fi

# --- applications -----------------------------------------------------

if wants apps; then
	interactive "$bb httpd -f -p 8080 -h /system/linux"
	check_fetch "$(peer_fetch h1 8080 /abi-probe)" 'BusyBox httpd first fetch'
	check_fetch "$(peer_fetch h2 8080 /abi-probe)" 'BusyBox httpd second fetch'
	peer_command 'http-get h3 192.0.2.10 8080 /abi-probe'
	peer_command 'http-get h4 192.0.2.10 8080 /abi-probe'
	check_fetch "$(peer_wait "^$results h3 ")" 'BusyBox httpd concurrent fetch'
	check_fetch "$(peer_wait "^$results h4 ")" 'BusyBox httpd concurrent fetch'
	# Ctrl-C ends httpd while it waits in accept.
	done_interactive interrupt

	run "$bb sh -c 'echo nc-tcp | $bb nc -w 3 192.0.2.53 7'"
	expect '^nc-tcp$' 'BusyBox nc TCP client'
	run "$bb sh -c 'echo nc-udp | $bb nc -u -w 3 192.0.2.53 7'"
	expect '^nc-udp$' 'BusyBox nc UDP client'
	interactive "$bb sh -c 'echo from-nc | $bb nc -l -p 9001'"
	sleep 2
	case $(peer_fetch n1 9001 to-nc tcp-chat) in
	'tcp-got n1-'*' bytes=8 text="from-nc\n"') checks=$((checks + 1)) ;;
	*) fail 'BusyBox nc listener reply' ;;
	esac
	done_interactive
	expect '^to-nc$' 'BusyBox nc listener received'

	run "$bb sh -c '$bb wget -q -O - http://example.test:8080/big | $bb sha256sum'"
	expect "^$big_sum  -\$" 'BusyBox wget by name'
	run "$bb sh -c '$bb wget -q -O - http://gone.test:8080/big; echo wget-exit=\$?'"
	expect 'bad address' 'BusyBox wget with an unknown name'
	expect '^wget-exit=1$' 'BusyBox wget status for an unknown name'
	run "$bb sh -c '$curl -sS -o /tmp/curl.out http://example.test:8080/big; echo curl-exit=\$?; $bb sha256sum /tmp/curl.out'"
	expect '^curl-exit=0$' 'curl exit status'
	expect "^$big_sum  /tmp/curl.out\$" 'curl download by name'
	run "$bb sh -c '$curl -sS -o /dev/null http://gone.test:8080/big; echo curl-exit=\$?'"
	expect '^curl-exit=6$' 'curl with an unknown name'
fi

# --- faults -----------------------------------------------------------

# fault_hold starts netprobe hold in the background, kills a service,
# waits for the service's restart marker, and waits for the probe's last
# line.
fault_hold() { # name mode seconds marker
	background "$probe hold 192.0.2.53 7 $2 $3 > /tmp/hold-$1.out &"
	await_file "/tmp/hold-$1.out" '^hold ready$' 30
	run "fault $1"
	refuse '^fault: ' "fault $1 is a known target"
	elapsed=0
	until [ "$(serial | grep -c "$4")" -gt "${5:-0}" ]; do
		[ "$elapsed" -lt 600 ] || fail "no $4 after fault $1"
		sleep 0.1
		elapsed=$((elapsed + 1))
	done
	await_file "/tmp/hold-$1.out" '^hold \(survived\|recovered\|failed\)' $(($3 + 40))
}

if wants faults; then
	before=$(serial | grep -c NET_DRIVER_RESTARTED_OK || true)
	fault_hold net-driver survive 25 NET_DRIVER_RESTARTED_OK "$before"
	expect '^hold survived$' 'a connection survives a driver restart'

	before=$(serial | grep -c NETD_RESTARTED_OK || true)
	fault_hold netd fail 30 NETD_RESTARTED_OK "$before"
	expect '^hold woke read=\(ENETRESET\|ECONNRESET\|ENETDOWN\|ECONNABORTED\|ETIMEDOUT\|EPIPE\) after=bounded$' 'netd death wakes a blocked read with a network error'
	refuse '^hold stale tcp=sent' 'an old TCP socket stays failed after netd restarts'
	refuse 'udp=echoed' 'an old UDP socket stays failed after netd restarts'
	refuse 'accept=accepted\|accept=EAGAIN' 'an old listener stays failed after netd restarts'
	expect '^hold recovered attempt=[0-9]*$' 'fresh sockets after netd restarts'

	before=$(serial | grep -c LINUXSOCKD_RESTARTED_OK || true)
	fault_hold linuxsockd fail 30 LINUXSOCKD_RESTARTED_OK "$before"
	expect '^hold woke read=ENETRESET after=bounded$' 'linuxsockd death wakes a blocked read with ENETRESET'
	expect '^hold stale tcp=ENETRESET udp=ENETRESET accept=ENETRESET$' 'old sockets refuse with ENETRESET'
	expect '^hold recovered attempt=[0-9]*$' 'fresh sockets in the same family after linuxsockd restarts'

	run "$probe fetch example.test 8080 /big"
	expect "^fetch status=200 bytes=300000 sha256=$big_sum\$" 'a new launch downloads after the faults'
fi

send poweroff
elapsed=0
until serial | grep -q '^Powering off\.\.\.$' || [ "$elapsed" -ge 300 ]; do
	sleep 0.1
	elapsed=$((elapsed + 1))
done
! grep -q "$fatal" "$log" || fail 'fatal marker in the log'
printf 'PASS linux-network (%s checks)\n  log: %s\n' "$checks" "$log"
