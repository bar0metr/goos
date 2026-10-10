package commands

import "testing"

func TestRemoteGrantRoundTrip(t *testing.T) {
	for _, grants := range []Grant{GrantRemote, GrantRemote | GrantVFS | GrantResolve} {
		index, ok := Append(nil, "remote", Manifest{Usage: "remote status|reload", Summary: "remote access", MinArgs: 1, MaxArgs: 1, Grants: grants})
		if !ok || !Valid(index) {
			t.Fatal("remote grant encoding rejected", grants)
		}
		entry, found := Lookup(index, []byte("remote"))
		if !found || entry.Grants != grants {
			t.Fatal("four-digit grant did not decode", grants)
		}
	}
	if _, _, ok := Next([]byte("x\t999999\t0\t0\tx\tx\t\n"), 0); ok {
		t.Fatal("oversized grant field accepted")
	}
	if _, _, ok := Next([]byte("x\t65535\t0\t0\tx\tx\t\n"), 0); ok {
		t.Fatal("unknown grant bits accepted")
	}
}
