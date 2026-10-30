package vfs

import (
	"fmt"
	"goos/kernel/ramfs"
	"testing"
)

const listingOwner Owner = 7

func createAll(t *testing.T, namespace *Namespace, directory bool, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if status := namespace.Create([]byte(path), directory); status != StatusOK {
			t.Fatalf("create %s: %d", path, status)
		}
	}
}

func installAll(t *testing.T, namespace *Namespace, subtree bool, first uint8, paths ...string) {
	t.Helper()
	for index, path := range paths {
		if status := namespace.InstallScope(first+uint8(index), []byte(path), subtree, Read|Write); status != StatusOK {
			t.Fatalf("scope %s: %d", path, status)
		}
	}
}

func visibleNames(t *testing.T, namespace *Namespace, owner Owner, path string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for cursor := 0; ; {
		entry, next, status := namespace.ReadDirFor(owner, []byte(path), cursor)
		if status == StatusNotFound {
			return names
		}
		if status != StatusOK {
			t.Fatalf("list %s at %d: %d", path, cursor, status)
		}
		names[string(entry.Name[:entry.Size])] = true
		cursor = next
	}
}

func wantListing(t *testing.T, namespace *Namespace, owner Owner, path string, want ...string) {
	t.Helper()
	names := visibleNames(t, namespace, owner, path)
	if len(names) != len(want) {
		t.Fatalf("list %s = %v, want %v", path, names, want)
	}
	for _, name := range want {
		if !names[name] {
			t.Fatalf("list %s = %v, want %v", path, names, want)
		}
	}
}

// A listing of unrelated entries pays for one walk of the protected subtree,
// and a repeated listing pays for none.
func TestListingAuthorityWorkIsBoundedByDirectory(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	createAll(t, namespace, true, "/apps", "/state", "/state/security", "/state/security/ssh", "/home")
	createAll(t, namespace, false, "/state/security/ssh/host", "/state/security/ssh/host.pub", "/home/authorized_keys")
	_, apps, _ := namespace.Resolve([]byte("/apps"))
	want := []string{}
	for index := 0; index < 150; index++ {
		name := fmt.Sprintf("tool%d", index)
		if _, status := fs.Create(apps, []byte(name)); status != ramfs.StatusOK {
			t.Fatal(status)
		}
		want = append(want, name)
	}
	installAll(t, namespace, true, 0, "/state/security/ssh")
	installAll(t, namespace, false, 1, "/home/authorized_keys")

	before := namespace.AuthorityCounters()
	wantListing(t, namespace, listingOwner, "/apps", want...)
	first := namespace.AuthorityCounters()
	if requests := first.ReadDirRequests - before.ReadDirRequests; requests != 151 {
		t.Fatalf("requests = %d", requests)
	}
	if walks := first.ScopeContains - before.ScopeContains; walks != 1 {
		t.Fatalf("subtree walks = %d, want one for the directory", walks)
	}
	if reads := first.ScopeBackendReadDir - before.ScopeBackendReadDir; reads > 4 {
		t.Fatalf("subtree directory reads = %d", reads)
	}
	if opens := first.ScopeOpenName - before.ScopeOpenName; opens != 1 {
		t.Fatalf("exact lookups = %d, want one", opens)
	}
	wantListing(t, namespace, listingOwner, "/apps", want...)
	second := namespace.AuthorityCounters()
	if second.ScopeContains != first.ScopeContains || second.ScopeBackendReadDir != first.ScopeBackendReadDir || second.ScopeOpenName != first.ScopeOpenName {
		t.Fatalf("repeated listing did backend work: %+v then %+v", first, second)
	}
}

