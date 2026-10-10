// Package tlsfixture exposes deterministic, host-only TLS test fixtures.
//
// The embedded certificates and private keys are deliberately public test data.
// They must never be installed as production trust anchors.
package tlsfixture

import (
	"crypto/tls"
	"embed"
	"fmt"
)

//go:embed testdata/*.pem
var files embed.FS

// RootPEM returns the test root certificate PEM. Callers must add it only to
// test trust pools; it is not a production trust anchor.
func RootPEM() []byte {
	return read("root-cert.pem")
}

// Certificate returns a server certificate for mode. The valid mode chains to
// RootPEM and is valid for tls.test, 192.0.2.53, 127.0.0.1, and 10.0.2.2.
//
// Supported modes are valid, wronghost, expired, future, untrusted,
// invalidsignature, unknowncriticalextension, and malformed. notyetvalid and
// untrustedroot are aliases for future and untrusted. The malformed
// mode returns the parsing error expected when a TLS server is configured with
// invalid certificate data. plainhttp is intentionally not a TLS certificate
// mode and is handled by tlspeer directly.
func Certificate(mode string) (tls.Certificate, error) {
	certName, issuerName := certificateNames(mode)
	if certName == "" {
		return tls.Certificate{}, fmt.Errorf("unknown TLS fixture mode %q", mode)
	}
	return tls.X509KeyPair(append(read(certName), read(issuerName)...), read("server-key-testonly.pem"))
}

func certificateNames(mode string) (cert, issuer string) {
	switch mode {
	case "valid", "wronghost", "expired", "future", "invalidsignature", "unknowncriticalextension", "malformed":
		return mode + "-cert.pem", "intermediate-cert.pem"
	case "notyetvalid":
		return "future-cert.pem", "intermediate-cert.pem"
	case "untrusted", "untrustedroot":
		return "untrusted-cert.pem", "untrusted-root-cert.pem"
	default:
		return "", ""
	}
}

func read(name string) []byte {
	data, err := files.ReadFile("testdata/" + name)
	if err != nil {
		panic("tlsfixture: missing embedded " + name + ": " + err.Error())
	}
	return data
}
