# Native SSH compatibility

Build with `sh tools/build-ssh.sh OUTPUT_ELF PACKAGE TAGS [OPT]`.
Run the host gates with `sh tools/build-ssh.sh --host-test`.

The builder pins `golang.org/x/crypto v0.53.0` (Go 1.25), Go 1.25.5,
and TinyGo 0.42.0. It copies sources into `build/sshcompat/`, generates
compatibility code there, and applies a temporary module replacement. It does
not change installed Go, TinyGo, the module cache, or the repository module file.
`sshcompat.json` records SHA256 for the complete upstream module, adapters,
and generated outputs. The generator checks a pinned complete-source-tree hash
and five individual patch inputs before publishing its output directory.

## Protocol changes

The native server uses upstream SSH framing, Ed25519 authentication and host
signatures, Curve25519 key exchange, AES-GCM, and key derivation. It replaces
background transport and channel goroutines with one explicit server pump.
It does not call upstream `NewServerConn` or the mux.

The fixed algorithm set is `curve25519-sha256`, raw `ssh-ed25519` host/user keys,
and `aes128-gcm@openssh.com`. Authentication retains upstream unsigned key
queries, signature verification, and session-bound signed data. The adapter
accepts only username `user` and up to 64 configured public keys. Native code
uses `cryptosys.Reader`; entropy failure aborts the exchange, including the
upstream KEX cookie read.

The server accepts one session channel, one PTY, shell, window-change, data,
EOF, and close. It requires PTY before shell. It rejects exec, subsystem, global
forwarding, agent and X11 requests. TERM must contain 1..64 printable
ASCII bytes (0x20..0x7e); rows and columns must lie in 1..1000, including the initial PTY.
Zero geometry does not request a default.

The packet-length limit is 32 KiB. Channel data spans at most 16 KiB, the local
window is 64 KiB, and peer credit is capped at 16 MiB. During rekey the pump
queues at most 32 KiB of control replies and withholds replenished channel
credit until it sends those replies after both NEWKEYS messages. It refuses
output after its own channel EOF/close and refuses a repeated exit status.

Before authentication, the transport accepts at most 64 packets and 128 KiB
of packet payload, including skipped IGNORE/DEBUG. `MaxAuthAttempts` bounds
failed authentication attempts to 1..10 using upstream `MaxAuthTries` semantics.
The first none attempt and accepted unsigned key queries do not consume that
failure budget. A separate hard limit allows at most 16 USERAUTH_REQUEST messages,
including those probes. The pump rejects rekey during initial authentication. During an established
exchange it consumes KEXINIT, optional guessed packet, ECDH, and NEWKEYS as
separate complete-frame steps. It rejects unrelated messages after peer KEXINIT.

## Caller and memory contract

`Handshake` uses blocking I/O. The caller must bound its reads and writes with
an absolute deadline. Afterward, `Feed` stages ciphertext and reports the
consumed prefix; `Poll` decrypts at most one complete frame or returns NeedInput.
It never reads the socket after authentication. Control replies and `Write` can
still write the socket, so the caller must also bound relay backpressure.
`NeedInput` returns the missing byte count. `Write` returns zero while a key
exchange or an empty peer window prevents output.

The native backend owns one 256 KiB packet region and two 256 KiB key regions.
It keeps the preceding key generation live through both NEWKEYS messages and
clears that generation only when the next exchange begins. Arena scopes enforce
LIFO exit and reject reentry. Region reset never discards shared slab pages.
The backend copies returned data, TERM, and errors before leaving transient
memory. Event data remains valid until the next Poll.

Keep the server and initial handshake objects above the caller's root arena
reset mark. Connection methods must not retain allocations from SSH scopes.
After `Exit`, continue pumping through peer channel close before terminating
the ciphertext relay. Bound that wait and the final peer disconnect drain.
The host OpenSSH test exercises this close handshake under a real 24x80 PTY.
