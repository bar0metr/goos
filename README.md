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
its foundation. Linux programs use a dedicated compatibility subsystem inside the kernel.

GoOS remains a personal research project, so it can reject conventions that
do not fit its design. Major decisions still come with a rationale, an ADR,
tests, and documented consequences.

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
|                                                               |
|   Native call dispatch              Linux ABI translation      |
|                                                               |
|   Processes / tasks / scheduler / address spaces              |
|   Capabilities / handles / resource authority                 |
|   IPC / streams / shared memory / waits                       |
|   interrupts / timers / device authority / DMA                |
+-----------------------------+---------------------------------+
                              |
                         typed IPC
                              |
+-----------------------------v---------------------------------+
|                         USERSPACE                              |
|                                                               |
|   PID 1 / supervision                                         |
|                                                               |
|   VFS ----- FAT32 ----- block ----- virtio-blk                 |
|                                                               |
|   resolved --- netd ---------------- virtio-net                |
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
the `udp` and `tcp` commands do. `dnsq` queries a DNS server directly, and
`fetch` transfers HTTP/1.1 responses through the resolver and TCP sessions.
There is no IPv6 or Linux socket interface.

The kernel supports preemptive SMP on up to 64 processors, per-process address
spaces, typed capability-checked handles, IPC, shared memory, fine-grained
kernel concurrency, CPU affinity, and per-CPU runtime statistics.

Native userspace programs are Go compiled with TinyGo on a GoOS-owned heap
that reclaims memory (ADR 0201). Each process traces and sweeps its
unreachable Go objects, reuses their storage, and returns free heap pages to
the kernel. Interrupt handlers allocate nothing, and the kernel keeps its own
bounded allocation discipline (ADR 0065).

Native processes can also run sibling threads in parallel (ADR 0203). Selected
images use a separate GoOS TinyGo target with an M:N goroutine runtime,
channels, select, sync primitives, timers, preemption, and scheduler-aware
blocking (ADR 0204). The VFS uses that goroutine runtime for its bounded read
pool by default; a serial VFS build remains available as a regression fallback.
The ordinary native target remains available for programs that do not need
goroutines.

GoOS also runs unmodified statically linked Linux x86_64 ELF programs through
the kernel-resident Linux ABI compatibility subsystem. Static musl and glibc
programs, Linux threads,
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
- [Native SSH access](docs/development/remote-access.md)
- [Linux x86_64 compatibility](docs/architecture/linux-x86_64-abi.md)
- [Static Linux application compatibility](docs/development/linux-application-compatibility.md)
- [ADRs](docs/adr/)
- [GoOS release notes](docs/releases/)
- [Development and testing](docs/development/)

Current priorities and future milestones are tracked in the
[roadmap](docs/development/roadmap.md).

## License

Copyright (C) 2026 Nikita Parfenovich (aka bar0metr).

GoOS is licensed under the GNU General Public License, version 3 only
(`GPL-3.0-only`). See [LICENSE](LICENSE) and [NOTICE](NOTICE). Embedded
third-party data retains its own license; see
[font provenance](docs/third-party-font.md).
