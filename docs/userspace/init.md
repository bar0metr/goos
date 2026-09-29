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
firmware handoff. The bootstrap reservation is capped at 1.5 MiB. The kernel
discovers `system/init`, validates its native image,
creates a distinct address space and process ID 1, and schedules it at ring 3.

Normal native exits and isolated CPU faults enter the same bounded task-exit
path. The process registry emits a process event only after its final Task dies,
then sends a typed lifecycle notification over a channel available only to PID
1. PID 1 uses one readiness-only `WaitAny` call for the lifecycle channel and,
after bootstrap, the shell-exec channel. A returned bit mask can identify both;
PID 1 then performs the typed receives, issues correlated launch requests, and
continues only after the replacement Process and Task exist.

PID 1 is its own executable source. Before it launches anything it maps the
immutable system image read-only (`ImageMap`, handle `0x100000015`), and it
resolves every executable path under `/system/` there with `exec.LocateImage`:
the path must be canonical, and hard links resolve to the program they name.
A missing path is answered "command not found" without asking the kernel.
Launch requests (lifecycle version 7) then identify the executable as an extent
of the image, `{image handle, offset, length}`, plus a display name for `ps`.
The kernel resolves no path: it requires Execute on the image handle, checks
that the extent lies inside the image, validates the native image, assigns a
fresh process identifier from a bounded pool, and creates a new address space
and initial task (ADR 0093).

A command path outside `/system` is a program on a filesystem. PID 1 opens it
through a VFS session of its own, creates a one-page executable object for the
native header, and asks VFS to pin the stable open object and fill that page.
`ExecutableSource` consumes the session and turns the sealed header object into
a source for the complete file (ADR 0121). Preparation does not block PID 1:
VFS answers on the job's own session,
which PID 1 waits on with its other channels, and lifecycle events, other jobs,
and restarts are served meanwhile. A job stopped while its programs are being
read ends with the launch status "stopped before it started" (ADR 0108). A file that is not a regular file, is
larger than the executable-source limit, or whose header cannot be read in
full, is refused. The launched process reads payload pages by offset when it
touches them and holds the source open while it runs. Those private pages are
charged to the process; the source itself retains only the header page
(ADR 0106, ADR 0121). Such a program receives its standard streams and no
other grant, and PID 1 closes its source handle once the kernel has answered.
Services are launched only from the
image, and paths in `/system` outside `/system/bin` are not commands.

Every current path above validates a GoOS native image before the kernel
initializes the Process with immutable `Native` ABI. GoOS 0.2.0 will let PID 1
classify ELF magic as a Linux launch candidate, but the kernel will validate
the Linux ELF independently and only then initialize `LinuxX8664`. That launch
will receive only its standard streams, one fresh VFS session, argv,
environment, cwd, and its resource profile; services remain native-only
(ADR 0132, ADR 0136).

PID 1 is compiled from Go with a freestanding amd64 TinyGo target. A small
assembly entry supplies the native kernel-call ABI; its stack and heap are
reservations the kernel supplies pages for as they are touched (ADR 0104),
bounded by PID 1's page quota. The host-side converter
accepts only static x86-64 ELF load segments and wraps them in the validated
GoOS native image envelope.

The Manager, service table, exit handling, restart decisions, backoff, and rate
limit execute in PID 1. The kernel only delivers process deaths and performs
authorized image loading and process creation. Each launch request carries the
service's bounded capability manifest and a `tiny`, `normal`, or `large`
resource profile. PID 1 chooses the profile, while the kernel owns its numeric
limits (ADR 0122). Manifest entries reference existing PID 1
handles and request an exact derived rights mask; the kernel requires Transfer
authority, prevents escalation, and installs the derived handles into the new
process before its first Task is created.
Channel capabilities identify their endpoint side. A launch grant may be marked
consumed, removing PID 1's source handle after successful derivation. This is
used for per-process VFS clients and prevents accumulation of temporary handles.

PID 1 also supplies a `GST4` startup template with a unique role name for each
manifest entry. It writes the template with `startup.Writer` into a 64 KiB
slot of the launch staging area shared with the kernel (handle `0x100000013`,
four slots), and the launch message (lifecycle version 9) names the slot and
the template's size (ADR 0062, ADR 0109). After installing all capabilities,
the kernel writes their actual generation-safe IDs into its copy of the
startup object before creating the first Task. Services resolve roles rather than depending on handle-table insertion
order. Names are descriptive metadata; type and rights enforcement remains in
the kernel handle table.

