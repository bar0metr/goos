#!/bin/sh
set -eu

# Native identity lifecycle and security gate (GOOS_0.4.0_M4_FIX §6.3).
#
# One disposable installed disk ($3) runs the full lifecycle: clean boot,
# provision, account/group/key mutation, identityd restart through PID 1's
# VFS-fault path, delete/recreate without UID/GID reuse, an untrusted launch,
# scope-denied VFS access, and an in-guest reboot that proves the DB and a key
# revocation persisted. A second disposable image is built here, seeded with a
# corrupt DB, and proves provision fails closed while reset --confirm recovers
# with a fresh epoch and no records.
#
# Run by the (lead-wired) test-identity-vm target:
#   make iso VALIDATION=1
#   make test-disk VALIDATION=1 TEST_DISK=build/identity-virtio-blk.img
#   MEMORY=$MEMORY tests/boot/identity.sh $ISO $OVMF_CODE build/identity-virtio-blk.img

log=build/logs/identity.log
corrupt_log=build/logs/identity-corrupt.log
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

require_count() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	observed=$(log_count "$file" "$pattern")
	[ "$observed" -eq "$expected" ] || fail_check "$name" "$expected" "$observed" "$file" "$anchor"
	checks=$((checks + 1))
}

require_min_count() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	anchor=$5
	observed=$(log_count "$file" "$pattern")
	[ "$observed" -ge "$expected" ] || fail_check "$name" "at least $expected" "$observed" "$file" "$anchor"
	checks=$((checks + 1))
}

# window prints the log lines after the first line containing $2 and before the
# next line containing $3, so checks can be tied to a single command's output.
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

