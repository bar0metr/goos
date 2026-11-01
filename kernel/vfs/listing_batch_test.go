package vfs

import (
	"bytes"
	"fmt"
	"goos/kernel/ramfs"
	"strings"
	"testing"
)

// batchListing reads every batch of a directory and returns the names in order
// and the batches it took.
func batchListing(t *testing.T, namespace *Namespace, owner Owner, path string) ([]string, int) {
	t.Helper()
	target := make([]byte, MaxDirBatch)
	var names []string
	batches := 0
	for cursor := 0; ; batches++ {
		if batches > 1000 {
			t.Fatal("listing did not end")
		}
		batch, status := namespace.ReadDirBatchFor(owner, []byte(path), cursor, target)
		if status != StatusOK {
			t.Fatalf("batch %s at %d: %d", path, cursor, status)
		}
		var record ramfs.DirRecord
		count := 0
		for records := target[:batch.Used]; len(records) != 0; count++ {
			size, ok := record.Decode(records)
			if !ok {
				t.Fatalf("malformed record at batch %d", batches)
			}
			names = append(names, string(record.Name))
			records = records[size:]
		}
		if count != batch.Count {
			t.Fatalf("count %d, records %d", batch.Count, count)
		}
		if batch.End {
			return names, batches + 1
		}
		if batch.Next <= cursor {
			t.Fatalf("no progress at %d", cursor)
		}
		cursor = batch.Next
	}
}

func singleListing(t *testing.T, namespace *Namespace, owner Owner, path string) []string {
	t.Helper()
	var names []string
	for cursor := 0; ; {
		entry, next, status := namespace.ReadDirFor(owner, []byte(path), cursor)
		if status == StatusNotFound {
			return names
		}
		if status != StatusOK {
			t.Fatalf("list %s: %d", path, status)
		}
		names = append(names, string(entry.Name[:entry.Size]))
		cursor = next
	}
}

func wantSameListing(t *testing.T, namespace *Namespace, owner Owner, path string) []string {
	t.Helper()
	single := singleListing(t, namespace, owner, path)
	batched, _ := batchListing(t, namespace, owner, path)
	if strings.Join(batched, "|") != strings.Join(single, "|") {
		t.Fatalf("batched %v, single %v", batched, single)
	}
	return batched
}

func populate(t *testing.T, namespace *Namespace, directory string, count int) {
	t.Helper()
	createAll(t, namespace, true, directory)
	for index := 0; index < count; index++ {
		createAll(t, namespace, false, fmt.Sprintf("%s/entry%04d", directory, index))
	}
}