The kernel also grants PID 1 a 512 KiB exec area of eight slots (handle `0x100000014`,
`Read|Write|Transfer`) for the shell exec protocol. It is not mapped at boot;
PID 1 maps it with `MemoryMap` and passes a derived `Read|Write` handle to the
shell through the launch manifest (ADR 0063).

Shell exec requests are restricted to simple names below `/system/bin`. A
request carries one to four invocation records, a shared cwd, and a shared
environment. The shell writes it into a 64 KiB slot of the exec area it
shares with PID 1, and the version 11 submit message names the slot and size
(ADR 0064, ADR 0109). The reply to a launch that failed names the stage that could not be
started, so the shell can report it by name (ADR 0079). PID 1 copies the slot before decoding it. Each record contains
its own executable path and structured argv. The decoder checks the complete
request, all lengths, system paths, vector encodings, and trailing bytes before
any resource operations. Oversized requests are rejected rather than truncated.

The request begins with twelve bytes: stage count, error source, flags (bit 0:
background, whose first stage reads an empty stream), a zero byte, the cwd's
length, and the number of environment variables. The cwd and the variables
follow; each stage then stores its path, its argument count, and its
arguments. Every variable, path, and argument is a 32-bit length followed by
its bytes; variables are `NAME=VALUE`. Arguments are limited by the slot's
bytes, not by a count, and PID 1 turns them into the startup object; one that
does not fit fails with "arguments too long". No handles are attached to the
exec request; PID 1 derives child capabilities from its own table.

For N stages PID 1 creates N-1 streams and starts stages from right to left,
so a downstream launch failure cannot leave a newly started producer waiting on
input. The first stage reads a standard-input stream of its own: a background
job's is closed at once, and a foreground job's writing end is moved to the
shell in an exec `Started` notice before the job's reply (ADR 0082; since
version 9 of the exec protocol the notice carries the job's correlation in its
payload, ADR 0089). Each middle stage receives only its upstream reader and
downstream writer. Whatever is not piped or redirected — the last stage's
stdout and every stage's stderr — is a copy of the terminal's writing end, which
PID 1 keeps (ADR 0089). Successful launch consumes the pipeline's own stream
handles; the terminal's copies are counted and PID 1's own stays. On any failure PID 1 closes every retained endpoint before waiting
for children already started. This releases stream waits through EOF or peer
closure. It is not forced cancellation of an arbitrary nonterminating program.

All stages start before process waits. The request then becomes one of up to
eight jobs (ADR 0111): PID 1 returns to its loop, which waits on the lifecycle channel, the
exec channel, and the next process of every job, so supervision and further
requests continue while commands run. PID 1 reaps every started process and
returns the first nonzero exit code in left-to-right pipeline order. Launch
failure instead returns a launch status with no successful process result.
Completion readiness is observed through the generation-safe `WaitAny` table;
PID 1 then consumes the exit code with the typed process wait.

PID 1 creates and transfers a fresh VFS session only for commands whose manifest
asks for one. It reads those manifests once, from the command index
`/system/commands` in the immutable system image, through a VFS session of its
own; no list of command names is kept in PID 1 (ADR 0080). Pure stream and
diagnostic commands receive no VFS endpoint.
The VFS control protocol version 2 identifies the add-session operation with an
explicit payload byte, preventing an unrelated control-shaped message from
being accepted as a session request.

## Application launch

A command whose manifest declares `GrantLaunch` and that runs as the only stage
of a foreground job also gets a `launch` channel (ADR 0130). PID 1 grants that
stage a counted copy of its standard input and keeps the reader. `gfm` uses
the channel to run `gview` and `edit`: it writes one exec-style stage into a
memory object of its own, attaches it to a `GLNCH001` request and waits. PID 1
copies the request out, accepts only a `/system/bin` command whose manifest
declares `GrantTerminal`, and launches it with its own manifest's grants, a new
VFS session, PID 1's copy of the caller's input and the session's terminal for
output. The child never gets `GrantLaunch`. PID 1 waits on the child in its
main loop and answers with the exit code or the launch status. A job that ends
while its child runs has the child stopped and reaped before the shell hears
of it.

