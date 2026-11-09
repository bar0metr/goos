package vfsservice

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/ramfs"
	"goos/kernel/vfs"
	"goos/userspace/ext4vol"
	"goos/userspace/fsproto"
)

// ext4Service is an ext4 filesystem service run in the test process with a
// file cache of its own, as each additional volume instance has.
type ext4Service struct {
	volume      ext4vol.Volume
	cache       []byte
	correlation uint64
	name        [ramfs.MaxName]byte
	requested   [ramfs.MaxName]byte
	replied     [ramfs.MaxName]byte
	fail        bool
}

type memoryDevice struct{ data []byte }

func (device *memoryDevice) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (*memoryDevice) WriteAt(uint64, []byte) bool { return false }
func (*memoryDevice) Flush() bool                 { return true }
func (device *memoryDevice) Capacity() uint64     { return uint64(len(device.data)) }

func ext4Image(t *testing.T, name string) *memoryDevice {
	t.Helper()
	compressed, err := os.ReadFile("../ext4/testdata/" + name + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return &memoryDevice{data: data}
}

func newExt4Service(t *testing.T, image string) *ext4Service {
	t.Helper()
	service := &ext4Service{cache: make([]byte, fsproto.CacheSize)}
	if !service.volume.Mount(ext4Image(t, image)) {
		t.Fatalf("%s refused: %s", image, service.volume.Refusal())
	}
	return service
}

func (service *ext4Service) Exchange(request fsproto.Request) (fsproto.Reply, ramfs.Status) {
	if service.fail {
		return fsproto.Reply{}, ramfs.StatusPeerDied
	}
	service.correlation++
	var message ipc.Message
	if !fsproto.EncodeRequest(request, service.correlation, &message, service.cache) {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	decoded, ok := fsproto.DecodeRequest(&message, service.cache, &service.requested)
	if !ok {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	var answer ipc.Message
	if !fsproto.EncodeReply(fsproto.Serve(&service.volume, decoded, service.cache, &service.name), message.Correlation, &answer, service.cache) {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	reply, ok := fsproto.DecodeReply(&answer, service.cache, &service.replied)
	if !ok || answer.Correlation != service.correlation {
		return fsproto.Reply{}, ramfs.StatusIOError
	}
	return reply, ramfs.StatusOK
}

func attachExt4(t *testing.T, service *Service, slot uint8, image, path string) *ext4Service {
	t.Helper()
	instance := newExt4Service(t, image)
	if status := service.AttachVolume(slot, instance, instance.cache, []byte(path)); status != vfs.StatusOK {
		t.Fatalf("attach %s at %s: %v", image, path, status)
	}
	return instance
}

func readPath(t *testing.T, service *Service, path string) (string, vfs.Status) {
	t.Helper()
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(path)})
	if opened.Status != vfs.StatusOK {
		return "", opened.Status
	}
	defer service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 256})
	return string(read.Data), read.Status
}

func TestTwoExt4VolumesBesideThePersistentOne(t *testing.T) {
	service := startedService(t, testFAT32Device())
	attachExt4(t, service, 1, "canonical", "/mnt/a")
	attachExt4(t, service, 2, "blockmap-1k", "/mnt/b")
	if text, status := readPath(t, service, "/mnt/a/hello.txt"); status != vfs.StatusOK || text != "hello ext4\n" {
		t.Fatalf("/mnt/a: %q %v", text, status)
	}
	if text, status := readPath(t, service, "/mnt/b/hello.txt"); status != vfs.StatusOK || text != "hello ext3\n" {
		t.Fatalf("/mnt/b: %q %v", text, status)
	}
	writePersistent(t, service, "/persist/note", "still writable")
	info := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationFilesystemInfo, Path: []byte("/mnt/a")})
	if info.Status != vfs.StatusOK || info.Filesystem.Type != ramfs.FilesystemExt4 || !info.Filesystem.ReadOnly {
		t.Fatalf("info %+v", info)
	}
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: []byte("/mnt/a/hello.txt")})
	if opened.Status == vfs.StatusOK {
		written := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: []byte("x")})
		if written.Status == vfs.StatusOK {
			t.Fatal("wrote to a read-only volume")
		}
		service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	}
	if created := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationCreate, Path: []byte("/mnt/a/new")}); created.Status == vfs.StatusOK {
		t.Fatal("created a file on a read-only volume")
	}
}

