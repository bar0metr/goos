#!/bin/sh
# Offline signed update, persistent reboot and corrupt-state recovery checks.
set -eu
iso=${1:?ISO required}
firmware=${2:?firmware required}
tree=${3:?validation system tree required}
[ "$#" = 3 ] || exit 2
read -r _hz _diag validation _opt _variant <build/config/build
[ "$validation" = 1 ] || { echo 'trust-update: VALIDATION=1 required' >&2; exit 2; }
work=$(mktemp -d /Volumes/Data/tmp/goos-trust-update.XXXXXX)
printf 'trust-update report: %s\n' "$work"
mkdir "$work/fixtures"
go run ./tools/trustfixture "$work/fixtures"
cmp -s "$work/fixtures/public.key" "$tree/system/security/trust-update-key.pub" || { echo 'trust-update: validation update key required' >&2; exit 2; }
cmp -s tools/tlsfixture/testdata/root-cert.pem "$tree/system/security/ca-certificates.pem" || { echo 'trust-update: validation bootstrap CA required' >&2; exit 2; }
[ "$(cat "$tree/system/security/trust-update-url")" = 'https://tls.test/update' ] || { echo 'trust-update: validation endpoint required' >&2; exit 2; }
go build -o "$work/trustpeer" ./tools/trustpeer
nc=$(command -v nc)
pids=
guest_pid=
checks=0
command_start=0
boot_number=0
report=$work/checks.tsv
printf 'check\tresult\n' >"$report"
cleanup() {
 exec 3>&- 2>/dev/null || true
 if [ -n "$guest_pid" ]; then kill "$guest_pid" 2>/dev/null || true; wait "$guest_pid" 2>/dev/null || true; fi
 for child in $pids; do kill "$child" 2>/dev/null || true; wait "$child" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
fail() { printf 'FAIL trust-update %s\nReport: %s\n' "$1" "$work" >&2; tail -60 "$log" >&2; exit 1; }
pass() { printf '%s\tpass\n' "$1" >>"$report"; checks=$((checks+1)); }
wait_for() {
 elapsed=0
 while [ "$elapsed" -lt "$2" ]; do
  grep -q -- "$1" "$3" 2>/dev/null && return 0
  kill -0 "$4" 2>/dev/null || break
  sleep 1; elapsed=$((elapsed+1))
 done
 fail "timeout: $1"
}
for mode in bootstrap newroot; do
 log=$work/peer-$mode.log
 "$work/trustpeer" -listen 127.0.0.1:0 -fixtures "$work/fixtures" -mode "$mode" >"$log" 2>&1 &
 peer_pid=$!; pids="$pids $peer_pid"
 wait_for "^trustpeer: listening 127\.0\.0\.1:[0-9][0-9]* mode=$mode$" 10 "$log" "$peer_pid"
 port=$(sed -n "s/^trustpeer: listening 127\.0\.0\.1:\([0-9][0-9]*\) mode=$mode$/\1/p" "$log")
 case "$port" in ''|*[!0-9]*) fail peer-port ;; esac
 if [ "$mode" = bootstrap ]; then bootstrap_port=$port; else newroot_port=$port; fi
 done
