# Native SSH access

GoOS implements SSH remote administration in `remoted`. PID 1 supervises the
service and owns remote shells and jobs. `remoted != OpenSSH sshd`; this service
uses native terminal sessions, configuration, and capability grants.

## Setup

Create `/home/user/.ssh` and install your Ed25519 public key in
`/home/user/.ssh/authorized_keys`. Copy the public key, not the private key.
The file accepts `ssh-ed25519 BASE64 [comment]`, blank lines, and `#` comments.
GoOS rejects options, certificates, unsupported key types, and malformed files.

The default listener is disabled. After installing the key:

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
The login name is `user`. Password and keyboard-interactive authentication are
unavailable. GoOS accepts one interactive PTY shell per connection; it rejects
`exec`, SFTP/SCP, forwarding, agent, and X11 requests.

To test the minimum algorithm set:

```sh
ssh -o KexAlgorithms=curve25519-sha256 \
    -o HostKeyAlgorithms=ssh-ed25519 \
    -o PubkeyAcceptedAlgorithms=ssh-ed25519 \
    -o Ciphers=aes128-gcm@openssh.com \
    -i ~/.ssh/id_ed25519 user@GOOS_ADDRESS
```

Automation should use its own `UserKnownHostsFile` and verify host identity.
Do not disable host-key checking globally.

## Configuration

`remoted` has a read-only `system.remote.` configuration session. Configure the
following keys through the normal typed `config` service, then use `remote reload`.

| Key suffix | Type | Default | Bounds |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | bool |
| `listen-address` | text | `0.0.0.0` | canonical IPv4 |
| `port` | uint | `22` | 1..65535 |
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
admission policy snapshot. Disabling the listener stops new admissions.

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

## Terminal and lifecycle

Local consoles and SSH workers use `tty.Engine` for edited lines, raw input,
Ctrl-C/Ctrl-D/Ctrl-U, and prompt-safe output. GTTY version 4 carries dimensions
and TERM. SSH `pty-req` requires dimensions in 1..1000 and printable TERM at most
64 bytes. `window-change` updates stored dimensions. The shell uses the latest
COLUMNS, LINES, and TERM before the next foreground launch.

Native terminal jobs share one PTY output stream for stdout and stderr. A command
can redirect stderr through the normal shell pipeline; an interactive SSH client
receives unredirected diagnostics on the same terminal as ordinary output.

Live resize delivery to an already-running application remains follow-up work;
no general signal subsystem enters this feature. Disconnect in raw mode ends
that session and its foreground job without changing another terminal's mode.

PID 1 creates remote shell resources only after authentication and PTY/shell
acceptance. The shell receives no TCP endpoint, SSH keys, console handle, or
worker-launch channel. On disconnect, shell exit, worker failure, or protocol
failure, PID 1 stops/reaps the session's jobs and releases its pages, endpoints,
streams, and broker sessions. Generation-tagged IDs reject late messages from
reused slots.

## Security boundaries and limits

- The controller holds one GTCP1 session. Workers receive only their bounded
  ciphertext relays and immutable policy page, not listener authority.
- PID 1 admits at most two pending handshakes and four authenticated sessions.
  Runtime session limits can lower the authenticated cap.
- Authorized-key files have a 64 KiB bound, 64-key cap, and 4096-byte line bound.
- SSH packet, channel-window, identification, request, terminal-buffer, and
  deadline bounds prevent a peer from allocating unbounded state.
- Object-aware VFS scopes protect private identity through aliases, renamed
  ancestors, retained handles, ordinary reads, and executable/bulk reads.
- `remote` commands get status/reload authority only. They do not receive private
  key access or a worker-launch channel.
- Logs record lifecycle, public fingerprints, and rejection reasons. They do not
  record terminal input, decrypted payloads, or private/session keys.

## Development verification

Run focused host tests during development and the full host suite before native
integration. The SSH compatibility build verifies pinned upstream source hashes
and prepares private Go/TinyGo roots under build output; it does not edit installed
toolchains. Reusable packet and key-generation regions bound native allocation
across traffic and rekey.

The final gate uses one primary VM boot with a real OpenSSH client, then one
reboot on the same disk. It must prove authentication/rejection, local and remote
terminal behavior, raw mode, geometry, stored resize, session cleanup/capacity,
reload rollback, unsupported requests, and persistent private-key isolation.
On 2026-10-10, `make test-remote VALIDATION=1` passed 65 assertions across both
boots. The headless Q35/TCG run verified the mandatory algorithms, public-key
login and refusal, terminal output and diagnostics, Ctrl-C/Ctrl-D, raw viewer
geometry, stored resize, four authenticated sessions, two pending handshakes,
forbidden requests, disconnect cleanup, reload rollback, and persistent identity.
The rebooted remote shell received permission denied when it tried to read the
host private key. Framebuffer pixel rendering and forced worker crashes are not
part of this headless gate; terminal isolation and worker-failure cleanup have
host-test coverage.

See [ADR 0199](../adr/0199-native-remote-terminal-sessions.md) for ownership and
compatibility decisions.
