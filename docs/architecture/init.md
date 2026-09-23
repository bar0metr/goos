# Init and service supervision

Init owns a bounded declarative service table. Each definition names one
executable, its dependencies, the exact typed capabilities installed at launch, and
one of the `never`, `on-failure`, or `always` restart policies. Dependencies
must be declared earlier, making configuration order an explicit acyclic
startup order rather than requiring runtime graph allocation.

The supervisor starts only services whose dependencies are running and healthy.
It records process identifiers returned by the process-launch boundary, detects
reported exits, marks running dependants unhealthy, and restores their health
after the dependency restarts. A successful exit obeys `never` and
`on-failure`; `always` schedules another launch.

Restart attempts use a fixed window, bounded attempt count, and fixed backoff.
Exhausting the allowance marks the service failed instead of entering an
unbounded crash loop. Registration, starts, exits, dependency failures,
scheduled restarts, and rate limiting enter a bounded diagnostic event ring.

The policy core is in `userspace/init`; process creation is supplied through a
narrow launcher callback. UEFI loads a strict, bounded USTAR initramfs before
firmware handoff. The bootstrap reservation is capped at 1.5 MiB. The kernel
discovers `system/init`, validates its native image,
creates a distinct address space and process ID 1, and schedules it at ring 3.

Normal native exits and isolated CPU faults enter the same bounded task-exit
path. The process registry emits a process event only after its final Task dies,
then sends a typed lifecycle notification over a channel available only to PID
1. PID 1 uses one readiness-only `WaitAny` call for the lifecycle channel and,
after bootstrap, the shell-exec channel. A returned bit mask can identify both;
PID 1 then performs the typed receives, issues correlated launch requests, and
continues only after the replacement Process and Task exist.

Launch requests identify an executable by a normalized absolute path. The
kernel resolves that path from the immutable boot archive, validates
the native image, assigns a fresh process identifier from a bounded pool, and
creates a new address space and initial task. Empty components and parent
traversal are rejected.

PID 1 is compiled from Go with a freestanding amd64 TinyGo target. A small
assembly entry supplies the native kernel-call ABI, while a fixed 32 KiB stack
and 64 KiB bump-allocation arena bound runtime memory. The host-side converter
accepts only static x86-64 ELF load segments and wraps them in the validated
GoOS native image envelope.

The Manager, service table, exit handling, restart decisions, backoff, and rate
limit execute in PID 1. The kernel only delivers process deaths and performs
authorized image loading and process creation. Each launch request also carries
the service's bounded capability manifest. Entries reference existing PID 1
handles and request an exact derived rights mask; the kernel requires Transfer
authority, prevents escalation, and installs the derived handles into the new
process before its first Task is created.
Channel capabilities identify their endpoint side. A launch grant may be marked
consumed, removing PID 1's source handle after successful derivation. This is
used for per-process VFS clients and prevents accumulation of temporary handles.

PID 1 also supplies a `GST3` startup template with a unique role name for each
manifest entry. It writes the template into a slot of the launch staging page
shared with the kernel (handle `0x100000013`), and the launch message names the
slot (ADR 0062). After installing all capabilities, the kernel writes their
actual generation-safe IDs into the startup block before creating the first
Task. Services resolve roles rather than depending on handle-table insertion
order. Names are descriptive metadata; type and rights enforcement remains in
the kernel handle table.

The kernel also grants PID 1 a one-page exec page (handle `0x100000014`,
`Read|Write|Transfer`) for the shell exec protocol. It is not mapped at boot;
PID 1 maps it with `MemoryMap` and passes a derived `Read|Write` handle to the
shell through the launch manifest (ADR 0063).

Shell exec requests are restricted to simple names below `/system/bin`. A
request carries one to four invocation records, a shared cwd, and a shared
environment. The shell writes it into a 1024-byte slot of the exec page it
shares with PID 1, and the version 7 submit message names the slot and size
(ADR 0064). The reply to a launch that failed names the stage that could not be
started, so the shell can report it by name (ADR 0079). PID 1 copies the slot before decoding it. Each record contains
its own executable path and structured argv. The decoder checks the complete
request, all lengths, system paths, vector encodings, and trailing bytes before
any resource operations. Oversized requests are rejected rather than truncated.

The request begins with five bytes: stage count, cwd byte length, environment
byte length, error source, and flags (bit 0: background, whose first stage
reads an empty stream). Cwd and environment bytes follow. Each
stage then stores one-byte path and argv lengths followed by those byte strings.
Argv and environment use the GST3 vector encoding, up to 255 bytes each. No handles are
attached to the exec request; PID 1 derives child capabilities from its own table.

For N stages PID 1 creates N-1 streams and starts stages from right to left,
so a downstream launch failure cannot leave a newly started producer reading
the console. Each middle stage receives only its upstream reader and downstream
writer; stderr remains on the console. Successful launch consumes those stream
handles. On any failure PID 1 closes every retained endpoint before waiting
for children already started. This releases stream waits through EOF or peer
closure. It is not forced cancellation of an arbitrary nonterminating program.

All stages start before process waits. The request then becomes one of up to
four jobs: PID 1 returns to its loop, which waits on the lifecycle channel, the
exec channel, and the next process of every job, so supervision and further
requests continue while commands run. PID 1 reaps every started process and
returns the first nonzero exit code in left-to-right pipeline order. Launch
failure instead returns a launch status with no successful process result.
Completion readiness is observed through the generation-safe `WaitAny` table;
PID 1 then consumes the exit code with the typed process wait.

PID 1 creates and transfers a fresh VFS session only for utilities that access
the filesystem. Pure stream and diagnostic utilities receive no VFS endpoint.
The VFS control protocol version 2 identifies the add-session operation with an
explicit payload byte, preventing an unrelated control-shaped message from
being accepted as a session request.

## Startup phases

Services start in two phases. The first registers and launches `vfs`, `block`
and `virtio-blk`, whose endpoints PID 1 holds from boot. The second starts the
log service and then the shell.

The split exists because the log service's VFS session cannot be handed over at
boot: it has to be asked for, through the VFS control endpoint, the same way an
application's session is, and that needs VFS already running (ADR 0073). Placing
it before the shell also means the prompt appears after every service line
rather than in the middle of them.

A failure to start the log service warns and continues. Every other service is
required, because the system cannot serve files or a prompt without them.

## Restarts

A restart is scheduled as a deadline, not delivered as an event, so the
supervision wait is bounded while one is outstanding and the expiry starts
whatever is ready. Waiting only on events left a service that was scheduled to
restart down forever when nothing else happened (ADR 0078).

A service that cannot be handed the endpoints it had needs them replaced before
it is launched. The storage driver is the one such service: its channel to the
block service died with it, so PID 1 creates a fresh pair, tells the block
service over a control endpoint, and updates both the launch about to happen and
the stored definition. Its device authority — ports, interrupt line, DMA domain
— is granted rather than consumed, so PID 1 can grant it again.

The validation build kills the driver once, after the system is up, so the path
a crashed driver takes is exercised on every boot rather than only when hardware
misbehaves.