func TestVolumeFailureStaysInItsSlot(t *testing.T) {
	service := startedService(t, testFAT32Device())
	first := attachExt4(t, service, 1, "canonical", "/mnt/a")
	attachExt4(t, service, 2, "canonical", "/mnt/b")
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/a/hello.txt")})
	if opened.Status != vfs.StatusOK {
		t.Fatal(opened.Status)
	}
	first.fail = true
	if _, status := readPath(t, service, "/mnt/a/dir/sub/deep.txt"); status == vfs.StatusOK {
		t.Fatal("a dead volume answered")
	}
	service.VolumeOffline(1)
	if text, status := readPath(t, service, "/mnt/b/hello.txt"); status != vfs.StatusOK || text != "hello ext4\n" {
		t.Fatalf("other volume: %q %v", text, status)
	}
	writePersistent(t, service, "/persist/after", "persistent volume unaffected")
	// A replacement serving the same volume takes over the open file, with
	// a cache of its own.
	replacement := newExt4Service(t, "canonical")
	if status := service.AttachVolume(1, replacement, replacement.cache, []byte("/mnt/a")); status != vfs.StatusOK {
		t.Fatalf("reattach: %v", status)
	}
	read := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: 64})
	if read.Status != vfs.StatusOK || string(read.Data) != "hello ext4\n" {
		t.Fatalf("open file after reattach: %q %v", read.Data, read.Status)
	}
	// The old service's cache is never read again.
	for index := range first.cache {
		first.cache[index] = 'X'
	}
	if text, status := readPath(t, service, "/mnt/a/dir/sub/deep.txt"); status != vfs.StatusOK || text != "deep file\n" {
		t.Fatalf("after old cache scribbled: %q %v", text, status)
	}
	if status := service.AttachVolume(1, replacement, replacement.cache, []byte("/mnt/other")); status != vfs.StatusAlreadyExists {
		t.Fatalf("reattach at another path: %v", status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
}

func TestDetachVolume(t *testing.T) {
	service := startedService(t, nil)
	attachExt4(t, service, 3, "canonical", "/mnt/c")
	opened := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/c/hello.txt")})
	if status := service.DetachVolume(3); status != vfs.StatusBusy {
		t.Fatalf("detach with an open file: %v", status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if status := service.DetachVolume(3); status != vfs.StatusOK {
		t.Fatalf("detach: %v", status)
	}
	if _, status := readPath(t, service, "/mnt/c/hello.txt"); status == vfs.StatusOK {
		t.Fatal("unmounted volume still answers")
	}
	if service.VolumeMounted(3) || service.DetachVolume(3) != vfs.StatusNotFound {
		t.Fatal("slot still mounted")
	}
	// The slot takes another volume, at another path.
	attachExt4(t, service, 3, "hash-tea", "/mnt/d")
	// A volume whose service died can still be unmounted.
	service.VolumeOffline(3)
	if status := service.DetachVolume(3); status != vfs.StatusOK {
		t.Fatalf("detach of an offline volume: %v", status)
	}
}

func TestVolumePathsAndSlots(t *testing.T) {
	service := startedService(t, nil)
	instance := newExt4Service(t, "canonical")
	for _, path := range []string{"/system", "/persist", "/mnt", "/mnt/", "/mnt/a/b", "/mnt/..", "/mnt/.", "mnt/a", "/bootstrap/x"} {
		if status := service.AttachVolume(1, instance, instance.cache, []byte(path)); status != vfs.StatusInvalidArgument {
			t.Errorf("path %q: %v", path, status)
		}
	}
	for _, slot := range []uint8{0, vfs.MaxVolumes + 1} {
		if status := service.AttachVolume(slot, instance, instance.cache, []byte("/mnt/a")); status != vfs.StatusInvalidArgument {
			t.Errorf("slot %d: %v", slot, status)
		}
	}
	if status := service.AttachVolume(1, instance, instance.cache[:100], []byte("/mnt/a")); status != vfs.StatusInvalidArgument {
		t.Errorf("short cache: %v", status)
	}
	attachExt4(t, service, 1, "canonical", "/mnt/a")
	other := newExt4Service(t, "canonical")
	if status := service.AttachVolume(2, other, other.cache, []byte("/mnt/a")); status != vfs.StatusAlreadyExists {
		t.Errorf("second volume on a used path: %v", status)
	}
	listing := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/mnt/a")})
	if listing.Status != vfs.StatusOK || listing.Kind != ramfs.KindDirectory {
		t.Fatalf("stat /mnt/a: %+v", listing)
	}
}

func TestRefusedVolumeReportsWhy(t *testing.T) {
	service := startedService(t, nil)
	device := ext4Image(t, "canonical")
	device.data[1024+0x60] |= 0x04 // needs_recovery, under a stale checksum
	instance := &ext4Service{cache: make([]byte, fsproto.CacheSize)}
	instance.volume.Mount(device)
	if status := service.AttachVolume(1, instance, instance.cache, []byte("/mnt/a")); status != vfs.StatusCorrupt {
		t.Fatalf("attach of a damaged volume: %v", status)
	}
	if service.VolumeMounted(1) {
		t.Fatal("refused volume mounted")
	}
}

func TestVolumeRootLists(t *testing.T) {
	service := startedService(t, nil)
	attachExt4(t, service, 1, "canonical", "/mnt/a")
	attachExt4(t, service, 4, "legacy", "/mnt/z")
	stat := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationStat, Path: []byte("/mnt")})
	if stat.Status != vfs.StatusOK || stat.Kind != ramfs.KindDirectory {
		t.Fatalf("stat /mnt: %+v", stat)
	}
	names := map[string]bool{}
	for cursor := uint64(0); ; {
		entry := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationReadDir, Path: []byte("/mnt"), Offset: cursor})
		if entry.Status == vfs.StatusNotFound {
			break
		}
		if entry.Status != vfs.StatusOK {
			t.Fatalf("readdir /mnt: %v", entry.Status)
		}
		names[string(entry.Name)] = true
		cursor = uint64(entry.Cursor)
	}
	if !names["a"] || !names["z"] || len(names) != 2 {
		t.Fatalf("/mnt lists %v", names)
	}
}
