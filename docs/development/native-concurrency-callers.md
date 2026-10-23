# Userspace library caller audit

This audit covers 63 library packages at `97c0d25`. The baseline already removes shared exec name scratch. Later fixes reject
a false overlap result in the VFS probe,
stop crypto probes from replacing process entropy, and prevent competing
saves from taking over another save's temporary file. The contract summary
and variable inventory live in [native-concurrency-audit.md](native-concurrency-audit.md).

The review traced imports, stateful exports, borrowed storage, callbacks and
teardown. It checked native/goroutine build selection and production callers
of `go` and `thread.Start`. It did not repeat the complete scheduler,
collector or generated SSH backend review. Runtime rows below state the
ownership protocol; they do not grant arbitrary concurrent access to runtime
internals.

## Classes

- **C**: concurrent calls with immutable inputs and separately owned writable buffers.
- **I**: one owner per instance, including its borrowed storage and transitive mutable dependencies.
- **O**: one process or service-loop owner.
- **R**: runtime-managed protocol, including pins and lifecycle restrictions.

Two objects sharing a Bulk, Stack, Resolver, transport, sink or temporary
pathname do not satisfy I merely because their struct addresses differ.
Callbacks must not start concurrent or otherwise unsupported operations on
the same instance. Designed synchronous visitor and reentry protocols, such
as Finder traversal, remain valid.

## Caller matrix

