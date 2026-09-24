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
- a bitmap page allocator for the first GiB;
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
- a typed native kernel-call dispatcher with Exit and bounded DebugWrite;
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
- per-process quotas on channels, streams, tasks, and pages, plus per-process
  CPU accounting, so one process cannot take a whole kernel table;
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
- a bounded FAT32 backend mounted at `/persist`, reached through VFS and a
  dedicated capability-scoped block-service endpoint, with persistence verified
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
- bounded stdout file redirection with `>` and `>>` through an isolated stream consumer
  and bounded stdin file redirection with `<` through an isolated producer.
  and capability-scoped VFS session.

The init policy core, initramfs discovery, PID 1 launch, general initramfs image
resolution, and kernel-side process death delivery are implemented. PID 1 runs
the policy loop and enforces bounded crash restart accounting. Spawn capability
installation, a userspace VFS service, an interactive filesystem-aware shell,
and capability-bounded native utility launch are implemented. The shell resolves
general native executables beneath `/system/bin` with bounded quoting and
structured arguments, pipelines of up to four processes, and bounded `;` command
sequences. Files can feed stdin through one bounded `<` target, and stdout can
be truncated or appended through `>` or `>>`; stderr redirection is not yet exposed. The current ISO boots a real
kernel, leaves UEFI boot services, installs its own page tables, runs preempted
kernel tasks,
verifies process address-space isolation, and runs supervised native programs
at CPL 3. It is now interactively usable for the built-in shell commands, but
is not yet a general application environment. Native programs no longer assume
numeric handle slots and the Priority 1 base application environment is complete.
The storage path reaches a persistent QEMU block device through an isolated
driver, and native utilities can access bounded FAT32 regular files at
`/persist`, `/home` and `/state` through VFS.

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
through a capability held only by PID 1. The FAT32 volume supports
subdirectories (ADR 0060), so the M7 acceptance of writing, rebooting from the
shell, and reading back runs inside one VM in the boot test, through `/home`.

The namespace describes itself. `ls /` names `system`, `tmp`, `persist`, `home`
and `state` because a mount is listed in the directory it is mounted on (ADR
0069). `/system` is the boot archive itself, mounted read-only (ADR 0070), so
the twenty utilities under `/system/bin` are visible and `mkdir /system/backdoor`
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
GoOS 0.1.0-dev

CPU: x86_64
Memory: 368 MiB usable
Timer: 100 Hz
Security: capabilities, NX, W^X
Scheduler: preemptive round-robin
Kernel initialized

Starting services...
[ OK ] vfs
[ OK ] block
[ OK ] virtio-blk
[ OK ] config
[ OK ] log
[ OK ] tty
[ OK ] shell

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
commands
goos> ls /system/bin
args
bench
cat
cp
echo
ls
mem
mkdir
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
1     native   waiting    138  6     init
4     native   waiting    84   4     vfs
5     native   waiting    47   2     block
6     native   waiting    50   0     virtio-blk
7     native   waiting    50   0     config
8     native   waiting    46   1     log
9     native   waiting    46   3     tty
10    native   waiting    48   4     shell
18    native   running    46   0     ps
goos> quota
PID   COMMAND           RESOURCE  HELD/LIMIT
1     init              chan      15/24
1     init              strm      2/24
1     init              task      1/8
1     init              page      138/512
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

`make run` attaches COM1 to the current terminal. `make run-debug` starts QEMU
paused with a GDB server on TCP port 1234. `make test-performance` stores
benchmark results under `build/reports/`: host benchmarks of the kernel data
structures and, through `make test-microbench`, context switch, IPC, VFS read,
and RAMFS metadata costs measured in a booted guest.
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
and `/state` separate user data from mutable system state. Per-process resource
quotas bound each process's share of every kernel table, and `ps` and `quota`
report what each process holds. A supervised log service persists the kernel
ring to `/state`, so records survive a reboot, and a typed configuration service
holds the system's settings, which the shell and the log service consult. The
0.1.0 feature set is complete; what remains is consolidation — judging the existing surface rather than adding to it.

Shell redirection extends the explicit native stdin/stdout/stderr handles and
stream primitives without adding POSIX file descriptors; stream merging such as
`2>&1` and job control are deliberately out of scope for now.

The terminal service relays all shell and command output, but the kernel's
warning echo and the system services that start before it still write the
console directly and can land in a line being typed, and its redraw assumes the
edited line fits one row (ADR 0089).

Linux ABI compatibility remains a later compatibility layer. It must translate
into the same GoOS process, VFS, IPC, handle, and capability model rather than
weakening the native architecture for POSIX compatibility.

See [roadmap](docs/development/roadmap.md) for the current implementation order.

Further documentation:

- Architecture: [overview](docs/architecture/overview.md),
  [boot sequence](docs/architecture/boot.md),
  [processes](docs/architecture/processes.md),
  [scheduler](docs/architecture/scheduler.md),
  [memory](docs/architecture/memory.md), [typed IPC](docs/architecture/ipc.md),
  [capabilities and handles](docs/architecture/capabilities.md),
  [filesystem stack](docs/architecture/filesystem.md),
  [Linux compatibility (planned)](docs/architecture/linux-compat.md).
- Kernel: [interrupts](docs/kernel/interrupts.md),
  [native images](docs/kernel/native-images.md),
  [byte streams](docs/kernel/streams.md).
- Userspace: [init and service supervision](docs/userspace/init.md),
  [terminal](docs/userspace/tty.md), [shell](docs/userspace/shell.md),
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
