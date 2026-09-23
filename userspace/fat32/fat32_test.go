package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

type memoryDevice struct {
	data          []byte
	reads, writes int
}

func (device *memoryDevice) ReadAt(offset uint64, target []byte) bool {
	device.reads++
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *memoryDevice) WriteAt(offset uint64, data []byte) bool {
	device.writes++
	return offset+uint64(len(data)) <= uint64(len(device.data)) && copy(device.data[offset:], data) == len(data)
}
func (*memoryDevice) Flush() bool             { return true }
func (device *memoryDevice) Capacity() uint64 { return uint64(len(device.data)) }

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

// TestSectorCacheServesRepeatedMetadata pins the traffic reduction that keeps
// interactive persistent-storage commands responsive: entries packed into one
// sector must cost one device read, not one read per entry.
func TestSectorCacheServesRepeatedMetadata(t *testing.T) {
	device := formattedDevice()
	var fs FileSystem
	root, ok := fs.Mount(device)
	if !ok {
		t.Fatal("mount failed")
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		encoded, _ := ramfs.MakeName([]byte(name))
		if _, status := fs.CreateName(root, encoded); status != ramfs.StatusOK {
			t.Fatalf("create %s: %v", name, status)
		}
	}
	name, _ := ramfs.MakeName([]byte("c.txt"))
	device.reads = 0
	id, status := fs.OpenName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if device.reads != 0 {
		t.Fatalf("resident directory scan issued %d device reads", device.reads)
	}
	if _, status := fs.Write(id, 0, []byte("cached")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	target := make([]byte, 6)
	if count, status := fs.Read(id, 0, target); status != ramfs.StatusOK || count != 6 || string(target) != "cached" {
		t.Fatalf("read %d %v %q", count, status, target)
	}
	device.reads = 0
	if count, status := fs.Read(id, 0, target); status != ramfs.StatusOK || count != 6 || string(target) != "cached" {
		t.Fatalf("second read %d %v %q", count, status, target)
	}
	if device.reads != 0 {
		t.Fatalf("repeated read issued %d device reads", device.reads)
	}
}

// TestSectorCacheStaysCoherentAcrossInstances proves the write-through cache
// never serves superseded bytes and that data really reaches the device.
func TestSectorCacheStaysCoherentAcrossInstances(t *testing.T) {
	device := formattedDevice()
	var fs FileSystem
	root, _ := fs.Mount(device)
	name, _ := ramfs.MakeName([]byte("log.txt"))
	id, status := fs.CreateName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	for round, payload := range []string{"first", "secnd", "third"} {
		if _, status := fs.Write(id, uint64(round*5), []byte(payload)); status != ramfs.StatusOK {
			t.Fatal(status)
		}
		target := make([]byte, 5)
		if _, status := fs.Read(id, uint64(round*5), target); status != ramfs.StatusOK || string(target) != payload {
			t.Fatalf("round %d read %q", round, target)
		}
	}
	// A cache spanning more sectors than it holds must still evict correctly.
	if _, status := fs.Write(id, 3000, []byte("far")); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	var reopened FileSystem
	root, ok := reopened.Mount(device)
	if !ok {
		t.Fatal("remount failed")
	}
	id, status = reopened.OpenName(root, name)
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	target := make([]byte, 15)
	if _, status := reopened.Read(id, 0, target); status != ramfs.StatusOK || string(target) != "firstsecndthird" {
		t.Fatalf("persisted %q %v", target, status)
	}
	if _, status := reopened.Read(id, 3000, target[:3]); status != ramfs.StatusOK || string(target[:3]) != "far" {
		t.Fatalf("persisted tail %q", target[:3])
	}
}

// mountRejects builds a formatted device, lets corrupt damage its boot sector,
// and requires the mount to refuse it. Every offset this filesystem computes
// comes from these fields, so an image that does not describe itself
// consistently must never be read.
func mountRejects(t testing.TB, name string, corrupt func(boot []byte, device *memoryDevice)) {
	t.Helper()
	device := formattedDevice()
	corrupt(device.data[:512], device)
	var fs FileSystem
	if _, ok := fs.Mount(device); ok {
		t.Fatalf("%s was mounted", name)
	}
}

func TestMountRejectsGeometryItCannotHonour(t *testing.T) {
	// A sane image still mounts, or the cases below prove nothing.
	var fs FileSystem
	if _, ok := fs.Mount(formattedDevice()); !ok {
		t.Fatal("a well-formed volume was refused")
	}
	mountRejects(t, "a missing signature", func(boot []byte, _ *memoryDevice) { boot[510] = 0 })
	mountRejects(t, "a foreign sector size", func(boot []byte, _ *memoryDevice) { put16(boot[11:13], 1024) })
	mountRejects(t, "multi-sector clusters", func(boot []byte, _ *memoryDevice) { boot[13] = 8 })
	mountRejects(t, "a second FAT", func(boot []byte, _ *memoryDevice) { boot[16] = 2 })
	mountRejects(t, "no reserved sectors", func(boot []byte, _ *memoryDevice) { put16(boot[14:16], 1) })
	mountRejects(t, "an empty FAT", func(boot []byte, _ *memoryDevice) { put32(boot[36:40], 0) })
	mountRejects(t, "a root cluster below two", func(boot []byte, _ *memoryDevice) { put32(boot[44:48], 1) })
	mountRejects(t, "a root cluster past the volume", func(boot []byte, _ *memoryDevice) { put32(boot[44:48], 1<<20) })

	// A volume larger than the device it sits on would put cluster offsets
	// beyond any storage that exists.
	mountRejects(t, "a volume larger than its device", func(boot []byte, _ *memoryDevice) { put32(boot[32:36], 1<<28) })
	mountRejects(t, "a volume of metadata only", func(boot []byte, _ *memoryDevice) { put32(boot[32:36], 96) })

	// A FAT too small to hold an entry per cluster would have a chain walk
	// reading entries out of the data region.
	mountRejects(t, "a FAT too small for its clusters", func(boot []byte, _ *memoryDevice) { put32(boot[36:40], 1) })

	// A device that cannot answer for its own size is not usable either.
	mountRejects(t, "a device of no capacity", func(_ []byte, device *memoryDevice) { device.data = device.data[:0] })
	mountRejects(t, "a device of partial sectors", func(_ []byte, device *memoryDevice) {
		device.data = device.data[:len(device.data)-1]
	})
}

func TestMountRejectsAnUnreadableDevice(t *testing.T) {
	var fs FileSystem
	if _, ok := fs.Mount(nil); ok {
		t.Fatal("a missing device was mounted")
	}
	if _, ok := fs.Mount(&memoryDevice{}); ok {
		t.Fatal("an empty device was mounted")
	}
}

// A rejected mount must leave nothing usable behind, so a later operation
// cannot act on geometry the mount refused.
func TestRejectedMountLeavesNoUsableFilesystem(t *testing.T) {
	device := formattedDevice()
	put32(device.data[32:36], 1<<28)
	var fs FileSystem
	if _, ok := fs.Mount(device); ok {
		t.Fatal("the oversized volume was mounted")
	}
	name, _ := ramfs.MakeName([]byte("file"))
	if _, status := fs.OpenName(rootNode, name); status == ramfs.StatusOK {
		t.Fatal("a lookup succeeded on a refused volume")
	}
	if _, status := fs.CreateName(rootNode, name); status == ramfs.StatusOK {
		t.Fatal("a create succeeded on a refused volume")
	}
}
