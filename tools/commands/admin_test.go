package main

import (
	"goos/userspace/commands"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAdministrationIndexAgreement(t *testing.T) {
	const root = "../../userspace/bin"
	for _, validation := range []bool{false, true} {
		index, err := buildSelected(root, validation)
		if err != nil || !commands.Valid(index) || len(index) > commands.MaxIndex {
			t.Fatalf("validation=%v index=%d err=%v", validation, len(index), err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, directory := range entries {
			if !directory.IsDir() {
				continue
			}
			name := directory.Name()
			_, found := commands.Lookup(index, []byte(name))
			want := validation || name != "cryptoprobe" && name != "trustprobe"
			if found != want {
				t.Fatalf("validation=%v name=%s found=%v", validation, name, found)
			}
		}
		for _, test := range []struct {
			name, bound string
			grant       commands.Grant
		}{
			{"head", "64 MiB", commands.GrantVFS}, {"tail", "two scans", commands.GrantVFS},
			{"grep", "8192-byte line", commands.GrantVFS}, {"wc", "ASCII words", commands.GrantVFS},
			{"find", "4096 nodes", commands.GrantVFS}, {"hexdump", "16 bytes/row", commands.GrantVFS},
			{"hostname", "1..63", commands.GrantHostname}, {"date", "UTC only", 0},
		} {
			entry, found := commands.Lookup(index, []byte(test.name))
			if !found || entry.Grants != test.grant || !strings.Contains(string(entry.Options), test.bound) {
				t.Fatal("native manifest", test.name, entry)
			}
			source, err := os.ReadFile(filepath.Join(root, test.name, "main.go"))
			if err != nil || !strings.Contains(string(source), "utility.Run(manifest, run)") {
				t.Fatal("native help/runtime missing", test.name, err)
			}
		}
	}
}

func TestHostnameGrantLiteral(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "hostname", `package main
var manifest = commands.Manifest{Usage: "hostname [NAME]", Summary: "hostname", MaxArgs: 1, Grants: commands.GrantHostname}`)
	index, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := commands.Lookup(index, []byte("hostname"))
	if !found || entry.Grants != commands.GrantHostname || entry.Grants&commands.GrantVFS != 0 {
		t.Fatal("hostname broader grant", entry)
	}
}
