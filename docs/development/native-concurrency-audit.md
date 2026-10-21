# Native concurrency audit

A native program may run several OS threads (ADR 0203) and, when built with
the `goos_goroutines` tag, many goroutines scheduled over them. This document
records intended ownership and concurrency contracts under `userspace/`.
It is an inventory, not a completed library audit. The current hardening and
host failure tests cover utility transactions and path-page reuse; other
package classifications below remain provisional until their call graphs,
callbacks, shared mappings and failure paths have been checked. A complete
variable inventory is not proof of concurrent safety.

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
- `runtime`: state that combines lifecycle, pinning, locks, atomics and
  collector rendezvous protocols (ADR 0201, 0203, 0204). This label does not
  certify arbitrary concurrent calls to the package's exported functions.

API classes, used per package below:

- Concurrent-safe: concurrent callers are supported after initialization,
  subject to the stated ownership, lifetime and failure preconditions.
- Independent instances safe: distinct values may be used at once from
  different goroutines; one value belongs to one goroutine at a time.
- Single-owner: one goroutine uses it, normally the one running `main`; the
  documentation of the API says which.
- Unusable concurrently (temporary): not safe from two goroutines and not yet
  fixed; listed with the reason.

## Runtime and service ownership boundaries

The G and M tables combine per-owner fields with shared CAS transitions and
queues. Host queue, state and stack-slot tests exercise those components; they
do not execute native context switches or prove the full table lifecycle.
The poll registration lock protects registrations, while the poller CAS gives
one M ownership of the snapshot until delivery finishes. Collector plans and
held-thread scratch belong to the active collector under its rendezvous
protocol. A `goprobe` reference in the table names a native fixture, not a
claim that all compiler, failure and KVM acceptance checks have passed.

`arena.SetStackRoots` and `thread.SetHooks` are runtime startup operations,
not concurrent setters. Callers must install them before application workers
start; the APIs do not enforce that ordering. `tls.Of` exposes mutable M state
for runtime use, not goroutine-local storage. M-local pointers require a pin
when migration is possible. The host-only `tls.hostBlock` is a singleton stub;
host tests do not establish native TLS isolation.

The syscall handler slots use atomic pointer publication. Each published
function-value holder must stay immutable until no caller can still read it.
The goroutine runtime's two handler holders satisfy that condition; atomic
publication does not permit a caller to mutate a holder in place.

The VFS driver owns admission, namespace state, replies and session generations.
Workers receive read plans and request-owned targets through channels. One
persistent lane owns the filesystem transport for a whole backend read; serial
barriers wait for preceding retirement before using that state. Driver helper
tests cover policy and bookkeeping. A quarantined cache is reattached only
behind PID 1's exit fence; both drivers apply offline state and attach at the
same owner barriers. Native IPC overlap, failure fences and resource cleanup
still need integration evidence.

## Rules for libraries

1. Prefer, in order: buffers on the caller's stack that do not escape;
   request or session objects; state that is immutable after start-up; and
   last, a `lock.Mutex` inside the one object it protects. There is no lock
   for a whole library.
2. A lock is not held across an IPC wait or a blocking stream read, except by
   an object that is itself the serialized resource, and then its
   documentation says so. The service sessions and the standard output
   streams below are such objects.
3. Keep hot-path scratch in a request, object or bounded pool when stack
   storage would escape. TinyGo may heap-allocate locals and composite
   literals; inspect `tinygo build -print-allocs` and linked frames rather
   than inferring storage from Go syntax. The main native stack reserves
   1 MiB; kernel-created threads reserve 512 KiB each. The runtime uses
   separate 64 KiB scheduler stacks for adopted main/pinned threads and
   reserves at least 256 KiB for a started G. The compiler target's default
   64 KiB request is not the native stack reservation. Stacks have fixed
   virtual bounds and demand-committed pages, without copying or relocation.
   Syscall descriptors remain per-M and pinned across their use.
