#!/bin/sh
# Checks the partitions tests/boot/ext4-rw.sh copied out of a GoOS guest with
# the pinned e2fsprogs build (docs/development/ext4-reference-versions.md).
# clean.img was unmounted by GoOS: e2fsck -fn must report nothing. dirty.img
# was cut off while mounted: e2fsck -fy must only replay the GoOS journal and
# finish the orphan list, and a second e2fsck -fn must report nothing. On
# both, debugfs must read the files the guest wrote.
set -eu
IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
dir=${1:?directory with clean.img and dirty.img required}
[ -f "$dir/clean.img" ] && [ -f "$dir/dirty.img" ] || {
	echo "check-guest-images: $dir lacks clean.img or dirty.img" >&2
	exit 1
}
command -v docker >/dev/null || {
	echo "check-guest-images: docker not found; this gate cannot be skipped" >&2
	exit 1
}
# shellcheck disable=SC2086
tar -C "$dir" -cf - clean.img dirty.img | docker run --rm -i ${DOCKER_FLAGS:-} --platform linux/amd64 "$IMAGE" sh -c '
set -eu
apk add -q e2fsprogs e2fsprogs-extra >/dev/null 2>&1
mke2fs -V 2>&1 | grep -q "^mke2fs 1.47.2 " || { echo "unexpected e2fsprogs: $(mke2fs -V 2>&1 | head -1)" >&2; exit 1; }
mkdir /w && cd /w && tar xf -
clean() {
	if ! e2fsck -fn "$1" >"$1.fsck" 2>&1 ||
		grep -qv -e "^e2fsck " -e "^Pass [1-5]:" -e ": [0-9]*/[0-9]* files" "$1.fsck"; then
		echo "e2fsck reported problems in $1:" >&2
		cat "$1.fsck" >&2
		exit 1
	fi
}
holds() {
	got=$(debugfs -R "cat $2" "$1" 2>/dev/null)
	[ "$got" = "$3" ] || { echo "$1: $2 holds \"$got\", want \"$3\"" >&2; exit 1; }
}
clean clean.img
holds clean.img /moved.txt payload
debugfs -R "stat /hello.txt" clean.img 2>&1 | grep -q "File not found" || { echo "clean.img: /hello.txt survived its unlink" >&2; exit 1; }
dumpe2fs -h dirty.img 2>/dev/null | grep -q needs_recovery || { echo "dirty.img: no journal to recover" >&2; exit 1; }
rc=0
e2fsck -fy dirty.img >dirty.replay 2>&1 || rc=$?
if [ "$rc" -gt 1 ] ||
	grep -qv -e "^e2fsck " -e "^Pass [1-5]:" -e ": [0-9]*/[0-9]* files" -e ": recovering journal" \
		-e "^$" -e ": \*\*\*\*\* FILE SYSTEM WAS MODIFIED \*\*\*\*\*$" \
		-e "^Clearing orphaned inode [0-9]* (uid=0, gid=0, mode=0[0-7]*, size=[0-9]*)$" \
		-e "^Truncating orphaned inode [0-9]* (uid=0, gid=0, mode=0[0-7]*, size=[0-9]*)$" dirty.replay; then
	echo "e2fsck replay of dirty.img did more than recover:" >&2
	cat dirty.replay >&2
	exit 1
fi
clean dirty.img
holds dirty.img /journal.txt before-fault
holds dirty.img /second.txt after-fault
echo "guest images: clean.img clean, dirty.img replayed by e2fsck, contents match"
'
