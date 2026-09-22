# ADR 0062: Startup data through shared memory

## Status

Accepted. Supersedes the size limits of ADR 0031 and ADR 0032.

## Context

PID 1 sent each process's GST2 startup template inline in the 256-byte
lifecycle launch message, next to the executable path and up to 16 bytes per
granted capability. The template was therefore limited to 128 bytes. The kernel
kept the bound block in a 128-byte array in every process record.

A utility with the four standard roles (`in`, `stdout`, `stderr`, `vfs`) spends
about 70 bytes on the header, handle bindings, and `PATH`, which leaves roughly
50 bytes for the command, working directory, and arguments. The spec's
`write /home/user/notes.txt "Hello from GoOS"` did not fit, so the M7 test wrote
its file through `echo … >`.

The shell imposed further limits. Each stage's argv and environment were
encoded into a 96-byte buffer, a single argument was limited to 63 bytes, and
PID 1 copied each stage's argv into a 128-byte array.

## Decision

The startup ABI becomes `GST3`. The encoding is unchanged, the block is at most
1024 bytes (`startup.Size`, `kcall.StartupSize`), and `StartupRead` copies
exactly that many bytes. The GST3 length fields bound the largest encodable
block at 818 bytes, so any valid combination of command, working directory,
argv, environment, and seven handle bindings fits.

**Launch staging page.** When the kernel creates PID 1, it also creates a
one-page shared-memory object. The object is granted to PID 1 as handle
`0x100000013` with `Read|Write`, and mapped through that handle as writable and
non-executable at `supervisor.StagingAddress` (`0x401ff000`). The page holds
four slots of 1024 bytes.

For each launch, PID 1 encodes the template straight into the next slot, and
the lifecycle launch message, now version 6, carries only the slot number and
template size. In the kernel, `consumeInitControl` copies the slot into a
kernel buffer once, before it validates the template or binds handles. Changes
PID 1 makes to the page afterwards therefore cannot affect a launch already in
progress. Launches are synchronous in PID 1, so four slots leave room for a
launch whose reply timed out.

**Child startup page.** After binding the installed handle IDs, the kernel
writes the block into a freshly allocated page. It maps the page into the child
as user, read-only, and non-executable at `vm.UserStartupAddress`
(`0x40001000`), the unused page below the image base. The page is an owned
mapping, so `DestroyUserAddressSpace` frees it together with the process on
every exit and failure path. `StartupRead` copies from that page, or returns
zeros for a process that has none. The process record no longer stores startup
data.

**Argument vectors.** `startup.EncodeVectors` now takes a 256-byte buffer and
limits argv and the environment to 255 bytes each, the width of their GST3
length fields. A single shell argument can be up to 127 bytes, the most that
fits on a 128-byte TTY line. PID 1 holds each stage's argv in 255 bytes.

The exec request from the shell to PID 1 is unchanged and remains the outer
bound: one 256-byte IPC payload for every stage. With inline text, `write`
splits its data across as many VFS write requests as it needs.

## Alternatives

A general user-mapped shared-memory call would let PID 1 create one staging
object per launch and hand it to the kernel. That needs mapping lifetime rules
for every process, and nothing but PID 1 would use it yet.

Sending the template as several lifecycle messages would keep the page out of
the ABI, but it adds reassembly state and ordering rules to a path that must
not block.

Growing the per-process array to 1024 bytes would add 32 KiB of kernel data
that sits unused for most of a process's lifetime, and the launch message
would still be too small.

## Consequences

`write PATH TEXT` from the spec works as written, and the M7 test uses it. The
boot test also writes a line whose startup block is more than 128 bytes.

PID 1 has one more fixed handle and one more mapped page. Every launched process
holds one read-only page for its startup block. The staging page is writable
by PID 1 alone. A malformed slot is caught by the same GST3 validation as
before and fails the launch with `LaunchInvalidImage`.
