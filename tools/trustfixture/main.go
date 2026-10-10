// trustfixture creates host-only signed trust-update fixtures. Its public test
// signing seed is not secret and must never authorize production updates.
package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"goos/tools/tlsfixture"
	"goos/userspace/truststore"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: trustfixture OUTPUT_EMPTY_DIR")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "trustfixture:", err)
		os.Exit(1)
	}
}

func testSigningKey() ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("GoOS trust update PUBLIC TEST ONLY signing seed v1"))
	return ed25519.NewKeyFromSeed(seed[:])
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func newCertificate() ([]byte, []byte, []byte, error) {
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	start := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	root := &x509.Certificate{SerialNumber: big.NewInt(701), Subject: pkix.Name{CommonName: "GoOS update PUBLIC TEST ONLY CA"}, NotBefore: start, NotAfter: end, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, nil, err
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(702), Subject: pkix.Name{CommonName: "tls.test"}, DNSNames: []string{"tls.test"}, IPAddresses: []net.IP{net.ParseIP("10.0.2.2")}, NotBefore: start, NotAfter: end, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, rootCert, &leafKey.PublicKey, rootKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return nil, nil, nil, err
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), rootPEM...)
	return rootPEM, chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func signedFiles(bundle []byte, count int, sequence uint64) ([]byte, []byte, error) {
	digest := sha256.Sum256(bundle)
	meta, err := truststore.CanonicalMetadata(truststore.Metadata{Format: 1, Sequence: sequence, Source: "mozilla", SourceDate: "2026-10-15T00:00:00Z", SourceSHA256: hex.EncodeToString(digest[:]), BundleSHA256: hex.EncodeToString(digest[:]), Certificates: count, BundleBytes: len(bundle)})
	if err != nil {
		return nil, nil, err
	}
	message, err := truststore.SigningMessage(meta, bundle)
	if err != nil {
		return nil, nil, err
	}
	return meta, ed25519.Sign(testSigningKey(), message), nil
}

func generate(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output must be a real empty directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("output is not empty")
	}
	root, chain, key, err := newCertificate()
	if err != nil {
		return err
	}
	old := tlsfixture.RootPEM()
	updated := append(append([]byte(nil), old...), root...)
	for _, mode := range []string{"update", "rollback", "badsig"} {
		bundle, count, sequence := updated, 2, uint64(2026101501)
		if mode == "rollback" {
			bundle, count, sequence = old, 1, 2026100901
		}
		meta, sig, err := signedFiles(bundle, count, sequence)
		if err != nil {
			return err
		}
		if mode == "badsig" {
			sig[0] ^= 1
		}
		subdir := filepath.Join(directory, mode)
		if err := os.Mkdir(subdir, 0755); err != nil {
			return err
		}
		for name, data := range map[string][]byte{"ca-certificates.pem": bundle, "ca-certificates.json": meta, "ca-certificates.sig": sig} {
			if err := writeNew(filepath.Join(subdir, name), data, 0644); err != nil {
				return err
			}
		}
	}
	private := testSigningKey()
	for _, file := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"public.key", private.Public().(ed25519.PublicKey), 0644},
		{"signing-seed-testonly.raw", private.Seed(), 0600},
		{"newroot-cert.pem", chain, 0644},
		{"newroot-key-testonly.pem", key, 0600},
	} {
		if err := writeNew(filepath.Join(directory, file.name), file.data, file.mode); err != nil {
			return err
		}
	}
	return nil
}