func TestBatchReturnsManyEntriesInOneCall(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/many", 20)
	target := make([]byte, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/many"), 0, target)
	if status != StatusOK || batch.Count != 20 || !batch.End {
		t.Fatalf("batch = %+v, %d", batch, status)
	}
	if counters := namespace.AuthorityCounters(); counters.BatchRequests != 1 || counters.BatchEntries != 20 {
		t.Fatalf("counters = %+v", counters)
	}
}

// A batch boundary loses and repeats nothing, and a cursor taken from any batch
// continues the listing.
func TestBatchBoundaryContinuesListing(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/many", 200)
	names := wantSameListing(t, namespace, listingOwner, "/many")
	if len(names) != 200 {
		t.Fatalf("listed %d", len(names))
	}
	if _, batches := batchListing(t, namespace, listingOwner, "/many"); batches < 2 || batches > 6 {
		t.Fatalf("%d batches for 200 entries", batches)
	}
	target := make([]byte, MaxDirBatch)
	first, status := namespace.ReadDirBatchFor(listingOwner, []byte("/many"), 0, target)
	if status != StatusOK || first.End || first.Count == 0 {
		t.Fatalf("first batch = %+v, %d", first, status)
	}
	rest, status := namespace.ReadDirBatchFor(listingOwner, []byte("/many"), first.Next, target)
	if status != StatusOK || rest.Count == 0 {
		t.Fatalf("second batch = %+v, %d", rest, status)
	}
	var record ramfs.DirRecord
	if _, ok := record.Decode(target[:rest.Used]); !ok || string(record.Name) != names[first.Count] {
		t.Fatalf("continued at %q, want %q", record.Name, names[first.Count])
	}
}

func TestBatchEndsAfterPartialBatch(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/few", 3)
	target := make([]byte, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/few"), 0, target)
	if status != StatusOK || batch.Count != 3 || !batch.End {
		t.Fatalf("batch = %+v, %d", batch, status)
	}
	// Asking again from the end answers the same end with nothing in it.
	again, status := namespace.ReadDirBatchFor(listingOwner, []byte("/few"), batch.Next, target)
	if status != StatusOK || again.Count != 0 || !again.End {
		t.Fatalf("after the end = %+v, %d", again, status)
	}
}

func TestBatchOfEmptyAndMissingDirectories(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	createAll(t, namespace, true, "/empty")
	target := make([]byte, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/empty"), 0, target)
	if status != StatusOK || batch.Count != 0 || !batch.End {
		t.Fatalf("empty = %+v, %d", batch, status)
	}
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/missing"), 0, target); status != StatusNotFound {
		t.Fatalf("missing = %d", status)
	}
}

// Hidden entries take no room, and their names never reach the memory the
// client can read.
func TestBatchHidesScopedEntriesAndKeepsSlots(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/mixed", 40)
	paths := []string{}
	for index := 0; index < 40; index += 2 {
		paths = append(paths, fmt.Sprintf("/mixed/entry%04d", index))
	}
	createAll(t, namespace, true, "/mixed/keys")
	createAll(t, namespace, false, "/mixed/keys/private")
	installAll(t, namespace, true, 0, "/mixed/keys")
	installAll(t, namespace, false, 1, paths[:7]...)
	names := wantSameListing(t, namespace, listingOwner, "/mixed")
	for _, name := range names {
		if name == "keys" || name == "entry0000" || name == "entry0012" {
			t.Fatalf("hidden entry %s listed in %v", name, names)
		}
	}
	if len(names) != 40-7 {
		t.Fatalf("listed %d", len(names))
	}
	target := bytes.Repeat([]byte{0xee}, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/mixed"), 0, target)
	if status != StatusOK {
		t.Fatal(status)
	}
	if bytes.Contains(target, []byte("keys")) || bytes.Contains(target, []byte("entry0000")) {
		t.Fatal("a hidden name reached the target")
	}
	if batch.Count != 40-7 || !batch.End {
		t.Fatalf("hidden entries took slots: %+v", batch)
	}
}

// A confined session sees exactly what its grants give it, in both forms.
func TestBatchFollowsConfinedAuthority(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/shared", 4)
	createAll(t, namespace, true, "/shared/mine")
	createAll(t, namespace, false, "/shared/mine/file")
	installAll(t, namespace, true, 0, "/shared/mine")
	installAll(t, namespace, true, 1, "/shared")
	authority := Authority{Confined: true}
	authority.Grants[1] = Read
	if namespace.SetAuthority(listingOwner, authority) != StatusOK {
		t.Fatal("authority")
	}
	if names := wantSameListing(t, namespace, listingOwner, "/shared"); len(names) != 4 {
		t.Fatalf("confined listing = %v", names)
	}
	authority.Grants[0] = Read
	if namespace.SetAuthority(listingOwner+1, authority) != StatusOK {
		t.Fatal("authority")
	}
	if names := wantSameListing(t, namespace, listingOwner+1, "/shared"); len(names) != 5 {
		t.Fatalf("granted listing = %v", names)
	}
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/shared/mine"), 0, make([]byte, MaxDirBatch)); status != StatusPermissionDenied {
		t.Fatalf("hidden directory = %d", status)
	}
}

func TestBatchMixesMountsWithBackendEntries(t *testing.T) {
	namespace, _ := mountedNamespace(t)
	populate(t, namespace, "/host", 3)
	other := &ramfs.FileSystem{}
	if status := namespace.Mount([]byte("/host/alias"), other, other.Init()); status != StatusOK {
		t.Fatalf("mount = %d", status)
	}
	secret := &ramfs.FileSystem{}
	if status := namespace.Mount([]byte("/host/vault"), secret, secret.Init()); status != StatusOK {
		t.Fatalf("mount = %d", status)
	}
	names := wantSameListing(t, namespace, listingOwner, "/host")
	if strings.Join(names, " ") != "entry0000 entry0001 entry0002 alias vault" {
		t.Fatalf("listing = %v", names)
	}
	installAll(t, namespace, true, 0, "/host/vault")
	if names := wantSameListing(t, namespace, listingOwner, "/host"); strings.Join(names, " ") != "entry0000 entry0001 entry0002 alias" {
		t.Fatalf("scoped mount = %v", names)
	}
	// A cursor inside the mount phase resumes there.
	target := make([]byte, MaxDirBatch)
	var record ramfs.DirRecord
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/host"), mountCursor, target)
	if status != StatusOK || batch.Count != 1 || !batch.End {
		t.Fatalf("mount cursor = %+v, %d", batch, status)
	}
	if _, ok := record.Decode(target[:batch.Used]); !ok || string(record.Name) != "alias" || record.Stat.MountID == 0 {
		t.Fatalf("mount record = %+v", record)
	}
}

func TestBatchCarriesLongNames(t *testing.T) {
	namespace, fs := mountedNamespace(t)
	createAll(t, namespace, true, "/long")
	_, directory, _ := namespace.Resolve([]byte("/long"))
	for index := 0; index < 12; index++ {
		name := fmt.Sprintf("%04d%s", index, strings.Repeat("x", ramfs.MaxName-4))
		if _, status := fs.Create(directory, []byte(name)); status != ramfs.StatusOK {
			t.Fatal(status)
		}
	}
	names := wantSameListing(t, namespace, listingOwner, "/long")
	if len(names) != 12 || len(names[11]) != ramfs.MaxName {
		t.Fatalf("listed %d", len(names))
	}
	if _, batches := batchListing(t, namespace, listingOwner, "/long"); batches < 3 {
		t.Fatalf("%d batches hold 12 records of %d bytes", batches, ramfs.MaxDirRecord)
	}
}

type scriptedBatchBackend struct {
	*ramfs.FileSystem
	batches int
	script  func(target []byte, cursor int) (ramfs.DirBatch, ramfs.Status)
}

func (backend *scriptedBatchBackend) ReadDirBatch(parent ramfs.NodeID, cursor int, target []byte) (ramfs.DirBatch, ramfs.Status) {
	backend.batches++
	return backend.script(target, cursor)
}

func scriptedNamespace(t *testing.T, script func([]byte, int) (ramfs.DirBatch, ramfs.Status)) (*Namespace, *scriptedBatchBackend) {
	t.Helper()
	fs := &ramfs.FileSystem{}
	backend := &scriptedBatchBackend{FileSystem: fs, script: script}
	namespace := &Namespace{}
	if namespace.Mount([]byte("/"), backend, fs.Init()) != StatusOK {
		t.Fatal("mount")
	}
	return namespace, backend
}

func scriptedRecord(target []byte, id ramfs.NodeID, next int, name string) int {
	stat := ramfs.Stat{ID: id, Kind: ramfs.KindFile}
	return ramfs.AppendDirRecord(target, &stat, next, []byte(name))
}

// Records gathered before a backend failure are delivered, and the next call
// reports the failure with no record to hide it.
func TestBatchDeliversRecordsBeforeBackendFailure(t *testing.T) {
	namespace, _ := scriptedNamespace(t, nil)
	backend := namespace.mounts[0].backend.(*scriptedBatchBackend)
	backend.script = func(target []byte, cursor int) (ramfs.DirBatch, ramfs.Status) {
		if cursor != 0 {
			return ramfs.DirBatch{}, ramfs.StatusIOError
		}
		used := scriptedRecord(target, 90, 4, "kept")
		return ramfs.DirBatch{Used: used, Count: 1, Next: 4}, ramfs.StatusOK
	}
	target := make([]byte, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), 0, target)
	if status != StatusOK || batch.Count != 1 || batch.End || batch.Next != 4 {
		t.Fatalf("partial = %+v, %d", batch, status)
	}
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), batch.Next, target); status != StatusBackendError {
		t.Fatalf("failure = %d", status)
	}
}

