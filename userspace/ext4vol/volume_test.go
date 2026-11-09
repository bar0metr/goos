package ext4vol

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"sort"
	"testing"

	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
)

var _ fsproto.Volume = (*Volume)(nil)

type memDevice struct{ data []byte }

func (device *memDevice) ReadAt(offset uint64, target []byte) bool {
	end := offset + uint64(len(target))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	copy(target, device.data[offset:end])
	return true
}

func (device *memDevice) WriteAt(uint64, []byte) bool { return false }
func (device *memDevice) Flush() bool                 { return true }
func (device *memDevice) Capacity() uint64            { return uint64(len(device.data)) }

// canonical loads the fixture userspace/ext4 pins; its tests check the
// image against SHA256SUMS.
func canonical(t *testing.T) *memDevice {
	t.Helper()
	compressed, err := os.ReadFile("../ext4/testdata/canonical.img.gz")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	image, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return &memDevice{data: image}
}

func mounted(t *testing.T) *Volume {
	t.Helper()
	var volume Volume
	if !volume.Mount(canonical(t)) {
		t.Fatalf("mount refused: %s", volume.Refusal())
	}
	return &volume
}

func name(t *testing.T, value string) ramfs.Name {
	t.Helper()
	result, ok := ramfs.MakeName([]byte(value))
	if !ok {
		t.Fatalf("invalid name %q", value)
	}
	return result
}

func TestOpenReadStat(t *testing.T) {
	volume := mounted(t)
	root, status := volume.Root()
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	id, status := volume.OpenName(root, name(t, "hello.txt"))
	if status != ramfs.StatusOK {
		t.Fatal(status)
	}
	link, status := volume.OpenName(root, name(t, "link-to-hello"))
	if status != ramfs.StatusOK || link != id {
		t.Fatalf("hard link resolves to %#x, want %#x: %v", link, id, status)
	}
	buffer := make([]byte, 64)
	count, status := volume.Read(id, 0, buffer)
	if status != ramfs.StatusOK || string(buffer[:count]) != "hello ext4\n" {
		t.Fatalf("read %q %v", buffer[:count], status)
	}
	stat, status := volume.Stat(id)
	if status != ramfs.StatusOK || stat.Kind != ramfs.KindFile || stat.Size != 11 || !stat.Valid() ||
		!stat.Modified.Available || stat.Modified.Seconds != 1767225600 || stat.AttributesAvailable {
		t.Fatalf("stat %+v %v", stat, status)
	}
	if _, status := volume.OpenName(root, name(t, "sym-fast")); status != ramfs.StatusUnsupported {
		t.Fatalf("symlink open: %v", status)
	}
	if _, status := volume.OpenName(root, name(t, "missing")); status != ramfs.StatusNotFound {
		t.Fatalf("missing: %v", status)
	}
	bad, _ := ramfs.MakeName([]byte{'b', 'a', 'd', '-', 0xff, 0xfe})
	names, _ := volume.OpenName(root, name(t, "names"))
	if _, status := volume.OpenName(names, bad); status != ramfs.StatusOK {
		t.Fatalf("non-UTF-8 name: %v", status)
	}
	if _, status := volume.OpenName(names, name(t, "mixed")); status != ramfs.StatusNotFound {
		t.Fatalf("names must match case exactly: %v", status)
	}
}

func TestStaleGeneration(t *testing.T) {
	volume := mounted(t)
	root, _ := volume.Root()
	id, _ := volume.OpenName(root, name(t, "hello.txt"))
	stale := id ^ 1<<40
	if _, status := volume.Stat(stale); status != ramfs.StatusNotFound {
		t.Fatalf("stale generation: %v", status)
	}
	if _, status := volume.AcquireObject(stale); status != ramfs.StatusNotFound {
		t.Fatalf("stale acquire: %v", status)
	}
	for _, number := range []ramfs.NodeID{4000, 5000, 1 << 31} {
		if _, status := volume.Stat(number); status != ramfs.StatusNotFound {
			t.Fatalf("inode %d: %v", number, status)
		}
	}
}

