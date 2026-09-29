# ADR 0065: Kernel heap discipline

## Status

Accepted.

## Context

TinyGo builds the kernel with `gc: leaking` (`tinygo/targets/uefi-amd64.json`).
`runtime.alloc` is a bump allocator over a 64 MiB arena and nothing is ever
freed, so every heap allocation the kernel makes is permanent. The arena also
cannot grow: `runtime.growHeap` asks UEFI boot services for more memory, and
the kernel has exited boot services long before it serves its first command, so
the call faults and the machine dies with no output on the serial line.

The consequence is that an allocation on a repeating path is a countdown. The
launch path allocated about 5.5 KiB per command — a `process.Process` returned
by value, the loader's page-frame array, the hard-link name the initramfs
lookup converted to a string, the path the lifecycle event converted to a
string, and a closure over the exiting process during teardown — which gave the
system roughly twelve thousand commands before it stopped. That is invisible in
testing and fatal in use, and nothing in the build reported it.

Go makes this easy to get wrong because the allocation is rarely written down.
TinyGo places a local on the heap whenever its address escapes the function,
and a pointer handed to a function that is not inlined counts as escaping. A
255-byte path buffer that is only read by a helper, an array of page frames
only walked by a rollback loop, a struct returned by value into a caller's
slot: each of these is heap memory in the kernel, and each looks like a stack
variable in the source.

## Decision

Kernel code on a repeating path does not allocate. A repeating path is anything
reachable from a native call, from `serviceKernel`, or from a process launch or
exit. Boot-time code and the validation self-tests may allocate, because they
run once.

Four patterns carry the rule.

**Shared fixed buffers.** State that a helper needs to read or write lives in a
buffer owned by the enclosing structure or by the package, not on the stack.
The native-call encode and decode buffers work this way, and so do the kernel
log's message and field buffers. `vfs.Namespace` now holds the two canonical
path buffers its operations need — `resolved` for `Resolve` and `requested` for
the operations that canonicalize a path before resolving its parent — and
`canonicalPath` writes into the one its caller owns. `loader.LoadNative` keeps
its page-frame array in the package. These buffers are safe because the kernel
runs on one CPU inside an exclusion domain (ADR 0053) and one lookup or one
image load is in flight at a time.

**In-place `Reset` and `Init` instead of returning big structures by value.**
A constructor that returns a `Process`, a `Channel`, a `Stream`, or a RAMFS
node builds the whole value as a temporary before the caller can store it.
`process.Init(target, id, abi, space)`, `(*ipc.Channel).Reset`,
`(*stream.Stream).Reset`, and the RAMFS node's `reset` prepare a slot through
its pointer instead. Zeroing in place — `*slot = T{}` — compiles to a memset
and is free; a composite literal with non-constant fields is not, so a `Reset`
zeroes first and assigns the fields afterwards. The by-value constructors stay
where host tests and the boot self-tests use them.

**No string conversions.** `string(bytes)` and `[]byte(string)` both copy onto
the heap. Paths and names therefore travel through the kernel as `[]byte` from
end to end: `supervisor.LifecycleEvent.ExecutablePath` returns the event's own
bytes through a pointer receiver, `spawnExecutable` and `process.SetName` take
bytes, and `initramfs.Find` takes and follows hard links as bytes. Where a
string is genuinely wanted at a boundary, the repo keeps a byte-taking
companion, as `validLaunchPath` and `validLaunchPathBytes` already did.

**No escaping locals.** A closure that captures anything allocates its context,
so `releaseProcessHandles` passes a top-level function to `CloseAll` and names
the process it is tearing down in a package variable. Concatenation allocates,
so `mark` writes its marker and its newline as two calls.

## Alternatives

A real garbage collector would remove the constraint, but a conservative
collector has to scan the kernel's stacks and identity-mapped memory, and it
would have to run with interrupts disabled inside the exclusion domain. The
kernel's working set is bounded and static; it does not need one.

Reserving the arena from the UEFI memory map and letting `growHeap` extend it
would keep allocation working after boot services, but it converts a hard limit
into a slow one and leaves the same leak in place.

Pooling the launch-path objects would recycle memory without changing the
calling conventions. The structures are already fixed arrays owned by the
kernel — `spawnedProcesses`, the channel and stream registries — so the pool
would only paper over the temporaries created on the way into them.

## Consequences

An idle system allocates nothing, and a launched command allocates nothing:
`runtime.heapptr` does not move across ten `uname` or ten `ls /` commands,
against about 5504 bytes per command before. The heap is now a boot-time budget
rather than a lifetime one.

Two checks keep it that way. The build is inspected with `objdump`:

```sh
make iso VALIDATION=1
objdump -d build/variants/hz100-validation/kernel/goos.efi \
  | awk '/^[0-9a-f]+ </{fn=$0} /callq.*<runtime.alloc>/{print fn}' \
  | sort | uniq -c | sort -rn
```

Every caller that remains must be boot-only: `runtime.*`,
`device/uefi.TicksFrequency`, `internal/task.start`,
`boot/uefi.LoadInitramfs`, `main.main`, and `main.runKernelSelfTests` with the
`process.New`, `ipc.NewChannel`, and `runtime.stringFromBytes` it reaches. A
new name in that list is a regression. The running system is checked by reading
`runtime.heapptr` — its address is in `build/variants/hz100/kernel/goos.sym` —
from the QEMU monitor before and after a batch of commands:

```sh
(printf 'x/1gx 0x<addr>\n'; sleep 0.4) | nc -U "$TMPDIR/mon.sock"
```

The boot test does this on every run: it attaches a monitor to the main phase,
reads the pointer at the first prompt and again after the last command, and
fails `kernel-heap.stable` if the two differ. Where the monitor or `nc -U` is
missing, the check reports itself as skipped rather than passing quietly.

Host tests use `testing.AllocsPerRun` where the code is reachable from them —
`internal/logfmt/logfmt_test.go`, `kernel/vfs/vfs_test.go`, and
`kernel/process/process_test.go`. That guard is weaker than the build: the host
compiler's escape analysis keeps values on the stack that TinyGo puts on the
heap, so a package can pass `AllocsPerRun` and still allocate in the kernel.
The `objdump` list is the authority.

The cost is style. Functions take a destination pointer instead of returning a
value, some buffers are package state instead of locals, and paths are bytes
rather than strings. The kernel's structures are fixed-size arrays already, so
this mostly makes an existing property visible.
