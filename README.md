# GoOS

GoOS is an experimental 64-bit operating system for `x86_64`, with its primary
codebase written in Go and compiled with TinyGo. It is not an attempt to rewrite
Linux in another language, nor is it another POSIX system with a new name. The
project explores what a small modern operating system could look like when built
around explicit authority, component isolation, typed IPC, and a minimal
privileged kernel.

## Project history

The history of GoOS began in 2011 as a personal operating-system research
project written in Free Pascal. The immediate objective was not to produce a
finished product. It was to understand how a machine begins executing custom
code, how bootstrapping, memory, and interrupts work, and where the boundary
between the kernel and applications should be drawn.

The project was later moved to C++. Development continued at an unhurried pace:
individual ideas were tested, parts of the code were rewritten, and the
architecture was reconsidered several times. It was never a continuous effort
driven by a release schedule. The project remained a personal laboratory to
return to whenever a new idea appeared or a particular area of systems
programming deserved deeper study.

The arrival of Go raised a different question: could it be used to build more
than a small educational kernel? Could it support a coherent operating system
with controlled complexity and components that are safe by default? The
standard Go runtime assumes an existing operating system, but TinyGo gradually
made this experiment practical. The old research project gained a new technical
foundation and evolved into the current GoOS design.

GoOS grew out of a long-standing interest in operating-system architecture and
dissatisfaction with the historical baggage carried by existing systems. Linux
demonstrates the value of openness, observability, broad hardware support, and
the ability to operate in almost any environment. macOS demonstrates the value
of a coherent system, predictable user experience, and consistency between its
components. Both also carry decades of accumulated compromises: excessive
ambient authority, large trusted components, unclear ownership boundaries,
stringly typed interfaces where structured protocols would be safer, global
mutable state, and compatibility requirements that increasingly dictate the
internal design of the system.

The objective is not to mechanically combine Linux and macOS. GoOS takes useful
ideas from both without treating their limitations as unavoidable. Its native
API does not have to reproduce Unix. `fork`, UID 0, universal `ioctl`, a global
root, and the principle that “everything is a file” do not become foundational
merely because they are familiar. Linux compatibility belongs in a separate
layer and must not define the internal process, security, or resource model.

GoOS is built according to the author's view of a technically coherent
operating system: little code running with maximum privilege, processes without
implicit authority, capability-based access to resources, services and drivers
in userspace, stable versioned IPC protocols, and measured fast paths for bulk
data. It is an opinionated research project and its decisions do not have to
repeat the industry consensus. Fundamental decisions must still have testable
reasoning, an ADR, appropriate tests, and clearly documented consequences.

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
- a PIT bootstrap timer at the rate named by `kernel/clock` through a remapped
  legacy PIC;
- hardware IRQ delivery with end-of-interrupt acknowledgement;
- a bounded architecture-neutral Task and round-robin run-queue model;
- timer-preempted x86_64 context switching across independent kernel stacks;
- two live worker tasks and an idle task;
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
- bounded readiness-only waits across up to 16 capability-checked channel,
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
- unit, integration, boot, and performance tests.
- reclamation of exited process address spaces and guarded kernel stacks.
- generation-safe task-slot reuse and bounded process completion records.
- capability-protected process handles and a blocking native wait call.
- bounded `/system/bin` executable lookup with per-process VFS sessions.
- the boot archive mounted read-only at `/system` through a capability only the
  filesystem service holds, so `ls /system/bin` lists the executables the launch
  path resolves and every write to them is refused.
- a versioned native startup ABI with named, capability-bounded handle roles.
- bounded environment data, child exit-status propagation, and read-only
  process/memory system information.
- a bounded versioned block-device protocol and a supervised ring-3 block
  service with a volatile validation backend.
- PCI discovery for a legacy virtio-blk function, capability-scoped port I/O,
  IRQ and DMA authority, and an isolated ring-3 virtio-blk probe driver.
- a persistent legacy virtio-blk data path with bounded DMA, IOAPIC-backed IRQ
  waits, a private driver channel, and QEMU-verified read/write/flush requests.
- a bounded FAT32 filesystem service, a process of its own mounted at
  `/persist` through VFS and reaching the disk over a dedicated
  capability-scoped block-service endpoint, with persistence verified
  across separate QEMU boots.
- user data at `/home` and mutable system state at `/state` on that volume,
  named apart from the immutable system image.
- distinct named standard-stream capabilities for external native utilities:
  read-only input role `in` and write-only `stdout` and `stderr`.
- bounded `readline` input through an isolated utility's inherited `in`
  capability.
- capability-protected byte streams with separate reader/writer endpoints,
  bounded buffering, backpressure, and close-driven EOF.
- scheduler-backed native pipelines of up to four processes with directional
  startup capabilities and no POSIX file descriptors.
