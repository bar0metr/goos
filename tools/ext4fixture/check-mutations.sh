#!/bin/sh
# Checks volumes written by the ext4 mutation code with the pinned
# e2fsprogs build (docs/development/ext4-reference-versions.md). The Go test
# TestExportMutatedImages writes each image with a manifest of what the
# GoOS reader sees; the container runs e2fsck -fn on every image, dumps its
# tree with debugfs, and compares that tree with the manifest.
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
(cd "$repo" && GOOS_EXT4_EXPORT="$work" go test -count=1 -run '^TestExportMutatedImages$' ./userspace/ext4)
# A skipped or renamed export test must not pass as a clean check.
images=$(find "$work" -name '*.img' | wc -l | tr -d ' ')
[ "$images" -eq 4 ] || {
	echo "check-mutations: expected 4 exported images, found $images" >&2
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
status=0
checked=0
for img in *.img; do
	name=${img%.img}
	if ! e2fsck -fn "$img" >"$name.fsck" 2>&1 ||
		grep -qv -e "^e2fsck " -e "^Pass [1-5]:" -e ": [0-9]*/[0-9]* files" "$name.fsck"; then
		echo "e2fsck reported problems in $name:" >&2
		cat "$name.fsck" >&2
		status=1
		continue
	fi
	mkdir "$name.tree"
	debugfs -R "rdump / $name.tree" "$img" >/dev/null 2>"$name.debugfs"
	rm -rf "$name.tree/lost+found"
	manifest "$name.tree" >"$name.linux.tsv"
	if ! cmp -s "$name.linux.tsv" "$name.manifest.tsv"; then
		echo "debugfs sees a different tree in $name:" >&2
		diff "$name.manifest.tsv" "$name.linux.tsv" | head -20 >&2
		status=1
		continue
	fi
	echo "$name: e2fsck clean, $(wc -l <"$name.manifest.tsv") entries match debugfs"
	checked=$((checked + 1))
done
[ "$checked" -eq 4 ] || status=1
exit $status
'
