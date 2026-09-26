package vfs

import (
	"goos/kernel/ramfs"
	"testing"
)

func mountedNamespace(t testing.TB) (*Namespace, *ramfs.FileSystem) {
	t.Helper()
	fs := &ramfs.FileSystem{}
	root := fs.Init()
	namespace := &Namespace{}
	if status := namespace.Mount([]byte("/"), fs, root); status != StatusOK {
		t.Fatalf("mount: %d", status)
	}
	return namespace, fs
}

func TestPathAndOpenFileLifecycle(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/etc"), true) != StatusOK || namespace.Create([]byte("/etc/motd"), false) != StatusOK {
		t.Fatal("create failed")
	}
	handle, status := namespace.Open(testOwner, []byte("//etc/./tmp/../motd"), Read|Write)
	if status != StatusOK {
		t.Fatalf("open: %d", status)
	}
	if count, status := namespace.Write(testOwner, handle, []byte("GoOS")); status != StatusOK || count != 4 {
		t.Fatalf("write: %d %d", count, status)
	}
	if namespace.Seek(testOwner, handle, 0) != StatusOK {
		t.Fatal("seek failed")
	}
	data := make([]byte, 4)
	if count, status := namespace.Read(testOwner, handle, data); status != StatusOK || count != 4 || string(data) != "GoOS" {
		t.Fatalf("read: %d %d %q", count, status, data)
	}
	if namespace.Close(testOwner, handle) != StatusOK {
		t.Fatal("close failed")
	}
	if _, status := namespace.Read(testOwner, handle, data); status != StatusNotFound {
		t.Fatal("stale open handle accepted")
	}
}

func TestMountRoutingUsesComponentBoundary(t *testing.T) {
	namespace, rootFS := mountedNamespace(t)
	devFS := &ramfs.FileSystem{}
	devRoot := devFS.Init()
	if namespace.Create([]byte("/dev"), true) != StatusOK || namespace.Create([]byte("/devx"), true) != StatusOK || namespace.Mount([]byte("/dev"), devFS, devRoot) != StatusOK {
		t.Fatal("mount setup failed")
	}
	if namespace.Create([]byte("/dev/console"), false) != StatusOK || namespace.Create([]byte("/devx/file"), false) != StatusOK {
		t.Fatal("routed create failed")
	}
	if _, status := devFS.Open(devRoot, []byte("console")); status != ramfs.StatusOK {
		t.Fatal("/dev did not route to mounted backend")
	}
	devx, _ := rootFS.Open(rootFS.Root(), []byte("devx"))
	if _, status := rootFS.Open(devx, []byte("file")); status != ramfs.StatusOK {
		t.Fatal("/devx incorrectly matched /dev mount")
	}
}

func TestMutationsInvalidateResolutionCache(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/old"), false) != StatusOK {
		t.Fatal("create failed")
	}
	if _, _, status := namespace.Resolve([]byte("/old")); status != StatusOK {
		t.Fatal("cache warmup failed")
	}
	if namespace.Rename([]byte("/old"), []byte("/new")) != StatusOK {
		t.Fatal("rename failed")
	}
	if _, _, status := namespace.Resolve([]byte("/old")); status != StatusNotFound {
		t.Fatal("stale cache survived rename")
	}
	if namespace.Unlink([]byte("/new")) != StatusOK {
		t.Fatal("unlink failed")
	}
	if _, _, status := namespace.Resolve([]byte("/new")); status != StatusNotFound {
		t.Fatal("stale cache survived unlink")
	}
}

func TestRightsAndInvalidPaths(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/file"), false) != StatusOK {
		t.Fatal("create failed")
	}
	handle, _ := namespace.Open(testOwner, []byte("/file"), Read)
	if _, status := namespace.Write(testOwner, handle, []byte("x")); status != StatusPermissionDenied {
		t.Fatal("read-only handle wrote data")
	}
	for _, path := range [][]byte{nil, []byte("relative"), append([]byte{'/'}, make([]byte, MaxPath)...), []byte{'/', 'a', 0}} {
		if _, _, status := namespace.Resolve(path); status != StatusInvalidArgument {
			t.Fatalf("invalid path accepted: %q status=%d", path, status)
		}
	}
}

