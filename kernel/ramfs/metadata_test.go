package ramfs

import "testing"

func TestSetMetadataTimeAndReadOnly(t *testing.T) {
	fs := &FileSystem{}
	root := fs.Init()
	id, _ := fs.Create(root, []byte("f"))
	stamp := Timestamp{Seconds: 42, Nanoseconds: 7, Available: true}
	if status := fs.SetMetadata(id, MetadataChange{SetModified: true, Modified: stamp, SetReadOnly: true, ReadOnly: true}); status != StatusOK {
		t.Fatal(status)
	}
	stat, _ := fs.Stat(id)
	if stat.Modified != stamp || stat.Attributes&AttrReadOnly == 0 {
		t.Fatalf("stat = %+v", stat)
	}
	if _, status := fs.Write(id, 0, []byte("x")); status != StatusPermissionDenied {
		t.Fatalf("write to read-only: %d", status)
	}
	if status := fs.Truncate(id, 0); status != StatusPermissionDenied {
		t.Fatalf("truncate of read-only: %d", status)
	}
	fs.SetMetadata(id, MetadataChange{SetReadOnly: true})
	if _, status := fs.Write(id, 0, []byte("x")); status != StatusOK {
		t.Fatalf("write after clearing: %d", status)
	}
	if status := fs.SetMetadata(root, MetadataChange{SetReadOnly: true, ReadOnly: true}); status != StatusIsDirectory {
		t.Fatalf("read-only directory: %d", status)
	}
	if status := fs.SetMetadata(id, MetadataChange{SetModified: true, Modified: Timestamp{Seconds: 1, Nanoseconds: 2e9, Available: true}}); status != StatusInvalidArgument {
		t.Fatalf("bad nanoseconds: %d", status)
	}
}
