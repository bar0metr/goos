#!/bin/sh
# One validation guest checks native HTTPS against disposable host peers.
# Run from the repository root: tls.sh ISO OVMF_CODE SYSTEM_TREE.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
tree=${3:?validation system tree required}
[ "$#" = 3 ] || { echo 'usage: tls.sh ISO OVMF_CODE SYSTEM_TREE' >&2; exit 2; }
[ -f "$iso" ] && [ -f "$firmware" ] && [ -d "$tree" ] || {
	echo 'tls: ISO, firmware, or system tree is missing' >&2; exit 2;
}
read -r _hz _diag validation _opt _variant <build/config/build
[ "$validation" = 1 ] || { echo 'tls: VALIDATION=1 build required' >&2; exit 2; }
cmp -s "$tree/system/security/ca-certificates.pem" tools/tlsfixture/testdata/root-cert.pem || {
	echo 'tls: system tree must select the validation fixture CA, not production roots' >&2; exit 2;
}
[ -f "$tree/system/bin/fetch" ] || { echo 'tls: fetch image missing' >&2; exit 2; }
nc=$(command -v nc) || { echo 'tls: nc required for guestfwd' >&2; exit 2; }
work=$(go run ./tools/scratch goos-tls-test-)
log=$work/guest.log
report=$work/checks.tsv
printf 'check\tresult\tdetail\n' >"$report"
printf 'tls report: %s\n' "$work"
checks=0
pids=
guest_pid=

cleanup() {
	exec 3>&-
	for child in $pids; do kill "$child" 2>/dev/null || true; done
	remaining=0
	while [ "$remaining" -lt 3 ]; do
		alive=0
		for child in $pids; do kill -0 "$child" 2>/dev/null && alive=1 || true; done
		[ "$alive" = 1 ] || break
		sleep 1
		remaining=$((remaining + 1))
	done
	for child in $pids; do
		kill -KILL "$child" 2>/dev/null || true
		wait "$child" 2>/dev/null || true
	done
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

fail() { # name expected log
	printf '%s\tfail\t%s\n' "$1" "$2" >>"$report"
	printf 'FAIL %s: %s\n  report: %s\n  log: %s\n' "$1" "$2" "$work" "$3" >&2
	tail -n 60 "$3" >&2 || true
	exit 1
}
pass() {
	printf '%s\tpass\t%s\n' "$1" "$2" >>"$report"
	checks=$((checks + 1))
}
wait_for() { # pattern seconds file pid
	elapsed=0
	while [ "$elapsed" -lt "$2" ]; do
		tr -d '\r' <"$3" 2>/dev/null | grep -q -- "$1" && return 0
		kill -0 "$4" 2>/dev/null || break
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail timeout "$1 within ${2}s" "$3"
}
window() { # command
	awk -v command="$1" '{ sub(/\r$/, "") } seen && index($0, "goos> ") == 1 { exit } seen { print } !seen && ($0 == "goos> " command || $0 == command) { seen = 1 }' "$log"
}
run_command() {
	printf '%s\n' "$1" >&3
	completion='SHELL_EXEC_OK$'
	case "$1" in 'hosts set '*) completion='HOSTS_SET_OK$' ;; esac
	elapsed=0
	while [ "$elapsed" -lt 30 ]; do
		# HTTP bodies have no trailing LF; the shell marker can share a line.
		if window "$1" | grep -q "$completion"; then sleep 1; return 0; fi
		kill -0 "$guest_pid" 2>/dev/null || break
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail command-timeout "$1 completed within 30s" "$log"
}
require_after() { # name command regex
	window "$2" | grep -Eq -- "$3" || fail "$1" "$3 after $2" "$log"
	pass "$1" "$2"
}
forbid_after() { # name command regex
	if window "$2" | grep -Eq -- "$3"; then fail "$1" "no $3 after $2" "$log"; fi
	pass "$1" "$2"
}
positive() { # name command response regex
	run_command "$2"
	require_after "$1.response" "$2" "$3"
	forbid_after "$1.exit" "$2" 'exit status:'
}
port_for() {
	awk -v mode="$1" '$1 == mode {print $2}' "$work/peers.tsv"
}

