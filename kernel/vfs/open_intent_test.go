package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

type atomicOpenBackend struct {
	*ramfs.FileSystem
	root           ramfs.NodeID
	replaceCreate  bool
	replaceAcquire bool
	failCreate     bool
	failAcquire    bool
	failTruncate   bool
	creates        int
	opens          int
	acquires       int
	releases       int
	truncates      int
}

func (backend *atomicOpenBackend) OpenName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	backend.opens++
	return backend.FileSystem.OpenName(parent, name)
}

func (backend *atomicOpenBackend) CreateName(parent ramfs.NodeID, name ramfs.Name) (ramfs.NodeID, ramfs.Status) {
	backend.creates++
	if backend.failCreate {
		return 0, ramfs.StatusIOError
	}
	created, status := backend.FileSystem.CreateName(parent, name)
	if status != ramfs.StatusOK || !backend.replaceCreate {
		return created, status
	}
	backend.replaceCreate = false
	old, _ := ramfs.MakeName([]byte("created-object"))
	if backend.FileSystem.RenameName(created, parent, old) != ramfs.StatusOK {
		return 0, ramfs.StatusIOError
	}
	replacement, replacementStatus := backend.FileSystem.CreateName(parent, name)
	if replacementStatus != ramfs.StatusOK {
		return 0, replacementStatus
	}
	backend.FileSystem.Write(replacement, 0, []byte("replacement"))
	return created, ramfs.StatusOK
}

func (backend *atomicOpenBackend) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	backend.acquires++
	if backend.failAcquire {
		return 0, ramfs.StatusIOError
	}
	if backend.replaceAcquire {
		backend.replaceAcquire = false
		old, _ := ramfs.MakeName([]byte("truncated-object"))
		if backend.FileSystem.RenameName(id, backend.root, old) != ramfs.StatusOK {
			return 0, ramfs.StatusIOError
		}
		name, _ := ramfs.MakeName([]byte("x"))
		replacement, status := backend.FileSystem.CreateName(backend.root, name)
		if status != ramfs.StatusOK {
			return 0, status
		}
		backend.FileSystem.Write(replacement, 0, []byte("replacement"))
	}
	return id, ramfs.StatusOK
}

func (backend *atomicOpenBackend) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	backend.releases++
	return id, ramfs.StatusOK
}

func (backend *atomicOpenBackend) Truncate(id ramfs.NodeID, size uint64) ramfs.Status {
	backend.truncates++
	if backend.failTruncate {
		return ramfs.StatusIOError
	}
	return backend.FileSystem.Truncate(id, size)
}

func atomicNamespace(t *testing.T) (*Namespace, *atomicOpenBackend) {
	t.Helper()
	filesystem := &ramfs.FileSystem{}
	root := filesystem.Init()
	backend := &atomicOpenBackend{FileSystem: filesystem, root: root}
	namespace := &Namespace{}
	if status := namespace.Mount([]byte("/"), backend, root); status != StatusOK {
		t.Fatal(status)
	}
	return namespace, backend
}

