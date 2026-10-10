# ADR 0199: Native remote terminal sessions

## Status

Accepted. Host tests, SSH compatibility tests, native builds, and the headless
OpenSSH integration gate pass. The gate checks 65 assertions across a primary
boot and a reboot of the same persistent disk.

Builds on ADR 0196 and ADR 0198.

## Context

GoOS needs public-key SSH access to its existing shell and native terminal
applications. The local terminal service combines line editing, raw mode,
redraw, and console syscalls. PID 1 owns shell launches and jobs. The userspace
runtime has no Go scheduler and uses a resettable bump allocator.

## Decision

Implement SSH in the supervised `remoted` service. Keep SSH outside the kernel
and `netd`. `remoted != OpenSSH sshd`: GoOS uses its native terminal/session
model and does not add `/dev/pts`, termios, or an OpenSSH configuration parser.

Extract `tty.Engine` from the local frontend. Local consoles and a remote worker
use one instance-based core for ReadLine, Continue, Redraw, RawMode, LineMode,
interrupts, output relay, and prompt state. GTTY version 4 carries geometry,
an authoritative-size flag independent of the framebuffer flag, and bounded
TERM. A resize updates the session's state. The shell queries that state before
launching a foreground job and passes COLUMNS, LINES, and TERM.

PID 1 launches a fixed worker executable for each admitted connection. The
controller retains GTCP1 listener and endpoint authority. Each worker receives
bounded ciphertext streams, an immutable policy page, and its own typed PID 1
control channel. PID 1 creates both relay streams, grants the worker its input
reader and output writer, and returns the opposite ends to the controller through
GWORKER1 version 2. IPC moves remove Transfer rights, so PID 1 cannot regrant
stream ends supplied by the controller. Workers receive no arbitrary
process-launch interface or host-key filesystem authority.

After public-key authentication, the worker reserves authenticated capacity
through GREMOTE1. A valid PTY request followed by a shell request lets PID 1
create shell exec endpoints, a shared exec page, VFS/config sessions, an output
stream, and a GTTY endpoint. The worker runs the terminal master; the shell
receives only its normal slave-side authorities. A generation-tagged ID and
channel ownership bind control messages to that session. PID 1 marks remote shell
startup with GOOS_SESSION=remote. Cooked EOF ends that shell; local sessions keep
their persistent-shell behavior. PID 1 retains the worker during a bounded output
drain so SSH receives the shell's exit status before teardown.

Keep four authenticated slots and two pending-handshake slots. Closing records
retain their reservation until PID 1 reaps processes/jobs and releases handles
and mappings. Reuse ephemeral lifecycle slots instead of appending connections
to the service supervisor. Budget remote jobs against the 64-task and 64-wait
bounds without taking local terminal capacity. PID 1 has a 32-object shared-memory
budget for local services, remote exec pages, and copied worker policies. Other
processes retain the 16-object limit; page quotas and the 256-object global table
remain unchanged.

Pin `golang.org/x/crypto/ssh` and generate an audited scheduler-free transport
adapter from verified upstream sources. Retain upstream crypto, signatures,
wire encoding, KEX, and packet protection. Replace goroutine-dependent transport
and mux coordination with a bounded pump. Retain incomplete ciphertext until a
whole frame is available; service terminal IPC and output between frames. Use
separate reusable packet and key-generation allocation regions so established
traffic and rekey do not consume the root arena over time.

Enable only curve25519-sha256, ssh-ed25519 host/user keys, and
aes128-gcm@openssh.com. Reject password and keyboard-interactive authentication,
forwarding, exec, subsystem, agent, and X11 requests. Require one PTY and one
interactive shell per connection. Handle the advertised OpenSSH ping extension
with upstream ping/pong messages and a 16 KiB opaque-payload bound. Defer pong
through the existing bounded control queue during rekey.

Load all `system.remote.*` values as a candidate. Bracket reads with GCF1
version 2 scoped revisions to detect concurrent writes, including ABA changes.
Prepare authorized keys, verified identity, scoped VFS authority, and a new
listener before committing. Reuse an unchanged listener. Existing connections
retain their policy generation; failed candidates leave the running policy
and listener intact.

Persist a per-installation 32-byte Ed25519 seed. Generate it with
`cryptosys.Reader` only when identity files are absent. Stage seed/public files,
close and reread them, verify the pair, publish, and verify the final files.
Keep corrupt or conflicting state for explicit repair instead of silently
changing host identity. FAT32 flushes completed mutations before success;
this does not claim a journal or arbitrary torn-sector atomicity.

PID 1 installs bounded object-aware VFS scopes before exposing shell access.
Confined remoted sessions get host-state read/write and authorized-key read
rights. Ordinary shell and job sessions cannot read, move, replace, truncate,
or expose protected identity through mount aliases, renamed ancestors, or
retained handles. Preserve the installed-system mutation guard. Reject unsafe
or exhausted scope candidates before reload commit.

## Alternatives

A direct socket-to-shell relay bypasses native line/raw semantics and leaves
session/process ownership ambiguous. A second SSH-specific line editor would
duplicate local terminal behavior.

Stock upstream SSH requires a Go scheduler, blocking channels, and allocation
reclamation. Enabling a general userspace scheduler would change syscall scratch
and runtime assumptions beyond this feature. Keep explicit bounded coordination
within isolated workers.

POSIX PTYs and an OpenSSH port would introduce device, account, and process
interfaces that native sessions do not need.

## Consequences

Maintain source hashes and compatibility patches alongside the toolchain pins.
Test authentication, packet limits, rekey, flow control, and allocation lifetime
when updating the dependency. A protocol or worker fault closes its connection;
PID 1 performs full remote teardown rather than retaining local-restart resources.

VFS scope slots remain bounded and retain old private-path protection during the
service lifetime. Reload can reject storage-path changes when those slots are
exhausted. Operators must repair corrupt identity state instead of relying on
regeneration.

Live delivery of resize to an already-running application remains follow-up
work if the native terminal model lacks that notification. Current geometry
must remain correct, and the next foreground launch must observe it.

No SFTP, SCP server, Unix account database, passwords, forwarding, SSH
certificates, or authorized_keys options enter this implementation.
