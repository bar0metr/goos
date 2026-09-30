# Static Linux fixtures

This directory holds the Linux x86_64 programs the system image carries under
`/system/linux`. They are ordinary static ELF executables. Nothing converts
them to the native image format.

| Fixture | Source | Shows |
| --- | --- | --- |
| `hello` | [hello/hello.S](hello/hello.S) | a program with no libc: `write` and `exit_group` |
| `hello-pie` | [hello-pie/hello.S](hello-pie/hello.S) | a static position-independent image: it checks `AT_PHDR` and `AT_ENTRY` against where it runs |
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
and late-answer checks of the boot test; `stack-overflow` recurses into the
guard below the stack; `random` says whether the kernel has a random source;
`exec` replaces the probe image and checks preserved identity, cwd, descriptor
position, `CLOEXEC`, and signal state; `fork` creates a child and checks that
memory is private and the file position is shared; `vfork` checks that
`vfork` returns only after the child exits, that `clone(SIGCHLD)` forks, and,
on GoOS, that `clone` refuses a thread; `orphan` checks that a process whose
parent exits reports PID 1 as its parent.

`/system/linux/raw-probe` is a seventh program with no libc. `tools/initramfs`
assembles it byte by byte, so it has no entry here.

## Pinned inputs

| Input | Version |
| --- | --- |
| Toolchain | TinyGo 0.42.0, with Go 1.25.5 and LLVM 22.1.4 |
| Compiler | the clang inside TinyGo (`tinygo clang`) |
| Linker | the lld inside TinyGo (`tinygo ld.lld`), LLD 22.1.4 |
| libc | musl 1.2.3, the sources TinyGo ships under `lib/musl` |
| Target | `x86_64-unknown-linux-musl`, static, non-PIE; `hello-pie` is linked `-pie` with no dynamic linker |

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
| `prebuilt/hello-pie` | `81d29ff66f43da7eb92f9be2509930b1758fda0e97410e63d0881e29c6a5b150` |
| `prebuilt/musl-hello` | `0219392433258d1b91944fdaf4d8ee5b1bad7d6eb9ad7871bc8cc4efd7efbf5f` |
| `prebuilt/fsprobe` | `7e1b34187f159cbf673d3a95c2ae366aba88f0aae6ad88fd284f51d1e24b3cc6` |
| `prebuilt/dirprobe` | `6d4767fffedf1e8d820fce56ea791aa813721e1552cd93cdedf4c4151e54ddca` |
| `prebuilt/abi-probe` | `2569323fc77d91036f5b42ec7c8001bae6d479ac917ffe4731ec7735467d171c` |

`fixtures.go` embeds the binaries and repeats the checksums. `go test
./userspace/linux` fails when a pair disagrees, and when the kernel ELF
parser refuses a binary. A rebuild that changes bytes updates the binary, the
constant, and this table in one commit.

The musl programs have `PT_PHDR`, `R`, `R-X`, and `RW-` load segments, and a
non-executable `PT_GNU_STACK`; the two with thread-local variables also have
`PT_TLS` and `PT_GNU_RELRO`. `hello` has one `R` and one `R-X` segment.
`hello-pie` is an `ET_DYN` file whose addresses start at zero, with a
`PT_DYNAMIC` header and no `PT_INTERP`; the kernel loads it at `0x40000000`.

The bundled musl has no `rcrt1` startup file, so the recipe builds no musl
static PIE. A copy of `abi-probe` built with Alpine's `gcc -static-pie`
against musl 1.2.5 relocates itself and passes its hostile mode on GoOS; the
boot image takes such a program through `LINUX_EXTRA=path`.

## Syscall trace

[abi-probe/linux.trace](abi-probe/linux.trace) is the sequence `abi-probe one
two` makes on Linux 6.8, from `arch_prctl` to `exit_group`, 392 calls. To
record it again:

```sh
TRACE=1 userspace/linux/run-on-linux.sh
```

`run-on-linux.sh` runs a fixture in an amd64 Alpine container and prints its
output and exit status; `make test-linux-reference` requires status zero. The build host is arm64, so the container runs under
QEMU user-mode emulation. `ptrace` does not work there and `strace` cannot
attach; the trace is QEMU's own call log (`QEMU_STRACE`). Most calls reach
the Linux kernel, which produces the results shown; the emulator answers a
few itself.

## Strict reference

`run-on-linux-kernel.sh KERNEL BUSYBOX` boots a real Linux kernel in
`qemu-system-x86_64` with the probe in an initramfs, so nothing stands
between the program and the kernel:

```sh
make test-linux-kernel-reference KERNEL=/path/to/vmlinuz-virt BUSYBOX=/path/to/busybox
```

The kernel and the static BusyBox that serves as `/init` come from the
caller. The run sets `ABI_PROBE_STRICT`, under which the probe accepts only
the exact Linux result in every check. GoOS is always held to that. The
default and the hostile mode pass strict on Linux 6.12.110, the Alpine 3.22
`virt` kernel, in about three seconds.

Under the container's emulator these answers differ from the kernel's, and
the probe is lenient there only:

| Check | Linux kernel and GoOS | Under user-mode emulation |
| --- | --- | --- |
| transfer across the end of a mapping | the bytes before the end | `EFAULT` |
| `read` or `write` with a length that leaves the user half | `EFAULT` at once | walks the range |
| `nanosleep` with a bad pointer | `EFAULT` | sleeps |
| null buffer to `/dev/null` | the count, or end of file | `EFAULT` |
| handler mask with `SIGKILL` and `SIGSTOP` set | both removed | both kept |
| `statx` with a null path and `AT_EMPTY_PATH` | the descriptor, on Linux from 6.11 | `EFAULT` on the 6.8 host kernel |

The program checks each result itself and prints the same lines on
Linux and on GoOS. Two calls it does not check differ:

| Call | Linux | GoOS |
| --- | --- | --- |
| `mmap(MAP_FIXED, PROT_NONE)` | maps malloc's guard page | `-EINVAL`; musl ignores the result |
| `set_tid_address` | the thread ID | the compatibility PID |

On GoOS, which the program recognizes from the version field of `uname`, a
few more results are checked: a `mkdirat` on the read-only system image must
fail with `EROFS`, a `writev` of 17 elements with `EINVAL`, an `access`
for writing on the image with `EACCES`, and `unlink`, `rmdir`, and `rename`
of a device name with their refusals, which a Linux run as root would carry
out. PID 1 also sets `GOOS_LINUX_LONG` to a 235-byte path, which the program
must open.

GoOS may also answer two checks in a way Linux never does. On a CPU without
RDRAND, `getrandom` returns `ENOSYS` and `AT_RANDOM` is absent; the program
requires the two to agree. A `getrandom` of 300 bytes returns 256 on GoOS
and 300 on Linux.

The `hostile` mode runs on Linux too (`userspace/linux/run-on-linux.sh
abi-probe hostile`, and the strict run above). Two of its checks are left to
GoOS by design: `O_NONBLOCK` at `open` is `EOPNOTSUPP`, and anonymous memory
is never executable.

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
