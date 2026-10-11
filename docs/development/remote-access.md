# Native SSH access

GoOS implements SSH remote administration in `remoted`. PID 1 supervises the
service and owns remote shells and jobs. `remoted != OpenSSH sshd`; this service
uses native terminal sessions, configuration, and capability grants.

## Setup

From the trusted local console, add your Ed25519 public key:

```text
remote key add "ssh-ed25519 AAAA... laptop"
remote keys
```

Use the complete public-key line, not private-key material. The default
configured file is `/home/user/.ssh/authorized_keys`; key add supports a missing
file. Images may also supply public keys there before boot. The file accepts
Ed25519 lines, blank lines and `#` comments. GoOS rejects options, certificates,
unsupported key types and malformed files. After scope installation, ordinary
filesystem sessions cannot write the authorized-key file; use `remote key`.

The default listener is disabled. After adding the key:

```text
config set system.remote.enabled true
config set system.remote.port 22
remote reload
remote status
```

Connect with a contemporary OpenSSH client:

```sh
ssh -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS
```

Check the host fingerprint against `remote status` before trusting a new host.
The default login name is `user`; `system.remote.user` selects the SSH login
label. It does not select an account or home directory. Password and
keyboard-interactive authentication are
unavailable. GoOS accepts one interactive PTY shell or one non-PTY command
execution per connection. SFTP/SCP, forwarding, agent, and X11 requests remain
unavailable.

To test the minimum algorithm set:

```sh
ssh -o KexAlgorithms=curve25519-sha256 \
    -o HostKeyAlgorithms=ssh-ed25519 \
    -o PubkeyAcceptedAlgorithms=ssh-ed25519 \
    -o Ciphers=aes128-gcm@openssh.com \
    -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS
```

For automation, obtain the host fingerprint through trusted serial
`remote status`. Compare it with the public key obtained by `ssh-keyscan`, then
pin that exact key in a dedicated `UserKnownHostsFile`. Use
`StrictHostKeyChecking=yes`; a scan alone does not establish host identity.

## Command execution

Run native GoOS commands without allocating a PTY:

```sh
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS uname
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'net status'
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'config get system.net.mode'
ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'echo one; echo two'
printf 'needle\nother\n' | ssh -T -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS 'grep needle'
```

The one-shot shell uses the native parser: a command line has a 4096-byte bound,
up to eight sequence commands, and up to four pipeline stages including
redirection helpers. Supported native pipelines and redirections work without
`/bin/sh -c`, variable expansion, or a POSIX shell. Background `&` rejects the
whole command before execution; quoted or escaped ampersands remain argument
data. A PTY request followed by exec also fails, so use `-T`, not `-t`.

The worker connects SSH stdin, stdout and stderr to separate native streams.
Input EOF closes stdin. GoOS sends the native command status after draining both
output streams, then closes the channel. Sequences return the last command's
status; pipelines retain GoOS's first-nonzero-stage status. One-shot startup has
no terminal handle, prompt, banner, echo or terminal newline conversion. It
receives the same command authorities as an interactive shell.

## Authorized keys

Use the native administration command after configuring the authorized-key path:

```text
remote keys
remote key add "ssh-ed25519 AAAA... laptop"
remote key remove SHA256:...
```

`remote keys` lists canonical public-key fingerprints and optional comments in
file order. Add accepts one Ed25519 public-key line and rejects duplicate keys.
Remove requires the full fingerprint and exactly one match; unknown or ambiguous
fingerprints fail. Removing the final key leaves a deny-all list. Existing
connections remain open; new authentication uses the committed set.

Successful mutations replace the configured authorized-key file and reload the
policy without reboot. The editor preserves valid entries and comments within
the existing file, key-count and line bounds. Parse, write and reload failures
keep the previous effective policy; rollback must not overwrite an unrelated
concurrent file change. Atomic file replacement does not promise a journal or
filesystem-plus-runtime crash atomicity.

PID 1 confines key administration to the configured public-key file and its
transactional staging siblings. The command receives no host private-key
access. Host identity protection and worker signing grants remain unchanged.

