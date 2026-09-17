# Processes and address spaces

GoOS separates resource ownership from execution. A Process owns an address
space, a process-local handle table, metadata, and a bounded collection of Task
identities. A Task is the schedulable CPU context. This avoids treating a
process and a thread as interchangeable kernel objects.

Process IDs and Task IDs reject zero. Each Process currently supports up to 32
Tasks and rejects duplicate ownership. A bounded registry maps architecture
Task exits back to their owning Process. It emits process death only when the
last owned Task exits. Exited registry entries and architecture scheduler slots
are reused after exit delivery, allowing repeated short-lived utilities. Exit
teardown returns guarded kernel stacks, private image pages, and process-owned
page tables to the physical allocator. Shared-memory leaves remain non-owning.

## Address-space isolation

Each process receives a distinct PML4 and PDPT. Supervisor-only kernel mappings
are shared, while user mappings use process-owned lower-level tables. The user
bit is present only along the user mapping path; shared kernel leaves remain
supervisor-only even though both address spaces can execute kernel code.

The live isolation test allocates two physical pages, writes different sentinel
bytes, and maps each page at virtual address `0x40000000` in a different process
root. With interrupts disabled, assembly switches CR3 to process A, reads the
byte, switches to process B, reads again, and restores the kernel CR3. Boot
requires both distinct values before emitting `PROCESS_ISOLATION_OK`.

## First ring 3 process

The bootstrap native process owns separate RX code, read-only data, and RW+NX
stack pages. Its architecture-level program first submits an invalid pointer,
then copies a valid message through the native DebugWrite call, and finally
requests Exit through interrupt vector `0x80`. Code is copied into a zeroed
physical page before that page becomes executable, preserving W^X.

The scheduler loads the process CR3 and enters CPL 3 through a complete `IRETQ`
frame. Timer IRQs save the user context on a task-local guarded RSP0 stack and
may select a kernel task or another process root. The native-call entry verifies
the saved user CS and invokes the architecture-independent dispatcher. Exit
marks the current slot non-runnable and immediately schedules its successor.

PID 1 is discovered in the UEFI-loaded initramfs and loaded into an independent
address space. Both bootstrap test processes are also loaded through the
versioned native image loader.
The second deliberately raises an invalid-opcode exception; the kernel removes
only that userspace task and continues. Kernel-originated exceptions remain
fatal. Initramfs discovery and bounded resource reclamation are complete.
The registry retains a bounded completion record after lifecycle delivery and
resource teardown, allowing waiters to distinguish running, exited, and unknown
processes without retaining reclaimed objects.

Successful launches install a typed process handle in PID 1 with only `Wait`
authority. Native `ProcessWait` validates that handle, blocks on a bounded wait
set, supports timer deadlines, and copies the exit code to validated user
memory. PID 1 uses this path for shell-launched applications and closes the
handle after completion.

Before the first Task becomes runnable, the kernel completes the bounded
versioned `GST1` startup block with the actual generation-safe IDs returned by
capability installation. Applications resolve handles by explicit role names
instead of assuming process-local slot numbers. Startup metadata never grants
rights; the installed handle-table entry remains authoritative.