4. Output: one logical record (an output line, a diagnostic, a log event) is
   assembled in one buffer and written whole. Records from different
   goroutines never interleave inside a record; their order is not defined.
   Fatal/OOM output is best-effort: a busy lock or stream can drop a record
   or its unwritten suffix rather than prevent exit.
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
`Stream`. Saves to one target also share its temporary pathname: only the
save that creates that path owns it. An existing temporary file must not be
deleted by a competing save. `VFSRequests` serves one owner's endpoint set; owners sequence
asynchronous sends and handoffs themselves. A used Bulk must not be copied.
Its Memory and any aliases remain caller-owned; do not replace or resize the
Memory slice.

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
  each other's replies. Locks cover codec storage, not the shared TX/RX
  slices returned to callers; concurrent misuse can race shared memory.
  A timeout or invalid reply disables sends/waits, but does not revoke pool
  slices already returned. Pool ownership and reopening remain unaudited.
- `ReadTerminalWithin`: the terminal has one reader.

Input contract: `ReadInput` is one native stream read per call, so goroutines
reading standard input at once each get different bytes and no byte twice.
Which goroutine gets which bytes is not defined.

Transaction contract:

- Synchronous utility calls hold one session from send through protocol
  validation. A failed wait, exhausted VFS retry budget, invalid reply or
  failed send quarantines that endpoint ID. No later caller sends or drains
  late replies on it. Local validation/allocation failure before a send does
  not by itself abandon an exchange. Correlations never reset on rebind.
- BindVFS waits for the current exchange and borrows the supplied endpoint;
  switching or failure never closes it. Healthy endpoints may be restored,
  as remoted's transient authority bindings require. Quarantined IDs cannot
  be restored. Recovery needs a genuinely fresh channel, not an alias of the
  failed channel. There is no automatic service reconnection, and no public
  cancellation API. An owner abandoning a sent exchange must quarantine both
  the endpoint and every shared object exposed by that exchange.
- BindVFS is not an authority scope for a multi-call operation. Callers must
  externally sequence rebind with ReadDir, CopyFile, SaveFile or any operation
  that requires a stable endpoint. File handles from an old VFS session are
  not valid on a freshly bound one.
- An abandoned path page is unmapped locally and its local handle closed;
  the next request creates a different object. Bulk uses private Memory plus
  shared staging, with one copy per transfer and an extra buffer. Failed
  transfers retire staging without exposing a late server write/read to the
  caller's Memory. A valid reply is the cooperative server's completion
  fence; a malicious server retaining mappings after success is not covered.
- Retiring a local mapping or endpoint does not revoke a peer's mappings or
  cancel a filesystem mutation. A peer retaining them may retain quota until
  it releases them or exits. Quarantined borrowed endpoints remain the
  original owner's responsibility. The failed-ID list grows with failures.
- VFS Name/Data are caller-owned on success, including attached names. Reply
  handles not accepted by the protocol are closed. Host transport tests do
  not establish kernel/native runtime correctness or OOM behavior in a VM.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0722 | `standard` | process | Blocks while a stream is full; no allocation; no callback | `output.mu` covers both standard streams; `writeRecord` holds it for the whole record. The handles are written by `Run` before `main` | One record may need several 256-byte stream writes; the lock keeps them together. Standard output and error usually reach one terminal, so one lock covers both | A full stream is waited on for 5 s at a time, still holding the lock; a stream that stays full or fails ends the process with status 2. A partial write continues from the byte the kernel took. No cancellation | `TestOutputRecordsDoNotInterleave`, `TestErrorTargetFallsBackToStdout` |
