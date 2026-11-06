package main

import "testing"

func TestEtcHoldsTheStaticLibcFiles(t *testing.T) {
	want := map[string]string{
		"system/etc/resolv.conf":   "nameserver 127.0.0.1\n",
		"system/etc/hosts":         "127.0.0.1 localhost\n",
		"system/etc/nsswitch.conf": "hosts: files dns\n",
	}
	members := etc()
	if members[0].header.Name != "system/etc/" {
		t.Fatalf("first member %q, want the directory", members[0].header.Name)
	}
	for _, entry := range members[1:] {
		text, found := want[entry.header.Name]
		if !found || string(entry.data) != text || entry.header.Mode != 0o444 {
			t.Fatalf("%s: %q mode %o", entry.header.Name, entry.data, entry.header.Mode)
		}
		delete(want, entry.header.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v", want)
	}
}
