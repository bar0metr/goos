//go:build cryptoprobe_x509

package cryptoprobe

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"time"
)

const Name = "x509"

func Run() error {
	// Probe-only CA; verification uses explicit roots and a fixed fixture time.
	const fixture = `-----BEGIN CERTIFICATE-----
MIIBbDCCARGgAwIBAgIBATAKBggqhkjOPQQDAjAdMRswGQYDVQQDExJHb09TIHBy
b2JlIGZpeHR1cmUwHhcNNzAwMTAxMDAwMDAwWhcNMzAwMTAxMDAwMDAwWjAdMRsw
GQYDVQQDExJHb09TIHByb2JlIGZpeHR1cmUwWTATBgcqhkjOPQIBBggqhkjOPQMB
BwNCAARrF9Hy4SxCR/i85uVjpEDydwN9gS3rM6D0oTlF2JjClk/jQuL+Gn+bjufr
SnwPnhYrzjNXazFezsu2QGg3v1H1o0IwQDAOBgNVHQ8BAf8EBAMCAoQwDwYDVR0T
AQH/BAUwAwEB/zAdBgNVHQ4EFgQUaYvqY9xEo0RmP/FCmuoQhC3ye2swCgYIKoZI
zj0EAwIDSQAwRgIhAJbnv1P+/C6oZqmHfEeErMYSn3rKx0waKwuKB05/LwOPAiEA
nFhgwi6DSWBdtf4Ur8hAHjDYVnKA4SusqdvjLNvsJDc=
-----END CERTIFICATE-----
`
	block, rest := pem.Decode([]byte(fixture))
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		return errors.New("certificate PEM decode failed")
	}
	var raw asn1.RawValue
	rest, err := asn1.Unmarshal(block.Bytes, &raw)
	if err != nil {
		return err
	}
	if len(rest) != 0 || raw.Tag != asn1.TagSequence || !raw.IsCompound {
		return errors.New("certificate DER is not one sequence")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if cert.Subject.CommonName != "GoOS probe fixture" || !cert.IsCA || cert.PublicKeyAlgorithm != x509.ECDSA {
		return errors.New("certificate fields mismatch")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return err
	}
	signature := append([]byte(nil), cert.Signature...)
	signature[len(signature)-1] ^= 1
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, signature); err == nil {
		return errors.New("X.509 accepted a changed signature")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	opts := x509.VerifyOptions{
		Roots: roots, CurrentTime: time.Unix(1577836800, 0),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}
	if _, err := cert.Verify(opts); err != nil {
		return err
	}
	opts.Roots = x509.NewCertPool()
	if _, err := cert.Verify(opts); err == nil {
		return errors.New("X.509 accepted an unknown root")
	}
	opts.Roots, opts.CurrentTime = roots, cert.NotAfter.Add(time.Second)
	if _, err := cert.Verify(opts); err == nil {
		return errors.New("X.509 accepted an expired certificate")
	}
	return nil
}
