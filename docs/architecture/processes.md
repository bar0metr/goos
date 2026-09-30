# Processes and address spaces

GoOS separates resource ownership from execution. A Process owns an address
space, a process-local handle table, immutable userspace ABI identity,
metadata, and a bounded collection of Task identities. A Task is the
schedulable CPU context. This avoids treating a process and a thread as
interchangeable kernel objects.

The ABI is set by the kernel when it initializes a process from a validated
executable and has no runtime setter. A native image selects `Native` and a
validated Linux ELF selects `LinuxX8664`. All tasks in a process share the ABI.
Entry gates authorize this field explicitly and never infer an ABI from a call
number (ADR 0132).

Process IDs and Task IDs reject zero. Each Process currently supports up to 32
Tasks and rejects duplicate ownership. A bounded registry maps architecture
Task exits back to their owning Process. It emits process death only when the
last owned Task exits. Exited registry entries and architecture scheduler slots
are reused after exit delivery, allowing repeated short-lived utilities. Exit
teardown returns guarded kernel stacks, private image pages, and process-owned
page tables to the physical allocator. Shared-memory leaves remain non-owning.

## Address-space isolation

Each process receives a distinct PML4. It shares the supervisor-only direct-map
slot but does not map the low kernel image. Private supervisor branches map the
high assembly trampolines, descriptor state, and the active task's high kernel
stack. User mappings use process-owned lower-level tables and carry the user
bit at every level. Ring 3 cannot read or execute the transition mappings.

The live isolation test allocates two physical pages, writes different sentinel
bytes, and maps each page at virtual address `0x00400000` in a different process
root. With interrupts disabled, assembly switches CR3 to process A, reads the
byte, switches to process B, reads again, and restores the kernel CR3. Boot
requires both distinct values before emitting `PROCESS_ISOLATION_OK`.

## First ring 3 process

The bootstrap native process owns separate RX code, read-only data, and RW+NX
stack pages. Its architecture-level program first requires the retired DebugWrite number to
be refused, submits an invalid pointer to `ConsoleWrite`, then copies a valid
message through `ConsoleWrite` with the console capability it was given, and
finally requests Exit through interrupt vector `0x80` (ADR 0094). Code is copied into a zeroed
physical page before that page becomes executable, preserving W^X.

The scheduler restores contexts under the kernel CR3. To enter CPL 3, it moves
to the high return prefix, loads the process CR3, and executes `iretq`. A user
interrupt first lands on a task-local guarded high RSP0 stack. Its high assembly
trampoline loads the kernel CR3 before transferring to the low-linked handler.
Timer IRQs may then select a kernel task or another process. The native-call
handler verifies the saved user CS and the owning Process's `Native` ABI before
invoking any normal or blocking native dispatcher. Exit marks the current slot
non-runnable and immediately schedules its successor (ADR 0120, ADR 0132).

PID 1 is discovered in the UEFI-loaded initramfs and loaded into an independent
address space. Both bootstrap test processes are also loaded through the
versioned native image loader.
The second deliberately reads a supervisor-only high trampoline mapping. The
resulting page fault removes only that userspace task and proves that the
transition mapping remains inaccessible from ring 3. Kernel-originated
exceptions remain fatal. Initramfs discovery and bounded resource reclamation
are complete.
The registry retains a bounded completion record after lifecycle delivery and
resource teardown, allowing waiters to distinguish running, exited, and unknown
processes without retaining reclaimed objects.

Every launch after PID 1 names its executable as an extent of the system image
together with PID 1's image capability, which must carry Execute; PID 1 has
resolved the path in its own read-only mapping of the image, and the kernel
checks only the authority, the bounds, and the native image (ADR 0093). A
program outside `/system` is named by a sealed file-backed source under its own
Execute right. The source owns a stable VFS open object and one retained header
page; process faults fetch the remaining bytes by offset (ADR 0121).

Successful launches install a typed process handle in PID 1 with `Wait` and
`Write` authority; `Write` authorizes `ProcessKill` (ADR 0067). Native `ProcessWait` validates that handle, blocks on a bounded wait
set, supports timer deadlines, and copies the exit code to validated user
memory. PID 1 uses this path for shell-launched applications and closes the
handle after completion.

Process handles can also participate in the readiness-only `WaitAny` operation.
The kernel validates their type and `Wait` authority and publishes completion to
a table keyed by the full generation-safe Task ID. `ProcessWait` remains the
operation that consumes and returns the exit code, preserving the existing
single-object interface.

Before the first Task becomes runnable, the kernel completes the bounded
versioned `GST4` startup object with the actual generation-safe IDs returned by
capability installation. Applications resolve handles by explicit role names
instead of assuming process-local slot numbers. Startup metadata never grants
rights; the installed handle-table entry remains authoritative.

