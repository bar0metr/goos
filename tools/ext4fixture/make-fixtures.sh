#!/bin/sh
# Regenerates the ext4 test images in userspace/ext4/testdata with the
# pinned e2fsprogs build (docs/development/ext4-reference-versions.md).
#
# mke2fs -d stamps inode change times and the superblock with the real
# clock, so a regenerated image differs from the committed one in those
# fields. Commit regenerated images together with the new SHA256SUMS.
set -eu
IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
here=$(cd "$(dirname "$0")" && pwd)
dest=${1:-$here/../../userspace/ext4/testdata}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The container cannot see this checkout through a bind mount on every
# host, so the script goes in on standard input and the results come back
# as a tar on standard output.
docker run --rm -i --platform linux/amd64 "$IMAGE" sh -s <"$here/populate.sh" >"$work/out.tar"
tar xf "$work/out.tar" -C "$work"
[ -s "$work/mke2fs-version" ] || {
	echo "fixture build failed" >&2
	exit 1
}
for report in "$work"/*.fsck; do
	if grep -qv -e '^e2fsck ' -e '^Pass [1-5]:' -e ': [0-9]*/[0-9]* files' "$report"; then
		echo "e2fsck reported problems in $report:" >&2
		cat "$report" >&2
		exit 1
	fi
done
grep -q '^mke2fs 1.47.2 ' "$work/mke2fs-version" || {
	echo "unexpected e2fsprogs: $(cat "$work/mke2fs-version")" >&2
	exit 1
}
mkdir -p "$dest"
cp "$work"/*.img.gz "$work"/*.dumpe2fs "$work"/*manifest.tsv "$dest"/
(cd "$dest" && shasum -a 256 *.img.gz >SHA256SUMS)
echo "fixtures written to $dest"