// Each entry's visibility matches a full identity check of that entry, for
// ordinary and confined owners, through the real path and a mounted alias.
func TestListingMatchesFullScopeCheck(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	createAll(t, namespace, true, "/mixed", "/mixed/private", "/mixed/private/deep", "/mixed/open", "/grant")
	createAll(t, namespace, false, "/mixed/secret", "/mixed/public", "/mixed/private/key", "/mixed/private/deep/key", "/grant/file")
	_, private, _ := namespace.Resolve([]byte("/mixed/private"))
	if namespace.Mount([]byte("/mixed/alias"), fs, private) != StatusOK || namespace.Mount([]byte("/alias"), fs, private) != StatusOK {
		t.Fatal("mount")
	}
	installAll(t, namespace, true, 0, "/mixed/private", "/grant")
	installAll(t, namespace, false, 2, "/mixed/secret")
	confined := Authority{Confined: true}
	confined.Grants[0] = Read
	if namespace.SetAuthority(listingOwner+1, confined) != StatusOK {
		t.Fatal("authority")
	}
	granted := Authority{}
	granted.Grants[2] = Read
	if namespace.SetAuthority(listingOwner+2, granted) != StatusOK {
		t.Fatal("authority")
	}
	wantListing(t, namespace, listingOwner, "/mixed", "public", "open")
	wantListing(t, namespace, listingOwner+2, "/mixed", "public", "open", "secret")
	wantListing(t, namespace, listingOwner+1, "/mixed/private", "key", "deep")
	wantListing(t, namespace, listingOwner+1, "/alias", "key", "deep")
	wantListing(t, namespace, listingOwner+1, "/mixed/private/deep", "key")
	withPrivate := Authority{}
	withPrivate.Grants[0] = Read
	if namespace.SetAuthority(listingOwner+3, withPrivate) != StatusOK {
		t.Fatal("authority")
	}
	wantListing(t, namespace, listingOwner+3, "/mixed", "public", "open", "private", "alias")
	for _, path := range []string{"/", "/mixed", "/mixed/open", "/mixed/private", "/alias", "/grant"} {
		for _, owner := range []Owner{listingOwner, listingOwner + 1, listingOwner + 2} {
			if namespace.AuthorizePath(owner, []byte(path), Read, false) != StatusOK {
				continue
			}
			for name := range visibleNames(t, namespace, owner, path) {
				child := path + "/" + name
				if path == "/" {
					child = "/" + name
				}
				mount, node, status := namespace.Resolve([]byte(child))
				if status != StatusOK {
					t.Fatalf("resolve %s: %d", child, status)
				}
				namespace.invalidate()
				mask, status := namespace.nodeScopes(namespace.mounts[mount].backend, node)
				if status != StatusOK || !namespace.permits(owner, mask, Read) {
					t.Fatalf("owner %d listed %s, full check mask=%b status=%d", owner, child, mask, status)
				}
			}
		}
	}
}

func TestListingFollowsNamespaceChanges(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	createAll(t, namespace, true, "/private", "/open", "/moved")
	createAll(t, namespace, false, "/open/file", "/open/other")
	installAll(t, namespace, true, 0, "/private")
	installAll(t, namespace, false, 1, "/open/secret")
	wantListing(t, namespace, listingOwner, "/open", "file", "other")

	// An exact name that was missing starts to hide what takes it.
	createAll(t, namespace, false, "/open/secret")
	wantListing(t, namespace, listingOwner, "/open", "file", "other")
	// Moving a name into and out of the subtree moves its visibility.
	if status := namespace.Rename([]byte("/open/file"), []byte("/private/file")); status != StatusOK {
		t.Fatal(status)
	}
	wantListing(t, namespace, listingOwner, "/open", "other")
	if status := namespace.Rename([]byte("/private/file"), []byte("/open/file")); status != StatusOK {
		t.Fatal(status)
	}
	wantListing(t, namespace, listingOwner, "/open", "file", "other")
	// A moved directory stays hidden by its new parent's subtree.
	if status := namespace.Rename([]byte("/moved"), []byte("/private/moved")); status != StatusOK {
		t.Fatal(status)
	}
	if status := namespace.AuthorizePath(listingOwner, []byte("/private/moved"), Read, false); status != StatusPermissionDenied {
		t.Fatalf("moved directory=%d", status)
	}
	// A renamed name that takes the exact scope is hidden; its old holder is gone.
	if status := namespace.Unlink([]byte("/open/secret")); status != StatusOK {
		t.Fatal(status)
	}
	wantListing(t, namespace, listingOwner, "/open", "file", "other")
	if status := namespace.Rename([]byte("/open/other"), []byte("/open/secret")); status != StatusOK {
		t.Fatal(status)
	}
	wantListing(t, namespace, listingOwner, "/open", "file")
	// A scope installed after a listing applies to the next one.
	installAll(t, namespace, false, 2, "/open/file")
	wantListing(t, namespace, listingOwner, "/open")
	// A mount added after a listing is checked as an alias of its node.
	_, private, _ := namespace.Resolve([]byte("/private"))
	if namespace.Mount([]byte("/open/alias"), fs, private) != StatusOK {
		t.Fatal("mount")
	}
	wantListing(t, namespace, listingOwner, "/open")
	if namespace.UnmountPath([]byte("/open/alias")) != StatusOK {
		t.Fatal("unmount")
	}
	wantListing(t, namespace, listingOwner, "/open")
}

type failingCreateBackend struct{ *ramfs.FileSystem }

func (backend failingCreateBackend) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	backend.FileSystem.CreateName(parent, name)
	return 0, ramfs.StatusIOError
}

// A backend may leave the name behind when its final durability step fails, so
// a failed create must not leave a remembered missing exact name.
func TestFailedCreateDropsRememberedExactName(t *testing.T) {
	fs := &ramfs.FileSystem{}
	root := fs.Init()
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), failingCreateBackend{fs}, root) != StatusOK {
		t.Fatal("mount")
	}
	if _, status := fs.Mkdir(root, []byte("open")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	installAll(t, namespace, false, 0, "/open/secret")
	wantListing(t, namespace, listingOwner, "/open")
	if status := namespace.Create([]byte("/open/secret"), false); status == StatusOK {
		t.Fatal("create succeeded")
	}
	wantListing(t, namespace, listingOwner, "/open")
	if _, status := namespace.Open(listingOwner, []byte("/open/secret"), Read); status != StatusPermissionDenied {
		t.Fatalf("open = %d", status)
	}
}
