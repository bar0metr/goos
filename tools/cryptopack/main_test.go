package main

import (
	"goos/tools/internal/commandset"
	"goos/userspace/commands"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	tree, probes := filepath.Join(root, "tree"), filepath.Join(root, "probes")
	bin := filepath.Join(tree, "system/bin")
	for _, directory := range []string{bin, probes} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var index []byte
	for _, name := range []string{"cat", "write", "echo", "ps", "quota", "cryptoprobe", "fetch"} {
		var ok bool
		index, ok = commands.Append(index, name, commands.Manifest{Usage: name, Summary: name})
		if !ok {
			t.Fatal("invalid fixture index")
		}
		if err := os.WriteFile(filepath.Join(bin, name), []byte(name), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tree, "system/commands"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	return tree, probes
}

func TestPackKeepsProbeIndexWithinBounds(t *testing.T) {
	tree, probes := fixture(t)
	if err := os.WriteFile(filepath.Join(probes, "sha256"), []byte("native"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pack(tree, probes); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(tree, "system/commands"))
	if err != nil || !commands.Valid(index) {
		t.Fatalf("index: %v", err)
	}
	if _, err := commandset.Names(filepath.Join(tree, "system/bin"), index); err != nil {
		t.Fatal(err)
	}
	if _, ok := commands.Lookup(index, []byte("crypto-sha256")); !ok {
		t.Fatal("probe not registered")
	}
	if _, err := os.Stat(filepath.Join(tree, "system/bin/fetch")); !os.IsNotExist(err) {
		t.Fatal("discarded command remains in disposable tree")
	}
}

func TestPackRefusesInvalidProbeEntry(t *testing.T) {
	for _, name := range []string{"bad.name", "directory", "way-too-long-for-a-command-index-probe-name"} {
		t.Run(name, func(t *testing.T) {
			tree, probes := fixture(t)
			path := filepath.Join(probes, name)
			var err error
			if name == "directory" {
				err = os.Mkdir(path, 0o755)
			} else {
				err = os.WriteFile(path, nil, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := pack(tree, probes); err == nil {
				t.Fatal("accepted invalid probe entry")
			}
		})
	}
}