func TestOpenIntentTruncatesTheAcquiredObject(t *testing.T) {
	namespace, backend := atomicNamespace(t)
	name, _ := ramfs.MakeName([]byte("x"))
	original, status := backend.FileSystem.CreateName(backend.root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	backend.FileSystem.Write(original, 0, []byte("original"))
	backend.replaceAcquire = true
	handle, kind, openStatus := namespace.OpenIntent(1, []byte("/x"), Write, OpenTruncate)
	if openStatus != StatusOK || kind != ramfs.KindFile {
		t.Fatalf("open: handle=%x kind=%d status=%d", handle, kind, openStatus)
	}
	opened, statStatus := namespace.StatOpen(1, handle)
	replacement, replacementStatus := backend.FileSystem.OpenName(backend.root, name)
	path, pathStatus := backend.FileSystem.Stat(replacement)
	if statStatus != StatusOK || opened.ID != original || opened.Size != 0 {
		t.Fatalf("opened %+v status=%d", opened, statStatus)
	}
	if replacementStatus != ramfs.StatusOK || pathStatus != ramfs.StatusOK || path.ID == original || path.Size != uint64(len("replacement")) {
		t.Fatalf("replacement %+v status=%d", path, pathStatus)
	}
	if backend.acquires != 1 || backend.truncates != 1 {
		t.Fatalf("acquires=%d truncates=%d", backend.acquires, backend.truncates)
	}
}

func TestOpenIntentKeepsTheNodeReturnedByCreate(t *testing.T) {
	namespace, backend := atomicNamespace(t)
	backend.replaceCreate = true
	before := backend.opens
	handle, kind, status := namespace.OpenIntent(1, []byte("/x"), Read|Write, OpenCreate)
	if status != StatusOK || kind != ramfs.KindFile {
		t.Fatalf("open: handle=%x kind=%d status=%d", handle, kind, status)
	}
	opened, openedStatus := namespace.StatOpen(1, handle)
	name, _ := ramfs.MakeName([]byte("x"))
	replacement, replacementStatus := backend.FileSystem.OpenName(backend.root, name)
	path, pathStatus := backend.FileSystem.Stat(replacement)
	if openedStatus != StatusOK || opened.Size != 0 || replacementStatus != ramfs.StatusOK || pathStatus != ramfs.StatusOK || opened.ID == path.ID || path.Size != uint64(len("replacement")) {
		t.Fatalf("opened=%+v/%d path=%+v/%d", opened, openedStatus, path, pathStatus)
	}
	if opens := backend.opens - before; opens != 1 {
		t.Fatalf("OpenName calls in atomic open=%d", opens)
	}
}

func TestOpenIntentFailuresDoNotInstallOrRetargetAHandle(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		namespace, backend := atomicNamespace(t)
		backend.failCreate = true
		if handle, _, status := namespace.OpenIntent(1, []byte("/new"), Write, OpenCreate); status != StatusBackendError || handle != 0 {
			t.Fatalf("handle=%x status=%d", handle, status)
		}
		if backend.creates != 1 || backend.acquires != 0 || namespace.OpenBy(1) != 0 {
			t.Fatalf("creates=%d acquires=%d live=%d", backend.creates, backend.acquires, namespace.OpenBy(1))
		}
	})
	t.Run("acquire after create", func(t *testing.T) {
		namespace, backend := atomicNamespace(t)
		backend.failAcquire = true
		before := backend.opens
		if handle, _, status := namespace.OpenIntent(1, []byte("/new"), Write, OpenCreate); status != StatusBackendError || handle != 0 {
			t.Fatalf("handle=%x status=%d", handle, status)
		}
		if backend.opens-before != 1 || namespace.OpenBy(1) != 0 {
			t.Fatalf("opens=%d live=%d", backend.opens-before, namespace.OpenBy(1))
		}
	})
	t.Run("truncate", func(t *testing.T) {
		namespace, backend := atomicNamespace(t)
		name, _ := ramfs.MakeName([]byte("x"))
		backend.FileSystem.CreateName(backend.root, name)
		backend.failTruncate = true
		if handle, _, status := namespace.OpenIntent(1, []byte("/x"), Write, OpenTruncate); status != StatusBackendError || handle != 0 {
			t.Fatalf("handle=%x status=%d", handle, status)
		}
		if backend.releases != 1 || namespace.OpenBy(1) != 0 {
			t.Fatalf("releases=%d live=%d", backend.releases, namespace.OpenBy(1))
		}
	})
	t.Run("open table", func(t *testing.T) {
		namespace, backend := atomicNamespace(t)
		name, _ := ramfs.MakeName([]byte("seed"))
		backend.FileSystem.CreateName(backend.root, name)
		for index := 0; index < MaxOpenPerOwner; index++ {
			if _, status := namespace.Open(1, []byte("/seed"), Read); status != StatusOK {
				t.Fatal(status)
			}
		}
		before := backend.creates
		if handle, _, status := namespace.OpenIntent(1, []byte("/new"), Write, OpenCreate); status != StatusResourceExhausted || handle != 0 {
			t.Fatalf("handle=%x status=%d", handle, status)
		}
		if backend.creates != before {
			t.Fatal("created before detecting open-table exhaustion")
		}
		if handle, _, status := namespace.OpenIntent(1, []byte("/seed"), Read, OpenTruncate); status != StatusResourceExhausted || handle != 0 {
			t.Fatalf("handle=%x status=%d", handle, status)
		}
		if backend.truncates != 0 {
			t.Fatalf("truncates=%d", backend.truncates)
		}
	})
}