func TestNamespacesKeepIndependentMountTables(t *testing.T) {
	shared := &ramfs.FileSystem{}
	root := shared.Init()
	first := &Namespace{}
	second := &Namespace{}
	if first.Mount([]byte("/"), shared, root) != StatusOK || second.Mount([]byte("/"), shared, root) != StatusOK {
		t.Fatal("root mounts failed")
	}
	private := &ramfs.FileSystem{}
	privateRoot := private.Init()
	if first.Mount([]byte("/private"), private, privateRoot) != StatusOK {
		t.Fatal("private mount failed")
	}
	if _, _, status := first.Resolve([]byte("/private")); status != StatusOK {
		t.Fatal("private mount missing from owner")
	}
	if _, _, status := second.Resolve([]byte("/private")); status != StatusNotFound {
		t.Fatal("private mount leaked into another namespace")
	}
}

func TestUnmountBackendRejectsLiveFilesAndDetachesAliasesAtomically(t *testing.T) {
	var filesystem ramfs.FileSystem
	root := filesystem.Init()
	file, status := filesystem.Create(root, []byte("file"))
	if status != ramfs.StatusOK || file == 0 {
		t.Fatal("file setup failed")
	}
	var namespace Namespace
	if namespace.Mount([]byte("/"), &filesystem, root) != StatusOK ||
		namespace.Mount([]byte("/alias"), &filesystem, root) != StatusOK {
		t.Fatal("mount setup failed")
	}
	handle, openStatus := namespace.Open(1, []byte("/alias/file"), Read)
	if openStatus != StatusOK {
		t.Fatalf("open = %d", openStatus)
	}
	if got := namespace.UnmountBackend(&filesystem); got != StatusBusy {
		t.Fatalf("unmount with live file = %d", got)
	}
	if _, _, got := namespace.Resolve([]byte("/alias")); got != StatusOK {
		t.Fatalf("busy unmount detached part of backend: %d", got)
	}
	if namespace.Close(1, handle) != StatusOK || namespace.UnmountBackend(&filesystem) != StatusOK {
		t.Fatal("clean unmount failed")
	}
	if _, _, got := namespace.Resolve([]byte("/alias")); got != StatusNotFound {
		t.Fatalf("alias remains after unmount: %d", got)
	}
}

func BenchmarkCachedResolve(b *testing.B) {
	namespace, _ := mountedNamespace(b)
	if namespace.Create([]byte("/cached"), false) != StatusOK {
		b.Fatal("create failed")
	}
	if _, _, status := namespace.Resolve([]byte("/cached")); status != StatusOK {
		b.Fatal("warmup failed")
	}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if _, _, status := namespace.Resolve([]byte("/cached")); status != StatusOK {
			b.Fatal("resolve failed")
		}
	}
}

func BenchmarkUncachedResolve(b *testing.B) {
	namespace, _ := mountedNamespace(b)
	if namespace.Create([]byte("/uncached"), false) != StatusOK {
		b.Fatal("create failed")
	}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		namespace.invalidate()
		if _, _, status := namespace.Resolve([]byte("/uncached")); status != StatusOK {
			b.Fatal("resolve failed")
		}
	}
}

func TestResolveDoesNotAllocate(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/zero-alloc"), false) != StatusOK {
		t.Fatal("create failed")
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, _, status := namespace.Resolve([]byte("//zero-alloc/./")); status != StatusOK {
			t.Fatalf("resolve: %d", status)
		}
	}); allocations != 0 {
		t.Fatalf("Resolve allocated: %v", allocations)
	}
	// The mount phase of a listing runs in the same leak-free kernel heap.
	mounted := &ramfs.FileSystem{}
	if namespace.Mount([]byte("/mounted"), mounted, mounted.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		if _, _, status := namespace.ReadDir([]byte("/"), mountCursor); status != StatusOK {
			t.Fatalf("readdir: %d", status)
		}
	}); allocations != 0 {
		t.Fatalf("ReadDir allocated: %v", allocations)
	}
}

