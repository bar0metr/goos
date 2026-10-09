package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestProductionScanRejectsTestAndPrivateMaterial(t *testing.T) {
	root := t.TempDir()
	system := filepath.Join(root, "system")
	if err := os.MkdirAll(filepath.Join(system, "system/security"), 0755); err != nil {
		t.Fatal(err)
	}
	prod, validation, seed := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32)
	paths := []string{filepath.Join(root, "production"), filepath.Join(root, "validation"), filepath.Join(root, "seed")}
	for i, data := range [][]byte{prod, validation, seed} {
		if err := os.WriteFile(paths[i], data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(system, "system/security/trust-update-key.pub"), prod, 0644); err != nil {
		t.Fatal(err)
	}
	check := func() error { return scan(system, paths[0], paths[1], paths[2]) }
	if err := check(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(system, "payload")
	for _, bad := range [][]byte{seed, validation, []byte("-----BEGIN PRIVATE KEY-----\npublic test fixture")} {
		if err := os.WriteFile(artifact, bad, 0644); err != nil {
			t.Fatal(err)
		}
		if err := check(); err == nil {
			t.Fatal("private/test bytes accepted")
		}
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(system, "private.seed"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if check() == nil {
		t.Fatal("private key filename accepted")
	}
}
