package vfsservice

import (
	"fmt"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"strings"
	"testing"
)

// listBatched reads a whole listing the way a client does and returns the
// names and how many requests it took.
func listBatched(t *testing.T, service *Service, owner vfs.Owner, path string) ([]string, int) {
	t.Helper()
	object := make([]byte, 2*vfs.MaxDirBatch)
	var names []string
	for cursor, requests := uint64(0), 1; ; requests++ {
		reply := service.ReadDirBatch(owner, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte(path), Offset: cursor, NameReturn: true}, object)
		if reply.Status != vfs.StatusOK {
			t.Fatalf("batch %s at %d: %d", path, cursor, reply.Status)
		}
		if reply.Size > vfs.MaxDirBatch {
			t.Fatalf("batch of %d bytes", reply.Size)
		}
		var record ramfs.DirRecord
		count := 0
		for data := object[:reply.Size]; len(data) != 0; count++ {
			size, ok := record.Decode(data)
			if !ok {
				t.Fatalf("malformed record in %s", path)
			}
			names = append(names, string(record.Name))
			data = data[size:]
		}
		if count != int(reply.BatchCount) {
			t.Fatalf("count %d, records %d", reply.BatchCount, count)
		}
		if reply.BatchEnd {
			return names, requests
		}
		cursor = uint64(reply.Cursor)
	}
}

// A request's Size bounds its records and what the filesystem service sends, a
// limit below the longest record rises to it, and following the cursors lists
// everything once.
func TestBatchLimitBoundsRecordsAndBackend(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/limited")})
	const entries = 40
	for index := 0; index < entries; index++ {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(fmt.Sprintf("/home/limited/program%03d", index))})
	}
	object := make([]byte, vfs.MaxDirBatch)
	before := service.BackendBatchCounters()
	first := service.ReadDirBatch(testSession, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/home/limited"), Size: 1, NameReturn: true}, object)
	if first.Status != vfs.StatusOK || first.BatchCount == 0 || first.Size > ramfs.MaxDirRecord {
		t.Fatalf("status %d, %d records in %d bytes", first.Status, first.BatchCount, first.Size)
	}
	if examined := service.BackendBatchCounters().Entries - before.Entries; examined != uint64(first.BatchCount) {
		t.Fatalf("service sent %d entries for %d records", examined, first.BatchCount)
	}
	seen := map[string]bool{}
	for cursor := uint64(0); ; {
		reply := service.ReadDirBatch(testSession, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/home/limited"), Offset: cursor, Size: ramfs.MaxDirRecord, NameReturn: true}, object)
		if reply.Status != vfs.StatusOK || reply.Size > ramfs.MaxDirRecord {
			t.Fatalf("status %d, %d bytes", reply.Status, reply.Size)
		}
		var record ramfs.DirRecord
		for data := object[:reply.Size]; len(data) != 0; {
			size, ok := record.Decode(data)
			if !ok || seen[string(record.Name)] {
				t.Fatalf("record %q, valid %v", record.Name, ok)
			}
			seen[string(record.Name)] = true
			data = data[size:]
		}
		if reply.BatchEnd {
			break
		}
		cursor = uint64(reply.Cursor)
	}
	if len(seen) != entries {
		t.Fatalf("listed %d", len(seen))
	}
}

// A FAT32 directory larger than a batch takes a handful of exchanges across
// both boundaries, the shape of a listing of /apps.
func TestFAT32ListingTakesFewExchanges(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/many")})
	const entries = 119
	for index := 0; index < entries; index++ {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(fmt.Sprintf("/home/many/program%03d", index))})
	}
	counters, _ := service.ListingCounters()
	backend := service.BackendBatchCounters()
	names, requests := listBatched(t, service, testSession, "/home/many")
	if len(names) != entries {
		t.Fatalf("listed %d", len(names))
	}
	if requests > 4 {
		t.Fatalf("%d client requests", requests)
	}
	after, _ := service.ListingCounters()
	exchanges := service.BackendBatchCounters()
	if requests := exchanges.Requests - backend.Requests; requests == 0 || requests > 4 {
		t.Fatalf("%d FAT32 batch exchanges", requests)
	}
	if returned := exchanges.Entries - backend.Entries; returned != entries {
		t.Fatalf("FAT32 returned %d entries", returned)
	}
	if returned := after.BatchEntries - counters.BatchEntries; returned != entries {
		t.Fatalf("VFS returned %d entries", returned)
	}
	single := listNames(t, service, testSession, "/home/many")
	for _, name := range names {
		if !single[name] {
			t.Fatalf("%s is only in the batched listing", name)
		}
	}
}

