package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tree writes files under a new directory and returns it.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

var sources = map[string]string{
	"kernel/a/a.go":             "//go:build uefi\n\npackage a\n\nconst (\n\tMaxSlots = 8\n\tmaxHidden = 2\n\tMax = 1\n)\n\nvar MaxBuffer = MaxSlots * 2\n",
	"kernel/a/a_test.go":        "package a\n\nconst MaxTest = 1\n",
	"userspace/w/wire.go":       "package w\n\nconst MaxName, MaxLabel = 255, 63\n",
	"userspace/w/testdata/t.go": "package t\n\nconst MaxIgnored = 1\n",
	"tools/x/x.go":              "package x\n\nconst MaxTool = 1\n",
}

func runDoc(t *testing.T, doc string) (int, string) {
	t.Helper()
	files := map[string]string{"policy.md": doc}
	for name, text := range sources {
		files[name] = text
	}
	dir := tree(t, files)
	var stdout, stderr bytes.Buffer
	code := run(dir, "policy.md", false, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func TestScanFindsExportedMaxBoundsOutsideTests(t *testing.T) {
	bounds, err := scan(tree(t, sources))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, b := range bounds {
		keys = append(keys, b.Key()+"="+b.Value)
	}
	got := strings.Join(keys, " ")
	want := "kernel/a.MaxBuffer=MaxSlots * 2 kernel/a.MaxSlots=8 userspace/w.MaxLabel=63 userspace/w.MaxName=255"
	if got != want {
		t.Fatalf("scan = %q, want %q", got, want)
	}
}

func TestCompleteInventoryPasses(t *testing.T) {
	doc := "| Bound | Class |\n| --- | --- |\n| `kernel/a.MaxSlots`, `kernel/a.MaxBuffer` | profile |\n| `userspace/w.Max*` | protocol |\n"
	if code, out := runDoc(t, doc); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestMissingBoundFails(t *testing.T) {
	doc := "| `kernel/a.MaxSlots` | profile |\n| `userspace/w.Max*` | protocol |\n"
	code, out := runDoc(t, doc)
	if code != 1 || !strings.Contains(out, "kernel/a.MaxBuffer has no resource-policy entry") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestProseDoesNotClassify(t *testing.T) {
	doc := "`kernel/a.MaxSlots` and `kernel/a.MaxBuffer` are mentioned here.\n| `userspace/w.Max*` | protocol |\n"
	code, out := runDoc(t, doc)
	if code != 1 || !strings.Contains(out, "kernel/a.MaxSlots") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestStaleEntriesFail(t *testing.T) {
	doc := "| `kernel/a.MaxSlots`, `kernel/a.MaxBuffer` | profile |\n| `userspace/w.Max*` | protocol |\n" +
		"| `kernel/a.MaxGone` | artifact |\n| `kernel/b.Max*` | protocol |\n"
	code, out := runDoc(t, doc)
	for _, want := range []string{"`kernel/a.MaxGone` names no Max* bound", "`kernel/b.Max*` matches no package"} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("code %d, want %q in: %s", code, want, out)
		}
	}
}

// TestRepositoryInventory runs the check on this repository, so a new Max*
// bound without a row fails `make test-unit`.
func TestRepositoryInventory(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(root, "docs/architecture/resource-policy.md", false, &stdout, &stderr); code != 0 {
		t.Fatalf("resourcepolicy exited %d:\n%s", code, stderr.String())
	}
}
