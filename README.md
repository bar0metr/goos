# GoOS

GoOS is an experimental 64-bit operating system for `x86_64`, with its primary
codebase written in Go and compiled with TinyGo. It is not an attempt to rewrite
Linux in another language, nor is it another POSIX system with a new name. The
project explores what a small modern operating system could look like when built
around explicit authority, component isolation, typed IPC, and a minimal
privileged kernel.

## Project history

I started GoOS in 2011 as a personal operating-system research project written in Free Pascal. I used it to study how a machine starts executing custom code, how memory and interrupts work, and how a kernel should interact with applications.

I later moved the project to C++. There was no release schedule or fixed roadmap. I worked on it when I wanted to test an idea or study a part of operating-system development in more detail. Some experiments led to new code; others made me reconsider the architecture and rewrite existing parts.

Go gave the project a new direction. I wanted to see whether I could use it for a complete operating system rather than a small teaching kernel. The standard Go runtime depends on services provided by an existing OS, but TinyGo made a freestanding Go system practical. I used that foundation to develop the current GoOS architecture.

My experience with Linux and macOS shaped the design. From Linux, I took openness, observability, hardware support, and the ability to run across different environments. From macOS, I took an appreciation for coherent system design, predictable behavior, and consistent interfaces.

I also wanted to avoid the constraints that decades of compatibility impose on both systems. GoOS uses explicit ownership, structured protocols, and narrow authority boundaries. Its native API has its own process, security, and resource model. Unix concepts such as `fork`, UID 0, universal `ioctl`, a global root, and “everything is a file” are not part of that foundation. Linux compatibility belongs in a separate layer.

I design GoOS around a small privileged kernel, capability-based access to resources, userspace services and drivers, versioned IPC protocols, and dedicated fast paths for bulk data. Processes receive only the authority granted to them. Services communicate through defined interfaces instead of shared global state.

GoOS remains a personal research project, so I can reject conventions that do not fit its design. I still expect major architectural decisions to have a testable rationale, an ADR, relevant tests, and documented consequences.

## What is being built

The target architecture is a pragmatic microkernel/hybrid:

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
       VFS              network           drivers
        |                 |                 |
        +-----------------+-----------------+
                          |
                   minimal kernel
                          |
                   UEFI / x86_64 / QEMU
