# Native concurrency audit

A native program may run several OS threads (ADR 0203) and, when built with
the `goos_goroutines` tag, many goroutines scheduled over them. This document
states, for every library under `userspace/`, which of its exported calls may
be made from two goroutines or threads at once, and, for every package-level
variable, who owns it and what keeps it consistent.

The machine-readable part is
[`native-concurrency-audit.tsv`](native-concurrency-audit.tsv): one row per
package-level variable, with a stable ID (`CA-0001` onwards), the package, the
symbol, its owner, its contract, and the test that exercises it.
`tools/concurrencyaudit` parses every non-test Go file under `userspace/`,
whatever its build tags, and fails when a package-level variable has no row
or a row names a variable that no longer exists:

```sh
make test-concurrency-audit   # the tool's tests, then the check itself
make test-userspace-libs      # the libraries' concurrency tests under -race
go run ./tools/concurrencyaudit -list   # every variable with file:line
```

A new variable takes the next unused ID; an ID is never reused for another
symbol. The C programs under `userspace/linux` use the Linux ABI and their
own runtime; only the Go fixture table in that directory is audited here.

## Terms

Owner, the `owner` column:

- `process`: one instance for the whole process.
- `thread`: one per OS thread (an M). State of this kind is never
  goroutine-local: a goroutine can move to another thread between any two
  calls, so only code that cannot be preempted or migrated may rely on it.
- `goroutine`: belongs to one goroutine at a time; handing it over is the
  caller's business.
- `session`: belongs to one service session, connection or request stream,
  and is used by the holder of that session.
- `call`: lives for one call.

Contract, the `contract` column:

- `immutable`: written before the process starts a second thread or
  goroutine (a table, an error value, a handle `Run` or `Init` stores), then
  only read. Starting a thread or goroutine orders those writes before
  everything the new one does.
- `atomic`: `sync/atomic` words.
- `lock`: a `lock.Mutex` (or, inside the runtime, its own spin lock) of the
  object the variable belongs to.
- `owned`: one owner at a time, and the API that exposes it says so.
- `per-thread`: per-M scratch of `userspace/runtime/sys`.
- `runtime`: allocator and collector state with its own protocol
  (ADR 0201, 0203).

API classes, used per package below:

- Concurrent-safe: any goroutine or thread may call it at any time.
- Independent instances safe: distinct values may be used at once from
  different goroutines; one value belongs to one goroutine at a time.
- Single-owner: one goroutine uses it, normally the one running `main`; the
  documentation of the API says which.
- Unusable concurrently (temporary): not safe from two goroutines and not yet
  fixed; listed with the reason.

## Rules for libraries

1. Prefer, in order: buffers on the caller's stack that do not escape;
   request or session objects; state that is immutable after start-up; and
   last, a `lock.Mutex` inside the one object it protects. There is no lock
   for a whole library.
2. A lock is not held across an IPC wait or a blocking stream read, except by
   an object that is itself the serialized resource, and then its
   documentation says so. The service sessions and the standard output
   streams below are such objects.
3. A hot path does not allocate. TinyGo puts a local larger than 256 bytes
   on the heap (`tinygo build -print-allocs` lists them), so a per-call
   buffer of that size becomes a per-object buffer or a locked pool. Buffers
   passed to `sys` wrappers are copied through `sys`'s own descriptors.
   The userspace target's default stack is 4 KiB, which rules out large
   stack buffers too.
4. Output: one logical record (an output line, a diagnostic, a log event) is
   assembled in one buffer and written whole. Records from different
   goroutines never interleave inside a record; their order is not defined.
5. Input: each byte of an input stream reaches at most one reader. An API
   that reads lines or keystrokes needs a single reader and says so.

## `runtime/lock`

`lock.Mutex` is the lock of rule 1. A host build and a program built with
`goos_goroutines` use `sync.Mutex`, so a contended `Lock` parks the goroutine
and lets the scheduler run another on the same thread. A native program
without goroutines, which can still start threads with `thread.Start`, gets a
compare-and-swap word whose waiters retry 64 times and then call
`sys.ThreadYield`. `Lock`, `Unlock` and `TryLock` are Concurrent-safe. A
Mutex is not re-entrant and must not be copied after first use. Tests:
`TestMutexExcludes` (2, 4 and 8 goroutines), `TestTryLock`.

## `utility`

`utility` is the runtime of the commands in `/system/bin` and of any program
that calls `utility.Run`. `Run` stores the startup handles before it calls
`main`; everything after that point follows the classes below.

