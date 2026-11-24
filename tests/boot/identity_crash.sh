#!/bin/sh
set -eu

# Identity crash-consistency gate (GOOS_0.4.0_M4_FIX §6.3, item 6).
#
# One disposable installed disk ($3) is provisioned, given two accounts and
# keys, and then a key revocation is committed durably (a completed
# `user key remove` followed by `user status` reporting the committed state).
# QEMU is then killed abruptly with SIGKILL -- no graceful `reboot`/poweroff --
# and the SAME disk image is booted again. The DB must load to a valid
# committed state: never corrupt or unavailable, with the revocation intact.
#
# The abrupt kill is distinguished from identity.sh's graceful `reboot` by the
# first boot never printing "Rebooting": the cut is recorded with the
# identity-crash-cut marker, after `user status` has shown the committed state.
#
# Run by a lead-wired target as:
#   tests/boot/identity_crash.sh $ISO $OVMF_CODE $DISK
# where $DISK is a fresh disposable installed disk (make test-disk).

crash_log=build/logs/identity-crash.log
crash_reboot_log=build/logs/identity-crash-reboot.log
log=$crash_log
pid=
checks=0
shell_ready_sequence=0
shell_ready_limit=600

cleanup() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
		pid=
	fi
}

print_context() {
	file=$1
	anchor=$2
	match=$(grep -n -F "$anchor" "$file" 2>/dev/null | sed -n '1p' || true)
	case "$match" in
	*:*)
		line=${match%%:*}
		start=$((line > 8 ? line - 8 : 1))
		end=$((line + 12))
		sed -n "${start},${end}p" "$file" >&2
		;;
	*) tail -n 40 "$file" >&2 || true ;;
	esac
}

fail_check() {
	name=$1
	expected=$2
	observed=$3
	file=$4
	anchor=$5
	printf 'FAIL %s\n  expected: %s\n  observed: %s\n  log: %s\n' "$name" "$expected" "$observed" "$file" >&2
	printf '  context:\n' >&2
	print_context "$file" "$anchor"
	exit 1
}

log_lines() { tr -d '\r' 2>/dev/null <"$1"; }

log_count() { log_lines "$1" | grep -c "$2" || true; }

require_marker() {
	name=$1
	marker=$2
	file=$3
	anchor=${4-$marker}
	grep -F -q "$marker" "$file" 2>/dev/null || fail_check "$name" "$marker" 'marker not found' "$file" "$anchor"
	checks=$((checks + 1))
}

require_regex() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	log_lines "$file" | grep -q "$pattern" || fail_check "$name" "$expected" 'marker not found' "$file" "$anchor"
	checks=$((checks + 1))
}

forbid_regex() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	if log_lines "$file" | grep -q "$pattern"; then
		observed=$(log_lines "$file" | grep "$pattern" | sed -n '1p')
		fail_check "$name" "$expected" "$observed" "$file" "$anchor"
	fi
	checks=$((checks + 1))
}

window() {
	log_lines "$1" | awk -v start="$2" -v stop="$3" 'seen && index($0, stop) { exit } seen { print } !seen && index($0, start) { seen = 1 }'
}

window_closed() {
	log_lines "$1" | awk -v start="$2" -v stop="$3" 'seen && index($0, stop) { found = 1; exit } !seen && index($0, start) { seen = 1 } END { exit !found }'
}

require_window() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	start=$5
	stop=$6
	if [ "$stop" != NO_SUCH_LINE ] && ! window_closed "$file" "$start" "$stop"; then
		fail_check "$name" "$expected" "no '$stop' after '$start'" "$file" "$start"
	fi
	window "$file" "$start" "$stop" | grep -q "$pattern" || fail_check "$name" "$expected" 'marker not found' "$file" "$start"
	checks=$((checks + 1))
}

wait_for_marker() {
	phase=$1
	marker=$2
	timeout=$3
	file=$4
	elapsed=0
	while [ "$elapsed" -lt "$timeout" ]; do
		log_lines "$file" | grep -q "$marker" && return
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail_check "timeout.$phase" "$marker within ${timeout}s" "timeout after ${elapsed}s" "$file" "$marker"
}

pass_group() { printf 'PASS %s\n' "$1"; }

# await_ready waits until the Nth boot in log has settled: a prompt printed and
# the validation build's storage driver fault recovered from.
await_ready() {
	waited=0
	while [ "$waited" -lt 240 ]; do
		if awk -v want="${2:-1}" '/BOOT_SERVICES_EXITED/ { boot++ } boot == want && /goos> / { prompt = 1 } boot == want && /BLOCK_DRIVER_REPLACED_OK/ { driver = 1 } END { exit !(prompt && driver) }' "$1" 2>/dev/null; then
			sleep 3
			return 0
		fi
		sleep 0.5
		waited=$((waited + 1))
	done
}

