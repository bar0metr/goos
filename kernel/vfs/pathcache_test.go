package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

// A mutation of one name drops only that component; an unchanged prefix stays
// cached, so a later path through it costs no extra backend lookups (ADR 0230).
func TestNarrowInvalidationKeepsUnrelatedPrefixes(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	for _, path := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/one", "/a/b/c/two", "/a/b/c/three"} {
		if namespace.Create([]byte(path), path == "/a" || path == "/a/b" || path == "/a/b/c") != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	// Warm the prefix and one child.
	if _, _, status := namespace.Resolve([]byte("/a/b/c/one")); status != StatusOK {
		t.Fatal(status)
	}
	backend.opens = 0
	// A sibling resolves through the cached prefix with a single lookup.
	if _, _, status := namespace.Resolve([]byte("/a/b/c/two")); status != StatusOK || backend.opens != 1 {
		t.Fatalf("sibling: status %d after %d lookups", status, backend.opens)
	}
	// An unrelated mutation leaves the prefix cached.
	if namespace.Create([]byte("/unrelated"), false) != StatusOK {
		t.Fatal("create unrelated")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b/c/two")); status != StatusOK || backend.opens != 0 {
		t.Fatalf("prefix after unrelated mutation: status %d after %d lookups", status, backend.opens)
	}
	// A mutation inside the cached prefix drops exactly the touched child.
	if namespace.Unlink([]byte("/a/b/c/two")) != StatusOK {
		t.Fatal("unlink two")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b/c/two")); status != StatusNotFound || backend.opens != 1 {
		t.Fatalf("unlinked child: status %d after %d lookups", status, backend.opens)
	}
	// The prefix is still cached: a sibling that was never resolved costs one
	// lookup, not a full walk.
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b/c/three")); status != StatusOK || backend.opens != 1 {
		t.Fatalf("never-resolved sibling: status %d after %d lookups", status, backend.opens)
	}
}

