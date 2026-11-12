package ext4vol

import (
	"testing"

	"goos/kernel/ramfs"
	"goos/userspace/ext4"
)

// writableDevice is a copy of a fixture the tests may write.
type writableDevice struct {
	memDevice
	refuseWrites bool
}

func (device *writableDevice) WriteAt(offset uint64, data []byte) bool {
	if device.refuseWrites {
		return false
	}
	end := offset + uint64(len(data))
	if end < offset || end > uint64(len(device.data)) {
		return false
	}
	copy(device.data[offset:end], data)
	return true
}

func clock() func() ext4.Timestamp {
	nanos := uint32(0)
	return func() ext4.Timestamp {
		nanos++
		return ext4.Timestamp{Seconds: 1800000000, Nanos: nanos}
	}
}

func writable(t *testing.T) (*Volume, *writableDevice) {
	t.Helper()
	device := &writableDevice{memDevice: *canonical(t)}
	var volume Volume
	if !volume.MountWritable(device, clock()) {
		t.Fatalf("writable mount refused: %s", volume.Refusal())
	}
	return &volume, device
}

func must(t *testing.T, status ramfs.Status) {
	t.Helper()
	if status != ramfs.StatusOK {
		t.Fatalf("status %d", status)
	}
}

func readAll(t *testing.T, volume *Volume, id ramfs.NodeID) string {
	t.Helper()
	buffer := make([]byte, 256)
	count, status := volume.Read(id, 0, buffer)
	must(t, status)
	return string(buffer[:count])
}

func TestWritableVolumeOperations(t *testing.T) {
	volume, device := writable(t)
	root, status := volume.Root()
	must(t, status)
	if info, _ := volume.FilesystemInfo(); info.ReadOnly {
		t.Fatal("writable volume reported read-only")
	}
	file, status := volume.CreateName(root, name(t, "notes"))
	must(t, status)
	if count, status := volume.Write(file, 0, []byte("first")); status != ramfs.StatusOK || count != 5 {
		t.Fatalf("write %d %d", count, status)
	}
	if got := readAll(t, volume, file); got != "first" {
		t.Fatalf("read %q", got)
	}
	dir, status := volume.MkdirName(root, name(t, "box"))
	must(t, status)
	if _, status := volume.CreateName(root, name(t, "notes")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("duplicate create %d", status)
	}
	must(t, volume.LinkName(file, dir, name(t, "alias")))
	if stat, _ := volume.Stat(file); stat.Unix.Links != 2 {
		t.Fatalf("links %d", stat.Unix.Links)
	}
	link, status := volume.SymlinkName(root, name(t, "to-notes"), []byte("notes"))
	must(t, status)
	target := make([]byte, 64)
	if count, status := volume.ReadLink(link, target); status != ramfs.StatusOK || string(target[:count]) != "notes" {
		t.Fatalf("readlink %q %d", target[:count], status)
	}

	// Renames name their source: the alias moves, the other link stays.
	if status := volume.RenameName(file, root, name(t, "moved")); status != ramfs.StatusUnsupported {
		t.Fatalf("node-only rename %d", status)
	}
	if status := volume.RenameEntry(dir, []byte("missing"), file, root, name(t, "moved")); status != ramfs.StatusNotFound {
		t.Fatalf("rename of a missing source %d", status)
	}
	if status := volume.RenameEntry(dir, []byte("alias"), link, root, name(t, "moved")); status != ramfs.StatusNotFound {
		t.Fatalf("rename of another node's name %d", status)
	}
	if status := volume.RenameEntry(dir, []byte("alias"), file, root, name(t, "notes")); status != ramfs.StatusAlreadyExists {
		t.Fatalf("rename onto a name %d", status)
	}
	must(t, volume.RenameEntry(dir, []byte("alias"), file, root, name(t, "moved")))
	if _, status := volume.OpenName(dir, name(t, "alias")); status != ramfs.StatusNotFound {
		t.Fatalf("old name %d", status)
	}
	if moved, _ := volume.OpenName(root, name(t, "moved")); moved != file {
		t.Fatal("moved name names another node")
	}

	// Replace swaps a file in and frees the one it covers.
	other, status := volume.CreateName(root, name(t, "draft"))
	must(t, status)
	_, status = volume.Write(other, 0, []byte("second"))
	must(t, status)
	if status := volume.ReplaceEntry(root, []byte("draft"), other, root, name(t, "box")); status != ramfs.StatusIsDirectory {
		t.Fatalf("replace onto a directory %d", status)
	}
	// Onto another link of the same inode: both names stay.
	must(t, volume.LinkName(other, root, name(t, "draft-link")))
	must(t, volume.ReplaceEntry(root, []byte("draft"), other, root, name(t, "draft-link")))
	for _, kept := range []string{"draft", "draft-link"} {
		if id, status := volume.OpenName(root, name(t, kept)); status != ramfs.StatusOK || id != other {
			t.Fatalf("%s after a same-inode replace: %d", kept, status)
		}
	}
	if stat, _ := volume.Stat(other); stat.Unix.Links != 2 {
		t.Fatalf("links after a same-inode replace: %d", stat.Unix.Links)
	}
	must(t, volume.UnlinkName(root, name(t, "draft-link")))
	must(t, volume.ReplaceEntry(root, []byte("draft"), other, root, name(t, "notes")))
	if replaced, _ := volume.OpenName(root, name(t, "notes")); replaced != other {
		t.Fatal("replace kept the old file")
	}
	if got := readAll(t, volume, file); got != "first" {
		t.Fatalf("other link lost its data: %q", got)
	}

	// A file whose last name goes while it is held stays readable.
	_, status = volume.AcquireObject(other)
	must(t, status)
	must(t, volume.UnlinkName(root, name(t, "notes")))
	if got := readAll(t, volume, other); got != "second" {
		t.Fatalf("held unlinked file %q", got)
	}
	_, status = volume.ReleaseObject(other)
	must(t, status)
	if _, status := volume.Stat(other); status != ramfs.StatusNotFound {
		t.Fatalf("released file %d", status)
	}

	must(t, volume.SetMetadata(file, ramfs.MetadataChange{SetReadOnly: true, ReadOnly: true,
		SetModified: true, Modified: ramfs.Timestamp{Seconds: 1600000000, Available: true}}))
	if stat, _ := volume.Stat(file); stat.Unix.Mode&0o222 != 0 || stat.Modified.Seconds != 1600000000 {
		t.Fatalf("metadata mode %o modified %d", stat.Unix.Mode, stat.Modified.Seconds)
	}
	must(t, volume.Truncate(file, 2))
	if status := volume.UnlinkName(root, name(t, "box")); status != ramfs.StatusOK {
		t.Fatalf("rmdir %d", status)
	}
	if !volume.Flush() {
		t.Fatal("flush")
	}
	must(t, volume.Unmount())
	if _, status := volume.Root(); status == ramfs.StatusOK {
		t.Fatal("unmounted volume still answers")
	}

	// A clean unmount leaves a volume the read-only mount accepts.
	var reread Volume
	if !reread.Mount(&device.memDevice) {
		t.Fatalf("read-only mount after unmount: %s", reread.Refusal())
	}
	again, _ := reread.Root()
	moved, status := reread.OpenName(again, name(t, "moved"))
	must(t, status)
	if got := readAll(t, &reread, moved); got != "fi" {
		t.Fatalf("after remount %q", got)
	}
	for _, gone := range []string{"notes", "box", "draft"} {
		if _, status := reread.OpenName(again, name(t, gone)); status != ramfs.StatusNotFound {
			t.Fatalf("%s survived: %d", gone, status)
		}
	}
}

