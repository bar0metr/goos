#!/bin/sh
# Rebuilds the static Linux fixtures from their assembly and C sources.
#
# The compiler, the linker, and the musl sources all come from one TinyGo
# release, so the recipe needs no network access and no separate
# cross-toolchain. README.md records the pinned versions and checksums.
#
# usage: userspace/linux/build.sh [output-directory]
set -eu

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/prebuilt}
work=${WORK:-$(mktemp -d)}
jobs=${JOBS:-8}

root=$(tinygo env TINYGOROOT)
musl=$root/lib/musl
[ -d "$musl/src" ] || { echo "no musl sources under $musl" >&2; exit 1; }

gen=$work/include
obj=$work/obj
mkdir -p "$gen/bits" "$obj" "$out"

# musl generates these two headers at build time. TinyGo ships the inputs and
# leaves out musl's tools directory, so the alltypes rewrite is repeated here.
cat > "$work/alltypes.sed" <<'EOF'
/^TYPEDEF/s/TYPEDEF \(.*\) \([^ ]*\);$/#if defined(__NEED_\2) \&\& !defined(__DEFINED_\2)\
typedef \1 \2;\
#define __DEFINED_\2\
#endif\
/
/^STRUCT/s/STRUCT * \([^ ]*\) \(.*\);$/#if defined(__NEED_struct_\1) \&\& !defined(__DEFINED_struct_\1)\
struct \1 \2;\
#define __DEFINED_struct_\1\
#endif\
/
/^UNION/s/UNION * \([^ ]*\) \(.*\);$/#if defined(__NEED_union_\1) \&\& !defined(__DEFINED_union_\1)\
union \1 \2;\
#define __DEFINED_union_\1\
#endif\
/
EOF
sed -f "$work/alltypes.sed" "$musl/arch/x86_64/bits/alltypes.h.in" "$musl/include/alltypes.h.in" > "$gen/bits/alltypes.h"
cp "$musl/arch/x86_64/bits/syscall.h.in" "$gen/bits/syscall.h"
sed -n -e 's/__NR_/SYS_/p' "$musl/arch/x86_64/bits/syscall.h.in" >> "$gen/bits/syscall.h"

cflags="--target=x86_64-unknown-linux-musl -std=c99 -nostdinc -ffreestanding \
-fexcess-precision=standard -frounding-math -fno-strict-aliasing \
-fno-pic -fno-pie -fno-stack-protector -fomit-frame-pointer \
-fno-unwind-tables -fno-asynchronous-unwind-tables \
-ffunction-sections -fdata-sections -O2 -w -Qunused-arguments \
-D_XOPEN_SOURCE=700 \
-I$musl/arch/x86_64 -I$musl/arch/generic -I$musl/src/include \
-I$musl/src/internal -I$gen -I$musl/include"

# An x86_64 file replaces the portable file of the same name, as in musl's own
# build. version.c needs a header only a musl release tree generates, and only
# the dynamic linker uses it.
sources=$work/sources
: > "$sources"
for file in $(cd "$musl" && ls src/*/*.c src/malloc/mallocng/*.c | LC_ALL=C sort); do
	base=${file##*/}
	stem=${base%.c}
	dir=${file%/*}
	[ "$file" = src/internal/version.c ] && continue
	[ -e "$musl/$dir/x86_64/$stem.c" ] || [ -e "$musl/$dir/x86_64/$stem.s" ] || [ -e "$musl/$dir/x86_64/$stem.S" ] && continue
	echo "$file" >> "$sources"
done
(cd "$musl" && ls src/*/x86_64/*.c src/*/x86_64/*.s src/*/x86_64/*.S 2>/dev/null | LC_ALL=C sort) >> "$sources"

cat > "$work/compile.sh" <<EOF
#!/bin/sh
exec tinygo clang $cflags -c "$musl/\$1" -o "$obj/\$(echo "\$1" | tr / _).o"
EOF
xargs -P "$jobs" -n 1 sh "$work/compile.sh" < "$sources"
# shellcheck disable=SC2086
tinygo clang $cflags -DCRT -c "$musl/crt/crt1.c" -o "$work/crt1.o"

library=$(sed -e 's|/|_|g' -e "s|^|$obj/|" -e 's|$|.o|' "$sources")

# hello has no libc: one assembly file and the linker.
tinygo clang --target=x86_64-unknown-linux-musl -c "$here/hello/hello.S" -o "$work/hello.o"
tinygo ld.lld -static -nostdlib --no-pie --strip-all --build-id=none \
	-z noexecstack -z max-page-size=4096 -e _start -o "$out/hello" "$work/hello.o"

# hello-pie is the same kind of program linked position-independent: an
# ET_DYN image with no interpreter, which the kernel loads at a base it
# chooses.
tinygo clang --target=x86_64-unknown-linux-musl -c "$here/hello-pie/hello.S" -o "$work/hello-pie.o"
tinygo ld.lld -static -nostdlib -pie --no-dynamic-linker --strip-all --build-id=none \
	-z noexecstack -z max-page-size=4096 -e _start -o "$out/hello-pie" "$work/hello-pie.o"

for fixture in musl-hello fsprobe dirprobe abi-probe tty-probe; do
	# shellcheck disable=SC2086
	tinygo clang $cflags -c "$here/$fixture/main.c" -o "$work/$fixture.o"
	# shellcheck disable=SC2086
	tinygo ld.lld -static -nostdlib --no-pie --gc-sections --strip-all \
		--build-id=none -z noexecstack -z max-page-size=4096 \
		-o "$out/$fixture" "$work/crt1.o" "$work/$fixture.o" \
		--start-lib $library --end-lib
done

(cd "$out" && shasum -a 256 hello hello-pie musl-hello fsprobe dirprobe abi-probe tty-probe)