# send_after_done types a command and waits for both its done-marker and the
# validation shell's acknowledgement, so the next command is never consumed as
# input for a still-running foreground job.
send_after_done() { # line done-pattern
	sent_before=$(log_count "$log" "$2")
	shell_ready_sequence=$((shell_ready_sequence + 1))
	shell_ready_pattern="^SHELL_READY token=$shell_ready_sequence\$"
	printf '%s; __boot_ready %s\n' "$1" "$shell_ready_sequence"
	shell_ready_waited=0
	while [ "$shell_ready_waited" -lt "$shell_ready_limit" ]; do
		if [ "$(log_count "$log" "$2")" -gt "$sent_before" ] &&
			log_lines "$log" | grep -q "$shell_ready_pattern"; then
			return 0
		fi
		sleep 0.1
		shell_ready_waited=$((shell_ready_waited + 1))
	done
	fail_check timeout.shell-ready "new '$2' and SHELL_READY token=$shell_ready_sequence" \
		"no completed foreground sequence after $shell_ready_limit polls" "$log" "$1"
}

trap cleanup EXIT
trap 'cleanup; exit 130' HUP INT TERM

mkdir -p build/logs

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
disk=${3:?test disk path required}
memory=${MEMORY:-512M}
smp=${SMP:-1}

# Fixed Ed25519 keys and their SHA-256 fingerprints (deterministic; see
# remotepolicy.ParseAuthorizedKeys and identity.Fingerprint).
key_a='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAOhB7/zzhC+HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4 KEY_A'
key_b='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGFb9FqqZXpOZNdSxu9TYozevd20kdyu+w/IyqWktj8j KEY_B'
fp_b='SHA256:HWmR3Us09oRUsroDGTHGm8pFWYkCTVHXRZWDAq5VJ28'

rm -f "$crash_log" "$crash_reboot_log"

# First boot: provision, add two accounts and keys, and commit a key revocation
# durably. `user status` then reports the committed state, and identity-crash-cut
# records the point where QEMU is killed abruptly, after durability is proven.
( await_ready "$crash_log"
	send_after_done 'user status; echo crash-status-0' '^crash-status-0$'
	send_after_done 'user provision; echo crash-provision' '^crash-provision$'
	send_after_done 'user add alice; echo crash-add-alice' '^crash-add-alice$'
	send_after_done 'user add bob; echo crash-add-bob' '^crash-add-bob$'
	send_after_done 'user home alice /home/alice; echo crash-home-alice' '^crash-home-alice$'
	send_after_done 'user home bob /home/bob; echo crash-home-bob' '^crash-home-bob$'
	send_after_done "user key add alice \"$key_a\"; echo crash-key-alice" '^crash-key-alice$'
	send_after_done "user key add bob \"$key_b\"; echo crash-key-bob" '^crash-key-bob$'
	send_after_done "user key remove bob $fp_b; echo crash-revoke-bob" '^crash-revoke-bob$'
	send_after_done 'user status; echo crash-status-1' '^crash-status-1$'
	printf 'echo identity-crash-cut\n'
	sleep 3
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$crash_log" 2>&1 &
pid=$!

wait_for_marker crash.cut '^identity-crash-cut$' 300 "$crash_log"

# Abrupt kill: SIGKILL, no graceful poweroff or reboot.
kill -9 "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
pid=

# Second boot: the SAME disk image, after the abrupt kill. The committed state
# (old or new, never corrupt) must load, with the revocation intact.
log=$crash_reboot_log
( await_ready "$crash_reboot_log"
	send_after_done 'user status; echo crash-status-2' '^crash-status-2$'
	send_after_done 'user show alice; echo crash-show-alice' '^crash-show-alice$'
	send_after_done 'user show bob; echo crash-show-bob' '^crash-show-bob$'
	printf 'echo identity-crash-done\n'
	sleep 3
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$crash_reboot_log" 2>&1 &
pid=$!

wait_for_marker crash.reboot '^identity-crash-done$' 300 "$crash_reboot_log"
cleanup

# --- assertions: the abrupt kill ---
require_marker crash.cut-marker 'identity-crash-cut' "$crash_log" 'identity-crash-cut'
forbid_regex crash.no-graceful-reboot 'Rebooting' 'no graceful reboot before the abrupt kill' "$crash_log" 'identity-crash-cut'
forbid_regex crash.no-poweroff 'poweroff' 'no graceful poweroff before the abrupt kill' "$crash_log" 'identity-crash-cut'
pass_group identity-crash-cut

# --- assertions: committed state survives the crash ---
require_window crash.reboot-status '^available accounts=2 groups=2 keys=1$' 'committed state survived the abrupt kill' "$crash_reboot_log" 'goos> user status; echo crash-status-2' 'crash-status-2'
require_window crash.reboot-show-alice '^alice 1000 1000 /home/alice yes 1$' 'alice key survived the crash' "$crash_reboot_log" 'goos> user show alice; echo crash-show-alice' 'crash-show-alice'
require_window crash.reboot-show-bob '^bob 1001 1001 /home/bob yes 0$' 'bob key revocation survived the crash' "$crash_reboot_log" 'goos> user show bob; echo crash-show-bob' 'crash-show-bob'
forbid_regex crash.reboot-not-unavailable '^unavailable$' 'no corrupt store after the crash' "$crash_reboot_log" 'identity-crash-done'
forbid_regex crash.reboot-not-unprovisioned '^unprovisioned$' 'no unprovisioned store after the crash' "$crash_reboot_log" 'identity-crash-done'
forbid_regex crash.reboot-no-panic 'PANIC' 'no panic after the crash' "$crash_reboot_log" 'identity-crash-done'
pass_group identity-crash

printf '\nPASS identity-crash (%s checks)\n' "$checks"
