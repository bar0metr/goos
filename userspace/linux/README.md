# Static Linux fixtures

This directory holds the Linux x86_64 programs the validation image runs under
`/system/linux`. They are ordinary static ELF executables. Nothing converts
them to the native image format.

| Fixture | Source | Checks |
| --- | --- | --- |
| `musl-hello` | [musl-hello/main.c](musl-hello/main.c) | libc startup, `argv`, `envp`, TLS, heap, stdio, VFS session exchanges, path resolution, file I/O, file status, directory listing, vector I/O, naming, identity, clocks, sleep |

`/system/linux/raw-probe` is a second fixture with no libc. `tools/initramfs`
assembles it byte by byte, so it has no entry here.

## Pinned inputs

| Input | Version |
| --- | --- |
| Toolchain | TinyGo 0.42.0, with Go 1.25.5 and LLVM 22.1.4 |
| Compiler | the clang inside TinyGo (`tinygo clang`) |
| Linker | the lld inside TinyGo (`tinygo ld.lld`), LLD 22.1.4 |
| libc | musl 1.2.3, the sources TinyGo ships under `lib/musl` |
| Target | `x86_64-unknown-linux-musl`, static, non-PIE |

TinyGo 0.42.0 pins `lib/musl` at commit
`7a43f6fea9081bdd53d8a11cef9e9fab0348c53d` of `tinygo-org/musl-libc`, the
upstream commit "release 1.2.3". The installed tree has no `VERSION` file, so
the sources are also pinned by content. This command prints
`7db2c261d8040935b9ad57e914874560ce76e3cb4759127bef1667daef9c0588` for the
tree the committed binary came from:

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
| `prebuilt/musl-hello` | `0aaad813f067c758fee26c4ba20603eb37e17757fb6ef8df01f644552f5a4780` |

`fixtures.go` embeds the binary and repeats the checksum. `go test
./userspace/linux` fails when the two disagree, and when the kernel ELF parser
refuses the binary. A rebuild that changes the bytes updates the binary, the
constant, and this table in one commit.

The image has four `PT_LOAD` segments (`R`, `R-X`, and two `RW-`), `PT_PHDR`,
`PT_TLS`, `PT_GNU_RELRO`, and a non-executable `PT_GNU_STACK`. Its entry is
`0x2024b0`.

## Syscall trace

[musl-hello/linux.trace](musl-hello/linux.trace) is the sequence the committed
binary makes on Linux 6.8, 185 calls from `arch_prctl` to `exit_group`. To
record it again:

```sh
TRACE=1 userspace/linux/run-on-linux.sh
```

`run-on-linux.sh` runs a fixture in an amd64 Alpine container and prints its
output and exit status; `make test-linux-reference` requires status zero. The build host is arm64, so the container runs under
QEMU user-mode emulation. `ptrace` does not work there and `strace` cannot
attach; the trace is QEMU's own call log (`QEMU_STRACE`). Every call still
reaches the Linux kernel, which produces the results shown.

The program checks each result itself and prints the same thirteen lines on
Linux and on GoOS. Three calls it does not check differ:

| Call | Linux | GoOS |
| --- | --- | --- |
| `ioctl(1, TIOCGWINSZ)` | `-ENOTTY` for a pipe | `-ENOSYS`; musl reads both as "no terminal" |
| `mmap(MAP_FIXED, PROT_NONE)` | maps malloc's guard page | `-EINVAL`; musl ignores the result |
| `set_tid_address` | the thread ID | the compatibility PID |

On GoOS, PID 1 also sets `GOOS_LINUX_LONG`. It adds one `openat` of a 235-byte
path, one `mkdirat` on the read-only system image that must fail with `EROFS`,
and one `writev` of 17 elements that must fail with `EINVAL`.

Results the program reads without comparing also differ: Linux lists `.` and
`..` in a directory and GoOS does not; the identity calls return the
container's values on Linux and the compatibility PID, parent 1, and user 1000
on GoOS; `RLIMIT_NOFILE` is 64 on GoOS.

What the startup calls are for:

| Call | Made by |
| --- | --- |
| `arch_prctl(ARCH_SET_FS)`, `set_tid_address` | `__init_tp`, installing the thread pointer |
| `ioctl(1, TIOCGWINSZ)` | the first stdout write, asking whether fd 1 is a terminal |
| first `writev(1)` | the first line; stdout is fully buffered afterwards |
| `brk(0)`, `brk(+2 pages)`, fixed `mmap` | malloc's metadata area and its guard page |
| two `mmap`, two `munmap` | the 64-byte and the 1 MiB allocation, and `free` |
| last `writev(1)`, `exit_group` | the stdio flush in `exit`, then `_Exit` |

musl reaches the kernel through the legacy numbers where x86_64 has them:
`open`, `stat`, `lstat`, `rename`, `rmdir`, `unlink`, `chdir`, and `getcwd`.
The musl sources TinyGo ships leave out the directories that hold `mkdir`,
the stat wrappers, and `readdir`, so the fixture makes those calls through
`syscall()` with the numbers musl's wrappers use.

Fields of `struct stat` differ between the two systems where the fixture does
not compare them: GoOS reports owner 1000, link count 1 for a directory, and
`st_blocks` from the size alone. `ioctl(TIOCGWINSZ)` on the file stdio opens
fails on both.

## License

The fixture sources in this directory are part of GoOS and carry its license.
`prebuilt/musl-hello` also contains musl code. musl is MIT licensed;
[COPYRIGHT.musl](COPYRIGHT.musl) is the notice from the sources TinyGo ships.
