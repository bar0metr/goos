#!/bin/sh
# Builds the ISO twice from a clean build tree and compares every artifact.
#
# The TinyGo targets name build/kernel/io.o and build/userspace/runtime.o, so
# both builds run in the ordinary build/ directory: the first build's outputs
# are copied aside, build/ is cleaned again, and the second build is compared
# against the copy. The Go and TinyGo caches live under build/cache and are
# removed before each build, so neither build reuses the other's objects.
set -eu

make=${MAKE:-make}
build=build
work=$build/reproducible
first=$work/first
second=$work/second
# Outputs the reproducibility check leaves alone: its own tree, the
# interactive disk, and logs and reports of other targets.
keep='reproducible virtio-blk.img boot-test-virtio-blk.img latency-virtio-blk.img logs reports'

clean_build() {
	[ -d "$build" ] || return 0
	for entry in "$build"/* "$build"/.[!.]*; do
		[ -e "$entry" ] || continue
		name=${entry#"$build"/}
		case " $keep " in
		*" $name "*) ;;
		*) rm -rf "$entry" ;;
		esac
	done
}

# collect copies the build outputs, without the caches, into $1.
collect() {
	mkdir -p "$1"
	for entry in "$build"/*; do
		name=${entry#"$build"/}
		case " $keep cache " in
		*" $name "*) ;;
		*) cp -R "$entry" "$1/" ;;
		esac
	done
}

build_once() {
	printf 'reproducible: %s build from a clean tree\n' "$1"
	clean_build
	if ! "$make" --no-print-directory iso >"$work/$1.log" 2>&1; then
		cat "$work/$1.log" >&2
		printf 'reproducible: %s build failed\n' "$1" >&2
		exit 1
	fi
	collect "$2"
}

rm -rf "$work"
mkdir -p "$work"
build_once first "$first"
build_once second "$second"

(cd "$first" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) >"$work/first.files"
(cd "$second" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) >"$work/second.files"

status=0
if ! cmp -s "$work/first.files" "$work/second.files"; then
	printf '[DIFF] the builds produced different file sets:\n' >&2
	diff "$work/first.files" "$work/second.files" >&2 || true
	status=1
fi

# Hash every artifact both builds produced; name each one that differs.
same=0
different=0
while IFS= read -r file; do
	[ -f "$second/$file" ] || continue
	a=$(shasum -a 256 "$first/$file" | cut -d' ' -f1)
	b=$(shasum -a 256 "$second/$file" | cut -d' ' -f1)
	if [ "$a" = "$b" ]; then
		same=$((same + 1))
	else
		printf '[DIFF] %s\n       first  %s\n       second %s\n' "$file" "$a" "$b" >&2
		different=$((different + 1))
		status=1
	fi
done <"$work/first.files"

# The key artifacts are named even when they match, so the report shows what
# the check covered.
config=$(cat "$first/config/build" 2>/dev/null || true)
set -- goos-x86_64.iso
for variant in "$first"/variants/*; do
	v=variants/${variant##*/}
	set -- "$@" "$v/goos-x86_64.iso" "$v/iso/efi.img" "$v/kernel/goos.efi" "$v/initramfs/initramfs.tar" "$v/userspace/init" "$v/userspace/shell"
done
for file in "$@"; do
	if [ -f "$first/$file" ] && [ -f "$second/$file" ] && cmp -s "$first/$file" "$second/$file"; then
		printf '[ OK ] %s %s\n' "$(shasum -a 256 "$first/$file" | cut -d' ' -f1)" "$file"
	fi
done

printf 'reproducible: %d artifacts identical, %d different (HZ DIAG VALIDATION OPT variant: %s)\n' "$same" "$different" "$config"
if [ "$status" -ne 0 ]; then
	printf 'reproducible: FAIL, the two clean builds differ; both trees are kept in %s\n' "$work" >&2
	exit 1
fi
rm -rf "$first" "$second"
printf 'reproducible: PASS, two clean builds are byte-identical\n'