```

The kernel owns only mechanisms that genuinely require maximum privilege: CPU
state, interrupts, physical and virtual memory, scheduling, processes, IPC,
handles, capabilities, and timers. Filesystem and TTY policy, log storage, configuration,
networking, and complex drivers belong in isolated userspace services.

The project's central principle is:

> Compromising one component must not compromise the entire system by default.

## Current status

The native kernel and the first isolated services are implemented:

- a UEFI kernel for `x86_64`, implemented in Go and built with TinyGo;
- a bootable El Torito ISO;
- QEMU/OVMF support;
- a COM1 serial console;
- an automated boot test with a timeout;
- UEFI memory-map capture;
- a correct `ExitBootServices` handoff;
- normalization of physical memory types;
- a segmented bitmap page allocator for all usable RAM, with a kernel direct map;
- reservation of low memory and firmware-owned regions;
- kernel-owned four-level x86_64 page tables;
- enforced NX, supervisor write protection, and W^X mappings;
- an unmapped low-memory guard region;
- kernel-owned GDT and IDT tables;
- gates for all architectural CPU exception vectors;
- a live INT3 exception-path self-test;
- a PIT bootstrap timer that calibrates the TSC and the local APIC timers,
  which then tick each processor at the rate named by `kernel/clock`;
- symmetric multiprocessing: every processor the firmware lists, up to 64,
  starts through a low trampoline and runs tasks from its own run queue, with
  reschedule interrupts, TLB shootdown, and domain locks in place of a kernel
  lock ([SMP](docs/kernel/smp.md));
- hardware IRQ delivery with end-of-interrupt acknowledgement;
- a bounded architecture-neutral Task and round-robin run-queue model;
- timer-preempted x86_64 context switching across independent kernel stacks;
- two live worker tasks and an idle loop on every processor;
- two unmapped guard pages below every created kernel-task stack, with every
  kernel frame bounded by them at build time;
- task blocking and explicit wakeup in the live IRQ scheduler;
- a basic capability primitive that prevents rights escalation;
- separate Process and Task models;
- process-local typed handle tables with generation checking;
- controlled, non-escalating capability transfer between handle tables;
- distinct hardware page-table roots for separate processes;
- an isolation test mapping different physical pages at one user address;
- a 64-bit TSS with a dedicated guarded RSP0 kernel stack;
- ring 3 code and data descriptors;
- separate RX user code and RW+NX user stack mappings;
- a 64 KiB unmapped gap below every user stack, enforced by the loader, and a
  build-time check that no stack frame can step over it;
- a DPL 3 native kernel-call gate;
- the first native program executing at CPL 3;
- a typed native kernel-call dispatcher with bounded, capability-checked calls;
- validated `CopyIn` and `CopyOut` across user page mappings;
- preemptively scheduled ring 3 tasks with process-specific CR3 roots;
- task-local guarded RSP0 stacks and isolated userspace fault termination;
- a versioned native image format and strictly validated segment loader;
- bounded versioned IPC with request/reply, notifications, and correlations;
- controlled IPC handle passing and peer-death reporting;
- capability-controlled NX shared-memory mappings for bulk IPC;
- restartable blocking IPC with message and timeout wakeups;
- bounded readiness-only waits across up to 64 capability-checked channel,
  stream, or process handles, with a ready bit mask and generation-safe wakeups;
- a userspace terminal service that alone reads the console and relays
  everything the shell and its commands print, redrawing the line being typed
  around it, so no ordinary process holds a console capability;
- reference-counted stream endpoints, so one writing end can be shared by the
  shell and every pipeline stage and closes only with its last holder;
- a bounded generation-safe RAMFS core and versioned service protocol;
- namespace-scoped VFS mounts, path resolution, open handles, and caches, with
  every mount listed in its parent directory and protected from removal;
- bounded `tiny`, `normal`, and `large` launch profiles for channels, streams,
  tasks, and pages, plus per-process CPU accounting;
- an interrupt-driven console whose input is a waitable object, so a reader
  blocks instead of holding a CPU while it waits to be typed at;
- a FAT32 mount that checks the geometry an image declares against the device it
  is on, and a system that still boots when it refuses one;
- a storage driver that restarts with a fresh channel when it crashes, without
  taking the services around it down;
- a terminal service and a shell that restart independently: whichever dies is
  relaunched on a fresh session and the survivor is handed the other end, and a
  dead shell's jobs are stopped with it;
- a supervised log service that drains the bounded kernel ring onto persistent
  storage, so a session's records outlive its reset;
- a typed configuration service with declared keys, namespace-scoped sessions,
  read-only published values, and a human-readable stored form, consulted by the
  shell for its prompt and by the log service for its own setting;
- bounded init policy with dependencies, capability manifests, supervised
  restart policies, health states, and rate limiting;
- deterministic USTAR initramfs discovery and a ring-3 native init as PID 1;
- bounded task-to-process exit events for supervisor crash detection;
- capability-protected PID 1 lifecycle IPC and a real supervised service
  restart into a fresh process address space;
- a freestanding amd64 TinyGo userspace runtime and the bounded supervision
  policy loop executing inside PID 1;
- capability manifests that derive PID 1 authority into newly spawned process
  handle tables without rights escalation;
- a supervised native shell with bounded quoting, structured `argv`, built-in
  `help`, `clear`, and `cd`, plus isolated native base utilities;
- a supervised userspace VFS service reached through explicit, non-transferable
  client and server channel endpoints;
- unit, integration, boot, and performance tests;
- reclamation of exited process address spaces and guarded kernel stacks;
- generation-safe task-slot reuse and bounded process completion records;
- capability-protected process handles and a blocking native wait call;
- bounded executable lookup along a search path with per-process VFS sessions;
- an installed system on the disk, published at `/system` and protected from
  every client under every name, with the boot archive mounted read-only at
  `/bootstrap` and a recovery mode that runs from it without the disk;
- a versioned native startup ABI with named, capability-bounded handle roles;
- bounded environment data, child exit-status propagation, and read-only
  process/memory system information;
- a bounded versioned block-device protocol and a supervised ring-3 block
  service with a volatile validation backend;
- PCI discovery for a legacy virtio-blk function, capability-scoped port I/O,
  IRQ and DMA authority, and an isolated ring-3 virtio-blk probe driver;
- a persistent legacy virtio-blk data path with bounded DMA, IOAPIC-backed IRQ
  waits, a private driver channel, and QEMU-verified read/write/flush requests;
- a bounded FAT32 filesystem service, a process of its own mounted at
  `/persist` through VFS and reaching the disk over a dedicated
  capability-scoped block-service endpoint, with persistence verified
  across separate QEMU boots;
- user data at `/home` and mutable system state at `/state` on that volume,
  named apart from the immutable system image;
- distinct named standard-stream capabilities for external native utilities:
  read-only input role `in` and write-only `stdout` and `stderr`;
- bounded `readline` input through an isolated utility's inherited `in`
  capability;
- capability-protected byte streams with separate reader/writer endpoints,
  bounded buffering, backpressure, and close-driven EOF;
- scheduler-backed native pipelines of up to four processes with directional
  startup capabilities and no POSIX file descriptors;
- bounded stdout file redirection with `>` and `>>` and stderr redirection with
  `2>` and `2>>`, each through an isolated stream consumer with its own
  capability-scoped VFS session, and bounded stdin file redirection with `<`
  through an isolated producer;
- a Linux x86_64 compatibility layer: static ELF programs run as GoOS
  processes, start and replace other Linux programs with `fork` and `execve`,
  connect them with pipes, reap them with `wait4`, and send and handle
  signals, without authority a native process with the same grants would
  lack.

PID 1 runs the supervision policy. It starts the storage services from its
read-only mapping of the boot archive, checks the system installed on the
disk, and starts the session services and every command from there; the
kernel launches an extent of the archive under an image capability with
Execute, or a sealed file-backed source holding a stable VFS open object, and
knows no path but the bootstrap `system/init` (ADR 0093, ADR 0121, ADR 0175).
The shell finds native commands in `/system/bin` and then `/home/user/bin`,
runs program paths such as `./args`, with
bounded quoting and structured arguments, runs
pipelines of up to four processes, `;` sequences, and background jobs, and
redirects stdin with `<`, stdout with `>`/`>>`, and stderr with `2>`/`2>>`.
Every command is a separate process that receives only its standard streams
and, if its manifest asks, one VFS session: the native application environment
of specification milestone 6 is complete. `gfm`, a two-pane file manager, is
the first full-screen program: the shell puts the terminal in raw mode for it
because its manifest asks, and it draws with cursor addressing and 16 ANSI
colours, with no authority beyond its VFS session
([file manager](docs/userspace/file-manager.md)). COM1 and the framebuffer are
separate consoles, each with its own terminal service and shell, so `gfm`
fills whichever screen you start it on (ADR 0125). The storage path reaches a
persistent QEMU block device through an isolated driver, block service, and
FAT32 filesystem service, and native utilities use `/persist`, `/home` and
`/state` through VFS. `gfm` views a file with `gview` and edits it with
`edit`, which PID 1 starts on its behalf with the child's own manifest grants
(ADR 0130); `edit` saves through a temporary file and a whole-file replacement
that leaves the original intact when a save fails (ADR 0129).

PID 1, VFS, and the block service use event-driven multi-object waits for their
independent control and client endpoints. A wake marks the task runnable and the
switch happens on the return from the native call or interrupt that caused it,
so no hand-off waits for a timer tick. SSE state is saved with every task
context. A single-process command takes about 5 ms at 100 Hz and a VFS round
trip about 0.15 ms, nearly independent of the tick rate (ADR 0057). The stress
suite passes at 100, 250, and 1000 Hz; the default remains 100 Hz.

A normal boot is quiet: a short banner, `Starting services...`, one `[ OK ]`
line per service, and the prompt. After that the prompt, what is typed, and
what the shell and its commands print all pass through the terminal service, which keeps the line being typed intact when a background job reports
(ADR 0082, ADR 0089). Kernel self-tests and markers live in the
`VALIDATION=1` build (ADR 0058). Services and the kernel log structured events
to a bounded kernel ring, printed by `log` (ADR 0059). The shell provides
`services`, `handles`, `caps`, `log`, and `reboot`, and `ps` shows PID, type,
state, and command (ADR 0061). `reboot` syncs storage and restarts the machine
through a capability held only by PID 1; `poweroff` syncs storage the same way
and switches the machine off through ACPI S5, which in QEMU ends the emulator
(ADR 0092). The FAT32 volume supports
subdirectories (ADR 0060), so the M7 acceptance of writing, rebooting from the
shell, and reading back runs inside one VM in the boot test, through `/home`.

The namespace describes itself. `ls /` names `bootstrap`, `system`, `apps`,
`persist`, `home` and `state` because a mount is listed in the directory it
is mounted on (ADR 0069). `/system` is the system installed on the disk; it
is protected under every name that reaches it, so `mkdir /system/backdoor`
and `mkdir /persist/SYSTEM/backdoor` both answer "read-only filesystem"
(ADR 0175). `/bootstrap` is the boot archive, read-only. `/home` and
`/state` name user data and mutable system state on the same volume
(ADR 0071).

## Building and running

The primary development environment is macOS. The build requires Go 1.25.5,
TinyGo 0.42.0, clang and make from the Xcode Command Line Tools, and
`qemu-system-x86_64` 11.0.1 with OVMF firmware. `tools/toolchain.versions` pins
these versions and `make toolchain-check` compares the installed tools against
it, printing a macOS install hint for anything missing. It does not silently
install or download dependencies.

```sh
make toolchain-check
make
make test
make test-all
make run
make run-gui
make run RECOVERY=1
```

The resulting image is written to `build/goos-x86_64.iso`. `make run` boots
the system installed on `build/goos-disk.img`, which it makes from the build
when the file does not exist and never changes afterwards; after a rebuild
of another release that disk boots into recovery until `make
disk-overwrite` installs the new system on it, replacing what is there.
`make disk` installs on a new disk and refuses an existing one, and `make
system-image` builds the ISO and a volume image together. All intermediate
files and caches remain under `build/`. Builds are reproducible: `make
test-reproducible` builds the ISO twice from a clean tree and requires every
artifact to be byte-identical (ADR 0086). Images are built with TinyGo's
`-opt=2` by default (ADR 0090); `make OPT=z` builds the smallest variant. The
release version is set in one place, `internal/version/version.go`, and
`make version` prints it.

A successful boot of a normal build produces:

```text
GoOS 0.2.3