| CA-0711, CA-0716 | `currentLine`, `lineBuffer` | goroutine | Writes through `standard` when the line ends or the buffer fills | Owned by the goroutine running `main` | The API is "the current line"; one line has one author | A line longer than 4096 bytes goes out a buffer at a time, each buffer one record; a Line with empty storage writes each append directly | none: single-owner, documented |
| CA-0712, CA-0713 | `errorBuffer`, `failure` | goroutine, process | Fatal output never waits for a lock or stream | TryLock gives one writer the diagnostic buffer; output uses TryLock and nonwaiting native writes | Recursive OOM must be able to exit while any utility lock is held | Busy locks drop the diagnostic; a full stream drops the remaining bytes. Racing exits have unspecified status. Native exit itself is not host-tested | `TestFatalOutputDoesNotWaitForHeldLock`, `TestFatalOutputDropsRecordWhenWriterCannotProgress` |
| CA-0714 | `files` | session | Bounded reply waits; reply copies and quarantine IDs can allocate | Session lock covers exchange and rebind, then path-page lock; availability is atomic | One request outstanding; response storage belongs to caller | Four 2 s waits; timeout, peer death and malformed replies quarantine the borrowed endpoint and retire exposed path memory; no automatic reconnect | `TestVFSAbandonedExchangeQuarantinesEndpoint`, `TestVFSRebindWaitsForWholeExchange`, `TestVFSReplyBuffersBelongToCaller`, `TestVFSConcurrentAttachedNamesStayWithCaller` |
| CA-0715 | `launcher` | session | Blocks until the child exits; creates the launch memory once | `session.mu` from request to the child's exit; the launch memory and path buffer belong to the holder | PID 1 answers when the child ends, and the child shares this command's standard input, so launches are serial | Waits 10 s at a time without limit while the channel lives. A dead channel or invalid reply disables the endpoint and answers `LaunchProcessFailed`; launch memory is never reused on that failed endpoint. A resize notice that arrives meanwhile replaces the resize endpoint (`resize` lock, taken inside) | `TestSessionExchangesDoNotCross` |
| CA-0717, CA-0721 | `networkSession`, `resolverSession` | session | Blocks on the reply; quarantine may allocate | `session.mu` from request to reply | One request outstanding per session | Bounded by the request's timeout plus 2 s (network) or 95 s (resolver); timeout, a dead service or an invalid reply fails the call and disables the endpoint | `TestSessionExchangesDoNotCross` |
| CA-0726, CA-0729 | `tcpSession`, `udpSession` | session | `TCPCall`, `TCPWait`, `UDPCall`, `UDPWait` block on a reply | `session.mu` around each send and each wait | Codec storage only; mapped pools and protocol are single-owner (see above) | Replies to other requests are dropped and their RX slots released; timeout, a dead service or invalid protocol disables the endpoint; previously exposed pool slices remain mapped | `TestSplitProtocolRejectionQuarantinesAndReleasesHandles` |
| CA-0725, CA-0727, CA-0728, CA-0730 | `tcpRX`, `tcpTX`, `udpRX`, `udpTX` | session | No | Set once by `OpenTCP` or `OpenUDP` | Mapped pools of the session | Live until exit | none: set before use, single-owner session |
| CA-0720 | `resize` | process | No; never held across a wait | `resize.mu` covers the endpoint, the snapshot and the frame buffer | `Launch` replaces the endpoint while the reader drains it | A failed read drops the endpoint. `ReadTerminalWithin` copies the endpoint into its wait list under the lock and waits without it | none: single reader, documented |
| CA-0719, CA-0724 | `pathBuffer`, `targetBuffer` | goroutine | No | Owned by the goroutine running `main` | The slices `Path`, `Target` and `Resolve` return point into them | None | none: single-owner, documented |
| CA-0709, CA-0710, CA-0723, CA-0718 | `commandName`, `current`, `stdinHandle`, `noInterests` | process | No | Written by `Run` before `main`, or never written | Set-once start-up state | None | none: immutable |

## `pathpage`

One two-page object per process carries long paths and returned names.
Hold/Release serialize access. Prepare, Name and Abandon require Hold.
After a successful send, either validate the reply and copy Name, or Abandon
before Release. The old server may still write its retained mapping, so
merely releasing a lock or rejecting a late correlation is insufficient.

Utility, shell and PID 1 use the same transaction implementation and
endpoint quarantine. Shell and PID 1 keep a `VFSRequests` object; their
service-loop correlation sequence covers synchronous and asynchronous VFS
requests. Call uses an absolute budget, including page preparation, and does
not restart a timeout while discarding replies. A wrong correlation, including
a future reply, fails closed. Shell retains the failed handle until PID 1
hands it a fresh session over its existing exec channel. It does not request
new authority or reconnect on its own.

