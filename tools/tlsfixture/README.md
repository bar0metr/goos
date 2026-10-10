# Host-only TLS fixtures

This package supplies deterministic TLS test data for host tools and host tests.
Every private key in `testdata/` is public **TEST ONLY** material. Do not import
or trust these fixtures in a production image.

Regenerate the committed files with:

```sh
go run ./tools/tlsfixture/cmd/generate
```

The generator uses fixed Ed25519 seeds for certificate authorities and a fixed
P-256 server key. Certificate signatures are Ed25519 and the certificate times,
serials, subjects, and SANs are fixed, so generation has byte-for-byte stable
output. `go test ./tools/tlsfixture/...` checks this against a temporary output
directory and verifies the intended chain, hostname, time, critical-extension,
and signature outcomes.
