package vfs

import (
	"fmt"
	"goos/kernel/ramfs"
	"testing"
)

func TestScopedDirectoryMountAliasesAndHandleRelocation(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	for _, path := range []string{"/private", "/private/nested"} {
		if status := namespace.Create([]byte(path), true); status != StatusOK {
			t.Fatal(status)
		}
	}
	for _, path := range []string{"/private/nested/key", "/source"} {
		if status := namespace.Create([]byte(path), false); status != StatusOK {
			t.Fatal(status)
		}
	}
	_, nested, status := namespace.Resolve([]byte("/private/nested"))
	if status != StatusOK {
		t.Fatal(status)
	}
	if namespace.Mount([]byte("/alias"), fs, nested) != StatusOK {
		t.Fatal("mount")
	}
	source, status := namespace.Open(2, []byte("/source"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if namespace.PrepareExecutable(2, source) != StatusOK {
		t.Fatal("prepare")
	}
	if namespace.InstallScope(0, []byte("/private"), true, Read|Write) != StatusOK {
		t.Fatal("scope")
	}
	if _, status := namespace.Open(2, []byte("/alias/key"), Read); status != StatusPermissionDenied {
		t.Fatalf("alias read=%d", status)
	}
	authority := Authority{Confined: true}
	authority.Grants[0] = Read | Write
	if namespace.SetAuthority(3, authority) != StatusOK {
		t.Fatal("authority")
	}
	alias, status := namespace.Open(3, []byte("/alias/key"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	namespace.Close(3, alias)
	// A trusted move refreshes already-open sources, including executable reads.
	if status := namespace.Rename([]byte("/source"), []byte("/private/source")); status != StatusOK {
		t.Fatal(status)
	}
	var bytes [1]byte
	if _, status := namespace.Read(2, source, bytes[:]); status != StatusPermissionDenied {
		t.Fatalf("relocated read=%d", status)
	}
	if _, status := namespace.ReadAt(2, source, 0, bytes[:]); status != StatusPermissionDenied {
		t.Fatalf("relocated executable read=%d", status)
	}
	if _, status := namespace.Write(2, source, bytes[:]); status != StatusPermissionDenied {
		t.Fatalf("relocated write=%d", status)
	}
	if _, status := namespace.StatOpen(2, source); status != StatusPermissionDenied {
		t.Fatalf("relocated stat=%d", status)
	}
	namespace.Close(2, source)
	if status := namespace.AuthorizePath(3, []byte("/alias/key"), Write, true); status != StatusOK {
		t.Fatalf("alias mutation=%d", status)
	}
	if status := namespace.AuthorizePath(3, []byte("/private"), Write, true); status != StatusPermissionDenied {
		t.Fatalf("scope root move=%d", status)
	}
	if status := namespace.UnmountBackend(fs); status != StatusBusy {
		t.Fatalf("unmount=%d", status)
	}
}

func TestAuthorityReplacementAndOwnerReuseDoNotRetainGrants(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	namespace.Create([]byte("/private"), true)
	namespace.Create([]byte("/private/key"), false)
	namespace.InstallScope(0, []byte("/private"), true, Read|Write)
	authority := Authority{Confined: true}
	authority.Grants[0] = Read
	if namespace.SetAuthority(1, authority) != StatusOK || namespace.SetAuthority(2, authority) != StatusOK {
		t.Fatal("initial authorities")
	}
	namespace.ForgetAuthority(1)
	file, status := namespace.Open(2, []byte("/private/key"), Read)
	if status != StatusOK {
		t.Fatal(status)
	}
	if namespace.SetAuthority(2, Authority{}) != StatusOK {
		t.Fatal("replace")
	}
	var data [1]byte
	if _, status := namespace.Read(2, file, data[:]); status != StatusNotFound {
		t.Fatal("old handle survived replacement")
	}
	if _, status := namespace.Open(2, []byte("/private/key"), Read); status != StatusPermissionDenied {
		t.Fatal("old authority retained after replacement")
	}
	if namespace.SetAuthority(1, Authority{}) != StatusOK {
		t.Fatal("reuse")
	}
	if _, status := namespace.Open(1, []byte("/private/key"), Read); status != StatusPermissionDenied {
		t.Fatal("reused owner inherited grant")
	}
	authority.Grants[1] = Read
	if namespace.SetAuthority(4, authority) != StatusNotFound {
		t.Fatal("uninstalled scope grant")
	}
	authority.Grants[1] = Rights(8)
	if namespace.SetAuthority(4, authority) != StatusInvalidArgument {
		t.Fatal("invalid grant")
	}
}

func TestScopedAncestryLimitsFailClosed(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	namespace.Create([]byte("/private"), true)
	namespace.Create([]byte("/public"), false)
	_, directory, _ := namespace.Resolve([]byte("/private"))
	for index := 0; index < maxScopeWalk+1; index++ {
		if _, status := fs.Create(directory, []byte(fmt.Sprintf("entry%d", index))); status != ramfs.StatusOK {
			t.Fatal(status)
		}
	}
	if namespace.InstallScope(0, []byte("/private"), true, Read|Write) != StatusOK {
		t.Fatal("scope")
	}
	if _, status := namespace.Open(1, []byte("/public"), Read); status != StatusPermissionDenied {
		t.Fatalf("inconclusive ancestry read=%d", status)
	}
	if status := namespace.AuthorizePath(1, []byte("/new"), Write, false); status != StatusPermissionDenied {
		t.Fatalf("inconclusive ancestry create=%d", status)
	}
	for slot := uint8(1); slot < MaxScopes; slot++ {
		if status := namespace.InstallScope(slot, []byte("/missing"), false, Read|Write); status != StatusOK {
			t.Fatal(status)
		}
	}
	if status := namespace.InstallScope(MaxScopes, []byte("/missing"), false, Read|Write); status != StatusInvalidArgument {
		t.Fatal("slot bounds")
	}
	if status := namespace.InstallScope(0, []byte("/other"), false, Read|Write); status != StatusInvalidArgument {
		t.Fatal("scope replacement")
	}
}
