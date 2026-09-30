# Static Linux fixtures

This directory holds the Linux x86_64 programs the system image carries under
`/system/linux`. They are ordinary static ELF executables. Nothing converts
them to the native image format.

| Fixture | Source | Shows |
| --- | --- | --- |
| `hello` | [hello/hello.S](hello/hello.S) | a program with no libc: `write` and `exit_group` |
| `musl-hello` | [musl-hello/main.c](musl-hello/main.c) | libc startup: `argv`, `envp`, TLS, heap, stdio |
| `fsprobe` | [fsprobe/main.c](fsprobe/main.c) | create, write, read, and stat a file |
| `dirprobe` | [dirprobe/main.c](dirprobe/main.c) | list a directory with `getdents64` |
| `abi-probe` | [abi-probe/main.c](abi-probe/main.c) | the conformance suite: every group of calls, checked result by result |

`abi-probe` with two ordinary arguments runs the conformance checks and
prints one line per group. A first argument selects another mode: `hostile`
passes bad pointers, lengths, and descriptors to every call; `exhaust` fills
the descriptor table and the page quota; `isolation` tries the native call
gate; `port-io` touches an I/O port; `pair NAME` runs beside another
instance in a pipeline; `block` and `sleep` wait to be interrupted; `bench`
times four calls; `session-loss` and `session-timeout` serve the VFS restart
and late-answer checks of the boot test.

`/system/linux/raw-probe` is a sixth program with no libc. `tools/initramfs`
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
| `prebuilt/hello` | `9c83f5b9fd8af078d35eff4b2fd852eefff421c25e2cb3eddc14ebea9d37a708` |
| `prebuilt/musl-hello` | `0219392433258d1b91944fdaf4d8ee5b1bad7d6eb9ad7871bc8cc4efd7efbf5f` |
| `prebuilt/fsprobe` | `7e1b34187f159cbf673d3a95c2ae366aba88f0aae6ad88fd284f51d1e24b3cc6` |
| `prebuilt/dirprobe` | `6d4767fffedf1e8d820fce56ea791aa813721e1552cd93cdedf4c4151e54ddca` |
| `prebuilt/abi-probe` | `59a0b7945256146df8c4798fee25e078b75d520cae6dd5ec3cf56faef389e7d1` |

`fixtures.go` embeds the binaries and repeats the checksums. `go test
./userspace/linux` fails when a pair disagrees, and when the kernel ELF
parser refuses a binary. A rebuild that changes bytes updates the binary, the
constant, and this table in one commit.

The musl programs have `PT_PHDR`, `R`, `R-X`, and `RW-` load segments, and a
non-executable `PT_GNU_STACK`; the two with thread-local variables also have
`PT_TLS` and `PT_GNU_RELRO`. `hello` has one `R` and one `R-X` segment.

## Syscall trace

[abi-probe/linux.trace](abi-probe/linux.trace) is the sequence `abi-probe one
two` makes on Linux 6.8, from `arch_prctl` to `exit_group`. To
record it again:

```sh
TRACE=1 userspace/linux/run-on-linux.sh
```

`run-on-linux.sh` runs a fixture in an amd64 Alpine container and prints its
output and exit status; `make test-linux-reference` requires status zero. The build host is arm64, so the container runs under
QEMU user-mode emulation. `ptrace` does not work there and `strace` cannot
attach; the trace is QEMU's own call log (`QEMU_STRACE`). Every call still
reaches the Linux kernel, which produces the results shown.

The program checks each result itself and prints the same lines on
Linux and on GoOS. Two calls it does not check differ:

| Call | Linux | GoOS |
| --- | --- | --- |
| `mmap(MAP_FIXED, PROT_NONE)` | maps malloc's guard page | `-EINVAL`; musl ignores the result |
| `set_tid_address` | the thread ID | the compatibility PID |

On GoOS, which the program recognizes from the version field of `uname`, a
few more results are checked: a `mkdirat` on the read-only system image must
fail with `EROFS`, a `writev` of 17 elements with `EINVAL`, and an `access`
for writing on the image with `EACCES`. PID 1 also sets `GOOS_LINUX_LONG` to
a 235-byte path, which the program must open.

The `hostile` mode runs on Linux too (`userspace/linux/run-on-linux.sh
abi-probe hostile`). Under the emulator of the reference run, a transfer
across the end of a mapping reports `EFAULT` where a Linux kernel reports
the bytes moved, so the program accepts both there and requires the short
count on GoOS. It leaves three checks to GoOS: a transfer of the largest
possible length and a `nanosleep` with a bad pointer, which the emulator
mishandles, and the rule that anonymous memory is never executable.

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
`st_blocks` from the size alone. `ioctl(TIOCGWINSZ)` fails with `ENOTTY` on
both.

## License

The fixture sources in this directory are part of GoOS and carry its license.
`prebuilt/musl-hello` also contains musl code. musl is MIT licensed;
[COPYRIGHT.musl](COPYRIGHT.musl) is the notice from the sources TinyGo ships.