CPU: x86_64
Graphics: GOP 1280x800x32
Memory: 364 MiB managed
Timer: 100 Hz
Security: capabilities, NX, W^X
Scheduler: preemptive round-robin
Kernel initialized

Starting services...
[ OK ] vfs
[ OK ] block
[ OK ] virtio-blk
[ OK ] fat32
[MODE] normal
[ OK ] config
[ OK ] log
[ OK ] tty
[ OK ] shell
[ OK ] tty-fb
[ OK ] shell-fb

goos>
```

The namespace is walkable from there. This is real output, not a transcript:

```text
goos> ls /
tmp/
run/
bootstrap/
persist/
home/
state/
system/
apps/
goos> ls /system
bin/
commands
linux/
motd
release
services/
goos> ls /system/bin
args
bench
cat
cp
df
echo
edit
fsck-fat
gfm
gview
ls
lscpu
mem
mkdir
mount
mv
ps
pwd
quota
readline
rm
sleep
stat
touch
uname
uptime
vmprobe
write
goos> ls /system/linux
abi-probe
dirprobe
fsprobe
hello
hello-pie
musl-hello
goos> ls /bootstrap
bin/
motd
init
vfs
shell
block
virtio-blk
tty
fat32
commands
goos> /system/linux/musl-hello one two
argc=3
hello from musl
goos> mkdir /system/backdoor
mkdir: /system/backdoor: read-only filesystem
exit status: 2
goos> mkdir /persist/SYSTEM/backdoor
mkdir: /persist/SYSTEM/backdoor: read-only filesystem
exit status: 2
goos> mkdir /home/user/notes
goos> write /home/user/notes/notes.txt "Hello from GoOS"
goos> cat /home/user/notes/notes.txt
Hello from GoOS
goos> cp /system/bin/uname /home/user/bin/myuname
goos> myuname
GoOS 0.2.3 x86_64
goos> fsck-fat --check auto
exec: cannot start fsck-fat: volume in use by the running system; check it in recovery mode
goos> ps
PID   TYPE     STATE      MEM  CPU   COMMAND
1     native   waiting    750  12    init
4     native   waiting    60   11    vfs
5     native   waiting    63   12    block
6     native   waiting    60   1     virtio-blk
7     native   waiting    119  16    fat32
8     native   waiting    69   40    config
9     native   waiting    56   28    log
10    native   waiting    26   0     tty
11    native   waiting    142  85    shell
12    native   waiting    26   0     tty
13    native   waiting    89   56    shell
27    native   running    27   6     ps
```

MEM is the pages the kernel allocated for the process and CPU the timer ticks it
was found running on. PIDs 12 and 13 are the terminal service and shell of the
framebuffer console.

A disk without an installed system of this release, or no disk at all, boots
into recovery mode, which says why and offers the archive's own commands
(system log lines left out):

```text
[ OK ] fat32
[MODE] recovery: stage=record path=/system reason=no installed system on the volume
[ OK ] tty
[ OK ] shell
[ OK ] tty-fb
[ OK ] shell-fb