# Build only the host peer; native binaries and ISO belong to the caller.
go build -o "$work/tlspeer" ./tools/tlspeer
mkdir "$work/go-fault"
go run ./tools/tlsfault "$(go env GOROOT)" "$work/go-fault"
GOROOT="$work/go-fault" go build -o "$work/tlspeer-badfinished" ./tools/tlspeer
printf 'mode\tport\tpid\n' >"$work/peers.tsv"
for mode in valid wronghost untrusted expired future invalidsignature unknowncriticalextension plainhttp malformed badverify tamper badfinished; do
	peer_log=$work/peer-$mode.log
	peer_binary=$work/tlspeer
	peer_mode=$mode
	if [ "$mode" = badfinished ]; then
		peer_binary=$work/tlspeer-badfinished
		peer_mode=valid
	fi
	"$peer_binary" -listen 127.0.0.1:0 -mode "$peer_mode" -fragment 137 >"$peer_log" 2>&1 &
	peer_pid=$!
	pids="$pids $peer_pid"
	wait_for "^tlspeer: listening 127\.0\.0\.1:[0-9][0-9]* mode=$peer_mode$" 10 "$peer_log" "$peer_pid"
	port=$(sed -n "s/^tlspeer: listening 127\.0\.0\.1:\([0-9][0-9]*\) mode=$peer_mode$/\1/p" "$peer_log")
	case "$port" in ''|*[!0-9]*) fail peer-port 'one decimal port in listening announcement' "$peer_log" ;; esac
	[ "$port" -ge 1 ] && [ "$port" -le 65535 ] || fail peer-port 'port in 1..65535' "$peer_log"
	printf '%s\t%s\t%s\n' "$mode" "$port" "$peer_pid" >>"$work/peers.tsv"
done
valid_port=$(port_for valid)
plain_port=$(port_for plainhttp)

# Copy into a new scratch directory; never overwrite the supplied tree or disk.
cp -R "$tree" "$work/tree"
go run ./tools/truststore -check security/ca-source.pem security/ca-certificates.pem
cp security/ca-certificates.pem "$work/tree/system/security/probe-production.pem"
go run ./tools/mkfat32 -new -tree "$work/tree" "$work/disk.img"
mkfifo "$work/console.in"
# The port-80 peer catches any forbidden HTTPS-to-HTTP downgrade request.
netdev="user,id=net0,guestfwd=tcp:10.0.2.100:443-cmd:$nc 127.0.0.1 $valid_port,guestfwd=tcp:10.0.2.100:80-cmd:$nc 127.0.0.1 $plain_port"
qemu-system-x86_64 -machine q35 -smp "${SMP:-1}" -accel tcg,thread=multi -m "${MEMORY:-512M}" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" -serial stdio -display none -no-reboot -cdrom "$iso" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/disk.img" -device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-netdev "$netdev" -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4 \
	<"$work/console.in" >"$log" 2>&1 &
guest_pid=$!
pids="$pids $guest_pid"
exec 3>"$work/console.in"
wait_for 'goos> ' 90 "$log" "$guest_pid"
wait_for 'NETD_READY 10\.0\.2\.' 30 "$log" "$guest_pid"
# Validation replaces the storage driver during startup.
sleep 4

positive roots.production 'trustprobe --production' '^TLSROOT_RESOURCE roots=121 '

run_command 'hosts set 10.0.2.100 tls.test'
forbid_after hosts.default443 'hosts set 10.0.2.100 tls.test' 'exit status:'
positive get.default443 'fetch https://tls.test/hello' '^helloSHELL_EXEC_OK$'
positive head.default443 'fetch -I https://tls.test/hello' '^HTTP/1\.1 200 OK'
forbid_after head.no-body 'fetch -I https://tls.test/hello' 'hello'
positive redirect.relative 'fetch -L https://tls.test/redirect' '^helloSHELL_EXEC_OK$'
command='fetch -L https://tls.test/downgrade'
run_command "$command"
require_after downgrade.exit "$command" '^exit status: 1$'
require_after downgrade.reason "$command" '[Dd]owngrade|HTTPS.*HTTP|https.*http'
positive file.write 'fetch -o /home/user/tls.txt https://tls.test/hello' 'SHELL_EXEC_OK$'
positive file.read 'cat /home/user/tls.txt' '^helloCAT_PROCESS_OK$'
positive numeric.ipv4 "fetch https://10.0.2.2:$valid_port/hello" '^helloSHELL_EXEC_OK$'