func TestMutationsRefused(t *testing.T) {
	volume := mounted(t)
	root, _ := volume.Root()
	id, _ := volume.OpenName(root, name(t, "hello.txt"))
	if _, status := volume.CreateName(root, name(t, "new")); status != ramfs.StatusPermissionDenied {
		t.Fatal(status)
	}
	if _, status := volume.Write(id, 0, []byte("x")); status != ramfs.StatusPermissionDenied {
		t.Fatal(status)
	}
	if volume.UnlinkName(root, name(t, "hello.txt")) != ramfs.StatusPermissionDenied ||
		volume.Truncate(id, 0) != ramfs.StatusPermissionDenied ||
		volume.RenameName(id, root, name(t, "x")) != ramfs.StatusPermissionDenied {
		t.Fatal("mutation accepted")
	}
	info, status := volume.FilesystemInfo()
	if status != ramfs.StatusOK || !info.Valid() || !info.ReadOnly || info.Type != ramfs.FilesystemExt4 || string(info.Name()) != "canonical" {
		t.Fatalf("info %+v %v", info, status)
	}
}

// listAll pages through dir in batches of the given byte budget and
// returns the sorted names.
func listAll(t *testing.T, volume *Volume, dir ramfs.NodeID, budget int) []string {
	t.Helper()
	var names []string
	cursor := 0
	for rounds := 0; ; rounds++ {
		if rounds > 10000 {
			t.Fatal("listing does not end")
		}
		target := make([]byte, budget)
		batch, status := volume.ReadDirBatch(dir, cursor, target)
		if status != ramfs.StatusOK {
			t.Fatal(status)
		}
		if !ramfs.ValidDirBatch(target[:batch.Used], batch.Count, cursor, batch.Next) {
			t.Fatal("malformed batch")
		}
		var record ramfs.DirRecord
		for data := target[:batch.Used]; len(data) > 0; {
			size, _ := record.Decode(data)
			names = append(names, string(record.Name))
			data = data[size:]
		}
		cursor = batch.Next
		if batch.End {
			break
		}
	}
	sort.Strings(names)
	return names
}

func TestReadDirBatch(t *testing.T) {
	volume := mounted(t)
	root, _ := volume.Root()
	want := []string{"big", "digests", "dir", "empty", "fragmented", "hello.txt", "link-to-hello", "lost+found", "names", "sparse"}
	if got := listAll(t, volume, root, ramfs.MaxDirBatch); len(got) != len(want) {
		t.Fatalf("root lists %q, want %q (symlinks hidden)", got, want)
	} else {
		for index := range want {
			if got[index] != want[index] {
				t.Fatalf("root lists %q, want %q", got, want)
			}
		}
	}
	big, _ := volume.OpenName(root, name(t, "big"))
	small := listAll(t, volume, big, 200)
	large := listAll(t, volume, big, ramfs.MaxDirBatch)
	if len(small) != 2000 || len(large) != 2000 {
		t.Fatalf("paged listings saw %d and %d names", len(small), len(large))
	}
	entry, next, status := volume.ReadDir(big, 0)
	if status != ramfs.StatusOK || next <= 0 || entry.Size == 0 {
		t.Fatalf("single entry %v %d", status, next)
	}
}

func TestServeThroughProtocol(t *testing.T) {
	volume := mounted(t)
	var listed [ramfs.MaxName]byte
	reply := fsproto.Serve(volume, fsproto.Request{Operation: fsproto.OperationMount}, nil, &listed)
	if reply.Status != ramfs.StatusOK || reply.Node == 0 {
		t.Fatalf("mount reply %+v", reply)
	}
	open := fsproto.Serve(volume, fsproto.Request{Operation: fsproto.OperationOpen, Parent: reply.Node, Name: []byte("hello.txt")}, nil, &listed)
	if open.Status != ramfs.StatusOK || open.Kind != ramfs.KindFile || open.Value != 11 {
		t.Fatalf("open reply %+v", open)
	}
	info := fsproto.Serve(volume, fsproto.Request{Operation: fsproto.OperationFilesystemInfo}, nil, &listed)
	if info.Status != ramfs.StatusOK || info.Filesystem.Type != ramfs.FilesystemExt4 {
		t.Fatalf("info reply %+v", info)
	}
}

func TestRefusedVolume(t *testing.T) {
	device := canonical(t)
	device.data[1024+0x10]++ // free inode count, under the superblock checksum
	var volume Volume
	if volume.Mount(device) {
		t.Fatal("damaged volume mounted")
	}
	if _, status := volume.Root(); status != ramfs.StatusCorrupt {
		t.Fatalf("root of refused volume: %v", status)
	}
	if _, status := volume.Read(1, 0, nil); status != ramfs.StatusIOError {
		t.Fatalf("read of refused volume: %v", status)
	}
}