// A rename and a replace drop the source and target names, not the rest of the
// cache. A subtree rename keeps every moved node's identity, so nothing below
// the moved name is dropped (ADR 0230).
func TestRenameAndReplaceInvalidateNarrowly(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	for _, path := range []string{"/x", "/x/deep", "/x/deep/leaf", "/x/deep/leaf2", "/a"} {
		if namespace.Create([]byte(path), path == "/x" || path == "/x/deep") != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	// Warm /x/deep/leaf and /a.
	if _, _, status := namespace.Resolve([]byte("/x/deep/leaf")); status != StatusOK {
		t.Fatal(status)
	}
	if _, _, status := namespace.Resolve([]byte("/a")); status != StatusOK {
		t.Fatal(status)
	}
	// Rename /a to /renamed (a new name): both names change, /x/deep/leaf stays.
	if namespace.Rename([]byte("/a"), []byte("/renamed")) != StatusOK {
		t.Fatal("rename")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/x/deep/leaf")); status != StatusOK || backend.opens != 0 {
		t.Fatalf("cached path after rename: status %d after %d lookups", status, backend.opens)
	}
	if _, _, status := namespace.Resolve([]byte("/a")); status != StatusNotFound {
		t.Fatalf("old rename source still resolves: %d", status)
	}
	// Replace moves /x/deep/leaf onto /x/deep/leaf2; the deep prefix stays.
	if namespace.Replace([]byte("/x/deep/leaf"), []byte("/x/deep/leaf2")) != StatusOK {
		t.Fatal("replace")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/x/deep")); status != StatusOK || backend.opens != 0 {
		t.Fatalf("deep prefix after replace: status %d after %d lookups", status, backend.opens)
	}
	if _, _, status := namespace.Resolve([]byte("/x/deep/leaf")); status != StatusNotFound {
		t.Fatalf("replaced source still resolves: %d", status)
	}
}

// A full flush (a filesystem-service replacement or a restart) forgets every
// component, so a stale name can never survive a backend generation (ADR 0230).
func TestFullInvalidateForgetsAllComponents(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	for _, path := range []string{"/p", "/p/q"} {
		if namespace.Create([]byte(path), true) != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	if _, _, status := namespace.Resolve([]byte("/p/q")); status != StatusOK {
		t.Fatal(status)
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/p/q")); status != StatusOK || backend.opens != 0 {
		t.Fatalf("cached resolve after %d lookups", backend.opens)
	}
	namespace.Invalidate()
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/p/q")); status != StatusOK || backend.opens != 2 {
		t.Fatalf("after Invalidate: status %d after %d lookups", status, backend.opens)
	}
}

// An unmount steps the mount generation, so a later mount of the same slot can
// never be answered from the earlier occupant's cache (ADR 0230).
func TestMountGenerationForgetsComponents(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	root := backend.Init()
	if namespace.Mount([]byte("/"), backend, root) != StatusOK {
		t.Fatal("mount failed")
	}
	if namespace.Create([]byte("/keep"), false) != StatusOK {
		t.Fatal("create")
	}
	if _, _, status := namespace.Resolve([]byte("/keep")); status != StatusOK {
		t.Fatal(status)
	}
	if namespace.UnmountBackend(backend) != StatusOK {
		t.Fatal("unmount")
	}
	if namespace.Mount([]byte("/"), backend, root) != StatusOK {
		t.Fatal("remount")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/keep")); status != StatusOK || backend.opens != 1 {
		t.Fatalf("after remount: status %d after %d lookups", status, backend.opens)
	}
}

// A cached component resolves a path to its node, never to an authorization
// decision: the rooted confinement and scope checks run around resolution, so
// a warm cache cannot widen a session's authority (ADR 0230).
func TestCacheDoesNotWidenRootedAuthority(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/inside"), true) != StatusOK || namespace.Create([]byte("/outside"), true) != StatusOK {
		t.Fatal("create dirs")
	}
	if namespace.Create([]byte("/outside/secret"), false) != StatusOK {
		t.Fatal("create secret")
	}
	// Warm the cache for a path a later session must not reach.
	if _, _, status := namespace.Resolve([]byte("/outside/secret")); status != StatusOK {
		t.Fatal(status)
	}
	root, ok := RootedAuthority([]byte("/inside"))
	if !ok {
		t.Fatal("root")
	}
	if namespace.SetAuthority(testOwner, root) != StatusOK {
		t.Fatal("set authority")
	}
	if namespace.AuthorizePath(testOwner, []byte("/outside/secret"), Read, false) != StatusPermissionDenied {
		t.Fatal("rooted session reached a cached outside path")
	}
	if namespace.AuthorizePath(testOwner, []byte("/inside"), Read, false) != StatusOK {
		t.Fatal("rooted session denied its own root")
	}
}

// The resolution counters prove fewer backend exchanges: a warm prefix is hit
// with no OpenName, and a cold one is a miss (ADR 0230).
func TestResolutionCountersProveFewerBackendExchanges(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount failed")
	}
	for _, path := range []string{"/x", "/x/y", "/x/y/z"} {
		if namespace.Create([]byte(path), path != "/x/y/z") != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	namespace.invalidate()
	backend.opens = 0
	before := namespace.AuthorityCounters()
	if _, _, status := namespace.Resolve([]byte("/x/y/z")); status != StatusOK {
		t.Fatal(status)
	}
	first := namespace.AuthorityCounters()
	if misses := first.ComponentMisses - before.ComponentMisses; misses != 3 || first.ComponentHits-before.ComponentHits != 0 {
		t.Fatalf("first resolve: %d misses %d hits, want 3 misses 0 hits", misses, first.ComponentHits-before.ComponentHits)
	}
	if _, _, status := namespace.Resolve([]byte("/x/y/z")); status != StatusOK {
		t.Fatal(status)
	}
	second := namespace.AuthorityCounters()
	if hits := second.ComponentHits - first.ComponentHits; hits != 3 || second.ComponentMisses-first.ComponentMisses != 0 {
		t.Fatalf("second resolve: %d hits %d misses, want 3 hits 0 misses", hits, second.ComponentMisses-first.ComponentMisses)
	}
	if backend.opens != 3 {
		t.Fatalf("backend lookups = %d, want 3", backend.opens)
	}
}

