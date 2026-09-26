package fat32

import (
	"goos/kernel/ramfs"
	"testing"
)

type memoryDevice struct {
	data          []byte
	reads, writes int
	// failAt, when nonzero, makes that write and every later one fail, as a
	// device that has gone away would.
	failAt int
}

func (device *memoryDevice) ReadAt(offset uint64, target []byte) bool {
	device.reads++
	return offset+uint64(len(target)) <= uint64(len(device.data)) && copy(target, device.data[offset:]) == len(target)
}
func (device *memoryDevice) WriteAt(offset uint64, data []byte) bool {
	device.writes++
	if device.failAt != 0 && device.writes >= device.failAt {
		return false
	}
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
	// File data is not kept in the window (the VFS caches it), so a read
	// costs exactly its data transfer: the entry and the FAT stay resident.
	target := make([]byte, 6)
	device.reads = 0
	if count, status := fs.Read(id, 0, target); status != ramfs.StatusOK || count != 6 || string(target) != "cached" {
		t.Fatalf("read %d %v %q", count, status, target)
	}
	if device.reads != 1 {
		t.Fatalf("a one-cluster read issued %d device reads, want 1", device.reads)
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
	mountRejects(t, "a zero cluster size", func(boot []byte, _ *memoryDevice) { boot[13] = 0 })
	mountRejects(t, "a cluster size that is not a power of two", func(boot []byte, _ *memoryDevice) { boot[13] = 3 })
	mountRejects(t, "no FAT", func(boot []byte, _ *memoryDevice) { boot[16] = 0 })
	mountRejects(t, "three FATs", func(boot []byte, _ *memoryDevice) { boot[16] = 3 })
	mountRejects(t, "no reserved sectors", func(boot []byte, _ *memoryDevice) { put16(boot[14:16], 0) })
	mountRejects(t, "an empty FAT", func(boot []byte, _ *memoryDevice) { put32(boot[36:40], 0) })
	mountRejects(t, "a FAT16 root directory", func(boot []byte, _ *memoryDevice) { put16(boot[17:19], 512) })
	mountRejects(t, "a FAT16 FAT size", func(boot []byte, _ *memoryDevice) { put16(boot[22:24], 64) })
	mountRejects(t, "an active FAT that does not exist", func(boot []byte, _ *memoryDevice) { put16(boot[40:42], 0x81) })
	mountRejects(t, "a root cluster below two", func(boot []byte, _ *memoryDevice) { put32(boot[44:48], 1) })
	mountRejects(t, "a root cluster past the volume", func(boot []byte, _ *memoryDevice) { put32(boot[44:48], 1<<20) })

	// A volume larger than the device it sits on would put cluster offsets
	// beyond any storage that exists.
	mountRejects(t, "a volume larger than its device", func(boot []byte, _ *memoryDevice) { put32(boot[32:36], 1<<28) })
	mountRejects(t, "a volume of metadata only", func(boot []byte, _ *memoryDevice) { put32(boot[32:36], 96) })

	// A FAT too small for its data region limits the volume to what it
	// describes, as Linux does, so no walk reads an entry past the FAT.
	device := formattedDevice()
	put32(device.data[36:40], 1)
	put32(device.data[44:48], 2)
	var small FileSystem
	if _, ok := small.Mount(device); !ok || small.clusters != 512/4-2 {
		t.Fatalf("a short FAT: mounted %v with %d clusters", ok, small.clusters)
	}

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

// TestMountSaysWhyItRefused covers the typed error model: a volume this
// service does not implement is unsupported, a contradictory one corrupt, and
// an unreadable device an I/O error.
func TestMountSaysWhyItRefused(t *testing.T) {
	var fs FileSystem
	for name, test := range map[string]struct {
		damage func(boot []byte)
		want   ramfs.Status
	}{
		"FAT16 root directory": {func(boot []byte) { put16(boot[17:19], 512) }, ramfs.StatusUnsupported},
		"FAT16 FAT size":       {func(boot []byte) { put16(boot[22:24], 32) }, ramfs.StatusUnsupported},
		"4096-byte sectors":    {func(boot []byte) { put16(boot[11:13], 4096) }, ramfs.StatusUnsupported},
		"no signature":         {func(boot []byte) { boot[510] = 0 }, ramfs.StatusCorrupt},
		"no FATs":              {func(boot []byte) { boot[16] = 0 }, ramfs.StatusCorrupt},
		"past the device":      {func(boot []byte) { put32(boot[32:36], 1<<30) }, ramfs.StatusCorrupt},
	} {
		device := formattedDevice()
		test.damage(device.data[:512])
		if _, ok := fs.Mount(device); ok || fs.Refused() != test.want {
			t.Errorf("%s: mounted %v, refused with %d, want %d", name, ok, fs.Refused(), test.want)
		}
	}
	if _, ok := fs.Mount(nil); ok || fs.Refused() != ramfs.StatusIOError {
		t.Errorf("no device refused with %d", fs.Refused())
	}
	if _, ok := fs.Mount(formattedDevice()); !ok || fs.Refused() != ramfs.StatusOK {
		t.Errorf("a good volume refused with %d", fs.Refused())
	}
}
