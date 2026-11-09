package ramfs

import (
	"bytes"
	"testing"
)

func TestSymlinkKeepsItsTarget(t *testing.T) {
	fs := &FileSystem{}
	root := fs.Init()
	name, _ := MakeName([]byte("link"))
	target := bytes.Repeat([]byte("a/"), 1000)
	id, status := fs.SymlinkName(root, name, target)
	if status != StatusOK {
		t.Fatal(status)
	}
	stat, _ := fs.Stat(id)
	if stat.Kind != KindSymlink || stat.Size != uint64(len(target)) || !stat.Valid() {
		t.Fatalf("stat %+v", stat)
	}
	buffer := make([]byte, MaxLinkTarget)
	if count, status := fs.ReadLink(id, buffer); status != StatusOK || !bytes.Equal(buffer[:count], target) {
		t.Fatalf("readlink %d bytes, %v", count, status)
	}
	if _, status := fs.ReadLink(id, buffer[:10]); status != StatusResourceExhausted {
		t.Fatalf("short buffer: %v", status)
	}
	if _, status := fs.Read(id, 0, buffer); status == StatusOK {
		t.Fatal("a link read as a file")
	}
	if _, status := fs.OpenName(id, name); status != StatusNotDirectory {
		t.Fatalf("lookup below a link: %v", status)
	}
	file, _ := fs.Create(root, []byte("file"))
	if _, status := fs.ReadLink(file, buffer); status != StatusInvalidArgument {
		t.Fatalf("readlink of a file: %v", status)
	}
	if _, status := fs.SymlinkName(root, name, []byte("x")); status != StatusAlreadyExists {
		t.Fatalf("second link of one name: %v", status)
	}
	for _, bad := range [][]byte{nil, {'a', 0}, bytes.Repeat([]byte("x"), MaxLinkTarget+1)} {
		other, _ := MakeName([]byte("bad"))
		if _, status := fs.SymlinkName(root, other, bad); status != StatusInvalidArgument {
			t.Fatalf("target of %d bytes: %v", len(bad), status)
		}
	}
	pages := fs.Usage().Pages
	if fs.UnlinkName(root, name) != StatusOK || fs.Usage().Pages != pages-1 {
		t.Fatalf("unlink kept the target's page: %d -> %d", pages, fs.Usage().Pages)
	}
}

func TestSymlinkRefusedWhenFull(t *testing.T) {
	fs := &FileSystem{}
	if !fs.Configure(&heapStore{}, Limits{Nodes: 8, Pages: 1}) {
		t.Fatal("configure")
	}
	root := fs.Init()
	file, _ := fs.Create(root, []byte("f"))
	if _, status := fs.Write(file, 0, []byte("x")); status != StatusOK {
		t.Fatal(status)
	}
	name, _ := MakeName([]byte("link"))
	if _, status := fs.SymlinkName(root, name, []byte("f")); status != StatusNoSpace {
		t.Fatalf("link on a full instance: %v", status)
	}
	if _, status := fs.OpenName(root, name); status != StatusNotFound {
		t.Fatalf("refused link left a name: %v", status)
	}
}

func TestUnixRecordRoundTrip(t *testing.T) {
	unix := Unix{Mode: ModeCharacter | 0o620, UID: 1, GID: 2, Links: 3, Generation: 4, Device: 0x11032c,
		Accessed: Timestamp{Seconds: -5, Nanoseconds: 6, Available: true}, Changed: Timestamp{Seconds: 7, Available: true}, Available: true}
	var record [UnixRecordSize]byte
	if !EncodeUnix(record[:], &unix) {
		t.Fatal("encode")
	}
	decoded, ok := DecodeUnix(record[:], KindSpecial)
	if !ok || decoded != unix {
		t.Fatalf("decoded %+v", decoded)
	}
	if _, ok := DecodeUnix(record[:], KindFile); ok {
		t.Fatal("a device's mode accepted for a file")
	}
	absent := Unix{}
	EncodeUnix(record[:], &absent)
	if decoded, ok := DecodeUnix(record[:], KindFile); !ok || decoded.Available {
		t.Fatalf("absent metadata %+v %v", decoded, ok)
	}
	record[4] = 1
	if _, ok := DecodeUnix(record[:], KindFile); ok {
		t.Fatal("an owner without metadata accepted")
	}
	for _, bad := range []Unix{
		{Mode: ModeRegular, Device: 1, Available: true},
		{Mode: 0x7000, Available: true},
		{Mode: ModeDirectory, Available: true},
		{UID: 1},
	} {
		if bad.Valid(KindFile) {
			t.Fatalf("%+v valid for a file", bad)
		}
	}
}