PID 1's seven ordinary VFS control exchanges also match correlation and reject reply
handles. A filesystem replacement notice the VFS sent before the awaited
reply is kept for PID 1 and does not fail the exchange; any other
notification still does, and a notice does not extend the budget. Between
exchanges PID 1 drains the endpoint without waiting through the same object,
keeping notices and discarding stray messages, and never reads a quarantined
endpoint. Filesystem replacement notices use the independent recovery endpoint;
its own request owner does not share quarantine with the ordinary control. Their result separates `sent` from `completed`: after an unsent
request the caller closes its attachments; after a successful send VFS owns
them, even if its reply never arrives. This applies to AddSession authority,
mount/unmount, scope installation, publication and sync. A valid VFS error
status completes an exchange and does not quarantine its endpoint. Work
that might already have finished but withheld its reply supplies no observed
completion fence, so timeout still quarantines the endpoint.

Async executable preparation validates the operation, correlation, handle
count and expected header size before adopting the source. Invalid replies
close their handles and quarantine the channel before cleanup, so cleanup
does not send Close into an unresolved exchange. Cancellation/expiry closes
the owner's unique object and endpoint; it does not revoke a retained peer
mapping or reuse that object.

Recovery needs existing supervisor policy: a real VFS restart renews PID 1's
control channel and sends clients new sessions. A timeout alone does not
trigger a restart. If VFS stays alive but withholds a reply, that endpoint
remains unavailable until a genuine restart/handoff. No replacement-grant or
automatic reconnect protocol was added for these endpoints. The VFS's own
filesystem service is different: its independent recovery endpoint accepts
replacement notices even after an ordinary control times out. PID 1 uses that
endpoint for an acknowledged, drained handoff to a fresh control ID, without
replaying ambiguous old operations. PID 1 then replaces the filesystem service
behind an exit fence (ADR 0204); the VFS refuses any attach without that fence.
Other supervisor/exec/config/network control protocols remain outside this
review.

Tests exercise these shared transaction methods with deterministic clocks
and transports; host source checks verify that shell/PID 1 route their VFS
calls through them. Native builds validate those adapters, not VM execution.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0654 | `page` (includes object and memory) | process | Held across VFS wait; creates mapping lazily and after abandonment | `page.mu` covers preparation, completion, copying and retirement | Late server access must address a retired object, never the next request's page | Abandon removes local mapping/handle; peer mappings are not revoked. Failure to unmap may leak a mapping, but it is never reused by the library | `TestAbandonIsolatesLateServerWrites`, utility VFS failure tests |
| CA-0800, CA-0801 | init/shell `filesystemRequests` | process | Bounded VFS waits, failed-ID storage may allocate | Per-owner session lock serializes transactions and quarantined IDs; handoffs stay in the owner's service loop | Data, control and async completions share failure ownership | A live VFS that withholds a completion needs a genuine supervisor restart/handoff; late reply handles stay unaccepted on the quarantined queue until its owner closes it | `TestOwnedVFSAbsoluteDeadlineBeforeAndAfterSend`, `TestVFSControlOwnershipAndCorrelations`, `TestOwnedAsyncCompletionVsMalformedReply` |

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
goroutine starts. Each caller encodes an event in its own 181-byte and
96-byte buffers and hands it to the kernel in one `LogWrite`, so events
are whole and may reorder. A failed write
returns false; nothing is retried.

| ID | Symbol | Owner | Blocks / allocates / calls back | Synchronization and visibility | Why | Cancellation, peer death, partial I/O, timeout, exit | Test |
|---|---|---|---|---|---|---|---|
| CA-0640, CA-0641 | `component`, `console` | process | No | Written by `Init`, then read | Identity of the component | None | none: `TestEncodeConcurrentRecordsIntact` passes its own component and reads neither |

## `startup`

`Read`, `Decode`, `DecodeTemplate`, `Size` and every `Data` method are
Concurrent-safe: `Data` is a read-only view of the startup object. `Writer`
is Independent instances safe. `BindHandles` changes the template its caller
passes and is Single-owner per template. `equals` (CA-0702) and `magic`
(CA-0703) are immutable tables. Test:
`TestDataConcurrentReaders`.

