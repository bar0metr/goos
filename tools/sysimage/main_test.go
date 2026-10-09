package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyApps(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "host")
	target := filepath.Join(root, "image", "apps")
	if err := os.MkdirAll(filepath.Join(source, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x7f, 'E', 'L', 'F', 0, 1}
	if err := os.WriteFile(filepath.Join(source, "tools", "probe"), want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkAppsSource(source, filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	if err := copyApps(source, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "tools", "probe"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("copied file = %q, want %q", got, want)
	}
}

func TestAppsSourceCannotOverlapOutput(t *testing.T) {
	root := t.TempDir()
	for _, pair := range [][2]string{
		{root, filepath.Join(root, "image")},
		{filepath.Join(root, "image"), root},
	} {
		if err := os.MkdirAll(pair[0], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := checkAppsSource(pair[0], pair[1]); err == nil || !strings.Contains(err.Error(), "overlaps output") {
			t.Fatalf("checkAppsSource(%q, %q) = %v", pair[0], pair[1], err)
		}
	}
}

func TestCopyAppsRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "host")
	target := filepath.Join(root, "image", "apps")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := copyApps(source, target); err == nil || !strings.Contains(err.Error(), "not a regular file or directory") {
		t.Fatalf("copyApps() = %v", err)
	}
}
