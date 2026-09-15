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
	handle, status := namespace.Open([]byte("//etc/./tmp/../motd"), Read|Write)
	if status != StatusOK {
		t.Fatalf("open: %d", status)
	}
	if count, status := namespace.Write(handle, []byte("GoOS")); status != StatusOK || count != 4 {
		t.Fatalf("write: %d %d", count, status)
	}
	if namespace.Seek(handle, 0) != StatusOK {
		t.Fatal("seek failed")
	}
	data := make([]byte, 4)
	if count, status := namespace.Read(handle, data); status != StatusOK || count != 4 || string(data) != "GoOS" {
		t.Fatalf("read: %d %d %q", count, status, data)
	}
	if namespace.Close(handle) != StatusOK {
		t.Fatal("close failed")
	}
	if _, status := namespace.Read(handle, data); status != StatusNotFound {
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
	handle, _ := namespace.Open([]byte("/file"), Read)
	if _, status := namespace.Write(handle, []byte("x")); status != StatusPermissionDenied {
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
