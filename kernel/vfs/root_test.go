package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

func rootedNamespace(t *testing.T) (*Namespace, Owner) {
	t.Helper()
	namespace, _ := mountedNamespace(t)
	for _, path := range []string{"/home", "/home/user", "/home/user/docs", "/home/other", "/state"} {
		if status := namespace.Create([]byte(path), true); status != StatusOK {
			t.Fatalf("mkdir %s: %v", path, status)
		}
	}
	for _, path := range []string{"/home/user/a", "/home/other/b", "/state/c"} {
		if status := namespace.Create([]byte(path), false); status != StatusOK {
			t.Fatalf("create %s: %v", path, status)
		}
	}
	authority, ok := RootedAuthority([]byte("/home/user"))
	if !ok {
		t.Fatal("root refused")
	}
	owner := Owner(9)
	if status := namespace.SetAuthority(owner, authority); status != StatusOK {
		t.Fatal(status)
	}
	return namespace, owner
}

func TestRootLimitsEveryPath(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	for _, path := range []string{"/home/user", "/home/user/", "/home/user/a", "/home/user/docs", "/home/user/docs/..", "/home/user/./a"} {
		if status := namespace.AuthorizePath(owner, []byte(path), Read, false); status != StatusOK {
			t.Fatalf("%s: %v", path, status)
		}
	}
	for _, path := range []string{"/", "/home", "/home/other/b", "/home/userx", "/home/user/..", "/home/user/../other/b", "/home/user/docs/../../other", "/state/c", "/system", "/bootstrap", "/HOME/USER/a", "/home/USER/a"} {
		if status := namespace.AuthorizePath(owner, []byte(path), Read, false); status != StatusPermissionDenied {
			t.Fatalf("%s: %v", path, status)
		}
	}
	// A name still to be created answers the same way.
	for _, path := range []string{"/home/other/new", "/state/new", "/home/user/../new"} {
		if status := namespace.AuthorizePath(owner, []byte(path), Write, false); status != StatusPermissionDenied {
			t.Fatalf("%s: %v", path, status)
		}
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/user/new"), Write, false); status != StatusOK {
		t.Fatal(status)
	}
}

func TestRootItselfCannotMove(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	for _, path := range []string{"/home/user", "/home/user/", "/home/user/docs/.."} {
		if status := namespace.AuthorizePath(owner, []byte(path), Write, true); status != StatusPermissionDenied {
			t.Fatalf("%s: %v", path, status)
		}
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/user/a"), Write, true); status != StatusOK {
		t.Fatal(status)
	}
}

func TestRootDoesNotLimitOtherSessions(t *testing.T) {
	namespace, _ := rootedNamespace(t)
	if status := namespace.AuthorizePath(Owner(3), []byte("/state/c"), Read, false); status != StatusOK {
		t.Fatal(status)
	}
	if namespace.Rooted(Owner(3)) || !namespace.Rooted(Owner(9)) {
		t.Fatal("Rooted")
	}
}

func TestRootKeepsScopeProtection(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	if status := namespace.Create([]byte("/home/user/keys"), false); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.InstallScope(0, []byte("/home/user/keys"), false, Write); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/user/keys"), Write, false); status != StatusPermissionDenied {
		t.Fatalf("write: %v", status)
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/user/keys"), Write, true); status != StatusPermissionDenied {
		t.Fatalf("move: %v", status)
	}
	// Moving the directory that holds the name would carry the protection away.
	if status := namespace.AuthorizePath(owner, []byte("/home/user/docs"), Write, true); status != StatusOK {
		t.Fatalf("unrelated directory: %v", status)
	}
}

func TestRootedListingStaysInside(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	target := make([]byte, ramfs.MaxDirBatch)
	if _, status := namespace.ReadDirBatchFor(owner, []byte("/home/user"), 0, target); status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.ReadDirBatchFor(owner, []byte("/home"), 0, target); status != StatusPermissionDenied {
		t.Fatal(status)
	}
	if _, _, status := namespace.ReadDirFor(owner, []byte("/state"), 0); status != StatusPermissionDenied {
		t.Fatal(status)
	}
}

func TestRootPathValidation(t *testing.T) {
	for _, path := range []string{"", "/", "home", "/home/", "/home//user", "/home/./user", "/home/../user", "/home/\x00"} {
		if ValidRootPath([]byte(path)) {
			t.Fatalf("accepted %q", path)
		}
		if _, ok := RootedAuthority([]byte(path)); ok {
			t.Fatalf("rooted at %q", path)
		}
	}
	if !ValidRootPath([]byte("/home/user")) {
		t.Fatal("refused canonical root")
	}
	var authority Authority
	authority.RootSize = MaxRootPath + 1
	if authority.Valid() {
		t.Fatal("oversize root")
	}
	authority = Authority{}
	authority.Root[0] = '/'
	if authority.Valid() {
		t.Fatal("bytes past the root's size")
	}
}

func TestRootTravelsInAddSession(t *testing.T) {
	authority, _ := RootedAuthority([]byte("/home/user"))
	message, ok := AddSessionWithAuthorityRequest(5, authority, 1)
	if !ok {
		t.Fatal("encode")
	}
	server, decoded, ok := DecodeAddSessionAuthority(message)
	if !ok || server != 5 || decoded != authority {
		t.Fatalf("decode %v %v %v", server, decoded, ok)
	}
	message.PayloadSize = 2 + MaxScopes + MaxRootPath + 1
	if _, _, ok := DecodeAddSessionAuthority(message); ok {
		t.Fatal("oversize root decoded")
	}
	// A bad root in the message is refused rather than installed.
	message, _ = AddSessionWithAuthorityRequest(5, authority, 1)
	message.Payload[2+MaxScopes+1] = '.'
	message.Payload[2+MaxScopes+2] = '.'
	message.Payload[2+MaxScopes+3] = '/'
	if _, _, ok := DecodeAddSessionAuthority(message); ok {
		t.Fatal("non-canonical root decoded")
	}
}

func TestForgottenRootDoesNotReachTheEntrysNextOwner(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	namespace.ForgetAuthority(owner)
	if status := namespace.SetAuthority(Owner(10), Authority{}); status != StatusOK {
		t.Fatal(status)
	}
	if namespace.Rooted(Owner(10)) {
		t.Fatal("next session inherited a root")
	}
	if status := namespace.AuthorizePath(Owner(10), []byte("/state/c"), Read, false); status != StatusOK {
		t.Fatal(status)
	}
}
