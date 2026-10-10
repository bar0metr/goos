package main

import (
	"goos/userspace/commands"
	"testing"
)

func TestBuildRemoteAdministrativeGrant(t *testing.T) {
	root := t.TempDir()
	writeCommand(t, root, "remote", `package main
var manifest = commands.Manifest{Usage: "remote status|reload", Summary: "remote access", MinArgs: 1, MaxArgs: 1, Grants: commands.GrantRemote}`)
	index, err := build(root)
	if err != nil {
		t.Fatal(err)
	}
	entry, found := commands.Lookup(index, []byte("remote"))
	if !found || entry.Grants != commands.GrantRemote || entry.MinArgs != 1 || entry.MaxArgs != 1 {
		t.Fatal("remote command authority missing", entry)
	}
}
