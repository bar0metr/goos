package ramfs

import "testing"

func TestFileLifecycle(t *testing.T) {
	var fs FileSystem
	root := fs.Init()
	directory, status := fs.Mkdir(root, []byte("etc"))
	if status != StatusOK {
		t.Fatalf("mkdir: %d", status)
	}
	file, status := fs.Create(directory, []byte("motd"))
	if status != StatusOK {
		t.Fatalf("create: %d", status)
	}
	if count, status := fs.Write(file, 0, []byte("GoOS")); status != StatusOK || count != 4 {
		t.Fatalf("write: %d %d", count, status)
	}
	data := make([]byte, 8)
	if count, status := fs.Read(file, 0, data); status != StatusOK || count != 4 || string(data[:count]) != "GoOS" {
		t.Fatalf("read: %d %d %q", count, status, data)
	}
	if stat, status := fs.Stat(file); status != StatusOK || stat.Kind != KindFile || stat.Size != 4 {
		t.Fatalf("stat: %#v %d", stat, status)
	}
	if fs.Truncate(file, 2) != StatusOK || fs.Truncate(file, 4) != StatusOK {
		t.Fatal("truncate failed")
	}
	data = []byte{9, 9, 9, 9}
	if count, _ := fs.Read(file, 0, data); count != 4 || data[0] != 'G' || data[1] != 'o' || data[2] != 0 || data[3] != 0 {
		t.Fatalf("truncate did not zero extension: %v", data)
	}
}

func TestDirectoryRenameAndUnlink(t *testing.T) {
	var fs FileSystem
	root := fs.Init()
	a, _ := fs.Mkdir(root, []byte("a"))
	b, _ := fs.Mkdir(root, []byte("b"))
	file, _ := fs.Create(a, []byte("old"))
	if fs.Rename(file, b, []byte("new")) != StatusOK {
		t.Fatal("rename failed")
	}
	opened, status := fs.Open(b, []byte("new"))
	if status != StatusOK || opened != file {
		t.Fatal("renamed file not found")
	}
	entry, cursor, status := fs.ReadDir(b, 0)
	if status != StatusOK || cursor == 0 || string(entry.Name[:entry.Size]) != "new" {
		t.Fatalf("readdir: %#v %d %d", entry, cursor, status)
	}
	if fs.Unlink(root, []byte("b")) != StatusNotEmpty || fs.Unlink(b, []byte("new")) != StatusOK || fs.Unlink(root, []byte("b")) != StatusOK {
		t.Fatal("directory emptiness enforcement failed")
	}
	if _, status := fs.Stat(file); status != StatusNotFound {
		t.Fatal("stale node ID remained valid")
	}
}

func TestRejectsInvalidNamesCyclesAndBounds(t *testing.T) {
	var fs FileSystem
	root := fs.Init()
	for _, name := range [][]byte{nil, []byte("."), []byte(".."), []byte("a/b"), []byte{'a', 0, 'b'}} {
		if _, status := fs.Create(root, name); status != StatusInvalidArgument {
			t.Fatalf("invalid name accepted: %q status=%d", name, status)
		}
	}
	directory, _ := fs.Mkdir(root, []byte("dir"))
	child, _ := fs.Mkdir(directory, []byte("child"))
	if fs.Rename(directory, child, []byte("cycle")) != StatusInvalidArgument {
		t.Fatal("directory cycle accepted")
	}
	file, _ := fs.Create(root, []byte("file"))
	if _, status := fs.Write(file, MaxFileSize, []byte{1}); status != StatusResourceExhausted {
		t.Fatal("oversized file accepted")
	}
	if _, status := fs.Create(file, []byte("child")); status != StatusNotDirectory {
		t.Fatal("file accepted as parent")
	}
}

func TestNodeCapacity(t *testing.T) {
	var fs FileSystem
	root := fs.Init()
	for index := 1; index < MaxNodes; index++ {
		name := []byte{byte('a' + index/10), byte('0' + index%10)}
		if _, status := fs.Create(root, name); status != StatusOK {
			t.Fatalf("node %d: %d", index, status)
		}
	}
	if _, status := fs.Create(root, []byte("full")); status != StatusResourceExhausted {
		t.Fatal("node capacity exceeded")
	}
}

func BenchmarkMetadataCreateUnlink(b *testing.B) {
	var fs FileSystem
	root := fs.Init()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		id, status := fs.Create(root, []byte("bench"))
		if status != StatusOK || id == 0 || fs.Unlink(root, []byte("bench")) != StatusOK {
			b.Fatal("metadata cycle failed")
		}
	}
}

func TestRequestProtocolRoundTrip(t *testing.T) {
	message, ok := EncodeRequest(Request{Operation: OperationWrite, Node: 7, Offset: 3, Name: []byte("file"), Data: []byte("data")}, 11)
	if !ok {
		t.Fatal("request encode failed")
	}
	request, ok := DecodeRequest(message)
	if !ok || request.Operation != OperationWrite || request.Node != 7 || request.Offset != 3 || string(request.Name) != "file" || string(request.Data) != "data" {
		t.Fatalf("unexpected request: %#v", request)
	}
	message.Payload[4] = 1
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("nonzero reserved field accepted")
	}
}
