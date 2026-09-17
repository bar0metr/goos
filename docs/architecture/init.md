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
1. Init blocks on that channel, issues a correlated launch request, waits for
the result, and continues only after the replacement Process and Task exist.

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

PID 1 also supplies a `GST2` startup template with a unique role name for each
manifest entry. After installing all capabilities, the kernel writes their
actual generation-safe IDs into the startup block before creating the first
Task. Services resolve roles rather than depending on handle-table insertion
order. Names are descriptive metadata; type and rights enforcement remains in
the kernel handle table.

Shell exec requests are restricted to simple names below `/system/bin`. PID 1
creates a VFS channel pair, moves its server endpoint to
VFS, supplies the client endpoint and console-write capability to the child,
forwards bounded startup data, waits through a capability-protected process
handle, and replies only after process exit.
