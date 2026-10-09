// Package tlsclient authenticates TLS streams against explicit system roots.
package tlsclient

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

const (
	RootPath        = "/system/security/ca-certificates.pem"
	MaxBundle       = 512 << 10
	MaxRoots        = 256
	MaxCertificates = 8
	MaxDER          = 32 << 10
	MaxDNSNames     = 64
)

// Roots owns the parsed, immutable system trust pool.
type Roots struct {
	pool     *x509.CertPool
	count    int
	derBytes int
}

func (r *Roots) Count() int    { return r.count }
func (r *Roots) DERBytes() int { return r.derBytes }

// LoadRoots accepts certificate blocks and comment/blank lines only.
func LoadRoots(data []byte) (*Roots, error) {
	if len(data) == 0 || len(data) > MaxBundle {
		return nil, errors.New("missing or oversized trust store")
	}
	roots := &Roots{pool: x509.NewCertPool()}
	for len(data) != 0 {
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			break
		}
		if data[0] == '#' {
			end := bytes.IndexByte(data, '\n')
			if end < 0 {
				break
			}
			data = data[end+1:]
			continue
		}
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----\n")) || roots.count == MaxRoots {
			return nil, errors.New("malformed trust store")
		}
		marker := []byte("\n-----END CERTIFICATE-----")
		end := bytes.Index(data, marker)
		if end < len("-----BEGIN CERTIFICATE-----\n") || bytes.Contains(data[len("-----BEGIN CERTIFICATE-----\n"):end], []byte("-----BEGIN")) {
			return nil, errors.New("malformed trust store")
		}
		end += len(marker)
		block, remainder := pem.Decode(data[:end])
		rest := data[end:]
		if len(bytes.TrimSpace(remainder)) != 0 {
			return nil, errors.New("malformed trust store")
		}
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(block.Bytes) == 0 || len(block.Bytes) > MaxDER {
			return nil, errors.New("malformed trust store")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.BasicConstraintsValid || !cert.IsCA || len(cert.DNSNames) > MaxDNSNames {
			return nil, errors.New("malformed trust store certificate")
		}
		if cert.KeyUsage != 0 && cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, errors.New("trust store certificate cannot sign")
		}
		roots.pool.AddCert(cert)
		roots.count++
		roots.derBytes += len(block.Bytes)
		data = rest
	}
	if roots.count == 0 {
		return nil, errors.New("empty trust store")
	}
	return roots, nil
}
