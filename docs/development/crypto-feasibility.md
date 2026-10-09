# Native crypto feasibility

## Scope and status

We ran independent native probes on 2026-10-09 with Go 1.25.5 and TinyGo
0.42.0. The target was `tinygo/targets/goos-userspace-amd64.json`, with
`goos_hz100 goos_validation`, optimization level 2, custom GC and no scheduler.
Each selected probe ran in its own GoOS process in one QEMU guest per toolchain
configuration. Host tests supplement these results; they do not establish
native compatibility.

Authenticated native HTTPS passed the offline TLS gate after we selected and
adapted the pinned Go software stack. The installed TinyGo TLS package wraps
a device-offloaded connection. Its `HandshakeContext` returns nil without
writing a ClientHello. We must not use it over GTCP1 or interpret its return
value as an authenticated handshake.

## Reproduction

```sh
make crypto-check
make test-crypto
make test-crypto CRYPTO_COMPAT=1
```

`crypto-check` builds every dependency independently and exits nonzero when any
build fails. `test-crypto` also executes the successful builds, even when other
packages fail to compile. The TLS availability probe returns failure until we
verify a real software handshake; construction alone cannot pass.

Reports go into a new directory under `/Volumes/Data/tmp/`. Set `CRYPTO_WORK`
to a fresh directory there to keep the path predictable. The report contains
`toolchain.txt`, `build.tsv`, `native.tsv`, per-package build logs, ELF/native
images and `native.log`. Reusing a directory with an existing disposable tree
or compatibility root fails rather than mixing results.

The command `/system/bin/cryptoprobe` checks kernel randomness and clocks.
The VM harness adds separately built `crypto-PACKAGE` commands to its disposable
system tree. It reduces the test command index to keep it within the existing
4096-byte limit; it does not modify the normal installed tree. Probe certificates,
signatures and fixed keys provide no security outside tests. They do not serve
as system trust material.

## Package classification

| Package | Native result | Classification |
| --- | --- | --- |
| `crypto/sha256` | Known answer and incremental digest pass | works unchanged |
| `crypto/sha512` | Known answer and incremental digest pass | works unchanged |
| `crypto/hmac` | RFC 4231 HMAC-SHA256 passes | works after a small local compatibility patch |
| `crypto/aes` | FIPS AES-128 encrypt/decrypt passes | works unchanged |
| `crypto/cipher` | AES-GCM vector, open and bad-tag rejection pass | works unchanged |
| `crypto/ecdh` | RFC 7748 X25519 agreement and zero-secret rejection pass | works unchanged |
| `crypto/ecdsa` | P-256 verification and altered-signature rejection pass | works after a small local compatibility patch |
| `crypto/rsa` | RSA-2048 PSS/SHA-256 verification and rejection pass | works after a small local compatibility patch |
| `crypto/rand` | Reader backed by native kernel entropy passes | works with a small GoOS adapter and local runtime compatibility patch |
| `encoding/asn1` | Marshal/unmarshal and truncated input checks pass | works after a small local compatibility patch |
| `encoding/pem` | Decode/encode and malformed input checks pass | works after a small local compatibility patch |
| `crypto/x509` | DER parsing, signature checks and explicit-root `Verify` pass | works after a small local compatibility patch |
| TinyGo `crypto/tls` | Builds; claims handshake success without wire data | not usable under TinyGo/GoOS as a software TLS client |

The X.509 probe uses one self-signed test CA, explicit roots and a fixed fixture
time. It rejects an unknown root, an expired certificate and a changed signature.
This result does not cover ordinary leaf chains, SAN policy, production bundle
loading or attacker-controlled chain-search bounds. Those need integration gates.
We did not add ChaCha20-Poly1305: the current module has no external crypto
dependency. A software TLS integration may reuse a suitable pinned implementation
rather than introduce a primitive here.

## Native runtime compatibility

