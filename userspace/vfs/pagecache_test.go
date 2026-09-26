package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/fsproto"
	"strings"
	"testing"
)

// cachedService is a service whose persistent volume is reached through a
// transport that counts exchanges.
func cachedService(t *testing.T) (*Service, *counting) {
	t.Helper()
	service := startedService(t, nil)
	transport := &counting{inner: newLoopback(testFAT32Device())}
	if !service.AttachFilesystem(transport) {
		t.Fatal("volume refused")
	}
	return service, transport
}

// readAll reads path to its end through one handle.
func readAll(t *testing.T, service *Service, path string) string {
	t.Helper()
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
	if opened.Status != vfs.StatusOK {
		t.Fatalf("open %s = %d", path, opened.Status)
	}
	var read []byte
	for {
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if reply.Status != vfs.StatusOK {
			t.Fatalf("read %s at %d = %d", path, len(read), reply.Status)
		}
		if len(reply.Data) == 0 {
			break
		}
		read = append(read, reply.Data...)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	return string(read)
}

// writeAt writes data at offset of an existing file.
func writeAt(t *testing.T, service *Service, path string, offset uint64, data string) {
	t.Helper()
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(path)})
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: offset})
	for len(data) != 0 {
		chunk := min(len(data), vfs.MaxProtocolData)
		reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte(data[:chunk])})
		if reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
			t.Fatalf("write %s = %+v", path, reply)
		}
		data = data[chunk:]
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

// pattern is size bytes that differ from page to page and within a page.
func pattern(size int, seed byte) string {
	data := make([]byte, size)
	for index := range data {
		data[index] = 'a' + byte(index/fsproto.PageSize)*3 + byte(index%23) + seed
	}
	return string(data)
}

// A file is read a page per exchange the first time, and from the cache with
// no exchange after that.
func TestPageCacheServesRepeatedReads(t *testing.T) {
	service, transport := cachedService(t)
	payload := pattern(16*1024, 0)
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/big")})
	writeAt(t, service, "/persist/big", 0, payload)
	service.persistent.pages.ForgetAll()
	before := transport.exchanges
	if readAll(t, service, "/persist/big") != payload {
		t.Fatal("first read changed the file")
	}
	// Four pages and the lookups of the open; a chunked read took 74.
	if cold := transport.exchanges - before; cold > 6 {
		t.Fatalf("cold read took %d exchanges", cold)
	}
	before = transport.exchanges
	if readAll(t, service, "/persist/big") != payload {
		t.Fatal("cached read changed the file")
	}
	if warm := transport.exchanges - before; warm != 0 {
		t.Fatalf("cached read took %d exchanges", warm)
	}
}

// Every change a write makes is visible to the next read, whether the page it
// changed was cached or not.
func TestPageCacheFollowsWrites(t *testing.T) {
	service, _ := cachedService(t)
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/file")})
	writeAt(t, service, "/persist/file", 0, "0123456789")
	if got := readAll(t, service, "/persist/file"); got != "0123456789" {
		t.Fatalf("read = %q", got)
	}
	// The page is cached now: an overwrite inside it and an append to it.
	writeAt(t, service, "/persist/file", 2, "AB")
	writeAt(t, service, "/persist/file", 10, "tail")
	if got := readAll(t, service, "/persist/file"); got != "01AB456789tail" {
		t.Fatalf("after writes to a cached page = %q", got)
	}
	// A write on a later page extends the file past the page that used to
	// end it: the hole between reads as the service fills it, not as the end.
	writeAt(t, service, "/persist/file", fsproto.PageSize+5, "far")
	got := readAll(t, service, "/persist/file")
	if len(got) != fsproto.PageSize+8 || !strings.HasPrefix(got, "01AB456789tail") || !strings.HasSuffix(got, "far") {
		t.Fatalf("after a write on the next page: %d bytes", len(got))
	}
	// A truncation drops what the cache held past the new end.
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationTruncate, Path: []byte("/persist/file"), Offset: 4}); reply.Status != vfs.StatusOK {
		t.Fatalf("truncate = %d", reply.Status)
	}
	if got := readAll(t, service, "/persist/file"); got != "01AB" {
		t.Fatalf("after truncate = %q", got)
	}
}

// A file removed and made again may get the node its predecessor had; the
// cache never serves the old bytes for it.
func TestPageCacheForgetsRemovedFiles(t *testing.T) {
	service, _ := cachedService(t)
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/reused")})
	writeAt(t, service, "/persist/reused", 0, "old contents")
	readAll(t, service, "/persist/reused")
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/persist/reused")}); reply.Status != vfs.StatusOK {
		t.Fatalf("unlink = %d", reply.Status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/reused")})
	writeAt(t, service, "/persist/reused", 0, "new")
	if got := readAll(t, service, "/persist/reused"); got != "new" {
		t.Fatalf("recreated file reads %q", got)
	}
}

// More pages than the cache holds are read correctly, the oldest giving way.
func TestPageCacheEvictsTheOldestPage(t *testing.T) {
	service, _ := cachedService(t)
	first, second := pattern(16*fsproto.PageSize, 0), pattern(16*fsproto.PageSize, 1)
	for path, payload := range map[string]string{"/persist/one": first, "/persist/two": second} {
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte(path)})
		writeAt(t, service, path, 0, payload)
	}
	for round := 0; round < 2; round++ {
		if readAll(t, service, "/persist/one") != first || readAll(t, service, "/persist/two") != second {
			t.Fatalf("round %d read the wrong bytes", round)
		}
	}
	if resident := service.persistent.pages.Resident(); resident != scratchSlot {
		t.Fatalf("%d pages resident, want the whole cache", resident)
	}
}

// liar is a service that claims to have read more than a page.
type liar struct{ *loopback }

func (service liar) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	reply, ok := service.loopback.Exchange(request)
	if request.Operation == fsproto.OperationRead {
		reply.Value = fsproto.PageSize + 1
	}
	return reply, ok
}

func TestPageCacheRefusesAnOverlongPage(t *testing.T) {
	service := startedService(t, nil)
	honest := newLoopback(testFAT32Device())
	if !service.AttachFilesystem(honest) {
		t.Fatal("volume refused")
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/file")})
	writeAt(t, service, "/persist/file", 0, "data")
	service.persistent.pages.ForgetAll()
	service.persistent.transport = liar{honest}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/persist/file")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 8}); reply.Status != vfs.StatusBackendError {
		t.Fatalf("overlong page read = %d", reply.Status)
	}
	if resident := service.persistent.pages.Resident(); resident != 0 {
		t.Fatalf("%d pages cached from a lying service", resident)
	}
}

// Without the shared mapping no file data moves, and nothing else is harmed.
func TestNoCacheMeansNoData(t *testing.T) {
	service := &Service{}
	if !service.Init() || !service.MountSystemImage(testSystemImage(t)) || !service.AttachFilesystem(newLoopback(testFAT32Device())) {
		t.Fatal("service init failed")
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/persist/file")})
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte("/persist/file")})
	if reply := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("x")}); reply.Status != vfs.StatusBackendError {
		t.Fatalf("write without a cache = %d", reply.Status)
	}
	if service.AttachCache(make([]byte, fsproto.PageSize)) {
		t.Fatal("a mapping smaller than the cache was accepted")
	}
}
