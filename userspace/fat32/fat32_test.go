package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

type memoryDevice struct{ data []byte }

func (device *memoryDevice) ReadAt(offset uint64, target []byte) bool {
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *memoryDevice) WriteAt(offset uint64, data []byte) bool {
	return offset+uint64(len(data)) <= uint64(len(device.data)) && copy(device.data[offset:], data) == len(data)
}
func (*memoryDevice) Flush() bool { return true }

func formattedDevice() *memoryDevice {
	device := &memoryDevice{data: make([]byte, 4*1024*1024)}
	boot := device.data[:512]
	put16(boot[11:13], 512)
	boot[13] = 1
	put16(boot[14:16], 32)
	boot[16] = 1
	put32(boot[32:36], 8192)
	put32(boot[36:40], 64)
	put32(boot[44:48], 2)
	boot[510], boot[511] = 0x55, 0xaa
	fat := device.data[32*512:]
	put32(fat[0:4], 0x0ffffff8)
	put32(fat[4:8], 0xffffffff)
	put32(fat[8:12], 0x0fffffff)
	return device
}

func TestPersistentFileLifecycle(t *testing.T) {
	device := formattedDevice()
	var fs FileSystem
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatal("mount failed")
	}
	name, _ := ramfs.MakeName([]byte("state.txt"))
	id, status := fs.CreateName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	payload := []byte("persistent data")
	if count, status := fs.Write(id, 509, payload); status != ramfs.StatusOK || count != len(payload) {
		t.Fatalf("write %d %d", count, status)
	}
	var mounted FileSystem
	root, ok = mounted.Mount(device)
	if !ok {
		t.Fatal("remount failed")
	}
	id, status = mounted.OpenName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	target := make([]byte, len(payload))
	if count, status := mounted.Read(id, 509, target); status != ramfs.StatusOK || count != len(payload) || string(target) != string(payload) {
		t.Fatalf("read %d %d %q", count, status, target)
	}
	if status = mounted.UnlinkName(root, name); status != ramfs.StatusOK {
		t.Fatal(status)
	}
}
