# Adding a native service

A service is a ring-3 TinyGo program in the initramfs, launched and supervised
by PID 1 with an explicit capability manifest. Use the block service
(`userspace/cmd/block`, `userspace/block`) as the reference: it is small, has
two endpoints, and uses the current wait and startup APIs.

## 1. Layout

```text
userspace/<name>/            host-testable protocol and logic, no unsafe, no kcall stubs
userspace/<name>/*_test.go   codec round trips, malformed input, bounds
userspace/cmd/<name>/main.go freestanding entry point and native-call glue
```

The protocol package defines a protocol identifier, a version, fixed field
sizes, and encode/decode functions that reject non-zero reserved fields and
out-of-range sizes. Payloads must fit the inline IPC limit (256 bytes before
the protocol header). See `userspace/block/protocol.go`.

## 2. Entry point

`userspace/cmd/<name>/main.go`:

- starts with `//go:build goos_userspace` and `package main`;
- exports `userspaceMain` (called by `_start` in `userspace/runtime/amd64.S`)
  and keeps an empty `func main() {}`;
- declares the native calls it uses with `//go:linkname`, for example
  `//go:linkname startupRead goos_startup_read`. The stubs are in
  `userspace/runtime/amd64.S`;
- provides the runtime hooks (`runtime.initHeap`, `runtime.alloc`,
  `runtime.free`, `runtime.GC`, ...) as a bump arena between `_heap_start` and
  `_heap_end`. `runtime.alloc` **must zero** every allocation (ADR 0048). Copy
  the hooks from an existing service.

## 3. Startup and named handles

At start, read and decode the startup block, then resolve every role by name:

```go
if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
    fatal("NAME_STARTUP_UNAVAILABLE\n")
}
data, ok := startup.Decode(startupWire[:])
if !ok {
    fatal("NAME_STARTUP_INVALID\n")
}
consoleHandle, ok = data.Handle("console")
```

Never assume handle numbers. Roles are lowercase, at most 15 bytes, unique;
there are at most seven (ADR 0031). A missing role is a fatal startup error.

## 4. Service loop

Wait on all endpoints with one readiness-only `WaitAny`, then receive from each
ready endpoint:

```go
arenaBase = heapPointer
for {
    heapPointer = arenaBase // request-bounded allocation
    var interests [userwait.MaxObjects]userwait.Interest
    for index, endpoint := range endpoints {
        interests[index] = userwait.Interest{Handle: endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
    }
    result, status := userwait.Any(&interests, len(endpoints), kcall.WaitAnyInfiniteTimeout)
    if status != kcall.StatusOK {
        fatal("NAME_WAIT_FAILED\n")
    }
    for index, endpoint := range endpoints {
        if result.ReadyMask&(1<<index) != 0 {
            // ipcReceive, decode, dispatch, reply
        }
    }
}
```

Rules:

- `WaitAny` never consumes data; always follow it with the typed receive.
- Reset the arena once per request so memory use stays bounded.
- Reply to every request you can attribute, including malformed ones, with an
  error status. Silence strands the caller (ADR 0048).
- Treat `PeerDied` on a client endpoint as that client being gone: close the
  handle and continue if the service can, instead of stopping.
- Every outbound request to another service uses a bounded deadline, not an
  infinite wait.

## 5. Build rules

In the `Makefile`:

1. Add `<NAME>_ELF := $(BUILD_DIR)/userspace/<name>.elf` and
   `<NAME>_IMAGE := $(BUILD_DIR)/userspace/<name>`.
2. Add an ELF rule modelled on `$(BLOCK_ELF)`. Its prerequisites list every
   source directory the program imports, the userspace target JSON, the linker
   script, `$(INIT_RUNTIME)`, and `$(BUILD_CONFIG)`, so `HZ` and `DIAG`
   changes rebuild it. It builds with
   `-target=tinygo/targets/goos-userspace-amd64.json` and `$(TINYGO_TAGS)`.
3. Add an image rule that runs `tools/elf2native` on the ELF.
4. Add `$(<NAME>_IMAGE)` to the `userspace` target and to the `initramfs`
   recipe.
5. Add the host test package to `test-security` if it validates untrusted
   input.

## 6. Initramfs

`tools/initramfs` takes a fixed positional list of service images and writes
each to a fixed path. Add an argument and a `system/<name>` entry (mode
`0o555`). Programs launched by name from the shell are commands, not services:
they belong in `userspace/bin/` and need none of this (see
[Adding a command](adding-command.md)). The kernel
looks up executables by exact path in this archive; there is no other source
of code.

## 7. Supervision and capabilities

Register the service in `userspace/cmd/init/main.go` with a
`supervisor.Definition`:

```go
supervisor.Definition{
    Name:         "name",
    Executable:   "/system/name",
    Dependencies: []string{"vfs"},
    Capabilities: []supervisor.Capability{
        {Name: "console", Handle: serviceDeviceHandle, Type: handle.TypeDevice, Rights: capability.Write},
        {Name: "service", Handle: serverEnd, Type: handle.TypeChannel, Rights: capability.Read | capability.Write | capability.Wait, Consume: true},
    },
    Restart: supervisor.RestartOnFailure,
}
```

- Grant the minimum rights. Never grant Transfer unless the service must
  forward the handle.
- Mark exclusive endpoints `Consume: true` so PID 1 drops its copy.
- Dependencies must already be registered; order is the start order.
- Limits: 16 services, 8 dependencies, 7 capabilities per service.

For a new channel between the service and a client, prefer creating it in
PID 1 with `ChannelCreate` (PID 1 only) and granting one end to each side.
Adding a boot-time kernel channel requires editing the fixed PID 1 handle
layout in `kernel/main.go` and `userspace/cmd/init/main.go` together.

## 8. Tests and documentation

- Host tests for the protocol package: round trips, every rejection path,
  overflow at size boundaries.
- A QEMU assertion in `tests/boot/qemu_boot.sh` in the matching group
  ([testing.md](testing.md)). Validation checks are moving to a separate
  validation build (roadmap Priority 4); put service self-checks there once
  it exists rather than in the normal boot path.
- An ADR for the protocol and the service boundary.
- An architecture document, linked from `docs/architecture/overview.md` and
  listed in the `docs` target.
- A row in [docs/security/capabilities.md](../security/capabilities.md) for
  the new holder.