func TestProtocolRoundTripAndReservedFields(t *testing.T) {
	message, ok := EncodeRequest(Request{Operation: OperationWrite, Rights: Write, Handle: 7, Offset: 4, Path: []byte("/tmp/x"), Data: []byte("data")}, 9)
	if !ok {
		t.Fatal("protocol encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || request.Operation != OperationWrite || request.Handle != 7 || request.Offset != 4 || string(request.Path) != "/tmp/x" || string(request.Data) != "data" {
		t.Fatalf("unexpected request: %#v", request)
	}
	message.Payload[28] = 1
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("nonzero reserved field accepted")
	}
}

func TestReplyProtocolRoundTripAndReservedFields(t *testing.T) {
	message, ok := EncodeReply(Reply{Operation: OperationReadDir, Status: StatusOK, Kind: ramfs.KindFile,
		Handle: 7, Size: 4, Cursor: 3, Name: []byte("motd"), Data: []byte("data")}, 11)
	if !ok {
		t.Fatal("reply encode failed")
	}
	reply, ok := DecodeReply(message)
	if !ok || reply.Operation != OperationReadDir || reply.Kind != ramfs.KindFile || reply.Handle != 7 ||
		reply.Size != 4 || reply.Cursor != 3 || string(reply.Name) != "motd" || string(reply.Data) != "data" {
		t.Fatalf("unexpected reply: %#v", reply)
	}
	message.Payload[28] = 1
	if _, ok := DecodeReply(message); ok {
		t.Fatal("nonzero reply reserved field accepted")
	}
	busy, ok := EncodeReply(Reply{Operation: OperationUnlink, Status: StatusBusy}, 12)
	if reply, valid := DecodeReply(busy); !ok || !valid || reply.Status != StatusBusy {
		t.Fatal("the last status does not round-trip")
	}
	if _, ok := EncodeReply(Reply{Operation: OperationUnlink, Status: lastStatus + 1}, 13); ok {
		t.Fatal("unknown status encoded")
	}
}

func TestSyncRequestIsDistinctFromAddSession(t *testing.T) {
	sync, ok := SyncRequest(4)
	if !ok || !IsSync(sync) {
		t.Fatal("sync request not recognized")
	}
	if _, ok := DecodeAddSession(sync); ok {
		t.Fatal("sync decoded as add-session")
	}
	add, _ := AddSessionRequest(7, 5)
	if IsSync(add) {
		t.Fatal("add-session recognized as sync")
	}
	reply, ok := AddSessionReply(StatusOK, 4)
	if status, valid := DecodeAddSessionReply(reply); !ok || !valid || status != StatusOK {
		t.Fatal("sync reply does not decode")
	}
}

// listing walks a directory the way a client does, through opaque cursors.
func listing(t testing.TB, namespace *Namespace, path string) []string {
	t.Helper()
	var names []string
	cursor := 0
	for len(names) <= MaxMounts+MaxCache {
		entry, next, status := namespace.ReadDir([]byte(path), cursor)
		if status == StatusNotFound {
			return names
		}
		if status != StatusOK {
			t.Fatalf("readdir %s: %d", path, status)
		}
		names = append(names, string(entry.Name[:entry.Size]))
		if next <= cursor {
			t.Fatalf("readdir %s: cursor did not advance past %d", path, cursor)
		}
		cursor = next
	}
	t.Fatalf("readdir %s did not terminate", path)
	return nil
}

func contains(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}

