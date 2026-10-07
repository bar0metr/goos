# GoOS

GoOS is an experimental 64-bit operating system for `x86_64`, written mostly
in Go and compiled with TinyGo. It is not Linux rewritten in another language,
and it is not another POSIX system with a new name. It explores what a small
modern operating system looks like when you build it around explicit
authority, isolated components, typed IPC, and a minimal privileged kernel.

## Project history

I started GoOS in 2011 as a personal research project in Free Pascal, to learn
how a machine starts running custom code, how memory and interrupts work, and
how a kernel should talk to applications. Later I moved it to C++ and worked on
it whenever I wanted to test an idea, rewriting parts as my understanding
changed.

Go gave the project a new direction. The standard Go runtime needs an
operating system underneath it, but TinyGo made a freestanding Go system
practical, and the current GoOS architecture grew from that.

Linux taught me the value of openness, observability, and broad hardware
support; macOS, the value of coherent design and predictable interfaces. I
also wanted to leave behind the constraints decades of compatibility put on
both. Native GoOS does not inherit Unix conventions by default: `fork`, UID 0,
universal `ioctl`, a global root, and "everything is a file" are not part of
its foundation. Linux programs run through a separate compatibility layer.

GoOS remains a personal research project, so it can reject conventions that
do not fit its design. Major decisions still come with a rationale, an ADR,
tests, and documented consequences.

## Architecture

GoOS is a pragmatic microkernel/hybrid:

```text
Native applications             Linux ELF applications
          |                               |
          |                    Linux compatibility layer
          +---------------+---------------+
                          |
                 capabilities + handles
                          |
                       typed IPC
                          |
        +-----------------+-----------------+
        |                 |                 |
       VFS             network          drivers
        |                 |                 |
        +-----------------+-----------------+
                          |
                   minimal kernel
                          |
                   UEFI / x86_64
```

The kernel keeps the mechanisms that need full privilege: CPU state,
interrupts, memory, scheduling, processes, IPC, handles, capabilities, and
timers. Filesystems, the terminal, configuration, logging, networking, and
complex drivers run as isolated userspace services that hold only the
authority they were given.

The central principle:

> Compromising one component must not compromise the entire system by default.

## What works today

GoOS boots on x86_64 UEFI systems under QEMU and KVM/Proxmox and runs a
capability-oriented native userspace built from isolated services. The
installed system includes persistent FAT32 storage, VFS, supervision, a native
shell and utilities, serial and framebuffer terminals, configuration and
logging services, and full-screen tools such as `gfm`, `gview`, and `edit`.
A userspace IPv4 service over a virtio-net driver configures its address
with DHCP or static settings and answers and sends ICMP echo; `ping` and
`net status` use it from the shell. A loopback interface, a hosts table and a
DNS stub resolver let `ping` and `resolve` take host names. Native programs
granted a UDP or TCP session use IPv4 datagrams or bounded byte streams, as
the `udp` and `tcp` commands do. There is no IPv6 or Linux socket interface.

The kernel supports preemptive SMP on up to 64 processors, per-process address
spaces, typed capability-checked handles, IPC, shared memory, fine-grained
kernel concurrency, CPU affinity, and per-CPU runtime statistics.

GoOS also runs unmodified statically linked Linux x86_64 ELF programs through
a separate compatibility layer. Static musl and glibc programs, Linux threads,
`fork`/`execve`, pipes, signals, futexes, polling, and multithreaded workloads
such as 7-Zip work without making the native ABI POSIX.

For the detailed implementation state, see the
[roadmap](docs/development/roadmap.md), [architecture docs](docs/architecture/),
[ADRs](docs/adr/), and [release notes](docs/releases/).

## Building and running

The primary development environment is macOS. The build needs Go 1.25.5,
TinyGo 0.42.0, clang and make from the Xcode Command Line Tools, and
`qemu-system-x86_64` 11.0.1 with OVMF firmware. `tools/toolchain.versions` pins
these versions; `make toolchain-check` compares your tools against it and
prints an install hint for anything missing. It never downloads anything.

```sh
make toolchain-check
make                  # build build/goos-x86_64.iso
make run              # boot in QEMU with the serial console here
make run SMP=4        # boot with four guest CPU cores
make run-gui          # boot with a graphical display
make run RECOVERY=1   # boot into recovery
make run NET=none     # boot without a network device
```

`make run` boots the system installed on `build/goos-disk.img`. It creates the
disk from the build the first time and never changes it afterwards. After you
build a new release, that disk boots into recovery until `make disk-overwrite`
installs the new system on it. `make disk` installs on a new disk and refuses
to touch an existing one. Everything the build produces stays under `build/`.

A normal build boots to the native `goos>` shell after starting the supervised
system services. `/system` contains the installed read-only system, `/home`
contains persistent user data, `/state` contains mutable system state, and
`/bootstrap` contains the boot/recovery environment.

Automated examples of expected boot, namespace, storage, Linux ABI, SMP, and
recovery behavior live in `tests/boot/`. See [building](docs/development/building.md)
and [testing](docs/development/testing.md) for the full set of targets.

## Documentation

- [Architecture](docs/architecture/)
- [Roadmap](docs/development/roadmap.md)
- [SMP design](docs/kernel/smp.md)
- [Networking](docs/architecture/networking.md)
- [Linux x86_64 compatibility](docs/architecture/linux-x86_64-abi.md)
- [ADRs](docs/adr/)
- [Release notes](docs/releases/)
- [Development and testing](docs/development/)

Current priorities and future milestones are tracked in the
[roadmap](docs/development/roadmap.md).

## License

Copyright (C) 2026 Nikita Parfenovich (aka bar0metr).

GoOS is licensed under the GNU General Public License, version 3 only
(`GPL-3.0-only`). See [LICENSE](LICENSE) and [NOTICE](NOTICE). Embedded
third-party data retains its own license; see
[font provenance](docs/third-party-font.md).
