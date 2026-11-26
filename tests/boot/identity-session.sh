#!/bin/sh
# Identity/session/creation gate: boots one guest with the installed FAT32
# system and one Linux-built ext4 partition, provisions an account with a high
# UID and GID, admits it over SSH, creates a directory and a file on the ext4
# volume through the account shell, and verifies the stored UID/GID both in
# the guest (a Linux stat) and on the host (debugfs) after the volume is
# extracted. Identity is data, never authority: the gate also proves a UID-0
# denial at admission.
set -eu

iso=${1:?ISO path required}
firmware=${2:?OVMF path required}
out=${3:?output directory required}
memory=${MEMORY:-512M}
smp=${SMP:-1}
command_timeout=${COMMAND_TIMEOUT:-60}
read -r _hz _diag validation _opt variant <build/config/build
[ "$validation" = 1 ] || { echo 'identity-session.sh needs a VALIDATION=1 build' >&2; exit 2; }
tree=build/variants/$variant/system
log=build/logs/identity-session.log
fatal='PANIC\|OUT_OF_MEMORY\|exec: no reply\|INIT_EXEC_.*FAILED\|VFS_.*FAILED\|BLOCK_.*FAILED'

mkdir -p build/logs "$out"
rm -f "$log" "$out"/*.img
work=$(mktemp -d "${TMPDIR:-/tmp}/goos-identity-session.XXXXXX")
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

printf 'system.mount.1 text ext4 name:goos-rw /mnt/w rw\n' >"$work/config"
go run ./tools/mkfat32 -new -size auto -tree "$tree" -put "$work/config:/state/config" "$work/system.img" >/dev/null
go run ./tools/diskcompose \
	-part "goos-system:data:$work/system.img" \
	-part goos-rw:linux:userspace/ext4/testdata/canonical.img.gz \
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

# The disk is a throwaway copy written in place; the host checks read what the
# guest left on it.
qemu-system-x86_64 -machine q35 -m "$memory" -smp "cpus=$smp,sockets=1,cores=$smp,threads=1" -accel tcg,thread=multi \
	-drive "if=pflash,format=raw,readonly=on,file=$firmware" \
	-drive "if=none,id=goosdisk,format=raw,file=$work/disk.img" \
	-device virtio-blk-pci,drive=goosdisk,disable-modern=on \
	-netdev "user,id=net0,hostfwd=tcp:127.0.0.1:22022-:22" \
	-device virtio-net-pci,netdev=net0,disable-modern=on,mac=52:54:00:12:34:56,addr=0x4 \
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

# Provision the database and create the account with a high UID (2000) and its
# auto-allocated private group.
run 'user provision'
run 'user add alice'
run 'user uid alice 70000'
run 'group add staff'
run 'group gid staff 80000'
run 'user gid alice 80000'
run 'user home alice /home/alice'
run 'user show alice'
expect 'alice 70000 80000 /home/alice' 'account alice with high UID 70000 GID 80000'
# Home provisioning is explicit: create the directory the shell is rooted at.
run 'mkdir /home/alice'
# A second account with a distinct UID, GID, home and key proves admission is
# per-account, not a single shared identity.
run 'group add ops'
run 'group gid ops 80001'
run 'user add bob'
run 'user uid bob 70001'
run 'user gid bob 80001'
run 'user home bob /home/bob'
run 'user show bob'
expect 'bob 70001 80001 /home/bob' 'account bob with UID 70001 GID 80001'
run 'mkdir /home/bob'
# A file bob owns; alice's rooted session must not reach it.
run 'echo bob-secret > /home/bob/secret'

# Generate a disposable Ed25519 client key and authorize it for the account
# directly. The legacy authorized_keys file is deliberately never created:
# the account database is the sole key authority and the listener must start
# and admit accounts without that file.
ssh-keygen -q -t ed25519 -N '' -C identity-session -f "$work/client"
client_pub=$(cat "$work/client.pub")
run "user key add alice \"$client_pub\""
ssh-keygen -q -t ed25519 -N '' -C identity-session-bob -f "$work/bob"
bob_pub=$(cat "$work/bob.pub")
run "user key add bob \"$bob_pub\""
run 'user show alice'
expect 'yes 1' 'authorized key committed'
run 'user show bob'
expect 'yes 1' 'second account key committed'
# An SSH helper used for the admission assertions below.
ssh_as() { # key user command
	ssh -F /dev/null -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o BatchMode=yes -o ConnectTimeout=30 -p 22022 -i "$1" \
		-o KexAlgorithms=curve25519-sha256 -o HostKeyAlgorithms=ssh-ed25519 -o PubkeyAcceptedAlgorithms=ssh-ed25519 \
		"$2"@127.0.0.1 "$3"
}
# ssh_ok retries only transport-level failures, so a slow QEMU banner under host
# load does not fail an assertion whose expected outcome is success.
ssh_ok() { # key user command
	attempt=0
	while [ "$attempt" -lt 3 ]; do
		if ssh_out=$(ssh_as "$1" "$2" "$3" 2>&1); then
			printf '%s' "$ssh_out"
			return 0
		fi
		attempt=$((attempt + 1))
		printf '%s' "$ssh_out" | grep -qiE 'timed out|connection reset|connection refused|broken pipe|kex_exchange' || {
			printf '%s' "$ssh_out"
			return 1
		}
		sleep 2
	done
	printf '%s' "$ssh_out"
	return 1
}

run 'config set system.remote.enabled true'
run 'config set system.remote.port 22'
run 'remote reload'
refuse() { ! grep -q "$1" "$work/output" || fail "$2"; }
refuse 'rejected' 'remote reload accepted the account listener without the legacy key file'

# The ext4 volume is configured to mount read-write at /mnt/w at boot.
run 'mount'
expect '/mnt/w.*ext4.*rw' 'configured ext4 volume mounted read-write'

# Create as the account over SSH, then verify the stored ownership in the
# guest with the native stat command (mode, uid, gid).
result=$(ssh_ok "$work/client" alice 'mkdir /mnt/w/alice-made; touch /mnt/w/alice-made/f.txt; stat /mnt/w/alice-made/f.txt') || fail "SSH exec as alice: $result"
printf 'guest stat: %s\n' "$result"
printf '%s' "$result" | grep -q 'uid=70000 gid=80000' || fail "guest stat does not report the account ownership: $result"

# The account database is the sole key authority after cutover: reload and
# admit the same account again while the legacy key file stays absent.
run 'remote reload'
refuse 'rejected' 'remote reload without the legacy key file'
result=$(ssh_ok "$work/client" alice 'stat /mnt/w/alice-made') || fail "SSH exec without the legacy key file: $result"
printf 'fileless stat: %s\n' "$result"
printf '%s' "$result" | grep -q 'uid=70000' || fail "fileless admission did not run: $result"

# The second account admits with its own identity and creates its own files.
result=$(ssh_ok "$work/bob" bob 'mkdir /mnt/w/bob-made; stat /mnt/w/bob-made') || fail "SSH exec as bob: $result"
printf 'bob stat: %s\n' "$result"
printf '%s' "$result" | grep -q 'uid=70001 gid=80001' || fail "bob stat does not report bob's ownership: $result"

# Cross-account home isolation: alice's rooted session cannot name bob's home.
if result=$(ssh_as "$work/client" alice 'cat /home/bob/secret' 2>&1); then
	fail "alice read another account's home: $result"
fi
printf 'cross-account denial: %s\n' "$result"
printf '%s' "$result" | grep -qi 'permission denied' || fail "cross-account read did not fail closed: $result"

# A key authorized for bob must not admit alice.
if result=$(ssh_as "$work/bob" alice 'true' 2>&1); then
	fail "bob's key admitted alice: $result"
fi
printf 'wrong-key denial: %s\n' "$result"

# A disabled account fails closed even with its own valid key, and re-enabling
# restores admission.
run 'user disable bob'
if result=$(ssh_as "$work/bob" bob 'true' 2>&1); then
	fail "disabled account was admitted: $result"
fi
printf 'disabled denial: %s\n' "$result"
run 'user enable bob'
result=$(ssh_ok "$work/bob" bob 'stat /mnt/w/bob-made') || fail "re-enabled account admission: $result"
printf '%s' "$result" | grep -q 'uid=70001' || fail "re-enabled admission did not run: $result"

# Cut the guest off and extract the written volume for the host debugfs check.
run 'sync'
kill "$pid"
wait "$pid" 2>/dev/null || true
pid=
go run ./tools/diskcompose -extract goos-rw "$work/disk.img" "$out/alice.img"

printf 'PASS identity-session (account creation, SSH admission, guest ownership)\n'