func TestMountsAreListedInTheirParentDirectory(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	if namespace.Create([]byte("/system"), true) != StatusOK || namespace.Create([]byte("/tmp"), true) != StatusOK {
		t.Fatal("create failed")
	}
	persistent := &ramfs.FileSystem{}
	if namespace.Mount([]byte("/persist"), persistent, persistent.Init()) != StatusOK {
		t.Fatal("persistent mount failed")
	}
	image := &ramfs.FileSystem{}
	imageRoot := image.Init()
	if _, status := image.Mkdir(imageRoot, []byte("bin")); status != ramfs.StatusOK {
		t.Fatalf("image mkdir: %d", status)
	}
	if namespace.Mount([]byte("/system/image"), image, imageRoot) != StatusOK {
		t.Fatal("image mount failed")
	}

	root := listing(t, namespace, "/")
	for _, wanted := range []string{"system", "tmp", "persist"} {
		if !contains(root, wanted) {
			t.Fatalf("root listing %v is missing %q", root, wanted)
		}
	}
	if contains(root, "image") {
		t.Fatalf("root listing %v leaked a nested mount", root)
	}
	if system := listing(t, namespace, "/system"); !contains(system, "image") {
		t.Fatalf("/system listing %v is missing its mount", system)
	}
	if nested := listing(t, namespace, "/system/image"); !contains(nested, "bin") {
		t.Fatalf("/system/image listing %v is missing backend content", nested)
	}

	// A mount is a directory to its parent and resolves into its own backend.
	stat, status := namespace.Stat([]byte("/persist"))
	if status != StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat mount point: %d %d", status, stat.Kind)
	}
	if fs.Root() == 0 {
		t.Fatal("owning backend lost its root")
	}
}

