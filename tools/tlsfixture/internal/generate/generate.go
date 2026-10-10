// Package generate creates the committed, host-only TLS test fixtures.
package generate

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

var (
	validFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	validTo   = time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Write emits deterministic test certificates and keys under directory.
// All emitted private keys are test fixtures only.
func Write(directory string) error {
	rootPublic, rootPrivate := ed25519Key(1)
	intermediatePublic, intermediatePrivate := ed25519Key(2)
	untrustedPublic, untrustedPrivate := ed25519Key(3)
	serverPrivate := serverKey()

	rootTemplate := caTemplate("GoOS TLS Fixture Root TEST ONLY", 1)
	rootDER, err := x509.CreateCertificate(nil, rootTemplate, rootTemplate, rootPublic, rootPrivate)
	if err != nil {
		return err
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return err
	}

	intermediateTemplate := caTemplate("GoOS TLS Fixture Intermediate TEST ONLY", 2)
	intermediateDER, err := x509.CreateCertificate(nil, intermediateTemplate, root, intermediatePublic, rootPrivate)
	if err != nil {
		return err
	}
	intermediate, err := x509.ParseCertificate(intermediateDER)
	if err != nil {
		return err
	}

	untrustedTemplate := caTemplate("GoOS TLS Fixture Untrusted Root TEST ONLY", 3)
	untrustedDER, err := x509.CreateCertificate(nil, untrustedTemplate, untrustedTemplate, untrustedPublic, untrustedPrivate)
	if err != nil {
		return err
	}
	untrusted, err := x509.ParseCertificate(untrustedDER)
	if err != nil {
		return err
	}

	files := map[string][]byte{
		"root-cert.pem":                   certPEM(rootDER),
		"root-key-testonly.pem":           keyPEM(rootPrivate),
		"intermediate-cert.pem":           certPEM(intermediateDER),
		"intermediate-key-testonly.pem":   keyPEM(intermediatePrivate),
		"untrusted-root-cert.pem":         certPEM(untrustedDER),
		"untrusted-root-key-testonly.pem": keyPEM(untrustedPrivate),
		"server-key-testonly.pem":         keyPEM(serverPrivate),
	}
	for _, fixture := range []struct {
		name     string
		parent   *x509.Certificate
		signer   ed25519.PrivateKey
		template *x509.Certificate
	}{
		{"valid-cert.pem", intermediate, intermediatePrivate, leafTemplate("tls.test", 10, validFrom, validTo)},
		{"wronghost-cert.pem", intermediate, intermediatePrivate, leafTemplate("wronghost.test", 11, validFrom, validTo)},
		{"expired-cert.pem", intermediate, intermediatePrivate, leafTemplate("tls.test", 12, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"future-cert.pem", intermediate, intermediatePrivate, leafTemplate("tls.test", 13, time.Date(2045, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"untrusted-cert.pem", untrusted, untrustedPrivate, leafTemplate("tls.test", 14, validFrom, validTo)},
		{"unknowncriticalextension-cert.pem", intermediate, intermediatePrivate, criticalLeafTemplate()},
	} {
		der, err := x509.CreateCertificate(nil, fixture.template, fixture.parent, &serverPrivate.PublicKey, fixture.signer)
		if err != nil {
			return fmt.Errorf("%s: %w", fixture.name, err)
		}
		files[fixture.name] = certPEM(der)
		if fixture.name == "valid-cert.pem" {
			der[len(der)-1] ^= 1
			files["invalidsignature-cert.pem"] = certPEM(der)
		}
	}
	files["malformed-cert.pem"] = []byte("-----BEGIN CERTIFICATE-----\nnot a certificate\n-----END CERTIFICATE-----\n")

	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func ed25519Key(seedByte byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = seedByte
	}
	private := ed25519.NewKeyFromSeed(seed)
	return private.Public().(ed25519.PublicKey), private
}

func serverKey() *ecdsa.PrivateKey {
	d := new(big.Int).SetBytes([]byte{0x2a, 0x1d, 0x71, 0x55, 0x93, 0x07, 0xc4, 0xf2, 0x19, 0x88, 0x3b, 0x6e, 0x29, 0xd1, 0x61, 0xa0, 0x55, 0x5e, 0x80, 0x11, 0x7d, 0x6c, 0xaf, 0x6b, 0xb1, 0xd4, 0x66, 0x8d, 0x23, 0x4f, 0x8a, 0x19})
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(d.Bytes())
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: d}
}

func caTemplate(commonName string, serial int64) *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: commonName}, NotBefore: validFrom, NotAfter: validTo, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature, SignatureAlgorithm: x509.PureEd25519}
}

func leafTemplate(name string, serial int64, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: notBefore, NotAfter: notAfter, DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("192.0.2.53"), net.ParseIP("127.0.0.1"), net.ParseIP("10.0.2.2")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, SignatureAlgorithm: x509.PureEd25519}
}

func criticalLeafTemplate() *x509.Certificate {
	t := leafTemplate("tls.test", 15, validFrom, validTo)
	t.ExtraExtensions = []pkix.Extension{{Id: []int{1, 2, 3, 4, 5}, Critical: true, Value: []byte{0x05, 0x00}}}
	return t
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func keyPEM(key any) []byte {
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
