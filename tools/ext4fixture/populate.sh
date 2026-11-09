#!/bin/sh
# Runs inside the pinned Alpine container (see make-fixtures.sh). Builds the
# ext4 fixture images with explicit mke2fs feature lists, checks each with
# e2fsck, and writes a tar of the gzipped images, their dumpe2fs headers,
# e2fsck reports and content manifests to standard output.
set -eu
apk add -q e2fsprogs e2fsprogs-extra >/dev/null 2>&1
export E2FSPROGS_FAKE_TIME=1767225600 # 2026-01-01 00:00:00 UTC
UUID=6f6f7367-6578-7434-0000-000000000001
SEED=6f6f7367-6578-7434-0000-0000000000aa
STAMP=202601010000.00
out=/out
mkdir -p "$out"

# The main tree: every object kind the reader handles.
mkdir -p /src/main && cd /src/main
printf 'hello ext4\n' >hello.txt
: >empty
mkdir -p dir/sub big names
printf 'deep file\n' >dir/sub/deep.txt
i=0
while [ $i -lt 2000 ]; do
	printf 'entry %d\n' $i >big/file-$i
	i=$((i + 1))
done
printf 'invalid utf-8 name\n' >"names/$(printf 'bad-\377\376')"
printf 'mixed case\n' >names/MiXeD
ln hello.txt link-to-hello
ln -s hello.txt sym-fast
long=$(printf 'dir/sub/%0200d' 0)
ln -s "$long" sym-slow
# Data at 0, a hole, data at 1 MiB, and one block past 5 GiB.
printf 'A%.0s' $(seq 1 4096) >sparse
printf 'B' | dd of=sparse bs=1 seek=1048576 conv=notrunc 2>/dev/null
printf 'C' | dd of=sparse bs=1 seek=5368709120 conv=notrunc 2>/dev/null
# 400 single-block extents separated by holes: deeper than the four extents
# an inode holds.
i=0
while [ $i -lt 400 ]; do
	printf 'block %04d' $i | dd of=fragmented bs=4096 seek=$((i * 2)) conv=notrunc 2>/dev/null
	i=$((i + 1))
done
i=0
while [ $i -lt 600 ]; do
	printf '%d' $i | sha256sum
	i=$((i + 1))
done >digests
find . -exec touch -h -t $STAMP {} +

# The hash tree: one indexed directory whose names include bytes above
# 0x7f, where the signed and unsigned hash variants differ.
mkdir -p /src/hash/names && cd /src/hash/names
i=0
while [ $i -lt 600 ]; do
	printf '%d\n' $i >"name-$i"
	printf '%d\n' $i >"$(printf 'h\351\377-%d' $i)"
	i=$((i + 1))
done
cd /src/hash && find . -exec touch -h -t $STAMP {} +

# The block-mapped tree: a file reaching the double-indirect blocks of a
# 1 KiB-block volume (12 direct + 256 single-indirect blocks before them).
mkdir -p /src/blockmap/names && cd /src/blockmap
printf 'hello ext3\n' >hello.txt
ln -s "$(printf 'target/%0100d' 0)" sym-slow
i=0
while [ $i -lt 600 ]; do
	printf 'block %04d' $i | dd of=indirect bs=1024 seek=$i conv=notrunc 2>/dev/null
	i=$((i + 1))
done
printf 'tail' | dd of=indirect bs=1024 seek=70000 conv=notrunc 2>/dev/null
i=0
while [ $i -lt 300 ]; do
	printf '%d\n' $i >names/entry-$i
	i=$((i + 1))
done
find . -exec touch -h -t $STAMP {} +

manifest() {
	(cd "$1" && find . -mindepth 1 | LC_ALL=C sort | while IFS= read -r p; do
		if [ -L "$p" ]; then
			printf 'symlink\t%s\t%s\n' "$p" "$(readlink "$p")"
		elif [ -d "$p" ]; then
			printf 'dir\t%s\n' "$p"
		else
			printf 'file\t%s\t%s\t%s\n' "$p" "$(stat -c %s "$p")" "$(sha256sum <"$p" | cut -d' ' -f1)"
		fi
	done)
}

# build NAME SOURCE FEATURES BLOCKSIZE BLOCKS-PER-GROUP BLOCKS JOURNAL-MIB
build() {
	name=$1 source=$2 features=$3 blocksize=$4 groupblocks=$5 count=$6 journal=$7
	img=/tmp/$name.img
	rm -f "$img"
	mke2fs -q -F -t ext4 -b "$blocksize" -g "$groupblocks" -I 256 -N 4096 \
		-O "none,$features" -U "$UUID" -L "$name" -J size="$journal" \
		-E "lazy_itable_init=0,lazy_journal_init=0,hash_seed=$SEED,root_owner=0:0" \
		-d "$source" "$img" "$count"
}

# finish NAME: rebuild directory indexes, check, and export.
finish() {
	name=$1
	img=/tmp/$name.img
	e2fsck -fyD "$img" >/dev/null 2>&1 || [ $? -le 1 ]
	e2fsck -fn "$img" >"$out/$name.fsck" 2>&1
	dumpe2fs -h "$img" >"$out/$name.dumpe2fs" 2>/dev/null
	gzip -9n <"$img" >"$out/$name.img.gz"
}

FULL=has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,metadata_csum,64bit
LEGACY=has_journal,ext_attr,resize_inode,dir_index,filetype,extent,flex_bg,sparse_super,large_file,huge_file,dir_nlink,extra_isize,uninit_bg
EXT3=has_journal,ext_attr,resize_inode,dir_index,filetype,sparse_super,large_file

for name in canonical legacy; do
	features=$FULL
	[ $name = legacy ] && features=$LEGACY
	build $name /src/main "$features" 4096 4096 16384 4
	debugfs -w -R 'ea_set /hello.txt user.small inline-value' /tmp/$name.img >/dev/null 2>&1
	debugfs -w -R "ea_set /hello.txt user.large $(printf 'x%.0s' $(seq 1 600))" /tmp/$name.img >/dev/null 2>&1
	debugfs -w -R 'fallocate /empty 0 7' /tmp/$name.img >/dev/null 2>&1
	finish $name
done
for hash in tea legacy; do
	build hash-$hash /src/hash "$FULL" 4096 4096 8192 4
	tune2fs -E hash_alg=$hash /tmp/hash-$hash.img >/dev/null
	finish hash-$hash
done
build blockmap-1k /src/blockmap "$EXT3" 1024 2048 16384 1
finish blockmap-1k

manifest /src/main >"$out/manifest.tsv"
manifest /src/hash >"$out/hash.manifest.tsv"
manifest /src/blockmap >"$out/blockmap.manifest.tsv"
mke2fs -V 2>&1 | head -1 >"$out/mke2fs-version"
cat /etc/alpine-release >"$out/alpine-release"
cd "$out" && tar cf - .