func TestFAT32BatchCarriesLongNamesThroughSharedPage(t *testing.T) {
	service := startedService(t, testFAT32Device())
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/long")})
	long := strings.Repeat("n", 250)
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/long/" + long)})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/long/short")})
	names, _ := listBatched(t, service, testSession, "/home/long")
	if strings.Join(names, " ") != long+" short" {
		t.Fatalf("names = %v", names)
	}
}

// A cross-linked directory gives each child the node ID of a protected child,
// so authority decided on the entry's own identity hides it in a batch too.
func TestCrossLinkedDirectoryBatchHidesScopedChildren(t *testing.T) {
	device := testFAT32Device()
	service := startedService(t, device)
	for _, path := range []string{"/state/keys", "/state/exact", "/home/alias", "/home/other"} {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte(path)})
	}
	writeFile(t, service, "/state/keys/private", "PRIVATE")
	writeFile(t, service, "/state/exact/secret", "PRIVATE")
	writeFile(t, service, "/state/exact/public", "PUBLIC")
	if status := service.UnmountFilesystem(); status != vfs.StatusOK {
		t.Fatalf("unmount = %d", status)
	}
	shareDirectoryCluster(t, device, "ALIAS", "KEYS")
	shareDirectoryCluster(t, device, "OTHER", "EXACT")
	service = startedService(t, device)
	scopeOK(t, service, 0, "/state/keys", true, vfs.Read|vfs.Write)
	scopeOK(t, service, 1, "/state/exact/secret", false, vfs.Read|vfs.Write)
	if names, _ := listBatched(t, service, testSession, "/home/alias"); len(names) != 0 {
		t.Fatalf("subtree alias listed %v", names)
	}
	if names, _ := listBatched(t, service, testSession, "/home/other"); len(names) != 1 || names[0] != "public" {
		t.Fatalf("exact alias listed %v", names)
	}
}

func TestBatchMountsAppearInRoot(t *testing.T) {
	service := startedService(t, testFAT32Device())
	names, _ := listBatched(t, service, testSession, "/")
	single := listNames(t, service, testSession, "/")
	if len(names) != len(single) {
		t.Fatalf("batched %v, single %v", names, single)
	}
	for _, want := range []string{"tmp", "persist", "home", "state", "bootstrap"} {
		if !single[want] {
			t.Fatalf("%s missing from %v", want, single)
		}
	}
	for _, name := range names {
		if !single[name] {
			t.Fatalf("batched listing added %s", name)
		}
	}
}

// A service that dies during a listing fails the next request, and a restarted
// one continues from the same cursor, which names a place on the volume.
func TestBatchSurvivesFilesystemRestart(t *testing.T) {
	device := testFAT32Device()
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
		t.Fatal("service init failed")
	}
	first := newLoopback(device)
	if !service.AttachFilesystem(first) {
		t.Fatal("volume refused")
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/many")})
	const entries = 100
	for index := 0; index < entries; index++ {
		dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(fmt.Sprintf("/home/many/file%03d", index))})
	}
	object := make([]byte, vfs.MaxDirBatch)
	request := vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/home/many"), NameReturn: true}
	reply := service.ReadDirBatch(testSession, request, object)
	if reply.Status != vfs.StatusOK || reply.BatchEnd || reply.BatchCount == 0 {
		t.Fatalf("first batch = %+v", reply)
	}
	listed := int(reply.BatchCount)

	first.fail = true
	request.Offset = uint64(reply.Cursor)
	if failed := service.ReadDirBatch(testSession, request, object); failed.Status != vfs.StatusPeerDied {
		t.Fatalf("with the service gone = %d", failed.Status)
	}
	service.DetachFilesystem()
	if failed := service.ReadDirBatch(testSession, request, object); failed.Status == vfs.StatusOK {
		t.Fatal("listed with no service attached")
	}
	if !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("restarted volume refused")
	}
	for {
		reply = service.ReadDirBatch(testSession, request, object)
		if reply.Status != vfs.StatusOK {
			t.Fatalf("after restart = %d", reply.Status)
		}
		listed += int(reply.BatchCount)
		if reply.BatchEnd {
			break
		}
		request.Offset = uint64(reply.Cursor)
	}
	if listed != entries {
		t.Fatalf("listed %d of %d across the restart", listed, entries)
	}
}