The unchanged target lacked `tinygo_longjmp`. HMAC, signature verifiers, PEM and
other packages need TinyGo's `defer/recover` continuation. We added the pinned
amd64 defer-frame sequence to the userspace assembly runtime. The stack checker
accepts only its exact three instructions under that symbol. Tests reject a
changed symbol, changed offset or added instruction; general dynamic stack
adjustments still fail.

ASN.1 and X.509 also pulled in UEFI clock and error paths. Native processes do
not have a UEFI system table. `CRYPTO_COMPAT=1` prepares a separate TinyGo root
using `tools/cryptocompat`, then substitutes a userspace backend and changes the
runtime's `time.now` implementation. It leaves installed tools and the kernel
runtime unchanged. The backend routes MONOTONIC, REALTIME, diagnostics and exit
through native GoOS hooks. Timer waits fail instead of pretending a scheduler
exists.

The tool pins TinyGo 0.42.0 and these source checksums:

- `runtime_uefi.go`: `e17517d3eca2177d9d4094abcc35347c3f650d5e52d1df92e3dd45ebe8b0f393`
- `baremetal.go`: `111e71bb241f682738895ec039dec7115b0ee16183fb240d42017545027b2e1f`

The tool records the replacement backend and patched source checksums in
`cryptocompat.json`. It creates real source directories with file symlinks;
TinyGo's GOROOT merger cannot merge symlinked namespace directories. On macOS,
preparation removes the verified-empty output directory before renaming its
staging directory, so there is a brief non-atomic gap. It refuses nonempty or
symlink output targets.

`sys.RandomRead(dst)` requests at most `kcall.MaxRandomRead` bytes per call. It
allocates nothing, fills the whole buffer on success and clears the whole buffer
on failure. `cryptosys.Reader` returns `0, error` on that failure. Its host tests
cover unsupported kernel calls; the native randomness probe also checks requests
crossing the 64-byte boundary. No userspace PRNG supplies these reads.

## Measured costs

These are observations from the compatibility run, not resource limits for a
TLS client. The native process uses a bump arena; the reported allocation delta
is also its peak unreclaimed probe allocation. `runtime.ReadMemStats` is a no-op
in this runtime, so the driver reads arena counters instead.

| Probe | Native image bytes | Probe allocation bytes | Largest frame bytes |
| --- | ---: | ---: | ---: |
| platform | 56,085 | 144 | 1,352 |
| SHA-256 | 58,550 | 320 | 1,352 |
| SHA-512 | 58,814 | 1,920 | 1,352 |
| HMAC | 77,221 | 1,072 | 1,368 |
| AES | 67,612 | 528 | 1,352 |
| AES-GCM | 71,401 | 1,184 | 1,352 |
| X25519 | 65,951 | 1,200 | 1,352 |
| ECDSA | 511,317 | 557,104 | 1,352 |
| RSA-PSS | 166,397 | 56,000 | 1,352 |
| kernel-backed `crypto/rand` | 57,372 | 64 | 1,352 |
| ASN.1 | 302,759 | 464 | 1,736 |
| PEM | 80,148 | 3,184 | 1,352 |
| X.509 | 872,870 | 568,384 | 2,104 |

The driver/runtime allocate another 32 bytes before the probe counter. The
X.509 fixture costs about 817 KiB of image bytes over the platform probe and
555 KiB of arena allocation. Most of the ECDSA allocation appears in both the
ECDSA and X.509 probes; we have not measured root-bundle or ordinary-chain costs.
A largest-frame check does not prove total call-depth safety.

## Software TLS follow-up

We also copied the software `crypto/tls` implementation from the installed,
pinned Go 1.25.5 source into a disposable compatibility root. An unchanged
native build rejects goroutines in `crypto/tls/conn.go`, `context` and `time`.
A temporary synchronous-handshake patch rejects cancelable contexts and QUIC,
leaving operation deadlines to the transport. That removes the handshake/context
compiler failures, but the build still reaches the timer in
`crypto/internal/sysrand.Read`.