func TestReadOnlyVolumeRefusesMutations(t *testing.T) {
	volume := mounted(t)
	root, _ := volume.Root()
	if _, status := volume.CreateName(root, name(t, "x")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("create %d", status)
	}
	if status := volume.RenameEntry(root, []byte("hello.txt"), 0, root, name(t, "y")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("rename %d", status)
	}
	if info, _ := volume.FilesystemInfo(); !info.ReadOnly {
		t.Fatal("read-only volume reported writable")
	}
	must(t, volume.Unmount())
}

// A dirty volume is refused read-only and recovered by the writable mount.
func TestWritableMountRecoversDirtyVolume(t *testing.T) {
	volume, device := writable(t)
	root, _ := volume.Root()
	_, status := volume.CreateName(root, name(t, "kept"))
	must(t, status)
	if !volume.Flush() {
		t.Fatal("flush")
	}
	// No unmount: the service died with the journal live.
	snapshot := &writableDevice{memDevice: memDevice{data: append([]byte(nil), device.data...)}}
	var readOnly Volume
	if readOnly.Mount(&snapshot.memDevice) {
		t.Fatal("read-only mount accepted a mounted volume")
	}
	if _, status := readOnly.Root(); status != ramfs.StatusUnsupported {
		t.Fatalf("refusal %d", status)
	}
	var recovered Volume
	if !recovered.MountWritable(snapshot, clock()) {
		t.Fatalf("recovery: %s", recovered.Refusal())
	}
	again, _ := recovered.Root()
	if _, status := recovered.OpenName(again, name(t, "kept")); status != ramfs.StatusOK {
		t.Fatalf("recovered tree %d", status)
	}
	must(t, recovered.Unmount())
}

// A write the device refuses fails the volume, so its service can hand the
// journal to a replacement.
func TestFailedWriteFailsTheVolume(t *testing.T) {
	volume, device := writable(t)
	root, _ := volume.Root()
	if volume.Failed() {
		t.Fatal("fresh volume failed")
	}
	device.refuseWrites = true
	// The create waits in the pending transaction; the flush that commits
	// it reaches the dead device.
	_, status := volume.CreateName(root, name(t, "lost"))
	must(t, status)
	if volume.Flush() {
		t.Fatal("flush on a dead device")
	}
	if !volume.Failed() {
		t.Fatal("refused write did not fail the volume")
	}
	device.refuseWrites = false
	var replacement Volume
	if !replacement.MountWritable(device, clock()) {
		t.Fatalf("replacement: %s", replacement.Refusal())
	}
	must(t, replacement.Unmount())
}