goos> ls /bootstrap/bin
cat
cp
df
echo
edit
fsck-fat
ls
mkdir
mount
mv
pwd
rm
stat
touch
write
goos> fsck-fat --check auto
files=1 directories=3 used-clusters=4 free-clusters=130808 errors=0
```

`make VALIDATION=1` builds the validation variant used by `make test-boot`. It
runs the kernel self-tests and fault-injection scenarios and prints the `*_OK`
markers that the boot test asserts.

`make run` is headless and attaches COM1 to the current terminal. `make
run-gui` opens QEMU's GOP-compatible VGA display while keeping COM1 on the
terminal. The graphical console uses a 1280x800 preferred GOP mode and accepts
US-layout PS/2 keyboard input. It runs its own shell: what you type in the
QEMU window reaches that shell, and what you type in the terminal reaches the
COM1 shell. Cocoa is the
default display backend; other hosts can run, for example, `make run-gui
QEMU_DISPLAY=gtk`. `make
run-debug` starts QEMU paused with a GDB server on TCP port
1234. `make test-performance` stores
benchmark results under `build/reports/`: host benchmarks of the kernel data
structures and, through `make test-microbench`, context switch, IPC, VFS read,
and RAMFS metadata costs measured in a booted guest.
`make test-console-gui` validates actual framebuffer pixels and injects a shell
command through QEMU's PS/2 keyboard path in a headless guest, then drives
`gfm` with arrows and function keys, quits it, stops a second one with Ctrl-C
and checks the shell afterwards. It then views and edits a file from `gfm` and
reads the saved result back in the shell. It does not require a graphical host session
or OCR. In `make run-gui`, type `gfm` in the QEMU window to use the file
manager.
`make test` is the fast host-side validation set. `make test-all` is the
canonical complete gate: lint, documentation checks, ISO creation, the
two-phase QEMU boot and persistence test, the installed-system and recovery
test, the Linux boot test, the console test, and two stress runs. `make release-check` adds a reproducible clean build,
FAT32 storage stress, and the release checks. Complete serial logs remain in
`build/logs/boot.log` and `build/logs/reboot.log`; successful harness output is
grouped and concise. See [`docs/development/testing.md`](docs/development/testing.md).

Optional KVM validation on real hardware runs on a Proxmox VE node through
`tools/pvelab`; see [`docs/development/proxmox-lab.md`](docs/development/proxmox-lab.md).
It is no part of `make test-all` or `make release-check`.

`HZ=250` or `HZ=1000` builds any target at a diagnostic timer rate, and
`DIAG=1` builds the diagnostic variant whose `diag` shell builtin prints the
kernel's process, handle, channel, and wait state. `make test-stress` boots
several guests in parallel and drives repeated command rounds through each.
`make test-latency` measures command and VFS latency with the shell's `time`
builtin and the `bench` utility and writes a table to `build/reports/`.
See [`docs/development/building.md`](docs/development/building.md).

## Development direction

Init, supervision, VFS, the native shell, bounded `/system/bin` lookup, the
versioned named-handle startup ABI, structured argv/environment data, exit
status propagation, the native base utilities, and the block-service IPC
boundary, capability-scoped virtio-blk driver, bounded FAT32 VFS mount, and
reboot-persistence gate are implemented. Priority 3 shell composition is
complete: stdin consumption, native streams, pipelines of up to four processes,
sequential execution, bounded stdout `>`/`>>` redirection, bounded stdin `<`
redirection, and bounded stderr `2>`/`2>>` redirection all work through explicit
typed handles. The namespace now describes the system it serves: mounts are
discoverable and protected, `/system` is the protected installed system, and
`/home` and `/state` separate user data from mutable system state. Launch resource
profiles bound each process's share of kernel tables, and `ps` and `quota`
report the applied limits and current use. A supervised log service persists the kernel
ring to `/state`, so records survive a reboot, and a typed configuration service
holds the system's settings, which the shell and the log service consult.
GoOS 0.1.0 is released; [docs/releases/0.1.0.md](docs/releases/0.1.0.md)
summarises it and lists its known limitations. GoOS 0.1.1
([docs/releases/0.1.1.md](docs/releases/0.1.1.md)) is an architectural
cleanup before Linux compatibility: executable-image launch authority, FAT32
as a separate service, no identity-authorized console path, userspace `unsafe`
confined to `userspace/runtime`, and smaller source files. GoOS 0.1.2
([docs/releases/0.1.2.md](docs/releases/0.1.2.md)) prepares storage for Linux
compatibility: programs run from the persistent volume through executable
objects, file data between the VFS and FAT32 moves through a shared file cache,
and every system service restarts on failure without taking its clients down.
GoOS 0.1.3 ([docs/releases/0.1.3.md](docs/releases/0.1.3.md)) removes the
prototype limits under every layer: FAT32 files up to 4 GiB and large
directories, page-backed RAMFS, physical memory beyond 1 GiB, virtual-memory
regions with demand-zero memory, lazily loaded programs of up to 256 MiB,
shared-memory bulk I/O, a byte-bounded startup ABI and 4096-byte command lines,
1024-byte native names, full 255-UTF-16-unit VFAT names, 4096-byte paths, and kernel registries that take
memory as they grow. GoOS 0.1.4
([docs/releases/0.1.4.md](docs/releases/0.1.4.md)) completes the storage and
memory contract needed by the next milestone: foreign FAT32 geometry and
metadata, raw/MBR/GPT block slices, dynamic mounts, native FAT repair, stable
open-file identity, high-half kernel entry trampolines, file-backed executable
pages, and bounded launch resource profiles.

GoOS 0.1.5
([docs/releases/0.1.5.md](docs/releases/0.1.5.md)) adds an interactive UEFI GOP
text console without replacing COM1: a permanent supervisor-only framebuffer,
Spleen 12x24 rendering, capability-checked terminal geometry, interrupt-driven
PS/2 input, `make run-gui`, and headless pixel and keyboard validation. GoOS
0.1.6 ([docs/releases/0.1.6.md](docs/releases/0.1.6.md)) adds the `gfm`
two-pane file manager and what a full-screen program needs under it: F1-F12
and navigation key sequences, cursor addressing and 16-colour SGR in the
framebuffer terminal, scoped raw terminal mode granted by the shell, a
separate terminal service and shell for COM1 and for the framebuffer, and
reusable terminal and non-fatal filesystem helpers. GoOS 0.1.7
([docs/releases/0.1.7.md](docs/releases/0.1.7.md)) completes the native
environment the Linux compatibility layer will build on. It adds typed file timestamps, stable native attributes, checked 64-bit
filesystem capacity, bounded mount enumeration and generation-safe,
non-authorizing mount identity through version 10 of the VFS and
filesystem-service protocols. Native `df` and read-only `mount` expose that
information without device authority. A bounded `userspace/tui` layer adds
caller-owned terminal-cell screens, checked geometry, dirty-row rendering and
text/list viewports. Native `gview` uses that layer and a fixed file window to
view text files of arbitrary size without loading them whole; control and binary
content cannot become terminal commands (ADR 0126, ADR 0127, ADR 0128). A new
`Replace` operation and `userspace/safesave` save a file without ever
truncating the original (ADR 0129). `gfm` starts `gview` and the new `edit`
editor through a capability-bounded launch channel to PID 1 (ADR 0130), shows
modification times and free space, and keeps its selection across edits.
`edit` holds up to 1 MiB in a gap buffer (ADR 0131). Line commands show
control characters in file names as `?`.

Shell redirection extends the explicit native stdin/stdout/stderr handles and
stream primitives without adding POSIX file descriptors. Background jobs can be
listed and stopped; stream merging such as `2>&1` and POSIX job control
(suspending a job, `fg`/`bg`) are deliberately out of scope for now.

The terminal service relays all shell and command output and uses the active
console geometry to redraw edited lines that wrap across rows. The kernel's
warning echo and the system services that start before it still write the
console directly and can land in a line being typed (ADR 0089).

GoOS 0.2.0 ([docs/releases/0.2.0.md](docs/releases/0.2.0.md)) is the first
Linux ABI release. Unmodified, statically linked Linux x86_64 programs run as
GoOS processes: `/system/linux/hello`, a static musl program, file and
directory probes, and applets of a static BusyBox the user supplies. A
bounded layer in the kernel translates 59 Linux syscall numbers, to the documented contract, into the VM,
stream, clock, and VFS primitives a native process uses. Static
position-independent executables load too. A Linux process gets
its standard streams and one VFS session, cannot enter the native call gate,
and holds no device authority. The
[ABI document](docs/architecture/linux-x86_64-abi.md) lists every call and
each difference from Linux.

GoOS 0.2.1 ([docs/releases/0.2.1.md](docs/releases/0.2.1.md)) adds Linux
process composition. A Linux program can `fork`, `execve`, connect children
with pipes, reap them with `wait4`, and send and handle signals; `poll` waits
on several descriptors. A static BusyBox `sh` runs programs, pipelines,
redirections, scripts, and background jobs non-interactively. BusyBox is no
part of the image: `make disk LINUX_EXTRA=/path/to/busybox` installs the one
you supply under `/system/linux`. Every Linux
descendant is an ordinary GoOS Process with no authority its parent lacks.
Dynamic linking, threads, a Linux terminal, and networking are outside this
release.

GoOS 0.2.2 ([docs/releases/0.2.2.md](docs/releases/0.2.2.md)) runs the
system from disk. The boot archive keeps the storage services, the shell, and
a few recovery commands; everything else is installed on the FAT32 volume and
published at `/system` after PID 1 has checked its installation record,
service images, and command index. The VFS protects the installed system on
its directory's node, so no client can change it through any name. Commands
are granted only by the index of their own origin. A missing or unusable
installation boots into a recovery mode that says why, and storage that fails
for good under a running system moves it there. This checks compatibility;
it is not a verified boot chain or an update system.

GoOS 0.2.3 ([docs/releases/0.2.3.md](docs/releases/0.2.3.md)) adds static
glibc compatibility, Linux threads with per-thread TLS and signal state, and
sleeping futex waits. The kernel also publishes a bounded, read-only CPU
inventory derived from CPUID and ACPI MADT data; the native `lscpu` command
formats it without exposing raw hardware access to userspace.

Every processor in that inventory now starts and runs tasks, so the threads of
one program compute in parallel; `lscpu` lists the processors that joined the
scheduler as online. Kernel calls take the locks of the domains they touch
instead of one kernel lock (ADR 0184): faults and the fast Linux calls of one
program's threads, and calls of different programs, run in the kernel on
several processors at once. `make test-smp` boots four processors and proves
overlapping execution in user mode and in the kernel.

Linux compatibility translates into the same GoOS process, VFS, IPC, handle,
and capability model and does not weaken the native architecture for POSIX
compatibility.

See [roadmap](docs/development/roadmap.md) for the current implementation order.

## License

Copyright (C) 2026 Nikita Parfenovich (aka bar0metr).

GoOS is licensed under the GNU General Public License, version 3 only
(`GPL-3.0-only`). See [LICENSE](LICENSE) and [NOTICE](NOTICE). Embedded
third-party data retains its own license; see
[font provenance](docs/third-party-font.md).

Further documentation:

- Architecture: [overview](docs/architecture/overview.md),
  [boot sequence](docs/architecture/boot.md),
  [processes](docs/architecture/processes.md),
  [scheduler](docs/architecture/scheduler.md),
  [memory](docs/architecture/memory.md), [typed IPC](docs/architecture/ipc.md),
  [capabilities and handles](docs/architecture/capabilities.md),
  [filesystem stack](docs/architecture/filesystem.md),
  [timekeeping](docs/architecture/timekeeping.md),
  [Linux compatibility](docs/architecture/linux-compat.md),
  [Linux x86_64 ABI](docs/architecture/linux-x86_64-abi.md).
- Kernel: [interrupts](docs/kernel/interrupts.md),
  [native images](docs/kernel/native-images.md),
  [byte streams](docs/kernel/streams.md).
- Userspace: [init and service supervision](docs/userspace/init.md),
  [terminal](docs/userspace/tty.md), [shell](docs/userspace/shell.md),
  [file manager](docs/userspace/file-manager.md),
  [terminal-cell TUI](docs/userspace/tui.md), [text viewer](docs/userspace/viewer.md),
  [VFS](docs/userspace/vfs.md), [RAMFS](docs/userspace/ramfs.md),
  [storage](docs/userspace/storage.md),
  [configuration](docs/userspace/configuration.md),
  [logging](docs/userspace/logging.md).
- Security: [threat model](docs/security/threat-model.md),
  [capabilities](docs/security/capabilities.md),
  [memory safety](docs/security/memory-safety.md),
  [attack surface](docs/security/attack-surface.md),
  [boot chain and signature verification (planned)](docs/security/boot-chain.md).
- Development: [building](docs/development/building.md),
  [debugging](docs/development/debugging.md),
  [testing](docs/development/testing.md),
  [Proxmox hardware lab](docs/development/proxmox-lab.md),
  [coding style](docs/development/coding-style.md),
  [adding a command](docs/development/adding-command.md),
  [adding a service](docs/development/adding-service.md),
  [roadmap](docs/development/roadmap.md).
- [Architecture decisions](docs/adr).