// A backend that stops moving cannot hold the VFS in a loop.
func TestBatchRefusesCursorThatDoesNotRise(t *testing.T) {
	namespace, backend := scriptedNamespace(t, func(target []byte, cursor int) (ramfs.DirBatch, ramfs.Status) {
		used := scriptedRecord(target, 90, cursor, "again")
		return ramfs.DirBatch{Used: used, Count: 1, Next: cursor}, ramfs.StatusOK
	})
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), 0, make([]byte, MaxDirBatch)); status != StatusBackendError {
		t.Fatalf("stuck cursor = %d", status)
	}
	if backend.batches != 1 {
		t.Fatalf("%d exchanges with a stuck backend", backend.batches)
	}
	namespace, backend = scriptedNamespace(t, func(target []byte, cursor int) (ramfs.DirBatch, ramfs.Status) {
		return ramfs.DirBatch{Next: cursor}, ramfs.StatusOK
	})
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), 0, make([]byte, MaxDirBatch)); status != StatusBackendError {
		t.Fatalf("empty unfinished batch = %d", status)
	}
	if backend.batches != 1 {
		t.Fatalf("%d exchanges with an empty backend", backend.batches)
	}
}

// One call examines at most maxScopeWalk entries, however many batches that
// takes, then reports the denial or returns what it holds.
func TestBatchBoundsExaminedEntries(t *testing.T) {
	const hiddenNode = ramfs.NodeID(500)
	visibleFirst := true
	namespace, backend := scriptedNamespace(t, nil)
	backend.script = func(target []byte, cursor int) (ramfs.DirBatch, ramfs.Status) {
		id, name := hiddenNode, "hidden"
		if visibleFirst && cursor == 0 {
			id, name = ramfs.NodeID(501), "visible"
		}
		used := scriptedRecord(target, id, cursor+1, name)
		return ramfs.DirBatch{Used: used, Count: 1, Next: cursor + 1}, ramfs.StatusOK
	}
	namespace.scopes[0] = scope{backend: backend, subtree: false, exact: hiddenNode, exactKnown: true, denied: Read | Write}
	target := make([]byte, MaxDirBatch)
	batch, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), 0, target)
	if status != StatusOK || batch.Count != 1 || batch.End || batch.Next != maxScopeWalk {
		t.Fatalf("visible then bound = %+v, %d", batch, status)
	}
	if backend.batches > maxScopeWalk+1 {
		t.Fatalf("%d exchanges past the bound", backend.batches)
	}
	visibleFirst = false
	if _, status := namespace.ReadDirBatchFor(listingOwner, []byte("/"), 0, target); status != StatusPermissionDenied {
		t.Fatalf("hidden run = %d", status)
	}
}
