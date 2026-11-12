package fsproto

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

// entryVolumeStub records the entry rename it was asked for.
type entryVolumeStub struct {
	refusedVolume
	source   string
	from, id ramfs.NodeID
	unmounts int
}

func (volume *entryVolumeStub) RenameEntry(from ramfs.NodeID, source []byte, id, _ ramfs.NodeID, _ ramfs.Name) ramfs.Status {
	volume.from, volume.id, volume.source = from, id, string(source)
	return ramfs.StatusOK
}

func (volume *entryVolumeStub) ReplaceEntry(from ramfs.NodeID, source []byte, id, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	volume.RenameEntry(from, source, id, parent, name)
	return ramfs.StatusAlreadyExists
}

func (volume *entryVolumeStub) Unmount() ramfs.Status { volume.unmounts++; return ramfs.StatusOK }

func TestEntryRenameCarriesItsSource(t *testing.T) {
	cache := make([]byte, CacheSize)
	copy(cache[3*PageSize:], "old-name")
	request := Request{Operation: OperationRenameEntry, Node: 9, Parent: 2, Offset: 5, Slot: 3, Size: 8, Name: []byte("new")}
	var message ipc.Message
	var name [ramfs.MaxName]byte
	if !EncodeRequest(request, 1, &message, cache) {
		t.Fatal("encode")
	}
	decoded, ok := DecodeRequest(&message, cache, &name)
	if !ok || decoded.Offset != 5 || decoded.Slot != 3 || decoded.Size != 8 {
		t.Fatalf("decoded %+v %v", decoded, ok)
	}
	volume := &entryVolumeStub{}
	if reply := Serve(volume, decoded, cache, &name); reply.Status != ramfs.StatusOK || volume.source != "old-name" || volume.from != 5 || volume.id != 9 {
		t.Fatalf("serve %d: %+v", reply.Status, volume)
	}
	decoded.Operation = OperationReplaceEntry
	if reply := Serve(volume, decoded, cache, &name); reply.Status != ramfs.StatusAlreadyExists {
		t.Fatalf("replace %d", reply.Status)
	}
	for _, bad := range []Request{
		{Operation: OperationRenameEntry, Node: 9, Parent: 2, Slot: 3, Size: 8, Name: []byte("n")},
		{Operation: OperationRenameEntry, Node: 9, Parent: 2, Offset: 5, Slot: 3, Name: []byte("n")},
		{Operation: OperationRenameEntry, Node: 9, Parent: 2, Offset: 5, Slot: 3, Size: ramfs.MaxName + 1, Name: []byte("n")},
		{Operation: OperationRenameEntry, Node: 9, Parent: 2, Offset: 5, Slot: CachePages, Size: 8, Name: []byte("n")},
	} {
		if EncodeRequest(bad, 2, &message, cache) {
			t.Fatalf("encoded %+v", bad)
		}
	}
	// A volume without entry renames says so, and the VFS falls back.
	if reply := Serve(&refusedVolume{}, Request{Operation: OperationRenameEntry, Node: 9, Parent: 2, Offset: 5, Slot: 3, Size: 8, Name: []byte("new")}, cache, &name); reply.Status != ramfs.StatusUnsupported {
		t.Fatalf("unsupported volume %d", reply.Status)
	}
}

func TestUnmount(t *testing.T) {
	cache := make([]byte, CacheSize)
	var name [ramfs.MaxName]byte
	volume := &entryVolumeStub{}
	if reply := Serve(volume, Request{Operation: OperationUnmount}, cache, &name); reply.Status != ramfs.StatusOK || volume.unmounts != 1 {
		t.Fatalf("unmount %d, %d calls", reply.Status, volume.unmounts)
	}
	plain := &refusedVolume{}
	if reply := Serve(plain, Request{Operation: OperationUnmount}, cache, &name); reply.Status != ramfs.StatusOK || !plain.flushed {
		t.Fatal("a volume without a clean state is flushed")
	}
}

// A version 13 request is refused: version 14 added the entry operations
// and the clean unmount.
func TestRequestRefusesVersion13(t *testing.T) {
	cache := make([]byte, CacheSize)
	var message ipc.Message
	var name [ramfs.MaxName]byte
	if !EncodeRequest(Request{Operation: OperationUnmount}, 1, &message, cache) || message.Version != 14 {
		t.Fatalf("encoded version %d", message.Version)
	}
	message.Version = 13
	if _, ok := DecodeRequest(&message, cache, &name); ok {
		t.Fatal("version 13 request accepted")
	}
}
