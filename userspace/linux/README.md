# Static Linux fixtures

This directory holds the Linux x86_64 programs the validation image runs under
`/system/linux`. They are ordinary static ELF executables. Nothing converts
them to the native image format.

| Fixture | Source | Checks |
| --- | --- | --- |
| `musl-hello` | [musl-hello/main.c](musl-hello/main.c) | libc startup, `argv`, `envp`, TLS, heap, stdio, one VFS session exchange |

`/system/linux/raw-probe` is a second fixture with no libc. `tools/initramfs`
assembles it byte by byte, so it has no entry here.

## Pinned inputs

| Input | Version |
| --- | --- |
| Toolchain | TinyGo 0.42.0, with Go 1.25.5 and LLVM 22.1.4 |
| Compiler | the clang inside TinyGo (`tinygo clang`) |
| Linker | the lld inside TinyGo (`tinygo ld.lld`), LLD 22.1.4 |
| libc | the musl sources TinyGo ships under `lib/musl` |
| Target | `x86_64-unknown-linux-musl`, static, non-PIE |

TinyGo ships the musl sources without a `VERSION` file. The headers place them
in the 1.2.3 series: `qsort_r` is declared, `statx` is absent, and
`_GNU_SOURCE` still implies the LFS64 names. The tree itself is pinned by
content. This command prints
`7db2c261d8040935b9ad57e914874560ce76e3cb4759127bef1667daef9c0588` for the
sources the committed binary came from:

```sh
cd "$(tinygo env TINYGOROOT)/lib/musl"
find . -type f | LC_ALL=C sort | xargs shasum -a 256 | shasum -a 256
```

## Build

```sh
make linux-fixtures
```

The target runs `userspace/linux/build.sh`, which needs only TinyGo on `PATH`
and no network. It generates
`bits/alltypes.h` and `bits/syscall.h` the way musl's own build does, compiles
every musl source TinyGo ships plus `crt1.c`, and links the fixture with the
musl objects as a lazy library. Two builds in different directories produce the
same bytes.

Compiler flags:

```text
--target=x86_64-unknown-linux-musl -std=c99 -nostdinc -ffreestanding
-fexcess-precision=standard -frounding-math -fno-strict-aliasing
-fno-pic -fno-pie -fno-stack-protector -fomit-frame-pointer
-fno-unwind-tables -fno-asynchronous-unwind-tables
-ffunction-sections -fdata-sections -O2 -w -Qunused-arguments
-D_XOPEN_SOURCE=700
```

Linker flags:

```text
-static -nostdlib --no-pie --gc-sections --strip-all --build-id=none
-z noexecstack -z max-page-size=4096
```

## Checksums

| File | SHA-256 |
| --- | --- |
| `prebuilt/musl-hello` | `d2d25146c7f6d26f85911e13b078ddb16a4c3d3606362c7c3eba7510f46505fb` |

`fixtures.go` embeds the binary and repeats the checksum. `go test
./userspace/linux` fails when the two disagree, and when the kernel ELF parser
refuses the binary. A rebuild that changes the bytes updates the binary, the
constant, and this table in one commit.

The image has four `PT_LOAD` segments (`R`, `R-X`, and two `RW-`), `PT_PHDR`,
`PT_TLS`, `PT_GNU_RELRO`, and a non-executable `PT_GNU_STACK`. Its entry is
`0x202110`.

## Syscall trace

The validation kernel recorded this sequence for
`/system/linux/musl-hello one two` with `GOOS_LINUX_PROBE=musl`:

| Call | Result | Made by |
| --- | --- | --- |
| `arch_prctl(ARCH_SET_FS)` | 0 | `__init_tp`, installing the thread pointer |
| `set_tid_address` | thread ID | `__init_tp` |
| `ioctl(1, TIOCGWINSZ)` | `-ENOSYS` | first stdout write, asking whether fd 1 is a terminal |
| `writev(1)` | bytes written | first line; stdout is fully buffered afterwards |
| `brk(0)`, `brk(+2 pages)` | new break | malloc metadata area |
| `mmap(MAP_FIXED, PROT_NONE)` | `-EINVAL` | guard page under the metadata area; musl ignores the result |
| `mmap` twice | addresses | the 64-byte and the 1 MiB allocation |
| `munmap` twice | 0 | `free` |
| `openat`, `close`, `close` | 3, 0, `-EBADF` | session check, run twice |
| `openat` of a missing path | `-ENOENT` | session check |
| `writev(1)` | bytes written | stdio flush in `exit` |
| `exit_group` | | `_Exit` |

This trace comes from GoOS. Nobody has yet run the same binary under `strace`
on Linux; the build host has no Linux kernel to run it on. To compare, copy
`prebuilt/musl-hello` to an x86_64 Linux machine and run:

```sh
GOOS_LINUX_PROBE=musl strace -f ./musl-hello one two
```

On Linux the `ioctl` returns `-ENOTTY` for a pipe and the fixed `mmap`
succeeds. The session check opens `argv[0]`, so it needs the binary at an
absolute path.

## License

The fixture sources in this directory are part of GoOS and carry its license.
`prebuilt/musl-hello` also contains musl code. musl is MIT licensed;
[COPYRIGHT.musl](COPYRIGHT.musl) is the notice from the sources TinyGo ships.
