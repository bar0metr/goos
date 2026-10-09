# System certificate authorities

GoOS carries the Mozilla-derived root bundle that curl published on 2026-09-25:

- Source: https://curl.se/ca/cacert.pem
- Dated archive: https://curl.se/ca/cacert-2026-09-25.pem
- Published checksum: https://curl.se/ca/cacert.pem.sha256
- Mozilla source date: 2026-09-25 03:12:01 UTC
- Extractor: curl `mk-ca-bundle.pl` 1.33
- Certificates: 121

The curl maintainers extract these certificates from Mozilla NSS
[`certdata.txt`](https://raw.githubusercontent.com/mozilla-firefox/firefox/refs/heads/release/security/nss/lib/ckfw/builtins/certdata.txt).
Their [CA extract documentation](https://curl.se/docs/caextract.html) identifies
the license as MPL 2.0 and warns that Firefox-specific CA name constraints do
not accompany this PEM export. GoOS uses certificate constraints from the
exported DER and its own TLS policy; it does not reproduce Firefox's trust policy.

## Pinned files

| File | Bytes | SHA-256 |
| --- | ---: | --- |
| `ca-source.pem` | 188900 | `a41b5d356aea97a529fe27e0f7316d2f9d946d75927476cf9cf1b90637d00505` |
| `ca-certificates.pem` | 181603 | `b5e44e6cf3ec2cda6131fec4e60a358ed022af5d5a8584da589b1851a56d0bb5` |

`ca-source.pem` preserves curl's original headers and certificate titles.
`ca-source.sha256` preserves the published checksum line, including its upstream
filename `cacert.pem`. The SHA-256 inside the source header describes curl's
extraction input; use the checksum above to verify the complete PEM file.
`ca-certificates.pem.json` records generation metadata.

We downloaded the PEM and published checksum over authenticated HTTPS into
separate empty temporary directories, then compared the published checksum with
the complete PEM digest. The repository carries both inputs. An ordinary build
uses the committed artifact and performs no download.

## Offline generation

From the repository root:

```sh
go run ./tools/truststore security/ca-source.pem security/ca-certificates.pem
```

The generator checks the pinned source digest, decodes all certificate blocks,
and writes certificate-only canonical PEM in source order. It rejects malformed
markers or undecodable blocks, more than 256 roots, DER larger than 32 KiB,
invalid CA basic constraints, and an explicit key usage that excludes certificate
signing. Source and canonical bundle sizes must fit 512 KiB.

The system-image builder requires `SYSTEM_CA_BUNDLE` and installs that file at
`system/security/ca-certificates.pem`. Normal builds select the committed bundle.
Validation builds can select a test CA through this explicit build input; do not
add test roots to the production artifact. The installed system namespace supplies
the read-only policy; FAT file modes alone do not enforce it.

## License

Mozilla publishes the source certificate store under MPL 2.0. See
[`LICENSE-MPL-2.0`](LICENSE-MPL-2.0) for the license text. We preserve the original
bundle and its public provenance alongside the normalized copy. You can obtain
the upstream source through the dated curl archive and the Mozilla NSS link above.
