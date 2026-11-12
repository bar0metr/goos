#!/bin/sh
# Checks volumes written by the ext4 mutation code and its journal with the
# pinned e2fsprogs build (docs/development/ext4-reference-versions.md).
#
# 1. TestExportMutatedImages writes cleanly unmounted images, each with a
#    manifest of what the GoOS reader sees: e2fsck -fn must report nothing
#    and the tree debugfs dumps must match the manifest.
# 2. TestExportCrashImages writes images cut by a simulated power loss,
#    with the tree GoOS recovers from each: e2fsck -fy must replay the
#    GoOS journal, a second e2fsck -fn must report nothing, and the dumped
#    tree must match what GoOS recovered.
# 3. debugfs writes JBD2 transactions (with and without checksums, with
#    revoke records and an escaped block, and one left uncommitted) into a
#    clean image; e2fsck replays a copy and records the blocks it wrote,
#    and TestReplayForeignJournal checks that GoOS replay writes the same.
set -eu
IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
command -v docker >/dev/null || {
	echo "check-mutations: docker not found; this gate cannot be skipped" >&2
	exit 1
}
(cd "$repo" && GOOS_EXT4_EXPORT="$work" go test -count=1 -run '^(TestExportMutatedImages|TestExportCrashImages)$' ./userspace/ext4)
# A skipped or renamed export test must not pass as a clean check.
images=$(find "$work" -maxdepth 1 -name '*.img' | wc -l | tr -d ' ')
cuts=$(find "$work/crash" -name '*.img' 2>/dev/null | wc -l | tr -d ' ')
[ "$images" -eq 5 ] && [ "$cuts" -eq 6 ] || {
	echo "check-mutations: expected 5 exported and 6 cut images, found $images and $cuts" >&2
	exit 1
}
# The images go in as a tar on standard input, as in make-fixtures.sh.
# DOCKER_FLAGS passes extra docker run options, such as --dns for a VM
# whose resolver containers cannot reach.
# shellcheck disable=SC2086
tar -C "$work" -cf - . | docker run --rm -i ${DOCKER_FLAGS:-} --platform linux/amd64 "$IMAGE" sh -c '
set -eu
apk add -q e2fsprogs e2fsprogs-extra >/dev/null 2>&1
mke2fs -V 2>&1 | grep -q "^mke2fs 1.47.2 " || { echo "unexpected e2fsprogs: $(mke2fs -V 2>&1 | head -1)" >&2; exit 1; }
mkdir /w && cd /w && tar xf -
manifest() {
	(cd "$1" && find . -mindepth 1 | LC_ALL=C sort | while IFS= read -r p; do
		if [ -L "$p" ]; then
			printf "symlink\t%s\t%s\n" "$p" "$(readlink "$p")"
		elif [ -d "$p" ]; then
			printf "dir\t%s\n" "$p"
		else
			printf "file\t%s\t%s\t%s\n" "$p" "$(stat -c %s "$p")" "$(sha256sum <"$p" | cut -d" " -f1)"
		fi
	done)
}
# clean IMAGE: e2fsck -fn reports nothing beyond its pass lines.
clean() {
	if ! e2fsck -fn "$1" >"$1.fsck" 2>&1 ||
		grep -qv -e "^e2fsck " -e "^Pass [1-5]:" -e ": [0-9]*/[0-9]* files" "$1.fsck"; then
		echo "e2fsck reported problems in $1:" >&2
		cat "$1.fsck" >&2
		return 1
	fi
}
# same IMAGE MANIFEST: the tree debugfs dumps matches MANIFEST.
same() {
	rm -rf "$1.tree" && mkdir "$1.tree"
	debugfs -R "rdump / $1.tree" "$1" >/dev/null 2>"$1.debugfs"
	rm -rf "$1.tree/lost+found"
	manifest "$1.tree" >"$1.linux.tsv"
	if ! cmp -s "$1.linux.tsv" "$2"; then
		echo "debugfs sees a different tree in $1:" >&2
		diff "$2" "$1.linux.tsv" | head -20 >&2
		return 1
	fi
}
status=0
checked=0
for img in *.img; do
	name=${img%.img}
	clean "$img" && same "$img" "$name.manifest.tsv" || { status=1; continue; }
	echo "$name: e2fsck clean, $(wc -l <"$name.manifest.tsv") entries match debugfs"
	checked=$((checked + 1))