Concurrent-safe: `Exit`, `Usage`, `Fail`, `FailStatus`, `Check`,
`CheckWritten`, `ResolveInto`, `Clock`, `ClockNanos`, `ClockNow`,
`SystemInfo`, `ProcessInfo`, `Equal`, `Write`, `WriteError`, `Mark`,
`ReadByte`, `ReadInput`, `ReadInputWithin`, `Retry` (input contract below),
`BindVFS`, `CallVFS`, `Request`, `Stat`, `ReadDir`, `FilesystemInfo`,
`Mkdir`, `CreateFile`, `Rename`, `Replace`, `Unlink`, `CallNetwork`,
`CallResolver`, `Lookup`, `LaunchAvailable`, `Launch`, `Sleep`,
`TakeResize`, `CreateStream`, `StreamWrite`, `StreamRead`, and the pure
helpers `AppendUint`, `BenchCount`, `BenchChunk`, `AppendBenchResult`,
`AppendBenchRate`, `AppendBenchCount`, `Median`, `TerminalGeometry`,
`TerminalSize`, `SizeUnitFlag`, `AppendSize`, `AppendCount`.

Independent instances safe: `Line` (`NewLine`), `Bulk` (`NewBulk`,
`ReadFile`, `WriteFile`), `CopyFile` and `SaveFile` with different Bulks,
`Stream`.

Single-owner:

- `Run`, once, from the program's entry point.
- The current output line: `Print`, `PrintName`, `PrintText`, `PrintUint`,
  `Pad`, `Arguments`, `EndLine`. They build one line in a package buffer;
  two goroutines appending to it would mix their lines. Other goroutines use
  a `Line` over storage they own.
- `Path`, `Target`, `Resolve`: they return package buffers. Other goroutines
  use `ResolveInto`.
- The TCP and UDP sessions: `OpenTCP`, `TCPCall`, `TCPStart`, `TCPWait`,
  `TCPPayload`, `TCPReceived`, `TCPRelease` and their UDP counterparts. A
  session writes through TX slot 0 and takes whichever reply arrives next, so
  two goroutines driving it would overwrite each other's payload and take
  each other's replies. The calls lock the session, so misuse cannot corrupt
  memory, only the protocol.
- `ReadTerminalWithin`: the terminal has one reader.

Input contract: `ReadInput` is one native stream read per call, so goroutines
reading standard input at once each get different bytes and no byte twice.
Which goroutine gets which bytes is not defined.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0722 | `standard` | process | Blocks while a stream is full; no allocation; no callback | `output.mu` covers both standard streams; `writeRecord` holds it for the whole record. The handles are written by `Run` before `main` | One record may need several 256-byte stream writes; the lock keeps them together. Standard output and error usually reach one terminal, so one lock covers both | A full stream is waited on for 5 s at a time, still holding the lock; a stream that stays full or fails ends the process with status 2. A partial write continues from the byte the kernel took. No cancellation | `TestOutputRecordsDoNotInterleave`, `TestErrorTargetFallsBackToStdout` |
| CA-0711, CA-0716 | `currentLine`, `lineBuffer` | goroutine | Writes through `standard` when the line ends or the buffer fills | Owned by the goroutine running `main` | The API is "the current line"; one line has one author | A line longer than 4096 bytes goes out a buffer at a time, each buffer one record | none: single-owner, documented |
| CA-0712, CA-0713 | `errorBuffer`, `failure` | goroutine, process | `Fail` never returns | The failing goroutine takes `failure` and never releases it; it owns `errorBuffer` until the process exits | A 4 KiB diagnostic buffer does not fit a 4 KiB goroutine stack; the first failure's message and status win | A second goroutine that fails waits on `failure` until the exit kills it. `finish` takes no session lock, so a failure while a session is held does not deadlock | `TestAppendFailureConcurrent`, `TestAppendFailureTruncatesAndKeepsNewline` |
| CA-0714 | `files` | session | Blocks on the VFS reply; allocates only to copy a name longer than a reply carries; no callback | `session.mu` from request to reply, then the path page as well (`pathpage.Hold`). The channel is an atomic word | The VFS answers one request of a session at a time; two in flight would take each other's replies | Each wait is 2 s, at most 4 times, then the call fails. A dead VFS fails the call. A reply that arrives after its caller gave up is read by the next caller, which sees the wrong correlation and fails that request. `finish` closes the channel without the lock | `TestSessionExchangesDoNotCross` |
| CA-0715 | `launcher` | session | Blocks until the child exits; creates the launch memory once | `session.mu` from request to the child's exit; the launch memory and path buffer belong to the holder | PID 1 answers when the child ends, and the child shares this command's standard input, so launches are serial | Waits 10 s at a time without limit while the channel lives. A dead channel answers `LaunchProcessFailed`. A resize notice that arrives meanwhile replaces the resize endpoint (`resize` lock, taken inside) | `TestSessionExchangesDoNotCross` |
| CA-0717, CA-0721 | `networkSession`, `resolverSession` | session | Blocks on the reply; no allocation | `session.mu` from request to reply | One request outstanding per session | Bounded by the request's timeout plus 2 s (network) or 95 s (resolver); a dead service or a reply out of protocol fails the call | `TestSessionExchangesDoNotCross` |
| CA-0726, CA-0729 | `tcpSession`, `udpSession` | session | `TCPCall`, `TCPWait`, `UDPCall`, `UDPWait` block on a reply | `session.mu` around each send and each wait | Memory safety; the protocol itself is single-owner (see above) | Replies to other requests are dropped and their RX slots released; a dead service fails the call | `TestSessionExchangesDoNotCross` |
| CA-0725, CA-0727, CA-0728, CA-0730 | `tcpRX`, `tcpTX`, `udpRX`, `udpTX` | session | No | Set once by `OpenTCP` or `OpenUDP` | Mapped pools of the session | Live until exit | none: set before use, single-owner session |
| CA-0720 | `resize` | process | No; never held across a wait | `resize.mu` covers the endpoint, the snapshot and the frame buffer | `Launch` replaces the endpoint while the reader drains it | A failed read drops the endpoint. `ReadTerminalWithin` copies the endpoint into its wait list under the lock and waits without it | none: single reader, documented |
| CA-0719, CA-0724 | `pathBuffer`, `targetBuffer` | goroutine | No | Owned by the goroutine running `main` | The slices `Path`, `Target` and `Resolve` return point into them | None | none: single-owner, documented |
| CA-0709, CA-0710, CA-0723, CA-0718 | `commandName`, `current`, `stdinHandle`, `noInterests` | process | No | Written by `Run` before `main`, or never written | Set-once start-up state | None | none: immutable |

