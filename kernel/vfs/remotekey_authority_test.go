package vfs

import (
	"goos/kernel/ramfs"
	"testing"
)

func keyScopeFixture(t *testing.T) (*Namespace, Authority) {
	t.Helper()
	ns, _ := mountedNamespace(t)
	ns.Create([]byte("/private"), true)
	ns.Create([]byte("/keys"), true)
	for _, path := range []string{"/private/host", "/keys/authorized", "/keys/old-authorized"} {
		ns.Create([]byte(path), false)
	}
	for slot, item := range []struct {
		path    string
		subtree bool
		denied  Rights
	}{
		{"/private", true, Read | Write}, {"/keys/authorized", false, Write}, {"/keys/authorized.key-new", false, Read | Write}, {"/keys/authorized.key-old", false, Read | Write}, {"/keys/old-authorized", false, Write},
	} {
		if status := ns.InstallScope(uint8(slot), []byte(item.path), item.subtree, item.denied); status != StatusOK {
			t.Fatal("scope", status)
		}
	}
	authority := Authority{Confined: true}
	authority.Grants[1], authority.Grants[2], authority.Grants[3] = Read|Write, Read|Write, Read|Write
	if ns.SetAuthority(7, authority) != StatusOK {
		t.Fatal("grant")
	}
	return ns, authority
}
func TestRemoteKeyAdminCannotWritePrivateOrRetainedPaths(t *testing.T) {
	ns, _ := keyScopeFixture(t)
	for _, path := range []string{"/private/host", "/keys/old-authorized", "/unrelated"} {
		for _, rights := range []Rights{Read, Write, Read | Write} {
			if status := ns.AuthorizePath(7, []byte(path), rights, false); status != StatusPermissionDenied {
				t.Fatal("broadened key grant", path, rights, status)
			}
		}
	}
	for _, path := range []string{"/keys/authorized", "/keys/authorized.key-new", "/keys/authorized.key-old"} {
		if status := ns.AuthorizePath(7, []byte(path), Read|Write, false); status != StatusOK {
			t.Fatal("key scope", path, status)
		}
	}
	// Ordinary shell/worker VFS sessions retain the private denial and cannot
	// obtain the administrative writer by opening a file before publication.
	if _, status := ns.Open(8, []byte("/private/host"), Read); status != StatusPermissionDenied {
		t.Fatal("private shell read", status)
	}
	if _, status := ns.Open(8, []byte("/keys/authorized"), Write); status != StatusPermissionDenied {
		t.Fatal("ordinary key writer", status)
	}
	if _, status := ns.Open(8, []byte("/keys/authorized"), Read); status != StatusOK {
		t.Fatal("ordinary public read", status)
	}
	controller := Authority{Confined: true}
	controller.Grants[0] = Read | Write
	controller.Grants[1], controller.Grants[4] = Read, Read
	if ns.SetAuthority(9, controller) != StatusOK {
		t.Fatal("controller")
	}
	for _, path := range []string{"/keys/authorized", "/keys/old-authorized"} {
		if _, status := ns.Open(9, []byte(path), Read); status != StatusOK {
			t.Fatal("retained controller read", status)
		}
		if status := ns.AuthorizePath(9, []byte(path), Write, false); status != StatusPermissionDenied {
			t.Fatal("controller key writer", status)
		}
	}
	if status := ns.AuthorizePath(9, []byte("/keys/authorized.key-new"), Read|Write, false); status != StatusPermissionDenied {
		t.Fatal("controller inherited key staging", status)
	}
}
func TestRemoteKeyHostMountAliasAndNewProtectionRevokeHandles(t *testing.T) {
	ns, fs := mountedNamespace(t)
	ns.Create([]byte("/private"), true)
	ns.Create([]byte("/private/host"), false)
	_, root, _ := ns.Resolve([]byte("/private"))
	if ns.Mount([]byte("/alias"), fs, root) != StatusOK {
		t.Fatal("alias")
	}
	old, status := ns.Open(8, []byte("/alias/host"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	ns.InstallScope(0, []byte("/private"), true, Read|Write)
	ns.InstallScope(1, []byte("/alias/host"), false, Write)
	ns.InstallScope(2, []byte("/alias/host.key-new"), false, Read|Write)
	ns.InstallScope(3, []byte("/alias/host.key-old"), false, Read|Write)
	grant := Authority{Confined: true}
	grant.Grants[1], grant.Grants[2], grant.Grants[3] = Read|Write, Read|Write, Read|Write
	ns.SetAuthority(7, grant)
	for _, path := range []string{"/alias/host", "/alias/host.key-new", "/alias/host.key-old"} {
		if status := ns.AuthorizePath(7, []byte(path), Read|Write, false); status != StatusPermissionDenied {
			t.Fatal("alias escaped private intersection", path, status)
		}
	}
	if _, status := ns.Write(8, old, []byte("bad")); status != StatusNotFound {
		t.Fatal("old handle survived installed protection", status)
	}
}
func TestRemoteKeyScopesExhaustionAndCaseOverlapFailClosed(t *testing.T) {
	ns, grant := keyScopeFixture(t)
	// A case-insensitive filesystem name match must not allow a create under a
	// staging-name alias through an ordinary session with no scope grant.
	ns.mounts[0].filesystemType = ramfs.FilesystemFAT32
	if status := ns.AuthorizePath(8, []byte("/keys/AUTHORIZED.KEY-NEW"), Write, false); status != StatusPermissionDenied {
		t.Fatal("case alias", status)
	}
	for slot := uint8(5); slot < MaxScopes; slot++ {
		if ns.InstallScope(slot, []byte("/extra"), false, Read|Write) != StatusOK {
			t.Fatal("remaining slot")
		}
	}
	if ns.InstallScope(MaxScopes, []byte("/ninth"), false, Read|Write) != StatusInvalidArgument {
		t.Fatal("scope exhaustion")
	}
	if ns.InstallScope(0, []byte("/new-private"), false, Read|Write) != StatusInvalidArgument {
		t.Fatal("protected slot recycled")
	}
	if ns.SetAuthority(7, grant) != StatusOK {
		t.Fatal("grant restored")
	}
	if status := ns.AuthorizePath(7, []byte("/private/host"), Read|Write, false); status != StatusPermissionDenied {
		t.Fatal("exhaustion broadened authority", status)
	}
}
