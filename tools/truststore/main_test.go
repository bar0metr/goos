package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goos/userspace/tlsclient"
	"goos/userspace/truststore"
)

func certificate(t *testing.T, ca bool, usage x509.KeyUsage) []byte {
	t.Helper()
	return namedCertificate(t, ca, usage, "validation CA only")
}

func namedCertificate(t *testing.T, ca bool, usage x509.KeyUsage, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Unix(0, 0), NotAfter: time.Unix(1893456000, 0), BasicConstraintsValid: true, IsCA: ca, KeyUsage: usage}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testOptions() options {
	return options{Sequence: 1, SourceDate: "2026-10-09T00:00:00Z", SourceURL: "https://curl.se/ca/cacert.pem"}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	source, output := filepath.Join(root, "source.pem"), filepath.Join(root, "output.pem")
	if err := os.WriteFile(source, certificate(t, true, x509.KeyUsageCertSign), 0644); err != nil {
		t.Fatal(err)
	}
	return source, output
}

func TestNormalizationDeterministic(t *testing.T) {
	root := certificate(t, true, x509.KeyUsageCertSign)
	secondRoot := certificate(t, true, x509.KeyUsageCertSign)
	source := append([]byte("# fixture metadata\nCertificate title\n=================\n"), root...)
	source = append(source, secondRoot...)
	first, n, err := normalize(source, maxRoots)
	if err != nil || n != 2 || !bytes.Equal(first, append(bytes.Clone(root), secondRoot...)) {
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
		nil, badPEM, append(bytes.Clone(root), badPEM...),
		append(bytes.Clone(root), []byte("-----BEGIN BROKEN-----\n")...),
		certificate(t, false, x509.KeyUsageDigitalSignature), certificate(t, true, x509.KeyUsageDigitalSignature),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0}}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: make([]byte, maxDER+1)}),
		make([]byte, maxBundle+1),
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

func TestCommittedArtifactReproducible(t *testing.T) {
	record, err := os.ReadFile("../../security/ca-certificates.json")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := truststore.ParseMetadata(record)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("../../security/ca-source.pem")
	if err != nil {
		t.Fatal(err)
	}
	canonical, count, err := normalize(source, maxRoots)
	if err != nil || count != meta.Certificates || hash(source) != meta.SourceSHA256 || hash(canonical) != meta.BundleSHA256 || len(canonical) != meta.BundleBytes {
		t.Fatalf("recorded identity mismatch: count=%d err=%v", count, err)
	}
	artifact, err := os.ReadFile("../../security/ca-certificates.pem")
	if err != nil || !bytes.Equal(canonical, artifact) {
		t.Fatal("committed artifact differs from source")
	}
	output := filepath.Join(t.TempDir(), "roots.pem")
	if err := generate("../../security/ca-source.pem", output, options{Sequence: meta.Sequence, SourceDate: meta.SourceDate, SourceURL: meta.SourceURL}, io.Discard); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(metadataPath(output))
	if err != nil || !bytes.Equal(generated, record) {
		t.Fatal("metadata not reproducible")
	}
	if err := check("../../security/ca-source.pem", output); err != nil {
		t.Fatal(err)
	}
}

