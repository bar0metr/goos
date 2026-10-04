package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/userspace/commands"
)

func writeCommand(t *testing.T, root, name, source string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildReadsManifestsInNameOrder(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "zz", `package main
var manifest = commands.Manifest{Usage: "zz", Summary: "last"}`)
	writeCommand(t, root, "aa", `package main
var manifest = commands.Manifest{Usage: "aa FILE", Summary: "first", Options: "-f, --first", MinArgs: 1, MaxArgs: commands.Unbounded, Grants: commands.GrantVFS}`)
	index, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	first, next, ok := commands.Next(index, 0)
	if !ok || string(first.Name) != "aa" || string(first.Options) != "-f, --first" || first.Grants != commands.GrantVFS || first.MaxArgs != commands.Unbounded || first.MinArgs != 1 {
		t.Fatalf("first = %+v", first)
	}
	if second, _, ok := commands.Next(index, next); !ok || string(second.Name) != "zz" {
		t.Fatalf("second = %+v", second)
	}
}

func TestBuildJoinsGrants(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "tui", `package main
var manifest = commands.Manifest{Usage: "tui", Summary: "full screen", Grants: commands.GrantVFS | commands.GrantTerminal}`)
	index, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, ok := commands.Next(index, 0)
	if !ok || entry.Grants != commands.GrantVFS|commands.GrantTerminal {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestBuildRefusesBadManifests(t *testing.T) {
	for reason, source := range map[string]string{
		"missing": `package main`,
		"twice":   "package main\nvar manifest = commands.Manifest{Usage: \"cmd\", Summary: \"x\"}\nvar (manifest = commands.Manifest{Usage: \"cmd\", Summary: \"x\"})",
		"computed": `package main
var manifest = commands.Manifest{Usage: "cmd" + "x", Summary: "x"}`,
		"wrong type": `package main
var manifest = other.Manifest{Usage: "cmd", Summary: "x"}`,
		"unknown field": `package main
var manifest = commands.Manifest{Usage: "cmd", Summary: "x", Root: true}`,
		"unknown grant": `package main
var manifest = commands.Manifest{Usage: "cmd", Summary: "x", Grants: commands.GrantEverything}`,
		"unknown joined grant": `package main
var manifest = commands.Manifest{Usage: "cmd", Summary: "x", Grants: commands.GrantVFS | commands.GrantEverything}`,
		"added grants": `package main
var manifest = commands.Manifest{Usage: "cmd", Summary: "x", Grants: commands.GrantVFS + commands.GrantTerminal}`,
		"foreign usage": `package main
var manifest = commands.Manifest{Usage: "other", Summary: "x"}`,
		"bad range": `package main
var manifest = commands.Manifest{Usage: "cmd", Summary: "x", MinArgs: 2, MaxArgs: 1}`,
		"positional": `package main
var manifest = commands.Manifest{"cmd", "x", 0, 0, 0}`,
	} {
		root := t.TempDir()
		writeCommand(t, root, "cmd", source)
		if _, err := build(root); err == nil {
			t.Fatalf("%s: accepted", reason)
		}
	}
}

func TestBuildRefusesAnInvalidCommandName(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "Bad_Name", `package main
var manifest = commands.Manifest{Usage: "Bad_Name", Summary: "x"}`)
	if _, err := build(root); err == nil || !strings.Contains(err.Error(), "Bad_Name") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheSystemCommandsBuild(t *testing.T) {
	index, err := build("../../userspace/bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ls", "cat", "echo", "write", "mem", "uptime"} {
		if _, found := commands.Lookup(index, []byte(name)); !found {
			t.Fatalf("%s missing from the index", name)
		}
	}
	lscpu, found := commands.Lookup(index, []byte("lscpu"))
	if !found {
		t.Fatal("lscpu missing from the index")
	}
	for _, option := range []string{"--help", "--version", "--bytes", "--json", "--extended", "--parse", "--caches", "--list-columns", "--all", "--online", "--offline", "--raw", "--hex", "--physical", "--output-all"} {
		if !strings.Contains(string(lscpu.Options), option) {
			t.Errorf("lscpu help missing %s: %q", option, lscpu.Options)
		}
	}
}