func TestBackendEntryShadowsItsMountPointName(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	if _, status := fs.Mkdir(fs.Root(), []byte("persist")); status != ramfs.StatusOK {
		t.Fatalf("mkdir: %d", status)
	}
	persistent := &ramfs.FileSystem{}
	if namespace.Mount([]byte("/persist"), persistent, persistent.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	names := listing(t, namespace, "/")
	seen := 0
	for _, name := range names {
		if name == "persist" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("root listing %v names the mount point %d times", names, seen)
	}
}

func TestReadDirRejectsNegativeAndInvalidCursors(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if _, _, status := namespace.ReadDir([]byte("/"), -1); status != StatusInvalidArgument {
		t.Fatalf("negative cursor accepted: %d", status)
	}
	if _, _, status := namespace.ReadDir([]byte("relative"), 0); status != StatusInvalidArgument {
		t.Fatalf("relative path accepted: %d", status)
	}
	if _, _, status := namespace.ReadDir([]byte("/"), mountCursor+MaxMounts); status != StatusNotFound {
		t.Fatalf("exhausted mount cursor: %d", status)
	}
}

func TestUnlinkRefusesToOrphanAMount(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	if namespace.Create([]byte("/system"), true) != StatusOK {
		t.Fatal("create failed")
	}
	volume := &ramfs.FileSystem{}
	volumeRoot := volume.Init()
	if namespace.Mount([]byte("/persist"), volume, volumeRoot) != StatusOK {
		t.Fatal("volume mount failed")
	}
	home, status := volume.Mkdir(volumeRoot, []byte("home"))
	if status != ramfs.StatusOK {
		t.Fatalf("mkdir home: %d", status)
	}
	if namespace.Mount([]byte("/home"), volume, home) != StatusOK {
		t.Fatal("home mount failed")
	}
	nested := &ramfs.FileSystem{}
	if namespace.Mount([]byte("/system/image"), nested, nested.Init()) != StatusOK {
		t.Fatal("nested mount failed")
	}

	// The same directory under its other name must stay protected.
	if status := namespace.Unlink([]byte("/persist/home")); status != StatusBusy {
		t.Fatalf("aliased mount root removed: %d", status)
	}
	if status := namespace.Unlink([]byte("/home")); status != StatusBusy {
		t.Fatalf("mount root removed: %d", status)
	}
	if status := namespace.Unlink([]byte("/system")); status != StatusBusy {
		t.Fatalf("directory carrying a mount removed: %d", status)
	}
	if _, _, status := namespace.Resolve([]byte("/home")); status != StatusOK {
		t.Fatalf("mount lost after refused removals: %d", status)
	}

	// An ordinary entry is still removable.
	if namespace.Create([]byte("/persist/note"), false) != StatusOK {
		t.Fatal("create note failed")
	}
	if status := namespace.Unlink([]byte("/persist/note")); status != StatusOK {
		t.Fatalf("ordinary unlink refused: %d", status)
	}
	if fs.Root() == 0 {
		t.Fatal("owning backend lost its root")
	}
}

func TestMapStatusKeepsTheReason(t *testing.T) {
	for _, test := range []struct {
		backend ramfs.Status
		want    Status
	}{
		{ramfs.StatusNotDirectory, StatusNotDirectory},
		{ramfs.StatusIsDirectory, StatusIsDirectory},
		{ramfs.StatusNotEmpty, StatusNotEmpty},
		{ramfs.StatusInvalidArgument, StatusInvalidArgument},
		{ramfs.StatusIOError, StatusBackendError},
		{ramfs.StatusNoSpace, StatusNoSpace},
		{ramfs.StatusCorrupt, StatusCorrupt},
		{ramfs.StatusTimeout, StatusTimeout},
		{ramfs.StatusPeerDied, StatusPeerDied},
		{ramfs.StatusUnsupported, StatusUnsupported},
	} {
		if got := mapStatus(test.backend); got != test.want {
			t.Fatalf("mapStatus(%d) = %d, want %d", test.backend, got, test.want)
		}
	}
}

const testOwner = Owner(1)

// A handle belongs to the session that opened it: another session is told it
// does not exist, and a session's files close when it goes.
func TestOpenFilesBelongToTheirOwner(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/file"), false) != StatusOK {
		t.Fatal("create failed")
	}
	mine, status := namespace.Open(1, []byte("/file"), Read|Write)
	if status != StatusOK {
		t.Fatalf("open = %d", status)
	}
	data := make([]byte, 4)
	if _, status := namespace.Read(2, mine, data); status != StatusNotFound {
		t.Fatalf("foreign read = %d", status)
	}
	if _, status := namespace.Write(2, mine, []byte("x")); status != StatusNotFound {
		t.Fatalf("foreign write = %d", status)
	}
	if namespace.Seek(2, mine, 0) != StatusNotFound || namespace.Close(2, mine) != StatusNotFound {
		t.Fatal("foreign seek or close accepted")
	}
	if _, status := namespace.Open(0, []byte("/file"), Read); status != StatusInvalidArgument {
		t.Fatalf("open without owner = %d", status)
	}
	other, _ := namespace.Open(2, []byte("/file"), Read)
	if closed := namespace.CloseOwner(1); closed != 1 {
		t.Fatalf("closed %d files", closed)
	}
	if _, status := namespace.Read(1, mine, data); status != StatusNotFound {
		t.Fatal("a file of a closed owner is still open")
	}
	if _, status := namespace.Read(2, other, data); status != StatusOK {
		t.Fatalf("another owner's file closed with it: %d", status)
	}
}

func TestOneOwnerCannotTakeTheWholeTable(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	namespace.Create([]byte("/file"), false)
	for index := 0; index < MaxOpenPerOwner; index++ {
		if _, status := namespace.Open(1, []byte("/file"), Read); status != StatusOK {
			t.Fatalf("open %d = %d", index, status)
		}
	}
	if _, status := namespace.Open(1, []byte("/file"), Read); status != StatusResourceExhausted {
		t.Fatalf("open past the share = %d", status)
	}
	if _, status := namespace.Open(2, []byte("/file"), Read); status != StatusOK {
		t.Fatalf("another owner refused: %d", status)
	}
}
