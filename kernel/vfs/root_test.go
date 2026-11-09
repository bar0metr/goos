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

// The root limits the paths a session names. An open file is a capability to
// its object and keeps working wherever another session moves that object.
func TestRetainedHandleSurvivesRenameOutsideRoot(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	file, status := namespace.Open(owner, []byte("/home/user/a"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if count, status := namespace.Write(owner, file, []byte("kept")); status != StatusOK || count != 4 {
		t.Fatalf("write %d %v", count, status)
	}
	before, status := namespace.StatOpen(owner, file)
	if status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.AuthorizePath(Owner(3), []byte("/home/user/a"), Write, true); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Rename([]byte("/home/user/a"), []byte("/home/other/moved")); status != StatusOK {
		t.Fatal(status)
	}
	// The handle still names the same object, for reading and for writing.
	after, status := namespace.StatOpen(owner, file)
	if status != StatusOK || after.ID != before.ID {
		t.Fatalf("identity %v -> %v, %v", before.ID, after.ID, status)
	}
	if status := namespace.Seek(owner, file, 0); status != StatusOK {
		t.Fatal(status)
	}
	target := make([]byte, 8)
	if count, status := namespace.Read(owner, file, target); status != StatusOK || string(target[:count]) != "kept" {
		t.Fatalf("read %q %v", target[:count], status)
	}
	if count, status := namespace.Write(owner, file, []byte("+")); status != StatusOK || count != 1 {
		t.Fatalf("write after move %d %v", count, status)
	}
	external := []byte("/home/other/moved")
	if stat, status := namespace.Stat(external); status != StatusOK || stat.ID != before.ID || stat.Size != 5 {
		t.Fatalf("other session sees %+v %v", stat, status)
	}
	// Nothing about the moved object can be reached again by path.
	for _, path := range []string{"/home/other/moved", "/home/other", "/home/user/../other/moved"} {
		if status := namespace.AuthorizePath(owner, []byte(path), Read, false); status != StatusPermissionDenied {
			t.Fatalf("%s: %v", path, status)
		}
	}
	if _, _, status := namespace.ReadDirFor(owner, []byte("/home/other"), 0); status != StatusPermissionDenied {
		t.Fatalf("listing: %v", status)
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/other/moved"), Write, true); status != StatusPermissionDenied {
		t.Fatalf("moving: %v", status)
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/other/back"), Write, true); status != StatusPermissionDenied {
		t.Fatalf("moving target: %v", status)
	}
}

// A retained handle keeps no authority a scope later takes away.
func TestRetainedHandleDoesNotBypassScope(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	file, status := namespace.Open(owner, []byte("/home/user/a"), Read|Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.InstallScope(0, []byte("/home/user/a"), false, Read|Write); status != StatusOK {
		t.Fatal(status)
	}
	if _, status := namespace.Read(owner, file, make([]byte, 1)); status == StatusOK {
		t.Fatal("handle read a file a scope has since protected")
	}
}

// The root confines paths, not backends: a mount below it stays reachable.
func TestRootDoesNotExcludeMountBelowIt(t *testing.T) {
	namespace, owner := rootedNamespace(t)
	other := &ramfs.FileSystem{}
	otherRoot := other.Init()
	if _, status := other.CreateName(otherRoot, mustName(t, "file")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Create([]byte("/home/user/mount"), true); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Mount([]byte("/home/user/mount"), other, otherRoot); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.AuthorizePath(owner, []byte("/home/user/mount/file"), Read, false); status != StatusOK {
		t.Fatal(status)
	}
	file, status := namespace.Open(owner, []byte("/home/user/mount/file"), Read)
	if status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.Close(owner, file); status != StatusOK {
		t.Fatal(status)
	}
}

func TestAddSessionWireVersionAndShape(t *testing.T) {
	if ControlVersion != 11 {
		t.Fatalf("control version %d", ControlVersion)
	}
	plain := Authority{Confined: true}
	plain.Grants[2] = Read
	message, ok := AddSessionWithAuthorityRequest(5, plain, 1)
	if !ok || message.Version != ControlVersion {
		t.Fatalf("encode %+v", message)
	}
	if _, decoded, ok := DecodeAddSessionAuthority(message); !ok || decoded != plain {
		t.Fatalf("rootless round trip %+v %v", decoded, ok)
	}
	long := make([]byte, MaxRootPath)
	long[0] = '/'
	for index := 1; index < len(long); index++ {
		long[index] = 'r'
	}
	rooted, ok := RootedAuthority(long)
	if !ok {
		t.Fatal("longest root refused")
	}
	message, ok = AddSessionWithAuthorityRequest(5, rooted, 1)
	if !ok {
		t.Fatal("encode longest root")
	}
	if _, decoded, ok := DecodeAddSessionAuthority(message); !ok || decoded != rooted {
		t.Fatalf("rooted round trip %+v %v", decoded, ok)
	}
	for _, version := range []uint16{ControlVersion - 1, ControlVersion + 1, 0} {
		old := message
		old.Version = version
		if _, _, ok := DecodeAddSessionAuthority(old); ok {
			t.Fatalf("version %d decoded", version)
		}
	}
	// A shorter payload than the grants, an over-long root, and bytes trailing
	// a root that make it something else all fail.
	for _, size := range []uint16{0, 2, 1 + MaxScopes, 2 + MaxScopes + MaxRootPath + 1} {
		short := message
		short.PayloadSize = size
		if _, _, ok := DecodeAddSessionAuthority(short); ok {
			t.Fatalf("payload size %d decoded", size)
		}
	}
	short, _ := RootedAuthority([]byte("/home/user"))
	for _, trailing := range []string{"/", "\x00", "/..", "//x"} {
		bad, _ := AddSessionWithAuthorityRequest(5, short, 1)
		size := bad.PayloadSize
		bad.PayloadSize += uint16(copy(bad.Payload[size:], trailing))
		if _, _, ok := DecodeAddSessionAuthority(bad); ok {
			t.Fatalf("trailing %q decoded", trailing)
		}
	}
}

func mustName(t *testing.T, value string) ramfs.Name {
	t.Helper()
	name, ok := ramfs.MakeName([]byte(value))
	if !ok {
		t.Fatal("name")
	}
	return name
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