done
replayed=0
for img in crash/*.img; do
	name=${img%.img}
	# e2fsck -fy replays the GoOS journal and finishes the orphan list;
	# anything else it would fix means GoOS wrote a damaged log or tree.
	# It exits 1 when it changed the image, which replay always does.
	rc=0
	e2fsck -fy "$img" >"$name.replay" 2>&1 || rc=$?
	if [ "$rc" -gt 1 ] ||
		grep -qv -e "^e2fsck " -e "^Pass [1-5]:" -e ": [0-9]*/[0-9]* files" -e ": recovering journal" \
			-e "^$" -e ": \*\*\*\*\* FILE SYSTEM WAS MODIFIED \*\*\*\*\*$" \
			-e "^Clearing orphaned inode [0-9]* (uid=0, gid=0, mode=0[0-7]*, size=[0-9]*)$" \
			-e "^Truncating orphaned inode [0-9]* (uid=0, gid=0, mode=0[0-7]*, size=[0-9]*)$" "$name.replay"; then
		echo "e2fsck replay of $img did more than recover:" >&2
		cat "$name.replay" >&2
		status=1
		continue
	fi
	clean "$img" && same "$img" "$name.manifest.tsv" || { status=1; continue; }
	echo "$name: e2fsck replayed the GoOS journal, tree matches GoOS recovery"
	replayed=$((replayed + 1))
done
[ "$checked" -eq 5 ] && [ "$replayed" -eq 6 ] || status=1
exit $status
'
# Journals written by debugfs. Each variant is a list of debugfs journal
# commands; the blocks they name are free in the base image.
foreign="$work/foreign"
mkdir -p "$foreign"
# shellcheck disable=SC2086
tar -C "$work" -cf - shapes.img | docker run --rm -i ${DOCKER_FLAGS:-} --platform linux/amd64 "$IMAGE" sh -c '
set -eu
apk add -q e2fsprogs e2fsprogs-extra >/dev/null 2>&1
mkdir /w /out && cd /w && tar xf -
range=$(dumpe2fs shapes.img 2>/dev/null | sed -n "s/^  Free blocks: \([0-9]*\)-\([0-9]*\).*/\1 \2/p" | awk "\$2 - \$1 >= 8 { print \$1 }" | tail -1)
[ -n "$range" ] || { echo "no free run in the base image" >&2; exit 1; }
b0=$range b1=$((range + 1)) b2=$((range + 2)) b3=$((range + 3))
for letter in A B C M; do head -c 4096 /dev/zero | tr "\0" "$letter" >"$letter"; done
# A block that starts with the JBD2 magic is escaped in the log.
printf "\300\073\071\230" >magic && head -c 4092 M >>magic
cat A A magic >f1 && cat B B >f2
make_image() {
	name=$1
	shift
	cp shapes.img "/out/$name.img"
	printf "%s\n" "$@" >cmds
	debugfs -w -f cmds "/out/$name.img" >"/out/$name.debugfs" 2>&1
	dumpe2fs -h "/out/$name.img" 2>/dev/null | grep -q needs_recovery || { echo "$name: debugfs left no journal to replay" >&2; exit 1; }
	cp "/out/$name.img" replayed.img
	e2fsck -fy replayed.img >"/out/$name.fsck" 2>&1 || { echo "$name: e2fsck replay failed" >&2; cat "/out/$name.fsck" >&2; exit 1; }
	e2fsck -fn replayed.img >/dev/null 2>&1 || { echo "$name: replayed image not clean" >&2; exit 1; }
	for block in $b0 $b1 $b2 $b3; do
		printf "%s\t%s\n" "$block" "$(dd if=replayed.img bs=4096 skip="$block" count=1 2>/dev/null | sha256sum | cut -d" " -f1)"
	done >"/out/$name.expect"
	rm -f "/out/$name.debugfs"
}
# Two committed transactions; the second revokes b0, so neither copy of it
# is replayed, and rewrites b1.
make_image csum-revoke "jo -c" "jw -b $b0,$b1,$b3 /w/f1" "jw -b $b1,$b2 -r $b0 /w/f2" "jc"
make_image plain-revoke "jo" "jw -b $b0,$b1,$b3 /w/f1" "jw -b $b1,$b2 -r $b0 /w/f2" "jc"
# A committed revoke-only transaction, then one without a commit block.
make_image csum-uncommitted "jo -c" "jw -b $b0,$b1 /w/f1" "jw -r $b0" "jw -b $b2 -c /w/C" "jc"
tar -C /out -cf - .
' | tar -C "$foreign" -xf -
count=$(find "$foreign" -name '*.img' | wc -l | tr -d ' ')
[ "$count" -eq 3 ] || {
	echo "check-mutations: expected 3 debugfs journals, found $count" >&2
	exit 1
}
(cd "$repo" && GOOS_EXT4_IMPORT="$foreign" go test -count=1 -run '^TestReplayForeignJournal$' -v ./userspace/ext4) >"$work/foreign.log" 2>&1 || {
	cat "$work/foreign.log" >&2
	exit 1
}
grep -E "^\s+(---|crash_test|journal_test)" "$work/foreign.log" || true
