# Processes and address spaces

GoOS separates resource ownership from execution. A Process owns an address
space, a process-local handle table, metadata, and a bounded collection of Task
identities. A Task is the schedulable CPU context. This avoids treating a
process and a thread as interchangeable kernel objects.

Process IDs and Task IDs reject zero. Each Process currently supports up to 32
Tasks and rejects duplicate ownership. Dynamic process teardown and ID reuse
are deferred until object lifetime and wait semantics are implemented.

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

The kernel loads the process CR3 and enters CPL 3 through a complete `IRETQ`
frame. The native-call entry verifies the saved user CS, preserves the complete
general-purpose register state, and invokes the architecture-independent
dispatcher. Returning calls resume at ring 3; Exit restores the kernel CR3 and
the suspended boot-task stack. Reaching `NATIVE_PROCESS_OK` proves that the
complete call sequence executed.

The native operation, argument, result, and typed-status contract is defined,
but `INT 0x80` remains a bootstrap transport. This program is not loaded from an
executable file. General process loading, process-aware scheduler CR3 changes,
and process teardown remain future work.