cp -R "$tree" "$work/tree"
mkdir -p "$work/tree/home/user/trust-fixtures"
# Only public package data enters the guest; all signing/TLS keys stay on host.
for mode in update rollback badsig; do cp -R "$work/fixtures/$mode" "$work/tree/home/user/trust-fixtures/$mode"; done
go run ./tools/mkfat32 -new -tree "$work/tree" "$work/disk.img"
boot() {
 boot_number=$((boot_number+1))
 log=$work/guest-$boot_number.log
 fifo=$work/console-$boot_number.in
 mkfifo "$fifo"
 qemu-system-x86_64 -machine q35 -smp "${SMP:-1}" -accel tcg,thread=multi -m "${MEMORY:-512M}" \
  -drive "if=pflash,format=raw,readonly=on,file=$firmware" -serial stdio -display none -no-reboot -cdrom "$iso" \
  -drive "if=none,id=goosdisk,format=raw,file=$work/disk.img" -device virtio-blk-pci,drive=goosdisk,disable-modern=on \
  -netdev "user,id=net0,guestfwd=tcp:10.0.2.100:443-cmd:$nc 127.0.0.1 $bootstrap_port" \
  -device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4 \
  <"$fifo" >"$log" 2>&1 &
 guest_pid=$!
 exec 3>"$fifo"
 wait_for 'goos> ' 90 "$log" "$guest_pid"
 wait_for 'NETD_READY 10\.0\.2\.' 30 "$log" "$guest_pid"
 sleep 4
}
window() { awk -v start="$command_start" -v command="$1" 'NR <= start {next} seen && index($0,"goos> ")==1 {exit} seen {print} !seen && ($0=="goos> " command || $0==command) {seen=1}' "$log"; }
run_command() {
 command=$1
 command_start=$(wc -l <"$log")
 printf '%s\n' "$command" >&3
 completion='SHELL_EXEC_OK$'
 case "$command" in 'hosts set '*) completion='HOSTS_SET_OK$' ;; esac
 elapsed=0
 while [ "$elapsed" -lt 60 ]; do
  if window "$command" | grep -q "$completion"; then sleep 1; return 0; fi
  kill -0 "$guest_pid" 2>/dev/null || break
  sleep 1; elapsed=$((elapsed+1))
 done
 fail "command timeout: $command"
}
require() { window "$2" | grep -Eq -- "$3" || fail "$1"; pass "$1"; }
positive() {
 run_command "$2"
 require "$1.response" "$2" "$3"
 if window "$2" | grep -q 'exit status:'; then fail "$1.exit"; fi
 pass "$1.exit"
}
negative() { run_command "$2"; require "$1.exit" "$2" '^exit status: 1$'; require "$1.reason" "$2" "$3"; }
poweroff() {
 printf 'poweroff\n' >&3
 wait_for '^Powering off\.\.\.$' 30 "$log" "$guest_pid"
 elapsed=0
 while kill -0 "$guest_pid" 2>/dev/null && [ "$elapsed" -lt 30 ]; do sleep 1; elapsed=$((elapsed+1)); done
 kill -0 "$guest_pid" 2>/dev/null && fail poweroff
 wait "$guest_pid" || fail qemu-exit
 guest_pid=
 exec 3>&-
 pass "boot$boot_number.poweroff"
}
package_paths() { printf '/home/user/trust-fixtures/%s/ca-certificates.pem /home/user/trust-fixtures/%s/ca-certificates.json /home/user/trust-fixtures/%s/ca-certificates.sig' "$1" "$1" "$1"; }
boot
positive fresh.status 'trust status' '^Trust store: system fallback$'
positive hosts 'hosts set 10.0.2.100 tls.test' 'HOSTS_SET_OK$'
negative before.newroot "fetch https://10.0.2.2:$newroot_port/hello" 'unknown.*authority'
positive package.verify "trust verify $(package_paths update)" '^Trust package verified: sequence 2026101501$'
positive update.install 'trust update' '^Trust store updated: sequence 2026101501$'
positive updated.status 'trust status' '^Sequence: 2026101501$'
require updated.source 'trust status' '^Trust store: updated$'
positive updated.newroot "fetch https://10.0.2.2:$newroot_port/hello" '^new-root-okSHELL_EXEC_OK$'
negative rollback "trust install $(package_paths rollback)" 'refusing.*rollback'
negative badsig "trust install $(package_paths badsig)" 'signature invalid'
positive same.sequence "trust install $(package_paths update)" '^Trust store updated: sequence 2026101501$'
poweroff
boot
positive persisted.status 'trust status' '^Sequence: 2026101501$'
positive persisted.newroot "fetch https://10.0.2.2:$newroot_port/hello" '^new-root-okSHELL_EXEC_OK$'
positive hosts.persisted 'hosts set 10.0.2.100 tls.test' 'HOSTS_SET_OK$'
positive selector.corrupt 'fetch -o /state/security/active https://tls.test/hello' 'SHELL_EXEC_OK$'
positive selector.recover 'trust status' '^Sequence: 2026101501$'
positive signature.corrupt 'fetch -o /state/security/generations/2026101501/ca-certificates.sig https://tls.test/hello' 'SHELL_EXEC_OK$'
poweroff
boot
positive corrupt.status 'trust status' '^Trust store: system fallback$'
require corrupt.reason 'trust status' '^Reason: state package invalid:'
positive hosts.recovery 'hosts set 10.0.2.100 tls.test' 'HOSTS_SET_OK$'
positive fallback.https 'fetch https://tls.test/hello' '^helloSHELL_EXEC_OK$'
negative corrupt.newroot "fetch https://10.0.2.2:$newroot_port/hello" 'unknown.*authority'
positive shell.survives 'cat /system/security/trust-update-url' 'https://tls.test/update'
poweroff
printf 'trust-update: %s assertions passed, 3 persistent boots\nReport: %s\n' "$checks" "$work"
