# System trust data

GoOS 0.2.6 keeps an immutable Mozilla-derived bootstrap bundle and accepts
independently signed CA updates. HTTPS protects transport, but the signed trust
package is authorized by the immutable trust-update public key.

## Bootstrap provenance

The bootstrap artifact comes from curl's Mozilla CA extract of 2026-09-25,
Mozilla source date 2026-09-25 03:12:01 UTC, extractor `mk-ca-bundle.pl` 1.33.
It contains 121 roots. Curl documents extraction at
[caextract](https://curl.se/docs/caextract.html); the dated source is
https://curl.se/ca/cacert-2026-09-25.pem. Mozilla publishes NSS
[certdata.txt](https://raw.githubusercontent.com/mozilla-firefox/firefox/refs/heads/release/security/nss/lib/ckfw/builtins/certdata.txt)
under MPL 2.0. Preserve [LICENSE-MPL-2.0](LICENSE-MPL-2.0).
Firefox-specific out-of-band CA name constraints do not accompany this PEM export.

| Artifact | Bytes | SHA-256 |
| --- | ---: | --- |
| `ca-source.pem` | 188900 | `a41b5d356aea97a529fe27e0f7316d2f9d946d75927476cf9cf1b90637d00505` |
| `ca-certificates.pem` | 181603 | `b5e44e6cf3ec2cda6131fec4e60a358ed022af5d5a8584da589b1851a56d0bb5` |

`ca-source.sha256` preserves curl's published checksum. `ca-certificates.json`
records the generated source/bundle digests, source date, sequence, count and size.
The former `ca-certificates.pem.json` remains a legacy artifact and is not an
input to builds or package verification. The tool derives hashes and counts from input bytes; it contains no fixed bundle
identity. The current bootstrap trust sequence is 2026092501; the OS version is
0.2.6. Changing signed CA data does not require an OS version change.

## Build inputs

Normal builds check the committed source and artifacts offline. They install:

```text
/system/security/ca-certificates.pem
/system/security/trust-update-key.pub
/system/security/trust-update-url
```

`TRUST_UPDATE_KEY` selects the raw 32-byte production public key, defaulting to
the committed public key. `TRUST_UPDATE_URL` supplies an HTTPS base URL for the
three package files. Its production default is `https://bar0metr.github.io/current`,
hosted in the `bar0metr/bar0metr.github.io` Pages repository. Maintainers can
override it at build time; an explicitly empty value disables network update.
Clients do not accept a mutable endpoint or a CLI trust-verification bypass.

Validation selects a separate key through `VALIDATION_TRUST_UPDATE_KEY`, an
explicit test bootstrap CA through `VALIDATION_CA_BUNDLE`, and an offline
`VALIDATION_TRUST_UPDATE_URL`. Production artifact checks reject test signing
material, private-key PEM and a validation update public key.

## Explicit key generation

Generate a long-lived production key only through a separate maintenance action.
Create a new empty directory outside any repository and ordinary build output:

```sh
keydir=$(mktemp -d "$HOME/.local/share/goos/trust-signing.XXXXXX")
go run ./tools/truststore -keygen "$keydir"
```

Create the parent directory first if needed. The tool sets the directory to
0700, writes `private.seed` with 0600 and `public.key` with 0644, and refuses
existing files, symlink directories and repository locations. Back up the
private seed in protected maintainer storage. Commit only the public key.
Ordinary build and validation targets never generate the production key.
The public validation seed is host-only fixture data and cannot authorize
production packages.

## Prepare and sign an update

Obtain the intended curl/Mozilla PEM and record its upstream source date.
Downloading source is an explicit maintainer step, never a normal build action.
Keep each downloaded input in a fresh scratch directory and run Go tooling from
the repository, not from the downloaded-data directory. Compare the upstream
published checksum and inspect the root-change report before publishing.

```sh
make trust-update \
  TRUST_SOURCE=/external/source.pem \
  TRUST_OUTPUT=/external/package/ca-certificates.pem \
  TRUST_SEQUENCE=2026101501 \
  TRUST_SOURCE_DATE=2026-10-15T00:00:00Z \
  TRUST_SOURCE_URL=https://curl.se/ca/cacert.pem \
  TRUST_OLD_BUNDLE=security/ca-certificates.pem \
  TRUST_SIGNING_KEY=/external/protected/private.seed
```

The output directory must already exist. The tool validates bounded PEM/X.509,
normalizes certificate blocks in source order, reports additions/removals and
subject-based replacements, and emits `ca-certificates.pem`, `.json` and `.sig`.
Omit the signing-key input to generate an unsigned review artifact; unsigned
artifacts cannot be installed. Do not publish partial output after a failed
maintenance command. Verify a signed package before publishing all three files.

Canonical metadata uses the declared field order, compact UTF-8 JSON and one LF.
It rejects alternative spellings, duplicate/unknown/missing fields, unsupported
format, zero sequence and inconsistent sizes/counts. `source_url` is optional
provenance, not a download or authorization input. Signature files contain 64 raw
Ed25519 bytes over:

```text
GOOS-TRUST-V1\n || exact_metadata_bytes || raw_SHA256(bundle)
```

Ordinary read-only artifact checks use:

```sh
go run ./tools/truststore -check security/ca-source.pem security/ca-certificates.pem
```

## Persistent installation

`trust verify FILE META SIG` authenticates the package without changing state.
`trust install FILE META SIG` or `trust update` commits it under
`/state/security/generations/SEQUENCE/`. All three files form one signed package.
Staging lives outside the generation list, under `/state/security/.trust-new-*`.
The installer closes and re-verifies staged data, writes a completion marker,
publishes the directory and replaces `/state/security/active` last. It then
reopens and verifies the committed selection. It preserves old/corrupt state.

The loader checks the highest surviving published generation against the
immutable public key. A lower selector cannot downgrade it. Lower incoming
sequences fail; equal sequence succeeds only for identical PEM/JSON/SIG bytes.
Invalid highest state reports a reason and uses immutable bootstrap roots rather
than trusting an older mutable update. A corrupt selector can recover a complete
signed generation. Missing or malformed bootstrap roots still fail HTTPS.

Serial installations refuse 64 existing published generations or 256 directory
entries before writing. Incomplete staging does not consume this listing.
There is no automatic history deletion or update daemon. A volatile `/run/trust-update.lock` serializes concurrent
installations and protects capacity checks through publication. A crashed
installer can leave the lock until reboot; the persistent roots stay usable. Inspect retained state before manual maintenance.

FAT32 flushes successful operations but provides no journal or arbitrary
torn-sector guarantee. Fault tests and the native reboot gate establish
old-or-complete-new selection and fail-closed recovery for the covered failures.
Rollback protection relies on surviving state; it cannot detect deletion of all
history or restoration of an older disk snapshot without protected monotonic
storage. Bootstrap roots remain a trusted fallback by policy.

## GitHub Pages production channel

The first signed publication at `https://bar0metr.github.io/current` has sequence
2026101001 and the unchanged 121-root bootstrap PEM. Its Mozilla source date stays
2026-09-25; sequence records publication/version independently of source age.
The Pages repository holds only public package files, provenance, an index and
the MPL license. The private seed stays in external maintainer storage.

Prepare replacements outside the Pages checkout, review root changes, verify,
then replace all three `current/ca-certificates.{pem,json,sig}` files in one
commit. Never publish a lower sequence or reuse one for different content.
GitHub deployment and CDN caching are not a cross-file atomic transaction;
mixed generations fail signature/digest verification and leave installed roots
unchanged. Keep old signed packages in Git history for diagnosis.

Verify a staged package, and verify its downloaded deployed bytes afterward:

```sh
go run ./tools/truststore -verify -public-key security/trust-update-key.pub \
  /external/package/ca-certificates.pem \
  /external/package/ca-certificates.json \
  /external/package/ca-certificates.sig
```

Verification reads bounded files and uses the same package and certificate
policy as the native loader; it does not read the signing seed or modify files.
The public package URLs append each filename to the base URL, without redirects.
Ordinary builds and offline tests do not download from Pages.
