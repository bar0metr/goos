package fat32

import (
	"testing"

	"goos/kernel/ramfs"
)

// SetMetadata records the modification time to FAT's two seconds and keeps
// a file's read-only attribute, which then refuses writes; a directory takes
// a time and refuses the attribute.
func TestSetMetadata(t *testing.T) {
	device := formatted(t, 8<<20, geometries[0].layout)
	fs, root := mounted(t, device)
	id, _ := fs.CreateName(root, mustName(t, "m.bin"))
	stamp := ramfs.Timestamp{Seconds: 1_700_000_001, Nanoseconds: 500_000_000, Available: true}
	if status := fs.SetMetadata(id, ramfs.MetadataChange{SetModified: true, Modified: stamp, SetReadOnly: true, ReadOnly: true}); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	metadata, _ := fs.Metadata(id)
	if !metadata.Modified.Known || metadata.Modified.UnixNanos/1e9 != 1_700_000_000 || metadata.Attributes&AttrReadOnly == 0 {
		t.Fatalf("metadata = %+v", metadata)
	}
	if _, status := fs.Write(id, 0, []byte("x")); status != ramfs.StatusPermissionDenied {
		t.Fatalf("write to a read-only file: %d", status)
	}
	if status := fs.SetMetadata(id, ramfs.MetadataChange{SetReadOnly: true}); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if _, status := fs.Write(id, 0, []byte("x")); status != ramfs.StatusOK {
		t.Fatalf("write after clearing read-only: %d", status)
	}
	dir, _ := fs.MkdirName(root, mustName(t, "d"))
	if status := fs.SetMetadata(dir, ramfs.MetadataChange{SetReadOnly: true, ReadOnly: true}); status != ramfs.StatusIsDirectory {
		t.Fatalf("read-only directory: %d", status)
	}
	if status := fs.SetMetadata(dir, ramfs.MetadataChange{SetModified: true, Modified: stamp}); status != ramfs.StatusOK {
		t.Fatal(status)
	}
	if status := fs.SetMetadata(id, ramfs.MetadataChange{}); status != ramfs.StatusInvalidArgument {
		t.Fatalf("empty change: %d", status)
	}
	check(t, device)
}