## Configuration

`remoted` has a read-only `system.remote.` configuration session. Configure the
following keys through the normal typed `config` service, then use `remote reload`.

| Key suffix | Type | Default | Bounds |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | bool |
| `listen-address` | text | `0.0.0.0` | canonical IPv4 |
| `port` | uint | `22` | 1..65535 |
| `user` | text | `user` | 1..32 ASCII letters, digits, `_`, `-` |
| `authorized-keys` | text | `/home/user/.ssh/authorized_keys` | canonical absolute path, at most 64 bytes |
| `host-key` | text | `/state/security/ssh/ssh_host_ed25519_key` | canonical path beneath `/state/`, at most 64 bytes |
| `max-sessions` | uint | `4` | 1..4 |
| `max-auth-attempts` | uint | `3` | 1..10 |
| `handshake-timeout-ms` | uint | `15000` | 1000..120000 |
| `idle-timeout-sec` | uint | `0` | 0..86400; zero disables |
| `kex` | text | `curve25519-sha256` | compiled algorithm allow-list |
| `ciphers` | text | `aes128-gcm@openssh.com` | compiled algorithm allow-list |
| `host-key-algorithms` | text | `ssh-ed25519` | compiled algorithm allow-list |
| `user-key-algorithms` | text | `ssh-ed25519` | compiled algorithm allow-list |

Use the full prefix, for example `system.remote.port`. Algorithm lists contain
comma-separated canonical names. Unknown, duplicate, malformed, and unsupported
names reject the candidate. This implementation compiles the minimum set above;
legacy SHA-1, CBC, RSA/SHA-1, DSA, and RC4 paths remain unavailable.

Reload validates the complete candidate, loads its keys, and prepares any
replacement listener before commit. A failed reload keeps the running policy.
Changing only policy reuses the listener. Established connections retain their
admission policy and host identity. Disabling the listener stops new admissions.
The authorized-key path is independent of the login label; changing `user` does
not rewrite `authorized-keys` or change shell/VFS authority.

If startup cannot commit a valid candidate, PID 1 keeps `remoted` running with a
confined bootstrap VFS session and no listener. `remote status` reports
`service=running listener=closed settings=invalid reason=...`. Fix the settings
and run `remote reload` to recover without restarting the service. A rejected
replacement after a valid commit keeps the previous listener and policy.

GCF1 scoped revisions bracket candidate reads, including concurrent changes that
restore a previous value. Path changes also need free VFS scope capacity. Eight
scope slots retain old private-path protection during the VFS lifetime; reload
rejects scope exhaustion rather than exposing a previous host key.

## Host identity

The image contains no SSH private host key. On the first successful identity
setup, `remoted` generates an Ed25519 seed with kernel entropy and stores:

```text
/state/security/ssh/ssh_host_ed25519_key
/state/security/ssh/ssh_host_ed25519_key.pub
```

The private file contains a raw 32-byte seed, not an OpenSSH private-key envelope.
The public file uses the SSH authorized-key format. Staging siblings use `.new`.
HostStore closes and rereads both files, verifies their relationship, publishes,
and verifies the final pair before listener activation.

Corrupt or conflicting final/staging identity fails closed. GoOS does not replace
it with a new host identity. Repair identity state through trusted maintenance;
do not delete it merely to suppress an unexplained host-key warning.

`remote status` exposes only the standard SHA256 public-key fingerprint. A reboot
using the same persistent disk must keep that fingerprint.

`remoted` owns private keys in a fixed identity pool. Connections with the same
host identity share one slot. Each worker holds a dedicated signing endpoint and
a public-only policy page. PID 1 creates the channel pair and grants the worker
endpoint without Transfer authority; the shell receives neither endpoint.

The pinned Curve25519 KEX computes `H = SHA256(exchange transcript)` and forwards
those 32 bytes unchanged to `ssh.Signer.Sign`, on initial exchange and rekey.
The controller signs H with ordinary Ed25519 and returns a 64-byte signature.
The worker verifies it against its admission public key. A signing timeout,
protocol violation or failed signature closes that signing route without a
private-key fallback. The five-second RPC bound also respects the remaining
initial-handshake deadline.