## `pathpage`

One shared-memory page per process carries paths too long for a VFS message
and names too long for a reply. `Hold` and `Release` are Concurrent-safe and
take the page for one request; `Prepare` and `Name` require the page held.
`utility.CallVFS`, the shell and PID 1 hold it from `Prepare` until the reply
and any name are read.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0654 | `page` | process | Held across the VFS wait of its holder | `lock.Mutex` | The page is the serialized resource: the VFS reads the paths and writes the name while the request is in flight | A holder whose VFS call times out releases the page. The VFS handles a session's requests in order, so a late write for an abandoned request lands before the next request's own | none: userspace only |
| CA-0652, CA-0653 | `memory`, `object` | process | Creates the object once | Written once by `ensure` under `page`; read under `page` | Lazy creation | Live until exit | none: userspace only |

## `runtime/pages`

`Pool` is Concurrent-safe: `Init`, `Allocate`, `Free`, `Used` and
`Permanent` take the pool's own lock, held for a few instructions and, in
`Free`, one `VMDiscard`. `Page` reads only what `Init` set and takes no lock.
The contents of a page belong to the goroutine that took it until it gives
it back. The package has no package-level variables. Tests:
`TestPoolConcurrentOwnership`, `TestPermanentConcurrentDistinct`.

## `log`

`Info`, `Warn`, `Error`, `Str`, `Bytes` and `Uint` are Concurrent-safe once
`Init` has run; `Init` is Single-owner and runs before a second thread or
goroutine starts. An event is encoded in buffers on the caller's stack (181
and 96 bytes, under TinyGo's 256-byte stack limit) and handed to the kernel
in one `LogWrite`, so events are whole and may reorder. A failed write
returns false; nothing is retried.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0640, CA-0641 | `component`, `console` | process | No | Written by `Init`, then read | Identity of the component | None | `TestEncodeConcurrentRecordsIntact` |

## `startup`

`Read`, `Decode`, `DecodeTemplate`, `Size` and every `Data` method are
Concurrent-safe: `Data` is a read-only view of the startup object. `Writer`
is Independent instances safe. `BindHandles` changes the template its caller
passes and is Single-owner per template. `equals` (CA-0702) and `magic`
(CA-0703) are immutable tables. Test:
`TestDataConcurrentReaders`.

## `fetchtransport`

An `Adapter` drives the command's TCP session, so it is Single-owner, and so
are the connections and outputs it returns. Its request, reply, path buffer
and file buffer are fields of the Adapter; the package has no package-level
variables. Name resolution goes through `utility.CallResolver` and is
Concurrent-safe.

## `exec`

The launch protocol codecs and builders are Independent instances safe.
`ArchiveMember` and `LocateImage` are Unusable concurrently (temporary): the
member name is built in a package buffer and returned. Only PID 1's single
service loop calls them. The fix is a caller-owned buffer.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0617 | `memberName` | process | No | Owned by PID 1's service loop | A 4 KiB local would be heap-allocated on every launch | None | none: single caller, documented |