The startup object also carries the command, the working directory, `argv`,
and the environment as a table of offsets into a string area, bounded by its
64 KiB size rather than by counts (ADR 0109). PID 1 passes the template
through a shared staging area, and the child receives the bound object on
read-only pages at `vm.UserStartupAddress` (ADR 0062, ADR 0109). The exec
control protocol returns final child status separately from launch status, so
PID 1 can report application failure without confusing it with loader or
capability-installation failure. The read-only `SystemInfo` call exposes a
fixed-size snapshot for native `ps` and `mem` without granting access to kernel
structures.

`GST4` remains the kernel-side source of validated launch inputs for a Linux
launch, but it is not exposed as the Linux application ABI. The stack builder
copies its command, arguments, and environment into a bounded System V
stack containing `argc`, pointer vectors, auxv, NUL-terminated strings, and
16 random bytes when the CPU has a generator.
Resolved GoOS handles and the working directory remain kernel-side
compatibility state. Only occupied initial-stack pages are mapped before the
task runs; the rest of the Linux stack reservation remains demand-zero.
The launch path binds this state to a `LinuxX8664` Process only after independent ELF
validation and removes it on failed launch or process teardown.

Each Linux task also has at most one fixed continuation slot for an asynchronous
syscall. The slot is keyed by the full generation-safe Task ID, not the reusable
scheduler index. Kill, fault, and process teardown cancel it before task-slot
reclamation, so a late VFS reply cannot wake a replacement task.

The compatibility state also holds the process's VFS session and its Linux
identity. The session is the channel PID 1 granted, a request counter, and a
failed flag that peer death sets and nothing clears (ADR 0138). The identity
is a PID between 2 and 32767, allocated at launch and unique among running and
zombie Linux processes. A bounded family record maps it to the generation-safe
GoOS Process ID and retains parentage plus wait status after resource teardown
(ADR 0154). Linux programs never see a native process or task ID. The working
directory starts as the one the launch supplied, held in canonical form as
the base for relative paths (ADR 0140), and `chdir` replaces it. The state
also names the process's file I/O memory object, created at its first file
read or write (ADR 0141), and holds the signal dispositions and blocked mask
the program registered, which nothing acts on (ADR 0151).

## Teardown liveness

Process teardown separates what other parties observe from what merely reclaims
resources. Once the kernel knows a process has exited it records the completion,
wakes every waiter, and closes the process's handles so that channel and stream
peers observe closure. Reclaiming the guarded kernel stack and the address space
is attempted independently; a failure there is counted and reported and never
prevents the steps above, because a waiter or a peer would otherwise block
forever on a process that has already ended.

The completion record is written unconditionally rather than depending on a
bounded queue having room, and lifecycle messages that the channel cannot accept
yet are retained and retried by the kernel loop instead of being dropped.

## Resource accounting

A process has a typed resource account alongside its handle table (ADR 0072,
0122).
Channels, streams, tasks and pages are charged to the process that causes them
to exist and released when they cease to; a charge that would exceed the quota
is refused and changes nothing, so a refused request never reaches the global
table. A quota is not a capability: it cannot be transferred, and no process can
raise its own or another's.

| Resource | `tiny` | `normal` | `large` | PID 1 | Charged at |
| --- | ---: | ---: | ---: | ---: | --- |
| channels | 2 | 4 | 8 | 256 | `ChannelCreate`, released when the registry frees the channel |
| streams | 8 | 16 | 32 | 64 | `StreamCreate`, released when the registry frees the stream |
| tasks | 1 | 4 | 8 | 8 | task binding, released when the task exits |
| pages | 2048 | 8192 | 16384 | 16384 | launch, demand-zero faults and the page tables they need, `MemoryMap`, `DMAAllocate`, `ImageMap`; released by `VMUnmap`, `VMDiscard`, and teardown |

PID 1 chooses a profile in each launch request. Commands and services default
to `normal`, `quota` uses `tiny`, and `fsck-fat` and `vmprobe` use `large`.
The kernel maps the profile to the table above; the request carries no numeric
limits.

PID 1 holds the larger share because it creates the endpoints every other
process communicates through. The share is granted once, at boot, and
`GrantSystemQuota` refuses every other process.

Channels and streams are charged to their creator until the object is released
rather than until the creator's handle closes, because both endpoints can be
transferred and the object outlives them. Launch memory is measured as the
difference in free pages across address-space creation and image loading, and
charged before the task exists, so an image that does not fit fails to launch.
Everything after that is charged as it is touched: heap, stack, BSS, and
anonymous pages arrive on demand (ADR 0104), and a process whose quota is spent
when it touches a new page is retired with a logged `page quota exhausted`.

CPU is sampled rather than measured: the timer charges a tick to the task it
interrupted, and a task's count folds into its process when it exits. `ps`
reports memory and CPU per process, and `quota` prints each process's held and
permitted share of every resource, which makes the applied profile inspectable.