- bounded stdout file redirection with `>` and `>>` and stderr redirection with
  `2>` and `2>>`, each through an isolated stream consumer with its own
  capability-scoped VFS session, and bounded stdin file redirection with `<`
  through an isolated producer.

PID 1 runs the supervision policy and resolves every service and system
command in its own read-only mapping of the system image; the kernel launches
an extent of that image under an image capability with Execute, or a sealed
file-backed source holding a stable VFS open object, and knows no path but the
bootstrap `system/init` (ADR 0093, ADR 0121). The shell resolves native
executables beneath `/system/bin`, runs program paths such as `./args`, with
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
line per service, and the prompt. Everything after that — the prompt, what is
typed, and what the shell and its commands print — passes through the terminal
service, which keeps the line being typed intact when a background job reports
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

The namespace describes itself. `ls /` names `system`, `tmp`, `persist`, `home`
and `state` because a mount is listed in the directory it is mounted on (ADR
0069). `/system` is the boot archive itself, mounted read-only (ADR 0070), so
the utilities under `/system/bin` are visible and `mkdir /system/backdoor`
answers "read-only filesystem". `/home` and `/state` name user data and mutable
system state on the persistent volume (ADR 0071).

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
```

The resulting image is written to `build/goos-x86_64.iso`. All intermediate
files and caches remain under `build/`. Builds are reproducible: `make
test-reproducible` builds the ISO twice from a clean tree and requires every
artifact to be byte-identical (ADR 0086). Images are built with TinyGo's
`-opt=2` by default (ADR 0090); `make OPT=z` builds the smallest variant. The
release version is set in one place, `internal/version/version.go`, and
`make version` prints it.

A successful boot of a normal build produces:

```text
GoOS 0.2.0

CPU: x86_64
Graphics: GOP 1280x800x32
Memory: 367 MiB managed
Timer: 100 Hz
Security: capabilities, NX, W^X
Scheduler: preemptive round-robin
Kernel initialized

Starting services...
[ OK ] vfs
[ OK ] block
[ OK ] virtio-blk
[ OK ] fat32
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
system/
persist/
home/
state/
goos> ls /system
bin/
motd
init
fault-probe
vfs
shell
block
virtio-blk
log
config
tty
fat32
commands
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
goos> mkdir /system/backdoor
mkdir: /system/backdoor: read-only filesystem
exit status: 2
goos> mkdir /home/user
goos> write /home/user/notes.txt "Hello from GoOS"
goos> cat /home/user/notes.txt
Hello from GoOS
goos> ps
PID   TYPE     STATE      MEM  CPU   COMMAND
1     native   waiting    160  4     init
4     native   waiting    84   6     vfs
5     native   waiting    47   4     block
6     native   waiting    50   2     virtio-blk
7     native   waiting    47   4     fat32
8     native   waiting    50   2     config
9     native   waiting    46   1     log
10    native   waiting    46   0     tty
11    native   waiting    48   7     shell
19    native   running    46   1     ps
goos> quota
PID   COMMAND           RESOURCE  HELD/LIMIT
1     init              chan      16/24
1     init              strm      2/24
1     init              task      1/8
1     init              page      160/512
4     vfs               chan      0/4
4     vfs               strm      0/4
4     vfs               task      1/4
...
```

MEM is the pages the kernel allocated for the process and CPU the timer ticks it
was found running on.

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
canonical complete gate and includes lint, documentation checks, ISO creation,
and the two-phase QEMU boot/persistence test. Complete serial logs remain in
`build/logs/boot.log` and `build/logs/reboot.log`; successful harness output is
grouped and concise. See [`docs/development/testing.md`](docs/development/testing.md).

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
discoverable and protected, `/system` is the read-only boot image, and `/home`
and `/state` separate user data from mutable system state. Launch resource
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
pages, and bounded launch resource profiles. The next milestone is Linux ELF
compatibility.

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
control characters in file names as `?`. GoOS is
licensed under GPL-3.0-only; third-party components retain their documented
licenses.

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
bounded layer in the kernel translates 59 Linux system calls into the VM,
stream, clock, and VFS primitives a native process uses. Static
position-independent executables load too. A Linux process gets
its standard streams and one VFS session, cannot enter the native call gate,
and holds no device authority. The
[ABI document](docs/architecture/linux-x86_64-abi.md) lists every call and
each difference from Linux. Dynamic linking, `fork`, `execve`, threads,
signal delivery, and networking are outside this release.

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
  [coding style](docs/development/coding-style.md),
  [adding a command](docs/development/adding-command.md),
  [adding a service](docs/development/adding-service.md),
  [roadmap](docs/development/roadmap.md).
- [Architecture decisions](docs/adr).
