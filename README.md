<div align="center">

# GoOS

**An experimental 64-bit operating system for `x86_64`, written mostly in Go and compiled with TinyGo.**

[![License: GPL-3.0-only](https://img.shields.io/badge/license-GPL--3.0--only-blue)](LICENSE)
![Architecture: x86_64](https://img.shields.io/badge/arch-x86__64-lightgrey)
![Go 1.25.5](https://img.shields.io/badge/Go-1.25.5-00ADD8?logo=go&logoColor=white)
![TinyGo 0.42.0](https://img.shields.io/badge/TinyGo-0.42.0-00ADD8)

[Architecture](#architecture) ·
[What works](#what-works-today) ·
[Build and run](#building-and-running) ·
[Documentation](#documentation) ·
[Contributors](#contributors)

</div>

GoOS is not Linux rewritten in another language, and it is not another POSIX
system with a new name. It explores what a small modern operating system looks
like when you build it around explicit authority, isolated components, typed
IPC, and a minimal privileged kernel.

> Compromising one component must not compromise the entire system by default.

## Architecture

GoOS uses a pragmatic microkernel/hybrid design. The diagram below shows the
main structure and privilege boundary, not a mandatory call sequence:

```text
 Native GoOS applications                     Static Linux ELF applications
+--------------------------+                           |
| GoOS native runtime      |                           |
| heap / allocator / GC    |                           |
| threads / M:N goroutines |                           |
+------------+-------------+                           |
             |                                         |
       native int 0x80                           Linux SYSCALL
             |                                         |
+------------v-----------------------------------------v---------+
|                             KERNEL                             |
|                                                                |
|   Native call dispatch              Linux ABI translation      |
|                                                                |
|   Processes / tasks / scheduler / address spaces               |
|   Capabilities / handles / resource authority                  |
|   IPC / streams / shared memory / waits                        |
|   interrupts / timers / device authority / DMA                 |
+-----------------------------+----------------------------------+
                              |
                         typed IPC
                              |
+-----------------------------v---------------------------------+
|                         USERSPACE                             |
|                                                               |
|   PID 1 / supervision                                         |
|                                                               |
|   VFS ----- FAT32 ----- block ----- virtio-blk                |
|                                                               |
|   resolved --- netd ---------------- virtio-net               |
|                                                               |
|   terminal / config / logging / remoted / other services      |
+-----------------------------+---------------------------------+
                              |
                         x86_64 hardware
```

The kernel keeps the mechanisms that need full privilege: CPU state,
interrupts, memory, scheduling, processes and tasks, IPC, streams, shared
memory, waits, handles, capabilities, timers, and device authority such as IRQ
routing and DMA control. Linux x86_64 syscall translation also lives inside
the kernel: the Linux `SYSCALL` entry is distinct from native `int 0x80`,
and translated operations use kernel primitives or capability-scoped IPC with
userspace services as needed.

Native GoOS programs are linked with a GoOS-owned runtime. It provides the
reclaiming heap, allocator, stop-the-world conservative GC, native threads and,
for selected images, an M:N goroutine scheduler. The runtime is part of each
native image rather than a shared privileged service; the kernel schedules
native tasks, while the goroutine runtime multiplexes Gs over those tasks.

Filesystems, terminal services, configuration, logging, networking, and
complex device drivers run as isolated userspace services with only their
granted authority. Storage and networking are composed from supervised
service chains such as VFS -> FAT32 -> block -> `virtio-blk` and
`resolved`/`netd` -> `virtio-net`. UEFI is used during boot, not as a
runtime layer below the kernel.

## What works today

GoOS boots on x86_64 UEFI systems under QEMU and KVM/Proxmox and runs a
capability-oriented native userspace built from isolated services.

| Area | State |
|---|---|
| **Kernel** | Preemptive SMP on up to 64 processors, per-process address spaces, typed capability-checked handles, IPC, shared memory, fine-grained kernel concurrency, CPU affinity, per-CPU runtime statistics |
| **Native runtime** | Go compiled with TinyGo on a GoOS-owned heap that reclaims memory (ADR 0201), sibling threads (ADR 0203), and an M:N goroutine runtime for selected images (ADR 0204) |
| **System** | Persistent FAT32 storage, VFS, supervision, configuration and logging services |
| **User environment** | Native shell and utilities, serial and framebuffer terminals, full-screen tools such as `gfm`, `gview`, and `edit` |
| **Networking** | Userspace IPv4 over virtio-net with DHCP or static settings, ICMP echo, a hosts table and DNS stub resolver, UDP and TCP sessions for granted programs, and `fetch` for HTTP/1.1. There is no IPv6 or Linux socket interface |
| **Remote access** | Native SSH with public-key login, a PTY shell or one command per connection, and SFTP version 3 from a confined per-session service (ADR 0209) |
| **Linux programs** | Unmodified static x86_64 ELF programs through a kernel-resident compatibility subsystem: musl and glibc, threads, `fork`/`execve`, pipes, signals, futexes, polling, and multithreaded workloads such as 7-Zip |

### Memory and concurrency

Each process traces and sweeps its unreachable Go objects, reuses their storage,
and returns free heap pages to the kernel. Interrupt handlers allocate nothing,
and the kernel keeps its own bounded allocation discipline (ADR 0065).

The goroutine target adds channels, select, sync primitives, timers, preemption,
and scheduler-aware blocking. The VFS uses it for its bounded read pool by
default; a serial VFS build remains available as a regression fallback. The
ordinary native target remains available for programs that do not need
goroutines.

Linux compatibility does not make the native ABI POSIX.

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
its foundation. Linux programs use a dedicated compatibility subsystem inside the kernel.

GoOS remains a personal research project, so it can reject conventions that
do not fit its design. Major decisions still come with a rationale, an ADR,
tests, and documented consequences.

## Documentation

| Topic | Where |
|---|---|
| Design | [Architecture](docs/architecture/), [SMP](docs/kernel/smp.md), [Networking](docs/architecture/networking.md) |
| Remote access | [Native SSH and SFTP](docs/development/remote-access.md) |
| Linux programs | [Linux x86_64 ABI](docs/architecture/linux-x86_64-abi.md), [Application compatibility](docs/development/linux-application-compatibility.md) |
| Decisions and history | [ADRs](docs/adr/), [Release notes](docs/releases/) |
| Working on GoOS | [Development and testing](docs/development/), [Roadmap](docs/development/roadmap.md) |

Current priorities and future milestones are tracked in the
[roadmap](docs/development/roadmap.md).

## Contributors

- **Nikita Parfenovich** ([@bar0metr](https://github.com/bar0metr)), creator and maintainer
- **Dmitry Shabalin** ([d_shabalin@g-togl.site](mailto:d_shabalin@g-togl.site))

See [CONTRIBUTORS](CONTRIBUTORS.md).

## License

Copyright (C) 2026 Nikita Parfenovich (aka bar0metr), with contributions from
the people listed in [CONTRIBUTORS](CONTRIBUTORS.md).

GoOS is licensed under the GNU General Public License, version 3 only
(`GPL-3.0-only`). See [LICENSE](LICENSE) and [NOTICE](NOTICE). Embedded
third-party data retains its own license; see
[font provenance](docs/third-party-font.md).
