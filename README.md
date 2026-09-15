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
- a 100 Hz PIT bootstrap timer through a remapped legacy PIC;
- hardware IRQ delivery with end-of-interrupt acknowledgement;
- a bounded architecture-neutral Task and round-robin run-queue model;
- timer-preempted x86_64 context switching across independent kernel stacks;
- two live worker tasks and an idle task;
- unmapped guard pages below every created kernel-task stack;
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
- a capability-isolated userspace TTY protocol and serial service;
- a bounded generation-safe RAMFS core and versioned service protocol;
- namespace-scoped VFS mounts, path resolution, open handles, and caches;
- bounded init policy with dependencies, capability manifests, supervised
  restart policies, health states, and rate limiting;
- deterministic USTAR initramfs discovery and a ring-3 native init as PID 1;
- bounded task-to-process exit events for supervisor crash detection;
- capability-protected PID 1 lifecycle IPC and a real supervised service
  restart into a fresh process address space;
- a freestanding amd64 TinyGo userspace runtime and the bounded supervision
  policy loop executing inside PID 1;
- unit, integration, boot, and performance tests.

The init policy core, initramfs discovery, PID 1 launch, general initramfs image
resolution, and kernel-side process death delivery are implemented. PID 1 runs
the policy loop and enforces bounded crash restart accounting. Capability
manifest installation for spawned services and the shell are not implemented
yet. The current ISO boots a real kernel, leaves UEFI
boot services, installs its own page tables, runs preempted kernel tasks,
verifies process address-space isolation, and executes a minimal native program
at CPL 3. It is not yet a usable operating environment.

## Building and running

The primary development environment is macOS. The build requires Go, TinyGo,
GNU Make, `qemu-system-x86_64` with OVMF, `hdiutil`, and `nm`. It does not
silently install or download dependencies.

```sh
make toolchain-check
make
make test
make test-boot
make run
```

The resulting image is written to `build/goos-x86_64.iso`. All intermediate
files and caches remain under `build/`.

A successful boot currently produces:

```text
GoOS 0.1-dev
Kernel

CPU: x86_64
Boot: UEFI
Security: capabilities (foundation)
BOOT_SERVICES_EXITED
Memory: 375 MiB usable
Physical pages free: 96043
PMM_OK
NX_WX_OK
VM_OK
EXCEPTION_OK
GDT_OK
IDT_OK
TIMER_OK
STACK_GUARDS_OK
TASK_BLOCK_OK
TASK_WAKE_OK
TASK_A_OK
TASK_B_OK
IDLE_TASK_OK
SCHEDULER_OK
HANDLE_TABLE_OK
PROCESS_MODEL_OK
IPC_WIRE_OK
IPC_REQUEST_REPLY_OK
IPC_HANDLE_TRANSFER_OK
IPC_BOUNDED_OK
IPC_PEER_DEATH_OK
SHMEM_OBJECT_OK
SHMEM_ZERO_COPY_OK
SHMEM_RIGHTS_OK
ADDRESS_SPACE_A_OK
ADDRESS_SPACE_B_OK
PROCESS_ISOLATION_OK
USER_RX_OK
USER_STACK_NX_OK
USER_COPYOUT_OK
KCALL_ABI_OK
NATIVE_IMAGE_LOADER_OK
USER_COPYIN_OK
USER_DEBUG_WRITE_OK
USER_EXIT_OK
USER_PREEMPT_OK
PROCESS_CR3_SCHED_OK
USER_FAULT_ISOLATION_OK
USER_RING3_OK
NATIVE_PROCESS_OK
INIT_PROCESS_OK
INIT_LIFECYCLE_IPC_OK
INIT_SERVICE_LAUNCH_OK
INIT_SERVICE_STARTED_OK
INITRAMFS_DISCOVERY_OK
PID1_INIT_OK
PROCESS_EXIT_EVENT_OK
SUPERVISOR_CRASH_EVENT_OK
LIFECYCLE_IPC_OK
PID1_SERVICE_LAUNCH_OK
Kernel initialized
BOOT_OK
```

`make run` attaches COM1 to the current terminal. `make run-debug` starts QEMU
paused with a GDB server on TCP port 1234. `make test-performance` stores
benchmark results under `build/reports/`.

## Development direction

The next stage adds init and service supervision, followed by the native shell.

The Linux ABI is a future compatibility layer. It does not block the first
self-contained GoOS release and must not weaken the native security model for
the sake of POSIX compatibility.

Further documentation:

- [architecture overview](docs/architecture/overview.md);
- [physical memory](docs/architecture/memory.md);
- [interrupts](docs/architecture/interrupts.md);
- [scheduler](docs/architecture/scheduler.md);
- [processes](docs/architecture/processes.md);
- [native images](docs/architecture/native-images.md);
- [typed IPC](docs/architecture/ipc.md);
- [init and service supervision](docs/architecture/init.md);
- [capabilities and handles](docs/architecture/capabilities.md);
- [building and debugging](docs/development/building.md);
- [roadmap](docs/development/roadmap.md);
- [architecture decisions](docs/adr/).
