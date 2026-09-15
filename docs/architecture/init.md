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
firmware handoff. The kernel discovers `system/init`, validates its native image,
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
assembly entry supplies the native kernel-call ABI, while a fixed 16 KiB stack
and 64 KiB bump-allocation arena bound runtime memory. The host-side converter
accepts only static x86-64 ELF load segments and wraps them in the validated
GoOS native image envelope.

The Manager, service table, exit handling, restart decisions, backoff, and rate
limit now execute in PID 1. The kernel only delivers process deaths and performs
authorized image loading and process creation. Capability manifests are policy
data in the Manager but are not yet serialized into spawn requests or installed
in the child handle table.