Existing connections retain their admission identity after a host-key reload.
The controller keeps a retired key until its last worker releases the slot, then
zeros it. Eight slots cover six worker-held identities, the current identity and
a staging candidate; failed candidates leave the active identity unchanged.
A compromised worker can request signatures on chosen 32-byte inputs. Framing
and capability checks do not establish SSH transcript provenance.

## Terminal and lifecycle

Local consoles and SSH workers use `tty.Engine` for edited lines, raw input,
Ctrl-C/Ctrl-D/Ctrl-U, and prompt-safe output. SSH `pty-req` requires dimensions
in 1..1000 and printable TERM at most 64 bytes. Sequence-ordered GTTY geometry
notifications deliver `window-change` to the current foreground raw application.
A running `gview`, `gfm` or `edit` redraws without restart. Background jobs receive
no resize authority; each application retains its screen-buffer size bounds.

Foreground startup supplies geometry at sequence N. The shell and child apply
only newer sequences, including a resize that races with launch or a nested
foreground handoff. An old startup snapshot cannot overwrite a newer event.
Cooked input uses the newest geometry after the raw application exits. Local
framebuffer and serial terminal size handling retain their existing behavior.

Interactive terminal jobs share one PTY output stream for stdout and stderr.
A command can redirect stderr through the native shell pipeline; unredirected
diagnostics appear on the same terminal as ordinary output. Non-PTY exec uses
separate SSH stdout and stderr instead.

Use `exit` or Ctrl-D to end an interactive SSH shell. `reboot` and `poweroff`
request the normal storage flush and machine lifecycle. PID 1 and the worker
drain output and close SSH before the machine resets or powers off. Ctrl-C stops
a foreground command or log follower and returns to the shell.

PID 1 creates shell resources after authentication and accepted shell/exec
requests. The shell receives no TCP endpoint, SSH keys, console handle, or
worker-launch channel. On disconnect, shell exit, worker failure, or protocol
failure, PID 1 stops/reaps the session's jobs and releases pages, endpoints,
streams and broker sessions. Generation-tagged IDs reject late messages from
reused slots. Disconnect in raw mode leaves other terminals unchanged.

## Native administration tools

Each tool runs as a separate native image with a capability manifest, bounded
working memory, `--help`, stderr diagnostics and a nonzero failure status.

| Command | Supported form |
| --- | --- |
| `head` | `head [-n N] FILE`, default 10 lines |
| `tail` | `tail [-n N] FILE`, default 10 lines, no follow flag |
| `grep` | `grep [-i] PATTERN [FILE]`, plain substring, stdin when omitted |
| `wc` | `wc [-l\|-w\|-c] [FILE]`, stdin when omitted |
| `find` | `find PATH [-name NAME]`, exact basename match, bounded traversal |
| `hexdump` | `hexdump FILE`, offset, hex and printable ASCII rows |
| `hostname` | `hostname [NAME]`, typed persistent `system.hostname` |
| `date` | `date [-u]`, current realtime UTC |

Hostname is a single ASCII label of 1..63 letters, digits or hyphens, with no
leading or trailing hyphen. Its default is `goos`; it controls display only.
`hostname NAME` uses a scoped acknowledged durable config mutation and reports
success after saving. If the service loses the replacement acknowledgement, the
command reports an uncertain persistence outcome instead of success. Repeat
the same value to confirm it. Existing `config set` semantics remain unchanged.

These tools expose native operations rather than GNU-compatible option surfaces.
Use `--help` for input and traversal bounds. No regex engine, find expression
language or timezone database enters this command set.

## System log

The shell's `log` builtin reads the bounded structured kernel log ring:

```text
log
log -n 20
log --level warn
log --component remoted
log -n 50 --level error
log -f --component remoted
```

Levels `debug`, `info`, `warn` and `error` match exactly. Component names also
match exactly. `-n N` selects the newest N matching retained records and prints
them in chronological order. The ring retains at most 64 records.