run_command 'hosts set 10.0.2.2 tls.test'
forbid_after hosts.explicit-ports 'hosts set 10.0.2.2 tls.test' 'exit status:'
for mode in wronghost untrusted expired future invalidsignature unknowncriticalextension plainhttp malformed badverify tamper badfinished; do
	port=$(port_for "$mode")
	command="fetch https://tls.test:$port/hello"
	run_command "$command"
	require_after "$mode.exit" "$command" '^exit status: 1$'
	case "$mode" in
		wronghost) reason='valid for.*not tls\.test|hostname' ;;
		untrusted) reason='unknown.*authority' ;;
		expired|future) reason='expired|not yet valid' ;;
		invalidsignature) reason='signature|verification failure|unknown.*authority' ;;
		badverify) reason='signature|verification failure' ;;
		badfinished) reason='Finished|finished' ;;
		tamper) reason='authentication|bad record MAC' ;;
		unknowncriticalextension) reason='critical extension' ;;
		plainhttp|malformed) reason='first record|TLS.*record|tls.*record' ;;
	esac
	require_after "$mode.reason" "$command" "$reason"
	forbid_after "$mode.no-body" "$command" '^hello'
done

printf 'poweroff\n' >&3
wait_for '^Powering off\.\.\.$' 30 "$log" "$guest_pid"
elapsed=0
while kill -0 "$guest_pid" 2>/dev/null && [ "$elapsed" -lt 30 ]; do
	sleep 1
	elapsed=$((elapsed + 1))
done
if kill -0 "$guest_pid" 2>/dev/null; then fail poweroff 'guest exited within 30s' "$log"; fi
wait "$guest_pid" || fail poweroff 'QEMU exited successfully' "$log"
pass poweroff 'guest exited within 30s'

for mode in wronghost untrusted expired future invalidsignature unknowncriticalextension plainhttp malformed badverify badfinished; do
	peer_log=$work/peer-$mode.log
	if grep -q '^tlspeer: request ' "$peer_log"; then fail "$mode.no-http" 'no HTTP request after rejected TLS' "$peer_log"; fi
	pass "$mode.no-http" 'zero peer HTTP requests, including downgrade port 80'
done
tampered_requests=$(grep -c '^tlspeer: request mode=tamper ' "$work/peer-tamper.log" || true)
[ "$tampered_requests" = 1 ] || fail tamper.request-count 'one authenticated request before record corruption' "$work/peer-tamper.log"
pass tamper.request-count 'one authenticated request, rejected corrupted response'
requests=$(grep -c '^tlspeer: request mode=valid ' "$work/peer-valid.log" || true)
[ "$requests" = 7 ] || fail valid.request-count "7 HTTP requests, saw $requests" "$work/peer-valid.log"
pass valid.request-count '7 authenticated requests including one refused downgrade response'
for spec in 'GET /hello 4' 'HEAD /hello 1' 'GET /redirect 1' 'GET /downgrade 1'; do
	set -- $spec
	count=$(grep -c "^tlspeer: request mode=valid method=$1 path=$2$" "$work/peer-valid.log" || true)
	[ "$count" = "$3" ] || fail valid.request-path "$spec, saw $count" "$work/peer-valid.log"
	pass "valid.$1.$2" "$count requests"
done
grep -E '^TLSROOT_RESOURCE |^TLS_HANDSHAKE_RESOURCE ' "$log" >"$work/resources.log" || true
printf 'tls: %s assertions passed\ntls report: %s\n' "$checks" "$work"
