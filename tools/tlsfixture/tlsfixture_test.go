package tlsfixture

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goos/tools/tlsfixture/internal/generate"
)

func TestGeneratedFixturesAreCommittedAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := generate.Write(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		got, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		want := read(entry.Name())
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from deterministic generator", entry.Name())
		}
	}
}

func TestFixtureVerificationOutcomes(t *testing.T) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(RootPEM()) {
		t.Fatal("root PEM did not parse")
	}
	intermediate := x509.NewCertPool()
	if !intermediate.AppendCertsFromPEM(read("intermediate-cert.pem")) {
		t.Fatal("intermediate PEM did not parse")
	}
	verify := func(mode, name string, at time.Time) error {
		cert, err := Certificate(mode)
		if err != nil {
			return err
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
		_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediate, DNSName: name, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		return err
	}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := verify("valid", "tls.test", at); err != nil {
		t.Fatalf("valid: %v", err)
	}
	for _, name := range []string{"192.0.2.53", "127.0.0.1", "10.0.2.2"} {
		if err := verify("valid", name, at); err != nil {
			t.Fatalf("IP SAN %s: %v", name, err)
		}
	}
	if _, err := Certificate("notyetvalid"); err != nil {
		t.Fatalf("notyetvalid alias: %v", err)
	}
	if _, err := Certificate("untrustedroot"); err != nil {
		t.Fatalf("untrustedroot alias: %v", err)
	}
	for _, check := range []struct {
		mode, name string
		at         time.Time
	}{
		{"wronghost", "tls.test", at},
		{"expired", "tls.test", at},
		{"future", "tls.test", at},
		{"untrusted", "tls.test", at},
		{"invalidsignature", "tls.test", at},
		{"unknowncriticalextension", "tls.test", at},
	} {
		if err := verify(check.mode, check.name, check.at); err == nil {
			t.Errorf("%s verified unexpectedly", check.mode)
		}
	}
	if _, err := Certificate("malformed"); err == nil {
		t.Error("malformed fixture loaded unexpectedly")
	}
}