// damagingTransport lets a test rewrite what a filesystem service answered.
type damagingTransport struct {
	Transport
	damage func(*fsproto.Reply)
}

func (transport *damagingTransport) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	reply, status := transport.Transport.Exchange(request)
	if status == ramfs.StatusOK && request.Operation == fsproto.OperationReadDirBatch && transport.damage != nil {
		transport.damage(&reply)
	}
	return reply, status
}

// The name page is the service's to write, so its contents and the reply's
// account of them are checked before the VFS uses either.
func TestRemoteRefusesDamagedBatch(t *testing.T) {
	for name, damage := range map[string]func(*fsproto.Reply){
		"size past the limit": func(reply *fsproto.Reply) { reply.Value = ramfs.MaxDirBatch + 1 },
		"short count":         func(reply *fsproto.Reply) { reply.Count-- },
		"long count":          func(reply *fsproto.Reply) { reply.Count++ },
		"cursor disagrees":    func(reply *fsproto.Reply) { reply.Cursor++ },
		"garbled record": func(reply *fsproto.Reply) {
			testCache[fsproto.NamePage*fsproto.PageSize+54] = 0xff
		},
	} {
		t.Run(name, func(t *testing.T) {
			device := testFAT32Device()
			service := &Service{}
			if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachCache(testCache) {
				t.Fatal("service init failed")
			}
			transport := &damagingTransport{Transport: newLoopback(device)}
			if !service.AttachFilesystem(transport) {
				t.Fatal("volume refused")
			}
			dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/some")})
			dispatchOK(t, service, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/some/file")})
			transport.damage = damage
			object := make([]byte, vfs.MaxDirBatch)
			reply := service.ReadDirBatch(testSession, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/home/some"), NameReturn: true}, object)
			if reply.Status != vfs.StatusBackendError {
				t.Fatalf("status = %d", reply.Status)
			}
		})
	}
}

// Without the shared page the namespace still lists, one exchange per entry.
func TestListingWithoutCacheFallsBackToSingleEntries(t *testing.T) {
	device := testFAT32Device()
	seed := startedService(t, device)
	dispatchOK(t, seed, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/home/few")})
	dispatchOK(t, seed, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/few/one")})
	dispatchOK(t, seed, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/home/few/two")})
	if status := seed.UnmountFilesystem(); status != vfs.StatusOK {
		t.Fatalf("unmount = %d", status)
	}
	service := &Service{}
	if !service.Init() || !service.MountBootstrap(testSystemImage(t)) || !service.AttachFilesystem(newLoopback(device)) {
		t.Fatal("service init failed")
	}
	if names, _ := listBatched(t, service, testSession, "/home/few"); strings.Join(names, " ") != "one two" {
		t.Fatalf("names = %v", names)
	}
	if exchanges := service.BackendBatchCounters().Requests; exchanges != 0 {
		t.Fatalf("%d batch exchanges with no shared page", exchanges)
	}
}

func TestBatchRefusesSmallObject(t *testing.T) {
	service := startedService(t, nil)
	reply := service.ReadDirBatch(testSession, vfs.Request{Operation: vfs.OperationReadDirBatch, Path: []byte("/"), NameReturn: true}, make([]byte, ramfs.MaxDirRecord-1))
	if reply.Status != vfs.StatusInvalidArgument {
		t.Fatalf("status = %d", reply.Status)
	}
}
