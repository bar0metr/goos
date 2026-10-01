package commandset

import (
	"os"
	"path/filepath"
	"testing"

	"goos/userspace/commands"
)

func testIndex(t *testing.T, names ...string) []byte {
	t.Helper()
	var index []byte
	for _, name := range names {
		var ok bool
		if index, ok = commands.Append(index, name, commands.Manifest{Usage: name, Summary: "test"}); !ok {
			t.Fatalf("append %s", name)
		}
	}
	return index
}

func TestNamesRequiresAgreement(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"ls", "cat"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("image"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names, err := Names(directory, testIndex(t, "cat", "ls"))
	if err != nil || len(names) != 2 || names[0] != "cat" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if _, err := Names(directory, testIndex(t, "cat")); err == nil {
		t.Fatal("an image without an entry was accepted")
	}
	if _, err := Names(directory, testIndex(t, "cat", "ls", "rm")); err == nil {
		t.Fatal("an entry without an image was accepted")
	}
}

func TestSubsetKeepsIndexOrderAndRefusesUnknownCommands(t *testing.T) {
	index := testIndex(t, "cat", "gfm", "ls")
	subset, err := Subset(index, []string{"ls", "cat"})
	if err != nil {
		t.Fatal(err)
	}
	if string(subset) != string(testIndex(t, "cat", "ls")) || !commands.Valid(subset) {
		t.Fatalf("subset = %q", subset)
	}
	if _, err := Subset(index, []string{"missing"}); err == nil {
		t.Fatal("a command outside the index was accepted")
	}
}
