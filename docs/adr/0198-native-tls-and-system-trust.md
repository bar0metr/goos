# ADR 0198: Native TLS and system trust

## Status

Accepted. Native TLS, signed trust updates and the complete release-check
matrix pass.
Builds on ADR 0196 and the [native crypto feasibility results](../development/crypto-feasibility.md).

## Context

Native `fetch` uses GTCP1 and HTTP/1.1. GoOS needs authenticated HTTPS without
moving protocol parsers or trust policy into the kernel or network services.
We ran the pinned crypto packages inside GoOS. Standard primitives and X.509
work with a native runtime compatibility overlay, but TinyGo's TLS package
returns handshake success without writing TLS records.

## Decision

Use the software `crypto/tls` implementation from pinned Go 1.25.5, compiled
with TinyGo 0.42.0. Reuse `crypto/x509`, SHA-256/SHA-512, HMAC, AES-GCM, X25519,
ECDSA and RSA. Do not implement replacement primitives, ASN.1 or a second TLS
record/handshake stack.

Keep the compatibility changes outside installed tools in a reproducible,
local toolchain overlay. Native runtime hooks provide kernel entropy,
MONOTONIC and REALTIME. Remove scheduler-based handshake cancellation and
reject cancelable contexts and QUIC. The GTCP1 adapter provides monotonic
operation deadlines. Replace the unsupported weak certificate cache with direct
parsing so certificates do not survive a request arena through library globals.
TLS consumers initialize the pinned packages after reserving their arena;
other native entry points keep their existing startup.

`userspace/tlsclient` wraps a generic stream. Native callers supply
`cryptosys.Reader`, UTC from `sys.ClockNow(timekeeping.Realtime)`, explicit
system roots and a bounded transport deadline. The configured protocol is TLS
1.3, with the standard implementation's TLS 1.3 suites and X25519 key exchange.
The client prefers AES-128-GCM/SHA-256, retaining the upstream TLS 1.3 suites.
The native interoperability gate negotiated that required suite. No session cache,
persisted tickets or client certificate enters the release configuration.

Install pinned, offline trust data under `/system/security/ca-certificates.pem`.
The 121-root production PEM occupies 181,603 bytes and consumed 1,360,864 bytes
of native arena allocation in the root probe, so we keep PEM. Missing, malformed
or empty trust data fails HTTPS. Production images exclude the validation CA
and test private keys. Trust source identity and measurements belong in the
trust artifact's provenance and development report.

Verify DER, signatures, a chain to explicit roots, REALTIME validity, CA/path
constraints, usages and DNS/IP SAN using X.509. Do not use a Common Name fallback
or fetch missing intermediates. Reject unknown critical extensions. Bound peer
chains to eight certificates, each DER object to 32 KiB, DNS SAN entries to 64
and chain construction depth to eight. Keep the standard verifier's signature
attempt budget. Parsing and verification remain native userspace code.

HTTPS uses the shared fetch request/body/output path. DNS names supply SNI;
numeric addresses require IP SAN and omit DNS SNI. Refuse followed
HTTPS-to-HTTP redirects before connecting to the next hop. Do not expose an
insecure CLI option or a user-selected root path. On a different scheme/host/port
origin, do not forward Authorization, Proxy-Authorization, Cookie or a custom
Host header.
Retry another resolved IP only after a TCP connection failure. Once TCP connects,
any TLS failure ends the request. Reject HTTP 101 instead of treating a protocol
switch as an informational response.

TLS requires no new capability: native fetch already holds resolve, TCP and VFS
grants. Read roots through its VFS authority. A narrower root-read authority
for future network-only clients is future work, not a trust-store service.

## Signed trust lifecycle

Keep immutable bootstrap roots, `/system/security/trust-update-key.pub` and
`/system/security/trust-update-url`. The key authorizes independently signed
packages; the URL is a build-time system-data input. An empty URL disables
network update until a publisher supplies an endpoint. Builds stay offline.

Use standard Ed25519, validated natively with RFC 8032 and tamper rejection.
Authenticate `GOOS-TRUST-V1\n`, exact canonical JSON bytes and raw SHA256(PEM).
Metadata carries a trust sequence independent of GoOS 0.2.6; provenance and
bundle identity are generated data, not constants in the tool.

Write a new staging directory under `/state/security`, close and reverify all
files, write its completion marker, then rename it into `generations/SEQUENCE`.
Replace `active` last. Do not rewrite existing generations. FAT32 flushes
completed changes; signed recovery and immutable fallback cover incomplete
publication without claiming a journal or arbitrary torn-sector atomicity.

Select the highest surviving published signed generation, ignoring a selector
that points lower. Refuse incoming lower sequences or equal sequences with
different bytes. Retain corrupt state for diagnosis. Staging does not consume
the bounded generation listing. Serial installs stop before 64 existing published
generations or 256 total directory entries; no automatic history deletion occurs.
A volatile `/run/trust-update.lock` serializes installation and publication.
An interrupted installer can leave that lock until reboot; it cannot change
root selection or persist across boots. Disk snapshot rollback
and deletion of all state history remain outside this software-only protection.

`trust status/verify/install/update` reuse existing VFS grants and shared native
fetch transport. Each update download retains current roots until installation
finishes. HTTPS protects transport, but the signed trust package is authorized
by the immutable trust-update public key. No kernel or network service verifies
packages. The updater follows no redirects and accepts only HTTP 200 bodies.

## Alternatives

- TinyGo's device-offloaded `crypto/tls` cannot secure a GTCP1 stream. The
  native wire probe observed a successful no-op handshake with zero output.
- The unchanged Go software TLS stack requires scheduler support and weak GC
  references. Native execution emitted ClientHello after the bounded runtime,
  context, entropy and cache adaptations. The native gate now confirms
  authenticated transfers and certificate, CertificateVerify, Finished and
  record-authentication rejection.
- A bounded userspace TLS implementation would duplicate maintained protocol
  code. Keep it as a fallback only if the selected stack fails native
  authentication or resource validation.

## Consequences

We must maintain the small compatibility patches against pinned source hashes
and test them when changing toolchains. Native programs use a bump allocator;
root state must outlive request arenas, and reset must not invalidate library
caches. Binary, heap, stack, root and chain costs require measurements rather
than `runtime.ReadMemStats`, which the native runtime does not implement.

Exclude TLS server mode, client certificates, resumption, 0-RTT, revocation
network requests, OCSP stapling validation, Certificate Transparency, AIA,
automatic CA updates, HTTP/2, HTTP/3 and QUIC. The kernel supplies randomness and
clocks; `netd` supplies TCP; `resolved` supplies names. None receives a TLS,
X.509 or HTTP parser from this work.