## Runtime packages

`runtime/sys`, `runtime/arena`, `runtime/tls` and `runtime/thread` are the
runtime and are maintained with ADR 0203 and its successors; their rows are
listed so the audit stays complete.

- `runtime/sys`: `scratch` (CA-0695) is per-M scratch: the descriptors one
  wrapper call fills and passes to the kernel. A wrapper uses its thread's
  slot only within one call, with preemption disabled during it, so no other
  goroutine runs on that thread meanwhile. It is the only per-thread scratch
  in userspace. Every `sys` wrapper is Concurrent-safe.
- `runtime/arena`: CA-0661 to CA-0694. The heap lock, the collector lock and
  the collector's atomics follow the protocol of ADR 0201 and 0203; the
  remaining rows are collector bookkeeping written under those locks or with
  every other thread suspended, and start-up values. Tests are the package's
  own, among them `TestOwnedSlabsRace`.
- `runtime/thread`: `starting` (CA-0696) is a spin lock over `starts`
  (CA-0697), the table of threads being started. `Start`, `Join`, `Self`,
  `Slot` and `Yield` are Concurrent-safe.
- `runtime/tls` has no package-level variables.

## Other libraries

These packages have no mutable package-level variables. Their rows in the
TSV, where they have any, are immutable tables, error values and constant
addresses.

| Package | Class of the exported API | Notes |
|---|---|---|
| `wait` | Concurrent-safe | `Any` and `Sleep` keep nothing between calls; the caller owns the interest array. |
| `fsproto` | Concurrent-safe | Codecs over a caller's message, cache and name buffer; `Serve` works on the caller's volume. Test: `TestCodecConcurrentCallerBuffers`. |
| `terminal` | Independent instances safe | `Output` batches into caller storage and hands a whole batch to its sink; `Parser` keeps per-instance state. Test: `TestIndependentOutputsAndParsers`. |
| `nativefiles` | Independent instances safe | A `Files` owns its `Bulk`; requests go through `utility`. |
| `tlsclient` | Independent instances safe | A client per connection; `Roots` is immutable once loaded. `LoadSystemRoots` uses the caller's Bulk. |
| `remoteapi` | Concurrent-safe | `Receive`, `Send`, `SendWithin` and `CloseHandles` use a wire buffer per call. |
| `remoteservice` | Independent instances safe | `Receive`, `Send` and `CloseHandles` (CA-0657 to CA-0659) are function values tests replace before anything runs; a `RelayConn` belongs to one worker. |
| `cryptosys`, `cryptoprobe` | Concurrent-safe | Random and clock reads are single native calls. |
| `admincmd` | Single-owner | Command bodies built on `utility`'s current line and `Path`. |
| `shell` | Concurrent-safe | Parsing and formatting over caller buffers; `Commands` (CA-0698) is an immutable table. |
| `editor`, `filemanager`, `viewer`, `tui`, `fsdisplay` | Independent instances safe | Screen state per instance; output through a `terminal.Output`. `tui.DefaultTheme` (CA-0707) is immutable by convention. |
| `network`, `netdev`, `netapi`, `tcpapi`, `udpapi`, `tcpdispatch`, `tcpserve`, `udpserve`, `tcpupstream`, `upstream` | Independent instances safe | Codecs are Concurrent-safe; stacks, pools and dispatch tables are objects run by one service loop. |
| `dns`, `dnswire`, `dnsq`, `dnstcp`, `resolvapi`, `hostname` | Independent instances safe | The resolver cache is a `dns` object owned by the resolver service's loop; codecs are Concurrent-safe. |
| `vfs`, `block`, `fat32`, `partition`, `safesave` | Independent instances safe | Volume, mount and save state per object, run by one service loop or one save. |
| `config`, `release`, `commands`, `truststate`, `truststore`, `trustupdate`, `remotepolicy`, `httpwire`, `fetch`, `sshserver`, `tty`, `init`, `linux` | Concurrent-safe codecs and policy, or Independent instances safe objects | Package-level variables are immutable tables and error values. |

## Programs

The rows for `userspace/cmd/*` and `userspace/bin/*` (CA-0001 to CA-0599)
are state of one program. Every program except `threadprobe` runs one thread
and one loop; its variables are `owned` by that loop or `immutable`, and no
library reaches them. A program that starts threads or goroutines must turn
its rows into one of the other contracts first, as `threadprobe` does:

- `threadprobe` (CA-0120 to CA-0145): the counters, flags and handed-over
  pointers its worker threads share are `sync/atomic` values; `cpuWires`
  (CA-0125) has one slot per thread, indexed by `thread.Slot`; `benchOps`
  and `gcRounds` are set before the workers start; `profile` and
  `profileLine` are used by the main thread only.