| Package | Class | Production callers and ownership boundary |
|---|---|---|
| `admincmd` | C helpers; I Finder; O native commands | Native LineArguments uses main-owned path scratch; NativeOutput writes records through utility.Write. Finder and file lifetimes are owner-sequenced. |
| `block` | C codecs; I Service | block, fat32, init, virtio-blk and fsck-fat run serially. Reply.Data borrows Service.buffer. |
| `commands` | C | init, shell and command manifests read metadata after startup. Lookup/append targets belong to the caller. |
| `config` | C codecs; I Store/Client | configd, init, netd, resolved, shell, logd and remoted use service-owned transports. PersistStore inherits safesave pathname ownership. |
| `cryptoprobe` | C with local readers | The command invokes Run once. Rand and TLS variants must not replace crypto/rand.Reader; TLS uses Config.Rand. |
| `cryptosys` | C | Crypto, SSH, fetch and trust callers supply distinct destinations to the kernel random reader. |
| `dns` | C policy; I Resolver | resolved and upstream clients share one service-loop resolver cache. |
| `dnsq` | I | The command owns Query, Options, Net and Output. |
| `dnstcp` | I | dns, dnsq and tcpupstream consume Message before reusing Sender/Receiver storage. |
| `dnswire` | C codecs; I writer | DNS clients own Name, Response and scratch; formatting follows its Writer owner. |
| `editor` | I | edit owns App, Buffer, Files and Output. |
| `exec` | C codecs/lookups; I builders | init and shell build launches. ArchiveExtent/LocateImage keep no shared name buffer. PID 1 copies the immutable linuxPathVariable slice. |
| `fat32` | I | fat32 and fsck-fat own FileSystem, Volume, Device and synchronous clock/check/repair callbacks. Distinct filesystems require distinct device scratch. |
| `fetch` | C parsing; I Run | fetch and trustupdate own Environment, connection and output for the duration of Run. |
| `fetchtransport` | O | fetch and trust create one Adapter each. Sockets share utility's process TCP session and slot zero; file outputs share the Adapter's Bulk, and stdout uses the standard output stream. Close is not concurrent cancellation. |
| `filemanager` | I | gfm owns App, Panels, FS, Programs and Output. Borrowed panel views expire on mutation. |
| `fsdisplay` | C | df, mount and filemanager supply formatting destinations. |
| `fsproto` | C with separate caches | VFS serializes a whole persistent exchange. fat32 runs one loop. Long names share NamePage, so one cache allows one exchange in flight. |
| `hostname` | C parsing; I Name | DNS/config/network callers own mutable Name; Bytes returns a borrowed view. |
| `httpwire` | I | fetch owns Reader and its connection Source/Sink. |
| `init` | C policy/codecs; I state | PID 1 owns Manager, jobs, registry and runners. Launch/Release callbacks are synchronous; mutation expires borrowed views. |
| `linux` | C read-only | Embedded fixture bytes and Image views remain immutable. Sysimage tools read them. |
| `log` | O Init; C emission | Services initialize once before workers. VFS workers do not log; the owner handles offline/fatal paths. Record buffers are call-local. |
| `nativefiles` | I | trust, remoted and TLS root loaders own Files with a separate Bulk. Lock/Unlock supplies the installer namespace protocol. |
| `netapi` | C | netd, init, resolved and utility own their request/reply buffers. |
| `netdev` | C codecs; I rings | virtio-net and netd own Pool, Queue and rings. DMA ownership does not allow two CPU writers. |
| `network` | C codecs; I Stack | netd owns all TCP/UDP state in one loop. Other clients use pure helpers. |
| `partition` | C selection; I scanning/writing | block and init own Scanner/Reader. Formatting tools own Writer and storage transaction. |
| `pathpage` | Locked process resource | utility acquires session then page.Hold; it copies Name before Release. Prepare/Abandon require the hold. No callback runs under this lock. |
| `release` | C | init and sysimage tools own parse/encode targets. |
| `remoteapi` | C codecs; I fragments | remoted owns KeyRequestBuffer and consumes its borrowed result before Reset/Accept. Endpoint correlation dispatch remains owner work. |
| `remotepolicy` | C validation; I stores | configd, init and remoted own Files and callbacks. HostStore.Load uses external Lock; KeyUpdate requires its dedicated guard. |
| `remoteservice` | I | remoted and worker own State, Queue, identities and RelayConn. Receive/Send hook replacement happens before use. RelayConn does not promise concurrent net.Conn cancellation. |
| `resolvapi` | C | resolved, resolve, ping, fetch and utility supply separate codec buffers. |
| `runtime/arena` | R | Collector and thread cache protocols govern allocations. Entered regions are M-owned; their callers must not park or migrate. |
| `runtime/goroutine` | R | VFS and goprobe initialize before starting workers. Scheduler controls use locks/atomics; G waits obey pin and region restrictions. |
| `runtime/lock` | C, nonreentrant | utility, pathpage and pages use Mutex without copying after use. G builds use sync; native-thread builds use CAS/yield. |
| `runtime/pages` | C metadata; I page lease | VFS initializes Pool before workers. Page bytes belong to the holder; Allocate/Free/Permanent serialize metadata. Failed discard must still leave zeroed reused pages. |
| `runtime/sys` | C calls; R hooks/scratch | Callers own buffer and handle lifetimes. M-local scratch is pinned; wait hooks run before scratch acquisition. Native host stubs do not emulate per-M TLS. |
| `runtime/thread` | C calls; R lifecycle | Scheduler, threadprobe and goprobe start native threads. Hook setters run at startup. Join has one owner; Slot/Self identify the current M, not a persistent G identity. |
| `runtime/tls` | R | Runtime callers pin while retaining an M-local Block pointer. hostBlock is a test singleton, not native TLS-isolation evidence. |
| `safesave` | One owner per request-owned temporary pathname | edit and configd use Save through utility/config. Atomic Create reserves unique candidates across processes; occupied names are skipped. No cleanup follows an ambiguous Replace; forward retries use a fresh candidate. |
| `shell` | C codecs; I state | shell and PID 1 own TerminalEnvironment. Entries aliases storage until Store/Reset. RunLog requires exclusive hooks and scratch. |
| `sshserver` | I, region-bound | remoted-worker owns Server, private Conn and callbacks. Event borrows storage until Poll. Synchronous pump scopes must not park or migrate. |
| `startup` | C immutable Data; I Writer | Programs read kernel startup mappings. PID 1 alone mutates templates through Writer/BindHandles before handing them over. |
| `tcpapi` | C codecs; I Slots | netd, resolved, remoted, tcp, dnsq and utility sequence pool operations by owner. |
| `tcpdispatch` | I | remoted owns Session, TX/RX and Send callback. Take copies borrowed RX data before Release. |
| `tcpserve` | I with exclusive Stack/pools | netd runs all Sessions in one loop. Different Sessions do not permit racing a shared Stack or Replier. |
| `tcpupstream` | I | resolved owns Client, Resolver, TX/RX and Sender. Bind/Unbind follow completed exchanges. |
| `terminal` | C pure helpers; I Output/Parser | edit, gfm and gview own storage and synchronous sinks. Printable's question-mark slice is only copied by current callers. |
| `tlsclient` | I Conn; C immutable Roots | trust, fetch and trustprobe own Stream/clock/deadline callbacks and root-loading Bulk. Internal tls.Conn locks do not make its raw Adapter safe to share. |
| `truststate` | I | trust, root loaders and policy stores own Files/Validate. Install uses Files.Lock; readers verify a complete signed generation. |
| `truststore` | C immutable inputs | trust, truststate and update callers use owner-safe validators. Verify clones its result slices. |
| `trustupdate` | I | trust runs three sequential fetch operations with an exclusive Environment and a new bounded output per operation. |
| `tty` | C codecs; I engine | tty, shell and worker own Engine, Discipline, RawInput, Screen and synchronous callbacks. Snapshot aliases expire on reuse. utility's resize state has its own mutex. |
| `tui` | I | editor and viewer own Screen cells, dirty state and Output. Production callers only read/copy DefaultTheme. |
| `udpapi` | C codecs; I Slots | Same service-loop pool ownership as TCP. |
| `udpserve` | I with exclusive Stack/pools | netd sequences all Sessions and Replier in one loop. |
| `upstream` | I | resolved owns Client, Resolver, pools and Sender. It processes/copies receive payload before Release. |
| `utility` | Split C/I/O | Parallel goprobe output uses Write with a whole-record lock. Print/Path/TCP/UDP scratch remains main-owned. Session mutexes cover synchronous RPC validation/copy; async sends/handoffs require endpoint-owner sequencing. |
| `vfs` | O Service; worker protocol | Owner admits/commits and controls namespace. Workers get ReadPlan, persistent work owns a whole Remote lane, barriers drain lifecycle changes. ReadPool.Close requires all completions retired. Probe state is process-wide. |
| `viewer` | I | gview owns App, Reader and Output. |
| `wait` | C | Interest arrays belong to callers. VFS owner and serial service loops use Any; runtime adapters govern G parking. |

## Program and test evidence

Goprobe, the VFS and the validation virtio-blk driver select the goroutine
target. `VFS_RUNTIME=serial` builds the serial VFS fallback (ADR 0204); a
normal build keeps a serial disk driver. The validation driver's serving G owns its requests
and DMA state; a peer G measures progress during IRQ waits. Threadprobe starts
native threads. The other shipped programs do not start workers through `go`
or `thread.Start`; the generated SSH backend uses its synchronous pump.
VFS's read workers call captured plans, while the completion bridge uses
stream/syscall output. Stateful utility/exec/terminal APIs linked into
goprobe are not thereby called by its workers.

The audit ran `go vet ./...` and `go test -race ./userspace/...` at the
baseline: 2,108 tests in 69 host packages passed. Tagged crypto tests at
that point did not exercise the global-reader replacement; follow-up tests
cover local entropy ownership and concurrent TLS probes. Host tests do not
establish native TLS isolation, kernel handle revocation or the whole
collector/preemption lifecycle. Those need the native test scenarios in
ADR 0204. No staticcheck result is claimed.
