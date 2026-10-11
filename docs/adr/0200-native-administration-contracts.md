# ADR 0200: Native administration contracts

## Status

Accepted. Host and QEMU validation results are recorded in the remote-access
guide and release notes. Exact-candidate Proxmox evidence is retained separately.

## Context

Operators need SSH commands with useful status codes, running terminal programs
that follow size changes, filtered live logs, and authorized-key updates from
GoOS. The existing interactive SSH path owns a GTTY session and one output
stream. It cannot serve byte-exact command input or separate SSH stderr without
a distinct startup contract.

The kernel retains 64 structured log records. Its text reader uses a relative
oldest-first index while bounds use absolute sequences. Combining those calls
races ring overwrite. The config service's existing set operation reports a
live update even if persistence fails; hostname needs a durable acknowledgement.

## Decision

### Non-PTY command execution

PID 1 launches the existing native shell in one-shot mode. The worker supplies
separate native stdin, stdout and stderr stream ends. One-shot startup requires
no terminal handle and emits no interactive prompt or terminal transformations.
The shell keeps the same command authorities and native parser limits as an
interactive session.

Accept one shell or exec start on the existing single session channel. Reject
PTY exec, malformed requests and duplicate starts. Reject unquoted background
syntax before executing any portion of a one-shot command. Quoted and escaped
ampersands remain argument data. Retain the supported sequence, pipeline and
redirection grammar without a POSIX shell.

The shell propagates builtin and child results. A sequence returns its last
command's status; a pipeline retains the native first-nonzero-stage result.
SSH EOF closes stdin only. The worker drains both output streams before sending
exit status and closing the channel, including completion-before-output races.
Interactive shell behavior and remote machine-shutdown draining remain intact.

### Foreground geometry

Version GTTY geometry notifications and tag each snapshot/event with a terminal
session sequence. Foreground launch receives snapshot N. Consumers apply only
a sequence greater than their highest applied value. This rule also covers
notifications that arrive before startup or foreground handoff completes:
an older snapshot cannot overwrite a newer size.

PID 1 owns the resize writer and grants a geometry-only receive endpoint to the
current foreground raw application. Background jobs receive no endpoint.
Terminal release, child exit and nested-launcher handoff close or replace the
route. Restore the parent's newest snapshot after its nested child returns.
Coalesce pending notifications to the newest sequence under backpressure.

Separate authoritative dimensions from framebuffer display identity. Native
TUI waits observe input and resize readiness together, then rebuild bounded
caller-owned screens. Preserve local framebuffer handling and serial fallback.

### Log cursors

Read structured records by absolute sequence under the ring lock, with retained
bounds and overwrite information from the same snapshot. Keep existing text
readers and persistent log consumers compatible.

Each reader owns its cursor. Cursor-aware readiness joins terminal cancellation
in one blocking wait, without changing the existing WaitAny ABI. The additive
WaitAnyCursor form carries a cursor for each log interest. Register before the
final readiness check. Append sets a pending flag after releasing the ring lock;
kernel housekeeping wakes eligible readers outside lifecycle lock sets, without
recursive logging.

Select the newest N matching retained records, then print in chronological
order. Levels and component names match structured fields exactly. Follow
advances over nonmatching records, reports an overwrite gap, resumes at the
oldest retained sequence and checks cancellation between bounded batches.

### Durable hostname mutation

Add `system.hostname`, default `goos`, as a display-only ASCII hostname label:
1..63 letters, digits or hyphens, with no leading or trailing hyphen. Enforce
validation in the config schema as well as the command.

Give the hostname image an exact-key config authority and a new acknowledged
durable mutation operation. Success means the service saved the candidate;
persistence failure must produce an error rather than a successful live-only
update. A definite failure rolls back the live value and revision. If the file
replacement acknowledgement is lost, return an unknown-durability error, retain
the tentative live value and retry the same save on VFS renewal. Repeating the
same hostname mutation can confirm persistence. Preserve existing `config set`
semantics and test both operation paths.

### Authorized-key transactions

Reuse the Ed25519 parser and SSH public-blob fingerprint implementation. Keep
64 KiB/file, 64 keys and 4096 bytes/line. The bounded editor preserves valid
entries/comments, rejects duplicate additions, and removes exactly one full
fingerprint match. An empty list denies new authentication.

PID 1 confines temporary administration authority to the configured public-key
file and named staging siblings. Preserve host private-key protection through
aliases, retained handles and old path scopes. Key mutations must not create,
rotate or rewrite host identity.

Serialize key edits and reloads with a remote-specific guard. Capture scoped
config revision and prior file ownership/content, validate the whole candidate,
close/reread the staged file, recheck ownership and replace in the same
filesystem. Acknowledge success after policy commit. Failed reload retains the
previous effective key set and restores previous persisted content while
transaction ownership still matches. Refuse rollback over an unrelated edit.
Established sessions retain their admission policy; new connections use the
committed keys.

## Consequences

Each new administration command remains a separate manifest-backed native image.
Shared helpers provide bounded streaming and native VFS/config/time access.
There is no GNU option-compatibility promise, regex engine, find expression
language or timezone database.

Atomic file replacement and acknowledged service commit do not establish a
journal or arbitrary crash atomicity across FAT and remoted. Config revision
checks and file replacement are not a cross-service transaction: a config change
after final validation can leave candidate bytes on disk while the previous
policy remains effective. Skip rollback when ownership no longer matches; retain
a verified backup if rollback fails. Test staged-write, replacement, reload and
rollback failures.

Release acceptance combines focused host tests, the repository race gate,
remote/network/console-GUI QEMU gates, and one retained Proxmox VM on the exact
candidate SHA. Pin SSH host identity from trusted serial status and use strict
checking with disposable client keys. Retain serial/report evidence and artifact
hashes, including observed reboot and poweroff results.

SFTP/SCP, forwarding, POSIX shells/PTYS, IPv6, dynamic linking and Linux networking
syscalls remain outside this change.
