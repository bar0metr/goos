#!/bin/sh
# Writes OUT, an empty ext4 volume for filesystem benchmarks: SIZE MiB
# (default 1024) with the supported write profile, a 32 MiB journal and the
# label goos-bench. It uses the same pinned Alpine image and feature list
# as make-fixtures.sh, so GoOS and the Linux reference run on the same
# layout. The volume is sparse on the host.
set -eu
IMAGE=alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
out=${1:?output image required}
size=${2:-1024}
FULL=has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,metadata_csum,64bit
# The container cannot see this checkout through a bind mount on every
# host, so the image comes back gzipped on standard output.
docker run --rm -i ${DOCKER_FLAGS:-} --platform linux/amd64 "$IMAGE" sh -s "$size" "$FULL" >"$out.gz" <<'EOF'
set -eu
apk add -q e2fsprogs >/dev/null 2>&1
mke2fs -V 2>&1 | head -1 | grep -q '^mke2fs 1.47.2 ' || { echo 'unexpected e2fsprogs' >&2; exit 1; }
mke2fs -q -F -t ext4 -b 4096 -I 256 -O "none,$2" -L goos-bench -J size=32 \
	-E lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0 /tmp/bench.img "$1M"
e2fsck -fn /tmp/bench.img >/dev/null
gzip -1 </tmp/bench.img
EOF
gzip -dc "$out.gz" >"$out"
rm -f "$out.gz"
printf 'bench volume %s\n' "$out"
