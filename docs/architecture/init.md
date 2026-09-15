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

The full policy state machine is still linked into the bootstrap kernel test
path. Moving it into PID 1 requires a freestanding amd64 userspace runtime; the
current PID 1 image remains a deliberately minimal generated native program.