We then routed `crypto/internal/sysrand` to the native kernel generator and
replaced TLS's weak certificate cache with direct X.509 parsing. These changes
avoid the remaining timer and unsupported weak-GC symbols. They do not replace
crypto primitives. The entropy override needs a separate local GOROOT as well
as TINYGOROOT; TinyGo's merger does not select that new internal override from
its own source tree alone.

The adapted software stack compiled and ran in native GoOS. The recording
transport observed ClientHello, followed by the probe's deliberate EOF. Its
native image was 1,414,077 bytes, the probe allocated 18,832 bytes and the
largest frame was 20,696 bytes in the included ML-KEM implementation. The probe
configured TLS 1.3/X25519; inclusion of ML-KEM does not mean negotiation or
release support. The probe still exits 1 because it has no authenticated peer.
That availability result preceded integration; the authenticated exchange and
negative results below come from the later native TLS gate.

Use the pinned software TLS stack with these narrow compatibility changes and
standard X.509, as recorded in [ADR 0198](../adr/0198-native-tls-and-system-trust.md).
The measured primitives justify reusing standard crypto. The availability probe alone does not establish root-bundle loading, production
trust policy or fetch HTTPS behavior. The offline integration gate now supplies
that evidence for the committed fixtures.

## Authenticated native integration

`make test-tls` passed 69 assertions on 2026-10-09. The guest ran GET, HEAD,
relative redirects, numeric-IP SAN verification and VFS output through native
fetch. It refused HTTPS downgrade, wrong identity, unknown CA, expired/future
certificates, invalid certificate signatures, unknown critical extensions,
plaintext/malformed records, a bad CertificateVerify, corrupted application
records and an invalid server Finished. The rejected handshake peers recorded
zero HTTP requests. The tamper peer received one authenticated request before
corrupting its response.

The peer uses host Go TLS, fragmented to 137-byte reads/writes. The Finished
fault uses a source-pinned, test-only stdlib server mutation that changes one
Finished byte before the standard record layer encrypts it. Neither test prints
secrets or implements another server stack.

Native TLS consumers now call the pinned runtime package initializers after
arena setup. Without that call, TLS alert names remained uninitialized despite
successful authentication. The initialized run reports the expected bad-record
MAC and invalid-Finished diagnostics. Non-TLS native entry points and the kernel
runtime remain unchanged.

The client prefers AES-128-GCM even without hardware AES flags, retaining all
three standard TLS 1.3 suites. The final gate negotiated cipher 4865
(`TLS_AES_128_GCM_SHA256`). An earlier authenticated native run negotiated 4867
(`TLS_CHACHA20_POLY1305_SHA256`), confirming that maintained primitive is usable
through the pinned stack.

| Measurement | Observed native value |
| --- | ---: |
| Canonical production PEM | 181,603 bytes, 121 roots |
| Production root DER total | 129,143 bytes |
| Production store allocation | 1,360,864 bytes |
| Production store process heap high-water | 1,363,216 bytes |
| Production store load time, this TCG run | 264,065,061 ns |
| Valid fixture handshake allocation, including verification | 361,200 bytes |
| Valid fixture process heap high-water at handshake | 382,256 bytes |
| Two-hop redirect process heap high-water | 806,656 bytes |
| Largest compiled TLS frame | 20,696 bytes |

The handshake measurement includes the presented leaf/intermediate chain,
verification state, ephemeral keys and record buffers; it does not isolate a
certificate-only allocator. The stack figure includes the compiled ML-KEM
routine, which the X25519-only client does not negotiate. The fixed redirect and
address limits bound connection attempts within a one-shot process. We do not
reset an arena underneath live TLS or library globals. TCP and VFS buffers use
existing shared mappings outside the bump-heap counter.

Full PEM loading fits the native quota and stack model, so the release keeps PEM
instead of adding a binary root format. These observations describe these
fixtures; they are not proofs of worst-case memory or stack behavior.
