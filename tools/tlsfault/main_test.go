package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (source, output, original string) {
	t.Helper()
	root, err := os.MkdirTemp("/Volumes/Data/tmp", "tlsfault-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source, output = filepath.Join(root, "source"), filepath.Join(root, "output")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, "VERSION"), goVersion+"\ntime fixture\n")
	write(t, filepath.Join(source, "LICENSE"), "Go license fixture\n")
	original = "// Copyright fixture\n\npackage tls\n\nfunc (hs *serverHandshakeStateTLS13) sendServerFinished() error {\n" + finishedSource + "\treturn nil\n}\n"
	write(t, filepath.Join(source, "src/crypto/tls/handshake_server_tls13.go"), original)
	write(t, filepath.Join(source, "src/crypto/tls/other.go"), "package tls\n")
	return source, output, original
}

func TestPrepareCopyOnWriteFaultRoot(t *testing.T) {
	source, output, original := fixture(t)
	if err := prepare(source, output, hash([]byte(original))); err != nil {
		t.Fatal(err)
	}
	patchedPath := filepath.Join(output, "src/crypto/tls/handshake_server_tls13.go")
	patched, err := os.ReadFile(patchedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patched, []byte(finishedPatched)) || bytes.Count(patched, []byte("finished.verifyData[0] ^= 1")) != 1 {
		t.Fatal("missing or duplicate Finished fault")
	}
	info, err := os.Lstat(patchedPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("patched source is not a regular file")
	}
	linked := filepath.Join(output, "LICENSE")
	if target, err := os.Readlink(linked); err != nil || target != filepath.Join(source, "LICENSE") {
		t.Fatalf("root LICENSE is not linked: %q %v", target, err)
	}
	data, err := os.ReadFile(filepath.Join(output, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var record manifest
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.GoVersion != goVersion || record.Source != hash([]byte(original)) || record.Output != hash(patched) {
		t.Fatalf("manifest = %+v", record)
	}
	stillOriginal, _ := os.ReadFile(filepath.Join(source, "src/crypto/tls/handshake_server_tls13.go"))
	if !bytes.Equal(stillOriginal, []byte(original)) {
		t.Fatal("source GOROOT was modified")
	}
}

func TestFinishedPatchIsStrict(t *testing.T) {
	original := []byte("package tls\n" + finishedSource)
	patched, err := patchFinished(original)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{nil, patched, bytes.Replace(original, []byte("finishedHash"), []byte("changedHash"), 1), append(original, original...)} {
		if _, err := patchFinished(invalid); err == nil {
			t.Fatal("changed or re-patched Finished source accepted")
		}
	}
}

func TestPrepareRejectsInvalidRootsBeforeWriting(t *testing.T) {
	source, output, original := fixture(t)
	for _, change := range []func(){
		func() { write(t, filepath.Join(source, "VERSION"), "go1.25.4\n") },
		func() {
			write(t, filepath.Join(source, "src/crypto/tls/handshake_server_tls13.go"), strings.Replace(original, "finishedHash", "changedHash", 1))
		},
	} {
		// Each iteration needs a fresh fixture after a mutation.
		source, output, original = fixture(t)
		change()
		if err := prepare(source, output, hash([]byte(original))); err == nil {
			t.Fatal("invalid source accepted")
		}
		if err := requireEmpty(output); err != nil {
			t.Fatal("output modified before validation failure")
		}
	}
}