func TestGenerationAndOverwriteGuards(t *testing.T) {
	source, output := fixture(t)
	if err := generate(source, output, options{}, io.Discard); err == nil {
		t.Fatal("missing sequence/date accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid metadata wrote output")
	}
	if err := generate(source, source, testOptions(), io.Discard); err == nil {
		t.Fatal("source overwrite accepted")
	}
	for _, kind := range []string{"symlink", "hardlink", "metadata alias", "signature alias", "seed alias"} {
		t.Run(kind, func(t *testing.T) {
			source, output := fixture(t)
			opts := testOptions()
			switch kind {
			case "symlink":
				if err := os.Symlink(source, output); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(source, output); err != nil {
					t.Fatal(err)
				}
			case "metadata alias":
				if err := os.Link(source, metadataPath(output)); err != nil {
					t.Fatal(err)
				}
			case "signature alias":
				opts.SignKey = filepath.Join(filepath.Dir(output), "seed")
				if err := os.WriteFile(opts.SignKey, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(source, signaturePath(output)); err != nil {
					t.Fatal(err)
				}
			case "seed alias":
				opts.SignKey = output
				if err := os.WriteFile(output, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(source)
			if err := generate(source, output, opts, io.Discard); err == nil {
				t.Fatal("input alias overwrite accepted")
			}
			after, _ := os.ReadFile(source)
			if !bytes.Equal(before, after) {
				t.Fatal("source modified")
			}
		})
	}
}

func TestCheckArtifactsReadOnly(t *testing.T) {
	source, output := fixture(t)
	if err := generate(source, output, testOptions(), io.Discard); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	infoBefore := map[string]os.FileInfo{}
	for _, path := range []string{output, metadataPath(output)} {
		if err := os.Chmod(path, 0444); err != nil {
			t.Fatal(err)
		}
		var err error
		before[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		infoBefore[path], err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := check(source, output); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, certificate(t, true, x509.KeyUsageCertSign), 0644); err != nil {
		t.Fatal(err)
	}
	if err := check(source, output); err == nil {
		t.Fatal("changed source accepted")
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("check changed artifact")
		}
		after, err := os.Stat(path)
		if err != nil || after.Mode() != infoBefore[path].Mode() || !after.ModTime().Equal(infoBefore[path].ModTime()) {
			t.Fatal("check changed mode/mtime")
		}
	}
}

func TestCheckRejectsTamperedArtifactsWithoutRepair(t *testing.T) {
	for _, tamper := range []string{"different CA", "metadata hash", "malformed metadata", "missing metadata"} {
		t.Run(tamper, func(t *testing.T) {
			source, output := fixture(t)
			if err := generate(source, output, testOptions(), io.Discard); err != nil {
				t.Fatal(err)
			}
			switch tamper {
			case "different CA":
				if err := os.WriteFile(output, certificate(t, true, x509.KeyUsageCertSign), 0644); err != nil {
					t.Fatal(err)
				}
			case "metadata hash":
				data, _ := os.ReadFile(metadataPath(output))
				meta, err := truststore.ParseMetadata(data)
				if err != nil {
					t.Fatal(err)
				}
				meta.BundleSHA256 = hash([]byte("different"))
				changed, err := truststore.CanonicalMetadata(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(metadataPath(output), changed, 0644); err != nil {
					t.Fatal(err)
				}
			case "malformed metadata":
				if err := os.WriteFile(metadataPath(output), []byte("{"), 0644); err != nil {
					t.Fatal(err)
				}
			case "missing metadata":
				if err := os.Remove(metadataPath(output)); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(output)
			metaBefore, metaErr := os.ReadFile(metadataPath(output))
			if err := check(source, output); err == nil {
				t.Fatal("tampered artifact accepted")
			}
			after, _ := os.ReadFile(output)
			metaAfter, afterErr := os.ReadFile(metadataPath(output))
			if !bytes.Equal(before, after) || !bytes.Equal(metaBefore, metaAfter) || os.IsNotExist(metaErr) != os.IsNotExist(afterErr) {
				t.Fatal("failed check repaired artifact")
			}
		})
	}
}

func TestSigningAndCertificateValidator(t *testing.T) {
	source, output := fixture(t)
	opts := testOptions()
	opts.SignKey = filepath.Join(filepath.Dir(output), "test.seed")
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	if err := os.WriteFile(opts.SignKey, seed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := generate(source, output, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	bundle, _ := os.ReadFile(output)
	meta, _ := os.ReadFile(metadataPath(output))
	sig, _ := os.ReadFile(signaturePath(output))
	key := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	pkg, err := truststore.Verify(bundle, meta, sig, key, func(data []byte) (int, error) {
		roots, err := tlsclient.LoadRoots(data)
		if err != nil {
			return 0, err
		}
		return roots.Count(), nil
	})
	if err != nil || pkg == nil {
		t.Fatalf("signed bundle rejected: %v", err)
	}
	unsigned := testOptions()
	if err := generate(source, output, unsigned, io.Discard); err == nil {
		t.Fatal("unsigned generation left stale signature")
	}
}

func TestRootDiff(t *testing.T) {
	before := certificate(t, true, x509.KeyUsageCertSign)
	after := certificate(t, true, x509.KeyUsageCertSign)
	diff, err := rootDiff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock, _ := pem.Decode(before)
	newBlock, _ := pem.Decode(after)
	if !strings.Contains(string(diff), "replaced-old "+hash(oldBlock.Bytes)) || !strings.Contains(string(diff), "replaced-new "+hash(newBlock.Bytes)) || !strings.Contains(string(diff), "validation CA only") {
		t.Fatalf("diff = %q", diff)
	}
	same, err := rootDiff(before, before)
	if err != nil || len(same) != 0 {
		t.Fatalf("same diff = %q, %v", same, err)
	}
	// A changed subject is an addition/removal, not a replacement.
	identities, _ := rootIdentities(after)
	for _, subject := range identities {
		if subject == "" {
			t.Fatal("missing subject")
		}
	}
}

func TestCLIFlags(t *testing.T) {
	source, output := fixture(t)
	for _, args := range [][]string{
		{source, output}, {"-sequence", "0", "-source-date", "2026-10-09T00:00:00Z", source, output},
		{"-sequence", "-1", source, output}, {"-check", "-sequence", "1", source, output},
		{"-keygen", t.TempDir(), "-sequence", "1"},
	} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
	if err := run([]string{"-sequence", "1", "-source-date", "2026-10-09T00:00:00Z", source, output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-check", source, output}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestKeygenExternalDirectory(t *testing.T) {
	// Only disposable host-test keys, never a production key.
	dir := t.TempDir()
	if err := keygen(dir); err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		name string
		size int
		mode os.FileMode
	}{{"private.seed", 32, 0600}, {"public.key", 32, 0644}} {
		path := filepath.Join(dir, file.name)
		info, err := os.Stat(path)
		if err != nil || info.Size() != int64(file.size) || info.Mode().Perm() != file.mode {
			t.Fatalf("key %s mode/size: %v, %v", file.name, info, err)
		}
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("key directory not private")
	}
	seed, _ := os.ReadFile(filepath.Join(dir, "private.seed"))
	public, _ := os.ReadFile(filepath.Join(dir, "public.key"))
	if !bytes.Equal(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), public) {
		t.Fatal("key pair mismatched")
	}
	if err := keygen(dir); err == nil {
		t.Fatal("existing keys overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "private.seed"))
	if !bytes.Equal(after, seed) {
		t.Fatal("failed keygen changed key")
	}
	if err := keygen("."); err == nil {
		t.Fatal("keygen accepted checkout directory")
	}
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := keygen(nested); err == nil {
		t.Fatal("keygen accepted another repository")
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(t.TempDir(), alias); err != nil {
		t.Fatal(err)
	}
	if err := keygen(alias); err == nil {
		t.Fatal("keygen accepted symlink")
	}
}

func TestRootAdditionsRemovalsAndGenerationReport(t *testing.T) {
	old := namedCertificate(t, true, x509.KeyUsageCertSign, "old CA")
	next := namedCertificate(t, true, x509.KeyUsageCertSign, "new CA")
	diff, err := rootDiff(old, next)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock, _ := pem.Decode(old)
	newBlock, _ := pem.Decode(next)
	if !strings.Contains(string(diff), "removed "+hash(oldBlock.Bytes)) || !strings.Contains(string(diff), "added "+hash(newBlock.Bytes)) || strings.Contains(string(diff), "replaced") {
		t.Fatalf("diff = %q", diff)
	}
	source, output := fixture(t)
	oldPath := filepath.Join(filepath.Dir(output), "old.pem")
	if err := os.WriteFile(source, next, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, old, 0644); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.Old = oldPath
	var report bytes.Buffer
	if err := generate(source, output, opts, &report); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(report.Bytes(), diff) {
		t.Fatalf("generation report = %q want %q", report.Bytes(), diff)
	}
}

func TestMissingRequiredFields(t *testing.T) {
	source, output := fixture(t)
	if err := generate(source, output, testOptions(), io.Discard); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(metadataPath(output))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"format", "sequence", "source", "source_date", "source_sha256", "bundle_sha256", "certificates", "bundle_bytes"} {
		t.Run(field, func(t *testing.T) {
			start := bytes.Index(data, []byte("\""+field+"\":"))
			if start < 0 {
				t.Fatal("field absent")
			}
			end := start
			for end < len(data) && data[end] != ',' && data[end] != '}' {
				end++
			}
			if data[end] == ',' {
				end++
			} else {
				start--
			}
			malformed := append(bytes.Clone(data[:start]), data[end:]...)
			if _, err := truststore.ParseMetadata(malformed); err == nil {
				t.Fatal("missing required field accepted")
			}
		})
	}
}

func TestVerifyPublishedPackageReadOnly(t *testing.T) {
	source, output := fixture(t)
	opts := testOptions()
	opts.SignKey = filepath.Join(filepath.Dir(output), "test.seed")
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	if err := os.WriteFile(opts.SignKey, seed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := generate(source, output, opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(filepath.Dir(output), "public.key")
	public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if err := os.WriteFile(publicPath, public, 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"-verify", "-public-key", publicPath, output, metadataPath(output), signaturePath(output)}
	before, _ := os.ReadFile(signaturePath(output))
	var report bytes.Buffer
	if err := run(args, &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "sequence=1 roots=1") {
		t.Fatalf("report: %q", report.String())
	}
	after, _ := os.ReadFile(signaturePath(output))
	if !bytes.Equal(before, after) {
		t.Fatal("verification changed signature")
	}
	for _, bad := range [][]string{
		{"-verify", output, metadataPath(output), signaturePath(output)},
		{"-verify", "-check", "-public-key", publicPath, output, metadataPath(output), signaturePath(output)},
		{"-verify", "-sequence", "2", "-public-key", publicPath, output, metadataPath(output), signaturePath(output)},
		{"-public-key", publicPath, source, output},
	} {
		if run(bad, io.Discard) == nil {
			t.Fatalf("accepted flags: %v", bad)
		}
	}
	before[0] ^= 1
	if err := os.WriteFile(signaturePath(output), before, 0644); err != nil {
		t.Fatal(err)
	}
	if run(args, io.Discard) == nil {
		t.Fatal("altered signature accepted")
	}
	if err := os.WriteFile(signaturePath(output), after, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicPath, bytes.Repeat([]byte{1}, 32), 0644); err != nil {
		t.Fatal(err)
	}
	if run(args, io.Discard) == nil {
		t.Fatal("wrong public key accepted")
	}
}