forbid_window() {
	name=$1
	pattern=$2
	expected=$3
	file=$4
	start=$5
	stop=$6
	observed=$(window "$file" "$start" "$stop" | grep "$pattern" | sed -n '1p' || true)
	[ -z "$observed" ] || fail_check "$name" "$expected" "$observed" "$file" "$start"
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

wait_for_count() {
	phase=$1
	pattern=$2
	expected=$3
	timeout=$4
	file=$5
	elapsed=0
	while [ "$elapsed" -lt "$timeout" ]; do
		observed=$(log_count "$file" "$pattern")
		[ "$observed" -ge "$expected" ] && return
		sleep 1
		elapsed=$((elapsed + 1))
	done
	fail_check "timeout.$phase" "$expected matches within ${timeout}s" "$observed matches after ${elapsed}s" "$file" "$pattern"
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

variant=$(cut -d' ' -f5 build/config/build)
system_tree=build/variants/$variant/system

# Fixed Ed25519 keys and their SHA-256 fingerprints (deterministic; see
# remotepolicy.ParseAuthorizedKeys and identity.Fingerprint).
key_a='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAOhB7/zzhC+HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4 KEY_A'
fp_a='SHA256:Vkdap1RjR0wChd9dvyvKtz2mUTWIOem3dIGy6rEHcIw'
key_b='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGFb9FqqZXpOZNdSxu9TYozevd20kdyu+w/IyqWktj8j KEY_B'
fp_b='SHA256:HWmR3Us09oRUsroDGTHGm8pFWYkCTVHXRZWDAq5VJ28'

rm -f "$log"

# One installed disk runs the whole lifecycle, including an in-guest reboot
# (no -no-reboot, like the M7 acceptance scenario).
( await_ready "$log"
	# Clean boot: unprovisioned.
	send_after_done 'user status; echo identity-status-0' '^identity-status-0$'

	# Provision, and a second provision is rejected without touching the DB.
	send_after_done 'user provision; echo identity-provision-1' '^identity-provision-1$'
	send_after_done 'user provision; echo identity-provision-2' '^identity-provision-2$'
	send_after_done 'user status; echo identity-status-1' '^identity-status-1$'

	# Two accounts, one group; UID/home/membership and keys.
	send_after_done 'user add alice; echo identity-add-alice' '^identity-add-alice$'
	send_after_done 'user add bob; echo identity-add-bob' '^identity-add-bob$'
	send_after_done 'group add staff; echo identity-add-staff' '^identity-add-staff$'
	send_after_done 'user uid alice 2000; echo identity-uid-alice' '^identity-uid-alice$'
	send_after_done 'user home alice /home/alice; echo identity-home-alice' '^identity-home-alice$'
	send_after_done 'user home bob /home/bob; echo identity-home-bob' '^identity-home-bob$'
	send_after_done "user key add alice \"$key_a\"; echo identity-key-alice" '^identity-key-alice$'
	send_after_done "user key add bob \"$key_b\"; echo identity-key-bob" '^identity-key-bob$'
	send_after_done 'group members staff 2000; echo identity-members-staff' '^identity-members-staff$'

	# Read-back: status, both lists, both shows, and the conflict paths.
	send_after_done 'user status; echo identity-status-2' '^identity-status-2$'
	send_after_done 'user list; echo identity-list-1' '^identity-list-1$'
	send_after_done 'user show alice; echo identity-show-alice-1' '^identity-show-alice-1$'
	send_after_done 'user show bob; echo identity-show-bob-1' '^identity-show-bob-1$'
	send_after_done 'group list; echo identity-grouplist-1' '^identity-grouplist-1$'
	send_after_done 'user add alice; echo identity-dup-alice' '^identity-dup-alice$'
	send_after_done 'group delete staff; echo identity-del-staff-ref' '^identity-del-staff-ref$'

	# Disable/enable and key revocation.
	send_after_done 'user disable alice; echo identity-disable-alice' '^identity-disable-alice$'
	send_after_done 'user show alice; echo identity-show-alice-2' '^identity-show-alice-2$'
	send_after_done 'user enable alice; echo identity-enable-alice' '^identity-enable-alice$'
	send_after_done 'user show alice; echo identity-show-alice-3' '^identity-show-alice-3$'
	send_after_done "user key remove bob $fp_b; echo identity-keyrevoke-bob" '^identity-keyrevoke-bob$'
	send_after_done 'user show bob; echo identity-show-bob-2' '^identity-show-bob-2$'
	send_after_done 'user status; echo identity-status-3' '^identity-status-3$'

	# The protected identity subtree denies every read and mutation to an
	# ordinary shell session, through both /state and the /persist alias.
	send_after_done 'echo x > /state/identity/db; echo identity-deny-write' '^identity-deny-write$'
	send_after_done 'touch /state/identity/extra; echo identity-deny-create' '^identity-deny-create$'
	send_after_done 'rm /state/identity/db; echo identity-deny-unlink' '^identity-deny-unlink$'
	send_after_done 'mv /state/identity/db /state/identity/db2; echo identity-deny-rename' '^identity-deny-rename$'
	send_after_done 'cp /system/motd /state/identity/db; echo identity-deny-replace' '^identity-deny-replace$'
	send_after_done 'mv /state/identity /state/identity-moved; echo identity-deny-parent' '^identity-deny-parent$'
	send_after_done 'cat /persist/state/identity/db; echo identity-deny-alias' '^identity-deny-alias$'

	# Restart identityd through PID 1's restart path: killing the VFS tears
	# down identityd's confined session, identityd exits, and the supervisor
	# relaunches it with fresh grants (ADR 0233).
	printf 'fault vfs\n'
	wait_for_count identity.vfs-restarted '^VFS_RESTARTED_OK$' 1 60 "$log"
	wait_for_count identity.identityd-relaunched '^SERVICE_FROM_DISK_OK identityd$' 2 60 "$log"

	# The shell's admin session does not survive the identityd restart: the
	# pre-restart endpoint is rejected. Fresh grants arrive with a fresh shell
	# on the reboot below.
	send_after_done 'user status; echo identity-status-4' '^identity-status-4$'

	# An ordinary launch from a non-index origin cannot obtain identity-admin,
	# and the identity subtree stays unreadable to an untrusted session.
	send_after_done 'mkdir /home/demo; echo identity-mkdir-demo' '^identity-mkdir-demo$'
	send_after_done 'cp /system/bin/user /home/demo/user; echo identity-cp-user' '^identity-cp-user$'
	send_after_done '/home/demo/user status; echo identity-untrusted' '^identity-untrusted$'
	send_after_done 'cat /state/identity/db; echo identity-scope-1' '^identity-scope-1$'
	# The scope is re-installed with the rebuilt namespace: the denials hold
	# again after the VFS restart.
	send_after_done 'touch /state/identity/extra; echo identity-deny-create-restart' '^identity-deny-create-restart$'
	send_after_done 'rm /state/identity/db; echo identity-deny-unlink-restart' '^identity-deny-unlink-restart$'
	send_after_done 'mv /state/identity /state/identity-moved; echo identity-deny-parent-restart' '^identity-deny-parent-restart$'

	# Reboot the same disk and confirm the DB and the bob key revocation held.
	printf 'reboot\n'
	await_ready "$log" 2
	send_after_done 'user status; echo identity-status-5' '^identity-status-5$'
	send_after_done 'user list; echo identity-list-3' '^identity-list-3$'
	send_after_done 'user show alice; echo identity-show-alice-5' '^identity-show-alice-5$'
	send_after_done 'user show bob; echo identity-show-bob-4' '^identity-show-bob-4$'
	send_after_done 'group list; echo identity-grouplist-3' '^identity-grouplist-3$'
	# Delete and recreate a disposable account after the reboot: the committed
	# UID/GID high-water marks must survive, so the numbers are never reused.
	send_after_done 'user add tmp; echo identity-add-tmp-1' '^identity-add-tmp-1$'
	send_after_done 'user home tmp /home/tmp; echo identity-home-tmp-1' '^identity-home-tmp-1$'
	send_after_done 'user show tmp; echo identity-show-tmp-1' '^identity-show-tmp-1$'
	send_after_done 'user delete tmp; echo identity-del-tmp-1' '^identity-del-tmp-1$'
	send_after_done 'user add tmp; echo identity-add-tmp-2' '^identity-add-tmp-2$'
	send_after_done 'user home tmp /home/tmp; echo identity-home-tmp-2' '^identity-home-tmp-2$'
	send_after_done 'user show tmp; echo identity-show-tmp-2' '^identity-show-tmp-2$'
	send_after_done 'user delete tmp; echo identity-del-tmp-2' '^identity-del-tmp-2$'
	send_after_done 'cat /state/identity/db; echo identity-scope-2' '^identity-scope-2$'
	# The scope holds across a reboot too.
	send_after_done 'touch /state/identity/extra; echo identity-deny-create-reboot' '^identity-deny-create-reboot$'
	send_after_done 'rm /state/identity/db; echo identity-deny-unlink-reboot' '^identity-deny-unlink-reboot$'
	send_after_done 'mv /state/identity /state/identity-moved; echo identity-deny-parent-reboot' '^identity-deny-parent-reboot$'
	# The real lookup session (item 4): alice still holds key_a, bob's key_b
	# was revoked, and carol is added and disabled so identityprobe proves a
	# disabled account denies. identityprobe is validation-only and PID 1
	# grants it a lookup session for declaring GrantIdentityLookup.
	send_after_done 'user add carol; echo identity-add-carol' '^identity-add-carol$'
	send_after_done 'user disable carol; echo identity-disable-carol' '^identity-disable-carol$'
	send_after_done 'identityprobe; echo identity-probe-done' '^identity-probe-done$'
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-shutdown >"$log" 2>&1 &
pid=$!

wait_for_marker identity '^identity-probe-done$' 900 "$log"
cleanup

# --- assertions: clean boot ---
require_regex identity.service-reported '^\[ OK \] identityd$' 'the identity service reported started' "$log" 'Starting services'
require_window identity.status-unprovisioned '^unprovisioned$' 'user status reports unprovisioned' "$log" 'goos> user status; echo identity-status-0' 'identity-status-0'
pass_group identity-clean

# --- assertions: provision ---
require_window identity.provision-rejected '^user: already provisioned$' 'a second provision is refused' "$log" 'goos> user provision; echo identity-provision-2' 'identity-provision-2'
require_window identity.status-empty '^available accounts=0 groups=0 keys=0$' 'provision committed an empty store' "$log" 'goos> user status; echo identity-status-1' 'identity-status-1'
pass_group identity-provision

# --- assertions: accounts and groups ---
require_window identity.status-full '^available accounts=2 groups=3 keys=2$' 'two accounts, three groups, two keys' "$log" 'goos> user status; echo identity-status-2' 'identity-status-2'
require_window identity.list-alice '^alice 2000 1000 yes /home/alice$' 'alice with explicit UID and home' "$log" 'goos> user list; echo identity-list-1' 'identity-list-1'
require_window identity.list-bob '^bob 1001 1001 yes /home/bob$' 'bob with its private primary group' "$log" 'goos> user list; echo identity-list-1' 'identity-list-1'
require_window identity.show-alice '^alice 2000 1000 /home/alice yes 1$' 'alice show carries the key count' "$log" 'goos> user show alice; echo identity-show-alice-1' 'identity-show-alice-1'
require_window identity.show-bob '^bob 1001 1001 /home/bob yes 1$' 'bob show carries the key count' "$log" 'goos> user show bob; echo identity-show-bob-1' 'identity-show-bob-1'
require_window identity.grouplist '^staff 1002$' 'staff got the next GID' "$log" 'goos> group list; echo identity-grouplist-1' 'identity-grouplist-1'
require_window identity.key-add-alice "^$fp_a$" 'alice key fingerprint reported' "$log" 'goos> user key add alice' 'identity-key-alice'
require_window identity.dup-alice '^user: already exists$' 'a duplicate account is refused' "$log" 'goos> user add alice; echo identity-dup-alice' 'identity-dup-alice'
require_window identity.del-staff-ref '^group: group still referenced$' 'a referenced group is not deletable' "$log" 'goos> group delete staff; echo identity-del-staff-ref' 'identity-del-staff-ref'
pass_group identity-accounts

# --- assertions: disable/enable and key revocation ---
require_window identity.disable '^alice 2000 1000 /home/alice no 1$' 'alice disabled' "$log" 'goos> user show alice; echo identity-show-alice-2' 'identity-show-alice-2'
require_window identity.enable '^alice 2000 1000 /home/alice yes 1$' 'alice enabled again' "$log" 'goos> user show alice; echo identity-show-alice-3' 'identity-show-alice-3'
require_window identity.revoke '^bob 1001 1001 /home/bob yes 0$' 'bob key revoked' "$log" 'goos> user show bob; echo identity-show-bob-2' 'identity-show-bob-2'
require_window identity.status-after-revoke '^available accounts=2 groups=3 keys=1$' 'one key left after revocation' "$log" 'goos> user status; echo identity-status-3' 'identity-status-3'
pass_group identity-lifecycle

# --- assertions: identityd restart (VFS fault) ---
require_marker identity.vfs-fault VFS_FAULT_INJECTED "$log" 'fault vfs'
require_marker identity.vfs-restarted VFS_RESTARTED_OK "$log" 'fault vfs'
require_marker identity.vfs-died IDENTITY_VFS_DIED "$log"
require_min_count identity.identityd-relaunched '^SERVICE_FROM_DISK_OK identityd$' 2 "$log" 'SERVICE_FROM_DISK_OK identityd'
require_window identity.restart-status '^available accounts=2 groups=3 keys=1$' 'committed state after identityd restart' "$log" 'user status; echo identity-status-4' 'identity-status-4'
pass_group identity-restart

# --- assertions: untrusted launch and scope denial ---
require_window identity.untrusted-launch '^user: identity service unavailable$' 'a non-index launch gets no identity-admin' "$log" 'goos> /home/demo/user status' 'identity-untrusted'
require_window identity.scope-denied 'permission denied' 'the identity DB stays unreadable after provisioning' "$log" 'goos> cat /state/identity/db; echo identity-scope-1' 'identity-scope-1'
pass_group identity-security

# --- assertions: reboot persistence ---
require_count identity.boots 'Kernel initialized' 2 "$log" 'Rebooting...'
require_window identity.reboot-status '^available accounts=2 groups=3 keys=1$' 'DB state survived reboot' "$log" 'user status; echo identity-status-5' 'identity-status-5'
require_window identity.reboot-list-alice '^alice 2000 1000 yes /home/alice$' 'alice record survived reboot' "$log" 'goos> user list; echo identity-list-3' 'identity-list-3'
require_window identity.reboot-show-alice '^alice 2000 1000 /home/alice yes 1$' 'alice key survived reboot' "$log" 'goos> user show alice; echo identity-show-alice-5' 'identity-show-alice-5'
require_window identity.reboot-show-bob '^bob 1001 1001 /home/bob yes 0$' 'bob key revocation survived reboot' "$log" 'goos> user show bob; echo identity-show-bob-4' 'identity-show-bob-4'
forbid_window identity.reboot-bob-no-revert 'yes 1$' 'no revoked key came back after reboot' "$log" 'goos> user show bob; echo identity-show-bob-4' 'identity-show-bob-4'
require_window identity.tmp-first '^tmp 2001 1003 /home/tmp yes 0$' 'first tmp got the next UID/GID' "$log" 'goos> user show tmp; echo identity-show-tmp-1' 'identity-show-tmp-1'
require_window identity.tmp-recreated '^tmp 2002 1004 /home/tmp yes 0$' 'recreated tmp got fresh, never-reused numbers' "$log" 'goos> user show tmp; echo identity-show-tmp-2' 'identity-show-tmp-2'
forbid_window identity.tmp-no-base-reuse ' 1000 ' 'recreated tmp did not fall back to AllocBase' "$log" 'goos> user show tmp; echo identity-show-tmp-2' 'identity-show-tmp-2'
require_window identity.reboot-scope 'permission denied' 'identity scope re-installed after reboot' "$log" 'goos> cat /state/identity/db; echo identity-scope-2' 'identity-scope-2'
pass_group identity-reboot

# --- assertions: protected subtree denials (item 5) ---
require_window identity.deny-write 'read-only filesystem' 'write to the identity DB denied' "$log" 'goos> echo x > /state/identity/db; echo identity-deny-write' 'identity-deny-write'
require_window identity.deny-create 'read-only filesystem' 'create in the identity subtree denied' "$log" 'goos> touch /state/identity/extra; echo identity-deny-create' 'identity-deny-create'
require_window identity.deny-unlink 'read-only filesystem' 'unlink of the identity DB denied' "$log" 'goos> rm /state/identity/db; echo identity-deny-unlink' 'identity-deny-unlink'
require_window identity.deny-rename 'read-only filesystem' 'rename within the identity subtree denied' "$log" 'goos> mv /state/identity/db /state/identity/db2; echo identity-deny-rename' 'identity-deny-rename'
require_window identity.deny-replace 'read-only filesystem' 'replace of the identity DB denied' "$log" 'goos> cp /system/motd /state/identity/db; echo identity-deny-replace' 'identity-deny-replace'
require_window identity.deny-parent 'read-only filesystem' 'moving the identity subtree root denied' "$log" 'goos> mv /state/identity /state/identity-moved; echo identity-deny-parent' 'identity-deny-parent'
require_window identity.deny-alias 'permission denied' 'the /persist/state/identity alias is denied too' "$log" 'goos> cat /persist/state/identity/db; echo identity-deny-alias' 'identity-deny-alias'
require_window identity.deny-create-restart 'read-only filesystem' 'create denied again after the VFS restart' "$log" 'goos> touch /state/identity/extra; echo identity-deny-create-restart' 'identity-deny-create-restart'
require_window identity.deny-unlink-restart 'read-only filesystem' 'unlink denied again after the VFS restart' "$log" 'goos> rm /state/identity/db; echo identity-deny-unlink-restart' 'identity-deny-unlink-restart'
require_window identity.deny-parent-restart 'read-only filesystem' 'parent move denied again after the VFS restart' "$log" 'goos> mv /state/identity /state/identity-moved; echo identity-deny-parent-restart' 'identity-deny-parent-restart'
require_window identity.deny-create-reboot 'read-only filesystem' 'create denied again after reboot' "$log" 'goos> touch /state/identity/extra; echo identity-deny-create-reboot' 'identity-deny-create-reboot'
require_window identity.deny-unlink-reboot 'read-only filesystem' 'unlink denied again after reboot' "$log" 'goos> rm /state/identity/db; echo identity-deny-unlink-reboot' 'identity-deny-unlink-reboot'
require_window identity.deny-parent-reboot 'read-only filesystem' 'parent move denied again after reboot' "$log" 'goos> mv /state/identity /state/identity-moved; echo identity-deny-parent-reboot' 'identity-deny-parent-reboot'
pass_group identity-vm-security

# --- assertions: lookup session (item 4) ---
require_marker identity.probe-ok 'identityprobe-ok' "$log" 'identityprobe'
forbid_regex identity.probe-no-fail 'identityprobe-fail' 'no identityprobe case mismatch' "$log" 'identityprobe'
pass_group identity-lookup

# --- corrupt DB recovery on a separate disposable image ---
corrupt_db_file=build/identity-corrupt.db
corrupt_disk=build/identity-corrupt-virtio-blk.img
rm -f "$corrupt_log" "$corrupt_disk"
printf '%s\n' 'this is not a valid GoOS identity snapshot' >"$corrupt_db_file"
go run ./tools/mkfat32 -tree "$system_tree" -put "$corrupt_db_file:/state/identity/db" "$corrupt_disk"

( await_ready "$corrupt_log"
	printf 'user status; echo corrupt-status-1\n'
	sleep 2
	printf 'user provision; echo corrupt-provision\n'
	sleep 2
	printf 'user reset --confirm; echo corrupt-reset\n'
	sleep 3
	printf 'user status; echo corrupt-status-2\n'
	sleep 2
	printf 'echo identity-corrupt-done\n'
) | qemu-system-x86_64 -machine q35 -smp "$smp" -accel tcg,thread=multi -m "$memory" \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$corrupt_disk" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-cdrom "$iso" -display none -serial stdio -no-reboot -no-shutdown >"$corrupt_log" 2>&1 &
pid=$!

wait_for_marker identity-corrupt '^identity-corrupt-done$' 90 "$corrupt_log"
cleanup

require_window identity-corrupt.status '^unavailable$' 'a corrupt DB reports unavailable' "$corrupt_log" 'goos> user status; echo corrupt-status-1' 'corrupt-status-1'
require_window identity-corrupt.provision '^user: identity store unavailable$' 'provision never overwrites corruption' "$corrupt_log" 'goos> user provision; echo corrupt-provision' 'corrupt-provision'
require_window identity-corrupt.reset-recovered '^available accounts=0 groups=0 keys=0$' 'reset --confirm recovered with a fresh empty store' "$corrupt_log" 'goos> user status; echo corrupt-status-2' 'corrupt-status-2'
forbid_regex identity-corrupt.no-panic 'PANIC' 'no panic recovering a corrupt DB' "$corrupt_log" 'identity-corrupt-done'
pass_group identity-corrupt

printf '\nPASS identity-vm (%s checks)\n' "$checks"
