package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goos/tools/tlsfixture"
	"goos/userspace/tlsclient"
	"goos/userspace/truststore"
)

func read(t *testing.T, directory, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSignedPackagesAndRollback(t *testing.T) {
	dir := t.TempDir()
	if err := generate(dir); err != nil {
		t.Fatal(err)
	}
	key := read(t, dir, "public.key")
	seed := read(t, dir, "signing-seed-testonly.raw")
	if len(seed) != 32 || !bytes.Equal(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), key) {
		t.Fatal("test signing key mismatch")
	}
	packages := map[string]*truststore.Package{}
	for _, mode := range []string{"update", "rollback", "badsig"} {
		pkg, err := truststore.Verify(read(t, dir, mode+"/ca-certificates.pem"), read(t, dir, mode+"/ca-certificates.json"), read(t, dir, mode+"/ca-certificates.sig"), key, func(data []byte) (int, error) {
			roots, err := tlsclient.LoadRoots(data)
			if err != nil {
				return 0, err
			}
			return roots.Count(), nil
		})
		if mode == "badsig" {
			if err == nil || pkg != nil {
				t.Fatal("accepted bad signature")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		packages[mode] = pkg
	}
	if packages["update"].Metadata.Sequence != 2026101501 || packages["update"].Metadata.Certificates != 2 || packages["rollback"].Metadata.Sequence != 2026100901 || packages["rollback"].Metadata.Certificates != 1 {
		t.Fatal("fixture sequence/count mismatch")
	}
	if truststore.CheckUpgrade(packages["update"], packages["rollback"]) == nil {
		t.Fatal("accepted rollback")
	}
	if err := truststore.CheckUpgrade(packages["rollback"], packages["update"]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(packages["rollback"].Bundle, tlsfixture.RootPEM()) {
		t.Fatal("rollback root differs")
	}
	before := read(t, dir, "public.key")
	if err := generate(dir); err == nil {
		t.Fatal("accepted nonempty output")
	}
	if !bytes.Equal(before, read(t, dir, "public.key")) {
		t.Fatal("overwrote existing fixture")
	}
}

func TestNewLeafRequiresUpdatedTrust(t *testing.T) {
	dir := t.TempDir()
	if err := generate(dir); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(read(t, dir, "newroot-cert.pem"), read(t, dir, "newroot-key-testonly.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	oldRoots := x509.NewCertPool()
	oldRoots.AppendCertsFromPEM(tlsfixture.RootPEM())
	newRoots := x509.NewCertPool()
	newRoots.AppendCertsFromPEM(read(t, dir, "update/ca-certificates.pem"))
	for _, host := range []string{"tls.test", "10.0.2.2"} {
		for _, check := range []struct {
			name  string
			pool  *x509.CertPool
			valid bool
		}{{"old", oldRoots, false}, {"new", newRoots, true}} {
			_, err := leaf.Verify(x509.VerifyOptions{Roots: check.pool, DNSName: host, CurrentTime: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)})
			if (err == nil) != check.valid {
				t.Fatalf("host=%s roots=%s verify=%v", host, check.name, err)
			}
		}
	}
	for _, name := range []string{"newroot-key-testonly.pem", "signing-seed-testonly.raw"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private test key permissions: %v, %v", info, err)
		}
	}
	for _, mode := range []string{"update", "rollback", "badsig"} {
		files, err := os.ReadDir(filepath.Join(dir, mode))
		if err != nil || len(files) != 3 {
			t.Fatal("guest package contains extra host data")
		}
		for _, file := range files {
			if file.Name() != "ca-certificates.pem" && file.Name() != "ca-certificates.json" && file.Name() != "ca-certificates.sig" {
				t.Fatal("host key in guest package")
			}
		}
	}
}
