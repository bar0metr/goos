//go:build !goos_userspace

package main

import (
	"goos/kernel/vfs"
	"testing"
)

func scopeFixture(path string, denied vfs.Rights, stage bool) remoteScope {
	var s remoteScope
	s.size = uint8(copy(s.path[:], path))
	s.denied, s.staging, s.installed = denied, stage, true
	return s
}
func TestRemoteKeyLeaseDelayedBeginAndRetriedEnd(t *testing.T) {
	var guard remoteKeyLease
	// PID 1 processes Begin, but the listener times out before its reply. The
	// already-known correlation is enough to cancel the eventual lease.
	if !guard.begin(41, 7, "/home/user/.ssh/authorized_keys") {
		t.Fatal("begin")
	}
	if guard.begin(42, 7, "/home/user/.ssh/authorized_keys") {
		t.Fatal("overlapping begin")
	}
	if !guard.end(41) {
		t.Fatal("uncertain Begin cancellation")
	}
	if !guard.begin(43, 8, "/home/user/.ssh/other") {
		t.Fatal("subsequent begin stranded")
	}
	if guard.end(41) {
		t.Fatal("late Begin reconciliation released another lease")
	}
	if guard.owns(43, 7, "/home/user/.ssh/other") || guard.owns(43, 8, "/home/user/.ssh/authorized_keys") {
		t.Fatal("revision/path scope lost")
	}
	// An End that was not sent leaves ownership intact; bounded retry closes
	// that same lease, and cannot release a different subsequent operation.
	if !guard.owns(43, 8, "/home/user/.ssh/other") || !guard.end(43) {
		t.Fatal("End retry")
	}
	if !guard.begin(44, 9, "/home/user/.ssh/other") || guard.end(43) {
		t.Fatal("stale End")
	}
}

func TestRemoteKeyAuthorityConfinedCurrentOnlyNoHostGrant(t *testing.T) {
	keys := "/home/user/.ssh/authorized_keys"
	scopes := []remoteScope{scopeFixture("/state/security/ssh", vfs.Read|vfs.Write, false), scopeFixture(keys, vfs.Write, false), scopeFixture(keys+".key-new", vfs.Read|vfs.Write, true), scopeFixture(keys+".key-old", vfs.Read|vfs.Write, true), scopeFixture("/home/user/.ssh/previous", vfs.Write, false), scopeFixture(keys, vfs.Read|vfs.Write, false), scopeFixture("/state/previous-host", vfs.Read|vfs.Write, false)}
	scopes[0].subtree = true
	got := remoteKeyAuthority(scopes, keys)
	if !got.Confined {
		t.Fatal("unconfined key grant")
	}
	for i, rights := range got.Grants {
		want := vfs.Rights(0)
		if i >= 1 && i <= 3 {
			want = vfs.Read | vfs.Write
		}
		if rights != want {
			t.Fatal("key authority broadened", i, rights, want)
		}
	}
	got = remoteControllerAuthority(scopes, true)
	for i, rights := range got.Grants {
		want := vfs.Rights(0)
		switch i {
		case 0, 5, 6:
			want = vfs.Read | vfs.Write
		case 1, 4:
			want = vfs.Read
		}
		if rights != want {
			t.Fatal("controller authority changed", i, rights, want)
		}
	}
	scopes[1].installed = false
	if remoteKeyAuthority(scopes, keys).Grants[1] != 0 {
		t.Fatal("uninstalled key scope granted")
	}
}
func TestRemoteKeyAuthorityDoesNotReuseRetiredScopes(t *testing.T) {
	scopes := make([]remoteScope, vfs.MaxScopes)
	for i := range scopes {
		scopes[i] = scopeFixture("/old/authorized", vfs.Write, false)
	}
	for _, rights := range remoteKeyAuthority(scopes, "/new/authorized").Grants {
		if rights != 0 {
			t.Fatal("retired scope granted to new path")
		}
	}
}

func TestIdentityScopeSlotContract(t *testing.T) {
	if identityScopeSlot != vfs.MaxScopes-1 {
		t.Fatalf("identity slot %d, want %d", identityScopeSlot, vfs.MaxScopes-1)
	}
	if remoteScopeMax != identityScopeSlot {
		t.Fatal("remote prefix not bounded to the slots before identity")
	}
}

func TestRemoteScopeSlotBound(t *testing.T) {
	if remoteScopeAtLimit(remoteScopeMax - 1) {
		t.Fatal("last remote slot refused")
	}
	if !remoteScopeAtLimit(remoteScopeMax) {
		t.Fatal("identity slot not protected from remote allocation")
	}
}

func TestIdentityAuthorityGrantsRootAndScope(t *testing.T) {
	authority, ok := identityAuthority([]byte("/state/identity"))
	if !ok {
		t.Fatal("identity authority refused")
	}
	if !authority.Confined {
		t.Fatal("identity authority not confined")
	}
	if !authority.Valid() {
		t.Fatal("identity authority invalid")
	}
	if string(authority.Root[:authority.RootSize]) != "/state/identity" {
		t.Fatalf("root %q", authority.Root[:authority.RootSize])
	}
	for slot := 0; slot < vfs.MaxScopes; slot++ {
		want := vfs.Rights(0)
		if slot == int(identityScopeSlot) {
			want = vfs.Read | vfs.Write
		}
		if authority.Grants[slot] != want {
			t.Fatalf("grant %d = %v, want %v", slot, authority.Grants[slot], want)
		}
	}
	if _, ok := identityAuthority([]byte("relative")); ok {
		t.Fatal("non-canonical root accepted")
	}
}
