package vfsservice

import (
	"testing"

	"goos/kernel/vfs"
	"goos/userspace/ext4"
	"goos/userspace/ext4vol"
	"goos/userspace/fsproto"
)

type writableMemoryDevice struct{ memoryDevice }

func (device *writableMemoryDevice) WriteAt(offset uint64, data []byte) bool {
	if offset+uint64(len(data)) > uint64(len(device.data)) {
		return false
	}
	copy(device.data[offset:], data)
	return true
}

func attachWritableExt4(t *testing.T, service *Service, slot uint8, path string) (*ext4Service, *writableMemoryDevice) {
	t.Helper()
	device := &writableMemoryDevice{memoryDevice: *ext4Image(t, "canonical")}
	instance := &ext4Service{cache: make([]byte, fsproto.CacheSize)}
	stamp := uint32(0)
	clock := func() ext4.Timestamp { stamp++; return ext4.Timestamp{Seconds: 1800000000, Nanos: stamp} }
	if !instance.volume.MountWritable(device, clock) {
		t.Fatalf("writable mount: %s", instance.volume.Refusal())
	}
	if status := service.AttachVolume(slot, instance, instance.cache, []byte(path)); status != vfs.StatusOK {
		t.Fatalf("attach: %v", status)
	}
	return instance, device
}

// Through the VFS, a writable ext4 volume creates, links, renames the name
// the path gives, replaces, and is left clean by the unmount.
func TestWritableExt4VolumeThroughTheVFS(t *testing.T) {
	service := startedService(t, testFAT32Device())
	_, device := attachWritableExt4(t, service, 1, "/mnt/w")
	info := service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationFilesystemInfo, Path: []byte("/mnt/w")})
	if info.Status != vfs.StatusOK || info.Filesystem.ReadOnly {
		t.Fatalf("info %+v", info)
	}
	writePersistent(t, service, "/mnt/w/a", "alpha")
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationLink, Path: []byte("/mnt/w/a"), Target: []byte("/mnt/w/b")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/mnt/w/b"), Target: []byte("/mnt/w/c")})
	for path, want := range map[string]vfs.Status{"/mnt/w/a": vfs.StatusOK, "/mnt/w/b": vfs.StatusNotFound, "/mnt/w/c": vfs.StatusOK} {
		if _, status := readPath(t, service, path); status != want {
			t.Fatalf("%s: %v, want %v", path, status, want)
		}
	}
	writePersistent(t, service, "/mnt/w/new", "beta")
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationReplace, Path: []byte("/mnt/w/new"), Target: []byte("/mnt/w/c")})
	if text, status := readPath(t, service, "/mnt/w/c"); status != vfs.StatusOK || text != "beta" {
		t.Fatalf("replaced: %q %v", text, status)
	}
	if text, _ := readPath(t, service, "/mnt/w/a"); text != "alpha" {
		t.Fatalf("other link: %q", text)
	}
	// A rename moves a symbolic link itself, not what it points at.
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationSymlink, Path: []byte("/mnt/w/link"), Target: []byte("a")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/mnt/w/link"), Target: []byte("/mnt/w/link2")})
	if text, status := readPath(t, service, "/mnt/w/link2"); status != vfs.StatusOK || text != "alpha" {
		t.Fatalf("moved link: %q %v", text, status)
	}
	if text, _ := readPath(t, service, "/mnt/w/a"); text != "alpha" {
		t.Fatal("rename of a link moved its target")
	}
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationMkdir, Path: []byte("/mnt/w/fresh-dir")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationRename, Path: []byte("/mnt/w/fresh-dir"), Target: []byte("/mnt/w/moved")})
	dispatchOK(t, service, vfs.Request{Operation: vfs.OperationUnlink, Path: []byte("/mnt/w/moved")})

	opened := dispatchOK(t, service, vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/mnt/w/a")})
	if status := service.DetachVolume(1); status != vfs.StatusBusy {
		t.Fatalf("detach with an open file: %v", status)
	}
	service.Dispatch(testSession, vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	if status := service.DetachVolume(1); status != vfs.StatusOK {
		t.Fatalf("detach: %v", status)
	}
	var reread ext4vol.Volume
	if !reread.Mount(&device.memoryDevice) {
		t.Fatalf("volume not clean after detach: %s", reread.Refusal())
	}
}

// A service that cannot leave its volume clean keeps the mount, offline;
// the next detach removes it.
func TestFailedCleanUnmountLeavesTheMountOffline(t *testing.T) {
	service := startedService(t, testFAT32Device())
	_, device := attachWritableExt4(t, service, 1, "/mnt/w")
	writePersistent(t, service, "/mnt/w/a", "alpha")
	device.data = device.data[:0:0]
	if status := service.DetachVolume(1); status != vfs.StatusBackendError {
		t.Fatalf("detach of a failing volume: %v", status)
	}
	if !service.VolumeMounted(1) {
		t.Fatal("failed unmount removed the mount")
	}
	if _, status := readPath(t, service, "/mnt/w/a"); status == vfs.StatusOK {
		t.Fatal("offline volume still serves")
	}
	if status := service.DetachVolume(1); status != vfs.StatusOK {
		t.Fatalf("second detach: %v", status)
	}
}