## `fetchtransport`

An `Adapter` is a process singleton: call `New` at most once per process.
Every Adapter uses utility's process-wide TCP session, its first transmit
slot and `TCPCall`, which drops replies meant for other requests, and `New`
maps the session's windows again without unmapping the old ones. Moving the
request, reply, path and file buffers into the Adapter therefore does not
make two Adapters independent. The Adapter, its connections and its outputs
belong to the goroutine that runs `main`, since `ConnectTLS` uses utility's
current line. `fetch` and `trust` each call `New` once. Name resolution goes
through `utility.CallResolver` and is Concurrent-safe.

## `exec`

The launch protocol codecs and builders are Independent instances safe.
`LocateImage` and `ArchiveExtent` are Concurrent-safe: they compare the
archive prefix and the rest of the path as two parts
(`initramfs.ExtentUnder`) and keep no buffer. `linuxPathVariable` (CA-0616)
is the remaining package-level slice; PID 1 copies it into its startup writer
and does not modify it. Tests: `TestLocateImageNamesTheBytesOfAnExecutable`,
`TestExtentUnderMatchesTheJoinedName`.

## Runtime packages

`runtime/sys`, `runtime/arena`, `runtime/tls` and `runtime/thread` are the
runtime and are maintained with ADR 0203 and its successors; their rows are
listed for inventory coverage, not re-audited by the utility changes.

- `runtime/sys`: `scratch` (CA-0695) is per-M scratch: the descriptors one
  wrapper call fills and passes to the kernel. A wrapper pins its G while
  it uses M-local descriptors; kernel task preemption remains possible.
  Ordinary I/O wrappers require caller-owned buffers and kernel-managed
  handle lifetimes. Runtime controls such as `ThreadSetBase`, preemption
  registration and suspend/resume are runtime-owned operations, not a promise
  that applications may invoke them concurrently with the scheduler or GC.
- `runtime/arena`: CA-0661 to CA-0694. The heap lock, the collector lock and
  the collector's atomics follow the protocol of ADR 0201 and 0203; the
  remaining rows are collector bookkeeping written under those locks or with
  every other thread suspended, and start-up values. Tests are the package's
  own, among them `TestOwnedSlabsRace`.
- `runtime/thread`: `starting` (CA-0696) is a spin lock over `starts`
  (CA-0697), the table of threads being started. `Start`, `Join`, `Self`,
  `Slot` and `Yield` are Concurrent-safe.
- `runtime/tls`: `blocks` (CA-0756) is the native per-M TCB table;
  `hostBlock` (CA-0757) is a host-only singleton stub. `TestBlockLayout`
  checks the wire layout, not native thread isolation. The runtime ownership
  boundaries above apply to mutable TCB access.

## Other libraries

The caller audit covers the 63 library packages listed below, including
transitive dependencies, borrowed buffers, callbacks and teardown. Its
baseline is `97c0d25`; subsequent fixes remove shared exec scratch, tighten
the read-pool probe and preserve temporary-file ownership. Package-local
state alone does not establish safety. Independent objects must also own
their writable buffers, transports, backing stores and sinks independently.
Runtime classifications describe a protocol, not permission to call every
runtime export from arbitrary goroutines. This audit does not replace the
native collector and scheduler tests.