Follow prints its selected backlog, then blocks on cursor-aware log readiness
in the same wait as terminal cancellation. New records wake readers without
polling; readers keep independent cursors. Ring overwrite produces a gap notice
and resumes at the oldest retained sequence. Ctrl-C stops follow and restores
the local or remote shell prompt.

## Security boundaries and limits

- The controller holds one GTCP1 session. Workers receive only their bounded
  ciphertext relays, public-only immutable policy and their signing endpoint,
  not a host seed/private key or listener authority.
- PID 1 admits at most two pending handshakes and four authenticated sessions.
  Runtime session limits can lower the authenticated cap.
- Authorized-key files have a 64 KiB bound, 64-key cap, and 4096-byte line bound.
- SSH packet, channel-window, identification, request, terminal-buffer, and
  deadline bounds prevent a peer from allocating unbounded state.
- Object-aware VFS scopes protect private identity through aliases, renamed
  ancestors, retained handles, ordinary reads, and executable/bulk reads.
- `remote` commands get bounded status/reload/key-management authority. They
  receive no private-key access or worker-launch channel.
- Logs record lifecycle, public fingerprints, and rejection reasons. They do not
  record terminal input, decrypted payloads, or private/session keys.

## Development verification

Run focused host tests during development and the full host suite before native
integration. The SSH compatibility build verifies pinned upstream source hashes
and prepares private Go/TinyGo roots under build output; it does not edit installed
toolchains. Reusable packet and key-generation regions bound native allocation
across traffic and rekey.

The QEMU gate uses one primary boot with a real OpenSSH client and one reboot
on the same disk. It checks command stdout/stderr/status, stdin EOF, exec
rejection cases, live resize of the same process, key add/remove authentication,
log filtering/follow, local cancellation, session cleanup/capacity and graceful
remote poweroff. Reboot checks cover hostname, key changes and private-key
isolation. Run `go test ./...`, `make test-race` and focused race tests for changed
host-testable packages before `make test-remote VALIDATION=1`, `make test-network`
and `make test-console-gui`.

Run the final release-candidate check on the exact clean candidate SHA:

```sh
make pve-run VALIDATION=0 PVE_FLAGS='--scenario remote-admin --require-clean'
```

Without the scenario override, `pve-run` selects smoke. Use disposable Ed25519
client keys and pin the host public key against trusted serial `remote status`.
Automated SSH calls require `StrictHostKeyChecking=yes`, including reconnects
and expected authentication failures. Retain the VM for manual validation and
save the report, serial log, candidate SHA and artifact hashes. Check reboot
and poweroff on that same VM; record its final power state.

The earlier `make test-remote VALIDATION=1` run on 2026-10-10 passed 76 assertions
across both boots. The headless Q35/TCG run verified degraded-start recovery, corrupt identity
retention, a custom SSH login label with an independent authorized-key path, and
forced OpenSSH rekey after reload. It also checked the mandatory algorithms,
public-key login and refusal, terminal output and diagnostics, Ctrl-C/Ctrl-D,
raw viewer geometry, stored resize, four authenticated sessions, two pending
handshakes, forbidden requests, disconnect cleanup, reload rollback and persistent
identity. The rebooted remote shell received permission denied when it tried to
read the configured host private key. Framebuffer pixel rendering and forced
worker crashes are not part of this headless gate; terminal isolation and
worker-failure cleanup have host-test coverage.

The initial hardening runs exposed a post-scan KEX failure: Go 1.25 weak key caches
retained allocations across controller request-arena resets. The private SSH
toolchain now uses a source-pinned stateless key-cache adapter. Its host tests,
native rebuild and updated two-boot gate pass. The adapter invokes upstream key
constructors without retaining expanded keys; it changes no cryptographic
algorithm. Both `test-all` and `release-check` invoke `test-remote`; development
uses focused host checks before one two-boot run.

See [ADR 0199](../adr/0199-native-remote-terminal-sessions.md) for ownership and
compatibility decisions.