func TestOpenIntentAliasMountsKeepBackendIdentity(t *testing.T) {
	namespace, backend := atomicNamespace(t)
	if namespace.Create([]byte("/home"), true) != StatusOK || namespace.Create([]byte("/persist"), true) != StatusOK || namespace.Create([]byte("/persist/home"), true) != StatusOK {
		t.Fatal("mount points")
	}
	if namespace.Mount([]byte("/home"), backend, backend.root) != StatusOK || namespace.Mount([]byte("/persist/home"), backend, backend.root) != StatusOK {
		t.Fatal("alias mounts")
	}
	name, _ := ramfs.MakeName([]byte("x"))
	node, _ := backend.FileSystem.CreateName(backend.root, name)
	first, _, firstStatus := namespace.OpenIntent(1, []byte("/home/x"), Read, 0)
	second, _, secondStatus := namespace.OpenIntent(1, []byte("/persist/home/x"), Read, 0)
	firstStat, _ := namespace.StatOpen(1, first)
	secondStat, _ := namespace.StatOpen(1, second)
	if firstStatus != StatusOK || secondStatus != StatusOK || firstStat.ID != node || secondStat.ID != node {
		t.Fatalf("first=%+v/%d second=%+v/%d", firstStat, firstStatus, secondStat, secondStatus)
	}
}

func TestOpenIntentDirectoryAndKindRules(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/dir"), true) != StatusOK || namespace.Create([]byte("/file"), false) != StatusOK {
		t.Fatal("setup")
	}
	if handle, kind, status := namespace.OpenIntent(1, []byte("/dir"), Read, OpenDirectory); status != StatusOK || kind != ramfs.KindDirectory || handle != 0 {
		t.Fatalf("directory: handle=%x kind=%d status=%d", handle, kind, status)
	}
	for _, test := range []struct {
		path   string
		rights Rights
		flags  OpenFlags
		status Status
	}{
		{"/dir", Write, 0, StatusIsDirectory},
		{"/dir", Read, OpenTruncate, StatusIsDirectory},
		{"/file", Read, OpenDirectory, StatusNotDirectory},
		{"/file", Read, OpenTrailingSlash, StatusNotDirectory},
		{"/missing", Write, OpenCreate | OpenTrailingSlash, StatusIsDirectory},
	} {
		if handle, _, status := namespace.OpenIntent(1, []byte(test.path), test.rights, test.flags); status != test.status || handle != 0 {
			t.Errorf("%s flags=%#x: handle=%x status=%d, want %d", test.path, test.flags, handle, status, test.status)
		}
	}
}

func TestOpenIntentCreateTruncateAndExclusiveResults(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	if namespace.Create([]byte("/file"), false) != StatusOK {
		t.Fatal("create")
	}
	before, _ := namespace.Stat([]byte("/file"))
	writable, status := namespace.Open(1, []byte("/file"), Write)
	if status != StatusOK {
		t.Fatal(status)
	}
	if count, status := namespace.Write(1, writable, []byte("payload")); status != StatusOK || count != 7 {
		t.Fatal(status)
	}
	namespace.Close(1, writable)
	handle, _, status := namespace.OpenIntent(1, []byte("/file"), Write, OpenCreate|OpenTruncate)
	after, statStatus := namespace.StatOpen(1, handle)
	if status != StatusOK || statStatus != StatusOK || after.ID != before.ID || after.Size != 0 {
		t.Fatalf("open=%d stat=%+v/%d", status, after, statStatus)
	}
	if exclusive, _, status := namespace.OpenIntent(2, []byte("/file"), Write, OpenCreate|OpenExclusive); status != StatusAlreadyExists || exclusive != 0 {
		t.Fatalf("exclusive handle=%x status=%d", exclusive, status)
	}
	created, _, status := namespace.OpenIntent(2, []byte("/new"), Read, OpenCreate|OpenTruncate)
	createdStat, createdStatus := namespace.StatOpen(2, created)
	pathStat, pathStatus := namespace.Stat([]byte("/new"))
	if status != StatusOK || createdStatus != StatusOK || pathStatus != StatusOK || createdStat.ID != pathStat.ID || createdStat.Size != 0 {
		t.Fatalf("created open=%d handle=%+v/%d path=%+v/%d", status, createdStat, createdStatus, pathStat, pathStatus)
	}
	if _, writeStatus := namespace.Write(2, created, []byte("x")); writeStatus != StatusPermissionDenied {
		t.Fatalf("create authority widened descriptor rights: %d", writeStatus)
	}
}
