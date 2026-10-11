package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/internal/testscratch"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
func text(t *testing.T, path, value string) { write(t, path, []byte(value)) }

type fixture struct {
	source, tinygo, output, rand, cache string
	expected                            pins
}

func setup(t *testing.T) fixture {
	t.Helper()
	root := testscratch.TempDir(t, "goos-tlscompat-")
	f := fixture{source: filepath.Join(root, "go"), tinygo: filepath.Join(root, "tinygo"), output: filepath.Join(root, "output"), rand: filepath.Join(root, "random.go"), cache: filepath.Join(root, "cache.go")}
	conn := []byte("// Copyright fixture\n\npackage tls\n" + cancellationBlock)
	cache := []byte("// Copyright fixture\n\npackage tls\n")
	random := []byte("// Copyright fixture\n\npackage sysrand\n")
	text(t, filepath.Join(f.source, "VERSION"), goVersion+"\ntime fixture\n")
	text(t, filepath.Join(f.source, "LICENSE"), "Go BSD license fixture\n")
	write(t, filepath.Join(f.source, "src/crypto/tls/conn.go"), conn)
	write(t, filepath.Join(f.source, "src/crypto/tls/cache.go"), cache)
	messages := []byte("package tls\n" + tls13CertificateAppend + "\n" + tls12CertificateScan + "\n")
	verify := []byte("package x509\n" + buildChainsEntry + "}\n")
	write(t, filepath.Join(f.source, "src/crypto/tls/handshake_messages.go"), messages)
	write(t, filepath.Join(f.source, "src/crypto/x509/verify.go"), verify)
	text(t, filepath.Join(f.source, "src/crypto/tls/defaults.go"), "package tls\n"+noAESCipherSuites+"\n")
	text(t, filepath.Join(f.source, "src/crypto/tls/common.go"), "package tls\n")
	text(t, filepath.Join(f.source, "src/crypto/tls/ignored_test.go"), "package tls\n")
	text(t, filepath.Join(f.source, "src/crypto/tls/generate.go"), "//go:build ignore\n\npackage main\n")
	text(t, filepath.Join(f.source, "src/crypto/tls/internal/fips140tls/fipstls.go"), "package fips140tls\n")
	write(t, filepath.Join(f.source, "src/crypto/internal/sysrand/rand.go"), random)
	text(t, filepath.Join(f.source, "src/internal/fixture.go"), "package internal\n")
	text(t, f.rand, "package sysrand\nfunc Read(b []byte) {}\n")
	text(t, f.cache, "package tls\ntype weakCertCache struct{}\nvar globalCertCache weakCertCache\n")
	native := []byte("native runtime fixture")
	bare := []byte("baremetal fixture")
	write(t, filepath.Join(f.tinygo, "src/runtime/runtime_uefi.go"), native)
	write(t, filepath.Join(f.tinygo, "src/runtime/baremetal.go"), bare)
	record := runtimeManifest{TinyGoVersion: "0.42.0", SourceUEFI: uefiPin, SourceBaremetal: baremetalPin, Backend: hash(native), PatchedBaremetal: hash(bare)}
	data, _ := json.Marshal(record)
	write(t, filepath.Join(f.tinygo, "cryptocompat.json"), data)
	installed := filepath.Join(root, "installed-tinygo")
	if err := os.MkdirAll(filepath.Join(f.tinygo, "src/crypto/tls"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cipher_suites.go", "common.go", "ticket.go", "tls.go"} {
		original := filepath.Join(installed, "src/crypto/tls", name)
		text(t, original, "original shim "+name)
		if err := os.Symlink(original, filepath.Join(f.tinygo, "src/crypto/tls", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(installed, "bin"), filepath.Join(f.tinygo, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.output, 0755); err != nil {
		t.Fatal(err)
	}
	f.expected = pins{Conn: hash(conn), Cache: hash(cache), Rand: hash(random), Messages: hash(messages), Verify: hash(verify), Defaults: hash([]byte("package tls\n" + noAESCipherSuites + "\n"))}
	return f
}
func TestPrepare(t *testing.T) {
	f := setup(t)
	original, _ := os.ReadFile(filepath.Join(f.source, "src/crypto/internal/sysrand/rand.go"))
	if err := prepare(f.source, f.tinygo, f.output, f.rand, f.cache, f.expected); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(f.tinygo, "src/crypto/tls/conn.go"))
	if bytes.Contains(got, []byte("context.WithCancel")) || !bytes.Contains(got, []byte(cancellationReplacement)) {
		t.Fatal("wrong cancellation patch")
	}
	defaults, _ := os.ReadFile(filepath.Join(f.tinygo, "src/crypto/tls/defaults.go"))
	if !bytes.Contains(defaults, []byte(aesFirstCipherSuites)) {
		t.Fatal("wrong TLS 1.3 no-AES cipher preference")
	}
	for _, path := range []string{"src/internal", "src/crypto/tls", "src/crypto/internal/sysrand"} {
		info, err := os.Lstat(filepath.Join(f.output, path))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("not real source directory %s: %v", path, err)
		}
	}
	if _, err := os.Readlink(filepath.Join(f.output, "src/internal/fixture.go")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ignored_test.go", "generate.go"} {
		if _, err := os.Stat(filepath.Join(f.tinygo, "src/crypto/tls", name)); !os.IsNotExist(err) {
			t.Fatal("nonproduction file copied")
		}
	}
	replaced, _ := os.ReadFile(filepath.Join(f.output, "src/crypto/internal/sysrand/rand.go"))
	if !bytes.HasPrefix(replaced, []byte("// Copyright fixture\n\n")) {
		t.Fatal("license notice missing")
	}
	if err := adapter(replaced, "sysrand"); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(filepath.Join(f.source, "src/crypto/internal/sysrand/rand.go"))
	if !bytes.Equal(unchanged, original) {
		t.Fatal("installed Go source changed")
	}
	bin, _ := os.Readlink(filepath.Join(f.tinygo, "bin"))
	unchanged, _ = os.ReadFile(filepath.Join(filepath.Dir(bin), "src/crypto/tls/common.go"))
	if string(unchanged) != "original shim common.go" {
		t.Fatal("installed TinyGo source changed")
	}
	data, err := os.ReadFile(filepath.Join(f.output, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.GoVersion != goVersion || m.TinyGoVersion != "0.42.0" || m.Sources["crypto/tls/conn.go"] != f.expected.Conn || m.Sources["crypto/tls/defaults.go"] != f.expected.Defaults || m.Outputs["crypto/tls/conn.go"] != hash(got) || m.Outputs["crypto/tls/defaults.go"] != hash(defaults) || m.Outputs["crypto/internal/sysrand/rand.go"] != hash(replaced) {
		t.Fatalf("wrong manifest: %+v", m)
	}
	if len(m.Adapters) != 2 || m.RuntimeManifest == "" || m.GoVersionFile == "" {
		t.Fatal("incomplete manifest")
	}
	if err := prepare(f.source, f.tinygo, f.output, f.rand, f.cache, f.expected); err == nil {
		t.Fatal("reused output accepted")
	}
}
func TestPrepareRejectsChangedDefaultsSource(t *testing.T) {
	f := setup(t)
	path := filepath.Join(f.source, "src/crypto/tls/defaults.go")
	text(t, path, "package tls\n"+strings.Replace(noAESCipherSuites, "TLS_AES_128_GCM_SHA256", "TLS_CHANGED", 1)+"\n")
	if err := prepare(f.source, f.tinygo, f.output, f.rand, f.cache, f.expected); err == nil {
		t.Fatal("changed defaults.go accepted")
	}
	if err := empty(f.output); err != nil {
		t.Fatal("output modified before defaults pin rejection")
	}
}

func TestStrictPatch(t *testing.T) {
	for _, data := range []string{"", strings.Replace(cancellationBlock, "defer cancel()", "defer changed()", 1), cancellationBlock + cancellationBlock, cancellationBlock + "handshakeCtx, cancel := context.WithCancel(ctx)"} {
		if _, err := patchConn([]byte(data)); err == nil {
			t.Fatal("changed cancellation source accepted")
		}
	}
}
func TestAdaptersRejectInitialization(t *testing.T) {
	for _, data := range []string{"package sysrand\nimport \"time\"\n", "package sysrand\nfunc init() {}", "package sysrand\nvar a = 1", "package sysrand\nfunc Read(b []byte) {go func(){}()}"} {
		if err := adapter([]byte(data), "sysrand"); err == nil {
			t.Fatalf("unsafe adapter accepted: %s", data)
		}
	}
}
func TestValidationPreservesInputs(t *testing.T) {
	for _, failure := range []string{"version", "hash", "runtime metadata", "shim file", "shim link", "backend timer", "nonempty output"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t)
			switch failure {
			case "version":
				text(t, filepath.Join(f.source, "VERSION"), "go1.25.4\n")
			case "hash":
				f.expected.Conn = "bad"
			case "runtime metadata":
				text(t, filepath.Join(f.tinygo, "cryptocompat.json"), `{"tinygo_version":"0.43.0"}`)
			case "shim file":
				text(t, filepath.Join(f.tinygo, "src/crypto/tls/unrecognized.go"), "keep")
			case "shim link":
				path := filepath.Join(f.tinygo, "src/crypto/tls/tls.go")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				text(t, path, "keep")
			case "backend timer":
				text(t, f.rand, "package sysrand\nimport \"time\"\n")
			case "nonempty output":
				text(t, filepath.Join(f.output, "keep"), "keep")
			}
			if err := prepare(f.source, f.tinygo, f.output, f.rand, f.cache, f.expected); err == nil {
				t.Fatal("invalid input accepted")
			}
			if failure != "nonempty output" {
				if err := empty(f.output); err != nil {
					t.Fatal("output modified before validation")
				}
			}
			got, err := os.Readlink(filepath.Join(f.tinygo, "src/crypto/tls/common.go"))
			if err != nil || !strings.HasSuffix(got, "common.go") {
				t.Fatal("generated shim modified before validation")
			}
		})
	}
}

func TestCertificateAllocationGuards(t *testing.T) {
	source := []byte("package tls\n" + tls13CertificateAppend + "\n" + tls12CertificateScan + "\n")
	patched, err := patchCertificates(source)
	if err != nil {
		t.Fatal(err)
	}
	text := string(patched)
	if !strings.Contains(text, "len(certificate.Certificate) >= 8 || len(cert) == 0 || len(cert) > 32768") || !strings.Contains(text, "numCerts >= 8 || certLen == 0 || certLen > 32768") {
		t.Fatal("missing preallocation guards")
	}
	if strings.Index(text, "len(certificate.Certificate) >= 8") > strings.Index(text, tls13CertificateAppend) || strings.Index(text, "numCerts >= 8") > strings.Index(text, tls12CertificateScan) {
		t.Fatal("guard follows allocation anchor")
	}
	for _, bad := range [][]byte{nil, append(source, source...), patched} {
		if _, err := patchCertificates(bad); err == nil {
			t.Fatal("invalid or repatched certificate source accepted")
		}
	}
}
func TestCipherPreferencePatchIsStrictAndPreservesSuites(t *testing.T) {
	source := []byte("package tls\n" + noAESCipherSuites + "\n")
	patched, err := patchCipherSuites(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patched, []byte(aesFirstCipherSuites)) || bytes.Count(patched, []byte("TLS_AES_128_GCM_SHA256")) != 1 || bytes.Count(patched, []byte("TLS_AES_256_GCM_SHA384")) != 1 || bytes.Count(patched, []byte("TLS_CHACHA20_POLY1305_SHA256")) != 1 {
		t.Fatal("cipher suite preference lost or duplicated a TLS 1.3 suite")
	}
	for _, bad := range [][]byte{nil, patched, bytes.Replace(source, []byte("TLS_CHACHA20_POLY1305_SHA256"), []byte("TLS_CHANGED"), 1), append(source, source...)} {
		if _, err := patchCipherSuites(bad); err == nil {
			t.Fatal("changed or re-patched defaults source accepted")
		}
	}
}

func TestCandidateDepthGuard(t *testing.T) {
	source := []byte("package x509\n" + buildChainsEntry + "}\nconst maxChainSignatureChecks = 100\n")
	patched, err := patchVerify(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patched, []byte("if len(currentChain) >= 8")) || !bytes.Contains(patched, []byte("const maxChainSignatureChecks = 100")) {
		t.Fatal("depth or signature budget mismatch")
	}
	for _, bad := range [][]byte{nil, append(source, source...), patched} {
		if _, err := patchVerify(bad); err == nil {
			t.Fatal("invalid or repatched verify source accepted")
		}
	}
}

func TestRunRejectsLinkedOutputAncestor(t *testing.T) {
	root := testscratch.TempDir(t, "goos-linked-output-")
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "output"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(link, "output")
	if err := run([]string{"unused-source", output, output, "unused-rand", "unused-cache"}); err == nil || !strings.Contains(err.Error(), "GoOS build directory") {
		t.Fatalf("linked output ancestor accepted: %v", err)
	}
	entries, err := os.ReadDir(real)
	if err != nil || len(entries) != 1 {
		t.Fatalf("linked target changed: %v: %v", entries, err)
	}
}