// fat32Backend reports a FAT32 filesystem type while delegating node work to
// RAMFS, so a test can exercise the mutation-invalidation policy without a
// full FAT32 service. Its OpenName counts lookups like countingBackend does.
type fat32Backend struct {
	*ramfs.FileSystem
	opens int
}

func (backend *fat32Backend) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	backend.opens++
	return backend.FileSystem.OpenName(parent, name)
}

func (fat32Backend) FilesystemInfo() (ramfs.FilesystemInfo, ramfs.Status) {
	return ramfs.FilesystemInfo{Type: ramfs.FilesystemFAT32}, ramfs.StatusOK
}

// OpenIntent with OpenCreate must use the same mutation policy as Create: on a
// stable backend an unrelated create keeps the warm prefix cached (ADR 0230).
func TestOpenIntentCreateKeepsUnrelatedPrefixesWarm(t *testing.T) {
	backend := &countingBackend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount")
	}
	for _, path := range []string{"/a", "/a/b", "/a/b/c"} {
		if namespace.Create([]byte(path), true) != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	if _, _, status := namespace.Resolve([]byte("/a/b/c")); status != StatusOK {
		t.Fatal(status)
	}
	if _, _, status := namespace.OpenIntent(testOwner, []byte("/other"), Read|Write, OpenCreate|OpenExclusive); status != StatusOK {
		t.Fatalf("open-create: %d", status)
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b/c")); status != StatusOK || backend.opens != 0 {
		t.Fatalf("prefix after open-create: status %d after %d lookups", status, backend.opens)
	}
}

// FAT32 names are case-insensitive and aliased and its NodeIDs are
// location-based, so any mutation must flush the whole cache: an unrelated
// create re-walks a previously warm prefix (ADR 0230).
func TestFAT32MutationFullyInvalidates(t *testing.T) {
	backend := &fat32Backend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount")
	}
	for _, path := range []string{"/a", "/a/b"} {
		if namespace.Create([]byte(path), true) != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	if _, _, status := namespace.Resolve([]byte("/a/b")); status != StatusOK {
		t.Fatal(status)
	}
	if namespace.Create([]byte("/unrelated"), false) != StatusOK {
		t.Fatal("create unrelated")
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b")); status != StatusOK || backend.opens != 2 {
		t.Fatalf("prefix after FAT32 mutation: status %d after %d lookups, want 2", status, backend.opens)
	}
}

// OpenIntent create on FAT32 must flush the whole cache as well, so a stale
// case/alias spelling can never survive a mutation.
func TestOpenIntentCreateFullyInvalidatesOnFAT32(t *testing.T) {
	backend := &fat32Backend{FileSystem: &ramfs.FileSystem{}}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, backend.Init()) != StatusOK {
		t.Fatal("mount")
	}
	for _, path := range []string{"/a", "/a/b"} {
		if namespace.Create([]byte(path), true) != StatusOK {
			t.Fatalf("create %s", path)
		}
	}
	if _, _, status := namespace.Resolve([]byte("/a/b")); status != StatusOK {
		t.Fatal(status)
	}
	if _, _, status := namespace.OpenIntent(testOwner, []byte("/other"), Read|Write, OpenCreate|OpenExclusive); status != StatusOK {
		t.Fatalf("open-create: %d", status)
	}
	backend.opens = 0
	if _, _, status := namespace.Resolve([]byte("/a/b")); status != StatusOK || backend.opens != 2 {
		t.Fatalf("prefix after FAT32 open-create: status %d after %d lookups, want 2", status, backend.opens)
	}
}