| Package | Class of the exported API | Notes |
|---|---|---|
| `wait` | Concurrent-safe | `Any` and `Sleep` keep nothing between calls; the caller owns the interest array. |
| `fsproto` | Concurrent-safe for distinct messages, name buffers and caches | A cache allows one exchange in flight, because names longer than the inline limit share its name page. `Serve` is as safe as its volume; fat32 runs one service loop. Test: `TestCodecConcurrentCallerBuffers`, inline names only. |
| `terminal` | Independent instances safe | `Output` batches into caller storage and hands a whole batch to its sink; `Parser` keeps per-instance state. Test: `TestIndependentOutputsAndParsers`. |
| `nativefiles` | Independent instances safe | A `Files` owns its `Bulk`; requests go through `utility`. |
| `tlsclient` | Independent instances safe | A client per connection; `Roots` is immutable once loaded. `LoadSystemRoots` uses the caller's Bulk. |
| `remoteapi` | Concurrent-safe codecs; Independent instances safe buffers | `Receive`, `Send`, `SendWithin` and `CloseHandles` use a wire buffer per call. `KeyRequestBuffer` belongs to one owner; consume its borrowed result before the next `Accept` or `Reset`. The endpoint owner dispatches correlations. |
| `remoteservice` | Independent instances safe | `Receive`, `Send` and `CloseHandles` (CA-0657 to CA-0659) are function values tests replace before anything runs; a `RelayConn` belongs to one worker. |
| `cryptosys`, `cryptoprobe` | Concurrent-safe with separate output buffers | Random and clock reads use the native syscall layer. Rand and TLS probes keep a reader per invocation and do not replace `crypto/rand.Reader`; TLS passes that reader in `tls.Config.Rand`. |
| `admincmd` | Single-owner | Command bodies built on `utility`'s current line and `Path`. |
| `shell` | Concurrent-safe codecs; Independent instances safe state | Parsing and formatting use caller buffers; `Commands` (CA-0698) is an immutable table. `TerminalEnvironment` has one owner; `Entries` aliases its storage until `Store` or `Reset`. `RunLog` requires exclusive hooks and scratch. |
| `editor`, `filemanager`, `viewer`, `tui`, `fsdisplay` | Independent instances safe | Screen state per instance; output through a `terminal.Output`. `tui.DefaultTheme` (CA-0707) is immutable by convention. |
| `network`, `netdev`, `netapi`, `tcpapi`, `udpapi`, `tcpdispatch`, `tcpserve`, `udpserve`, `tcpupstream`, `upstream` | Independent instances safe | Codecs are Concurrent-safe; stacks, pools and dispatch tables are objects run by one service loop. |
| `dns`, `dnswire`, `dnsq`, `dnstcp`, `resolvapi`, `hostname` | Independent instances safe | The resolver cache is a `dns` object owned by the resolver service's loop; codecs are Concurrent-safe. |
| `vfs`, `block`, `fat32`, `partition` | Independent instances safe with separate backends | Volume and mount state belong to an owner; VFS workers use captured read plans and obey the persistent lane and barriers. The fixture's `validationReads` (CA-0760) has owner-only arms; worker gates use published atomics and channels. |
| `safesave` | One owner per temporary pathname | Separate workspaces do not isolate saves to the same target. A competing save refuses an existing temporary file; it must not delete or publish another save's data. |
| `config`, `release`, `commands`, `truststate`, `truststore`, `trustupdate`, `remotepolicy`, `httpwire`, `fetch`, `sshserver`, `tty`, `init`, `linux` | Concurrent-safe codecs and policy, or Independent instances safe objects | Package-level variables are immutable tables and error values. |

## Programs

The program rows describe their callers' ownership. VFS and goprobe select
the goroutine target; threadprobe starts native threads. The other shipped
programs use one service loop and do not start workers through `go` or
`thread.Start`. The SSH backend uses a synchronous pump; its region scopes
must not park or migrate. Linking a library into goprobe does not mean its
stateful APIs run on workers.

VFS separates namespace/admission/commit ownership from worker access to
published read plans. The persistent lane owns a whole Remote operation;
mutations and lifecycle changes drain it. The completion bridge reads the
console handle set at startup, so that handle is immutable rather than
owner-only. Fixture state is process-wide, not independent per Service.
Goprobe workers use request-local records through `utility.Write`, not the
main goroutine's `Print` or `Path` scratch.

A program that adds workers must review both its globals and the objects its
workers borrow, as threadprobe does:

- `threadprobe` (CA-0120 to CA-0145): the counters, flags and handed-over
  pointers its worker threads share are `sync/atomic` values; `cpuWires`
  (CA-0125) has one slot per thread, indexed by `thread.Slot`; `benchOps`
  and `gcRounds` are set before the workers start; `profile` and
  `profileLine` are used by the main thread only.