## Startup phases

Services start in two phases. The first registers and launches `vfs`, `block`,
`virtio-blk` and `fat32`, whose endpoints PID 1 holds from boot or creates
before registering them. Once `fat32` runs, PID 1 hands the VFS its channel to
it through the VFS control endpoint and waits for the persistent volume to be
mounted, so the services started next find `/state` (ADR 0096). `fat32`
restarts on failure; PID 1 then gives the new instance fresh channels to the
VFS and the block service and hands their other ends over as at first start. The second starts the
log service and then one terminal session per console: `tty` and `shell` on
COM1, then `tty-fb` and `shell-fb` on the framebuffer when the machine has one
(ADR 0125).

The split exists because the log service's VFS session cannot be handed over at
boot: it has to be asked for, through the VFS control endpoint, the same way an
application's session is, and that needs VFS already running (ADR 0073). Placing
it before the shell also means the prompt appears after every service line
rather than in the middle of them.

A failure to start the log service warns and continues. Every other service is
required, because the system cannot serve files or a prompt without them.

## Restarts

A restart is scheduled as a deadline, not delivered as an event, so the
supervision wait is bounded while one is outstanding and the expiry starts
whatever is ready. Waiting only on events left a service that was scheduled to
restart down forever when nothing else happened (ADR 0078).

A service that cannot be handed the endpoints it had needs them replaced before
it is launched. The storage driver is the one such service: its channel to the
block service died with it, so PID 1 creates a fresh pair, tells the block
service over a control endpoint, and updates both the launch about to happen and
the stored definition. Its device authority — ports, interrupt line, DMA domain
— is granted rather than consumed, so PID 1 can grant it again.

The validation build kills the driver once, after the system is up, so the path
a crashed driver takes is exercised on every boot rather than only when hardware
misbehaves.

The terminal services and the shells are the other two (ADR 0089). They run in
pairs, one per console (ADR 0125), and PID 1 keeps a `terminalSession` for each
pair: its console, output stream, terminal control channel, exec channel and
exec page. All are `restart: always`, and the session between a terminal
service and its shell is renewed on either's restart, within that pair.
The VFS, the configuration and log services and the block service renew what
they held the same way, and their clients are handed new sessions instead of being restarted
(ADR 0099, 0100, 0101):

| Restarting | Launched with | Handed to the survivor |
| --- | --- | --- |
| `tty`, `tty-fb` | a new session (server end) and a new control channel | the session's client end, to its shell over its exec channel |
| `shell`, `shell-fb` | a new session (client end), exec channel, VFS session and configuration session | the session's server end, to its terminal over its control channel |
| `vfs` | a new control channel (client end kept) and first session | the first session's client end to `shell`, and new VFS sessions to the other shells and the config and log services once `fat32` is attached, as session notices (ADR 0099) |
| `config` | a new VFS session and control channel (client end kept) | new configuration sessions to every shell and the log service, as session notices (ADR 0100) |
| `log` | a new VFS session, configuration session and control channel (client end kept) | nothing; it starts with `RESTART=1` and resumes after the last record in the file (ADR 0100) |
| `block` | a new control channel (client end kept) and public endpoint, no driver or filesystem endpoint | nothing; the driver and `fat32` exit, restart after it, and hand it their endpoints over the control channel (ADR 0101) |

A handle renewed for a launch that then fails is closed at the next renewal, so
failures cannot fill PID 1's table. Each shell numbers the slots of its own
exec page, and PID 1 tags every job with the shell that submitted it. When a
shell exits, PID 1 stops every stage of that shell's running jobs; they are reaped as usual and answered to no one, and
their streams close with them. The exec channel is not waited on until the next
shell brings a new one. After boot, a restarted service is recorded in the log
rather than printed as an `[ OK ]` line, which would land in the line being
typed. The validation build's `fault tty` and `fault shell` kill the pair of
the shell that asks; `fault fat32`,
`fault vfs`, `fault config`, `fault log` and `fault block` (exec control
`ControlFault`) kill
each once during the boot test.
