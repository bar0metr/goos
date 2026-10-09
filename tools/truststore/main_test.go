package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func certificate(t *testing.T, ca bool, usage x509.KeyUsage) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "validation CA only"}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1893456000, 0), BasicConstraintsValid: true, IsCA: ca, KeyUsage: usage}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
func TestNormalizationDeterministic(t *testing.T) {
	root := certificate(t, true, x509.KeyUsageCertSign)
	source := append([]byte("# fixture metadata\nCertificate title\n=================\n"), root...)
	first, n, err := normalize(source, maxRoots)
	if err != nil || n != 1 || !bytes.Equal(first, root) {
		t.Fatalf("normalize: count=%d err=%v", n, err)
	}
	second, _, err := normalize(source, maxRoots)
	if err != nil || !bytes.Equal(second, first) {
		t.Fatal("nondeterministic output")
	}
}
func TestRejectMalformedAndUntrustedCA(t *testing.T) {
	root := certificate(t, true, x509.KeyUsageCertSign)
	badPEM := []byte("-----BEGIN CERTIFICATE-----\n!\n-----END CERTIFICATE-----\n")
	cases := [][]byte{
		nil, badPEM, append(append([]byte(nil), root...), badPEM...),
		append(append([]byte(nil), root...), []byte("-----BEGIN BROKEN-----\n")...),
		certificate(t, false, x509.KeyUsageDigitalSignature), certificate(t, true, x509.KeyUsageDigitalSignature),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0}}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: make([]byte, maxDER+1)}),
	}
	for i, data := range cases {
		if _, _, err := normalize(data, maxRoots); err == nil {
			t.Fatalf("bad source %d accepted", i)
		}
	}
	if _, _, err := normalize(bytes.Repeat(root, maxRoots+1), maxRoots); err == nil {
		t.Fatal("too many roots accepted")
	}
}
func TestPinnedArtifactReproducible(t *testing.T) {
	source, err := os.ReadFile("../../security/ca-source.pem")
	if err != nil {
		t.Fatal(err)
	}
	if hash(source) != sourceSHA256 {
		t.Fatal("source pin mismatch")
	}
	canonical, count, err := normalize(source, maxRoots)
	if err != nil || count != 121 {
		t.Fatalf("pinned roots=%d err=%v", count, err)
	}
	artifact, err := os.ReadFile("../../security/ca-certificates.pem")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, artifact) {
		t.Fatal("committed artifact differs from normalized source")
	}
	output := filepath.Join(t.TempDir(), "roots.pem")
	if err := generate("../../security/ca-source.pem", output, sourceSHA256); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(generated, artifact) {
		t.Fatal("generated artifact mismatch")
	}
	recordBytes, err := os.ReadFile(output + ".json")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile("../../security/ca-certificates.pem.json")
	if err != nil || !bytes.Equal(recordBytes, committed) {
		t.Fatal("metadata is not reproducible")
	}
	var record metadata
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		t.Fatal(err)
	}
	if record.SourceSHA256 != sourceSHA256 || record.BundleSHA256 != hash(artifact) || record.Certificates != count || record.BundleBytes != len(artifact) {
		t.Fatal("incorrect metadata")
	}
}
func TestSourcePinAndOverwriteGuards(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.pem")
	output := filepath.Join(root, "output.pem")
	data := certificate(t, true, x509.KeyUsageCertSign)
	if err := os.WriteFile(source, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := generate(source, output, sourceSHA256); err == nil {
		t.Fatal("unpinned source accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed source validation wrote output")
	}
	if err := generate(source, source, hash(data)); err == nil {
		t.Fatal("source overwrite accepted")
	}
	alias := filepath.Join(root, "alias.pem")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}
	if err := generate(source, alias, hash(data)); err == nil {
		t.Fatal("source alias overwrite accepted")
	}
}

func TestCheckPinnedArtifactsReadOnly(t *testing.T) {
	output := filepath.Join(t.TempDir(), "roots.pem")
	source := "../../security/ca-source.pem"
	if err := generate(source, output, sourceSHA256); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	infoBefore := map[string]os.FileInfo{}
	for _, path := range []string{output, output + ".json"} {
		if err := os.Chmod(path, 0444); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		infoBefore[path] = info
	}
	if err := check(source, output, sourceSHA256); err != nil {
		t.Fatal(err)
	}
	if err := check(source, output, "bad pin"); err == nil {
		t.Fatal("invalid source pin accepted by check")
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("read-only check changed artifact")
		}
		after, err := os.Stat(path)
		if err != nil || after.Mode() != infoBefore[path].Mode() || !after.ModTime().Equal(infoBefore[path].ModTime()) {
			t.Fatal("read-only check changed metadata or mode")
		}
	}
}
func TestCheckRejectsTamperedArtifactsWithoutRepair(t *testing.T) {
	for _, tamper := range []string{"valid different CA", "metadata hash", "malformed metadata", "missing metadata"} {
		t.Run(tamper, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "roots.pem")
			source := "../../security/ca-source.pem"
			if err := generate(source, output, sourceSHA256); err != nil {
				t.Fatal(err)
			}
			switch tamper {
			case "valid different CA":
				different := certificate(t, true, x509.KeyUsageCertSign)
				if err := os.WriteFile(output, different, 0644); err != nil {
					t.Fatal(err)
				}
			case "metadata hash":
				data, err := os.ReadFile(output + ".json")
				if err != nil {
					t.Fatal(err)
				}
				var record metadata
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatal(err)
				}
				record.BundleSHA256 = hash([]byte("different"))
				changed, err := json.MarshalIndent(record, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(output+".json", append(changed, '\n'), 0644); err != nil {
					t.Fatal(err)
				}
			case "malformed metadata":
				if err := os.WriteFile(output+".json", []byte("{"), 0644); err != nil {
					t.Fatal(err)
				}
			case "missing metadata":
				if err := os.Remove(output + ".json"); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(output)
			metaBefore, metaErr := os.ReadFile(output + ".json")
			if err := check(source, output, sourceSHA256); err == nil {
				t.Fatal("tampered artifact accepted")
			}
			after, _ := os.ReadFile(output)
			metaAfter, afterErr := os.ReadFile(output + ".json")
			if !bytes.Equal(before, after) || !bytes.Equal(metaBefore, metaAfter) || os.IsNotExist(metaErr) != os.IsNotExist(afterErr) {
				t.Fatal("failed check repaired or created artifact")
			}
		})
	}
}
