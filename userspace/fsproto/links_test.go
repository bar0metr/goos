package fsproto

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

// linkVolumeFake keeps one symbolic link and records the link calls.
type linkVolumeFake struct {
	refusedVolume
	target []byte
	linked ramfs.NodeID
}

func (volume *linkVolumeFake) ReadLink(id ramfs.NodeID, target []byte) (int, ramfs.Status) {
	if id != 7 {
		return 0, ramfs.StatusInvalidArgument
	}
	return copy(target, "../target"), ramfs.StatusOK
}

func (volume *linkVolumeFake) SymlinkName(parent ramfs.NodeID, name ramfs.Name, target []byte) (ramfs.NodeID, ramfs.Status) {
	volume.target = append([]byte(nil), target...)
	return 8, ramfs.StatusOK
}

func (volume *linkVolumeFake) LinkName(id ramfs.NodeID, parent ramfs.NodeID, name ramfs.Name) ramfs.Status {
	volume.linked = id
	return ramfs.StatusOK
}

func TestLinkOperationsThroughTheCache(t *testing.T) {
	var name [ramfs.MaxName]byte
	cache := make([]byte, CacheSize)
	volume := &linkVolumeFake{}
	reply := Serve(volume, Request{Operation: OperationReadLink, Node: 7, Slot: 3, Size: ramfs.MaxLinkTarget}, cache, &name)
	if reply.Status != ramfs.StatusOK || reply.Value != 9 || string(cache[3*PageSize:3*PageSize+9]) != "../target" {
		t.Fatalf("readlink %+v", reply)
	}
	copy(cache[5*PageSize:], "/abs/path")
	reply = Serve(volume, Request{Operation: OperationSymlink, Parent: 1, Name: []byte("l"), Slot: 5, Size: 9}, cache, &name)
	if reply.Status != ramfs.StatusOK || reply.Node != 8 || string(volume.target) != "/abs/path" {
		t.Fatalf("symlink %+v target %q", reply, volume.target)
	}
	if reply := Serve(volume, Request{Operation: OperationLink, Node: 4, Parent: 1, Name: []byte("h")}, cache, &name); reply.Status != ramfs.StatusOK || volume.linked != 4 {
		t.Fatalf("link %+v", reply)
	}
	plain := &refusedVolume{}
	for _, request := range []Request{
		{Operation: OperationReadLink, Node: 7, Slot: 3, Size: 10},
		{Operation: OperationSymlink, Parent: 1, Name: []byte("l"), Slot: 5, Size: 9},
		{Operation: OperationLink, Node: 4, Parent: 1, Name: []byte("h")},
	} {
		if reply := Serve(plain, request, cache, &name); reply.Status != ramfs.StatusUnsupported {
			t.Fatalf("operation %d on a volume without links: %v", request.Operation, reply.Status)
		}
	}
	for _, request := range []Request{
		{Operation: OperationReadLink, Node: 7, Slot: 3, Size: ramfs.MaxLinkTarget + 1},
		{Operation: OperationReadLink, Node: 7, Slot: 3, Offset: 1, Size: 1},
		{Operation: OperationSymlink, Parent: 1, Name: []byte("l"), Slot: CachePages, Size: 1},
		{Operation: OperationSymlink, Parent: 1, Name: []byte("l"), Slot: 1},
	} {
		var message ipc.Message
		if EncodeRequest(request, 1, &message, cache) {
			t.Fatalf("encoded %+v", request)
		}
	}
}

func TestReplyCarriesUnixMetadata(t *testing.T) {
	cache := make([]byte, CacheSize)
	unix := ramfs.Unix{Mode: ramfs.ModeSymlink | 0o777, UID: 42, GID: 43, Links: 1, Generation: 9,
		Accessed: ramfs.Timestamp{Seconds: 1, Available: true}, Changed: ramfs.Timestamp{Seconds: 2, Available: true}, Available: true}
	var message ipc.Message
	if !EncodeReply(Reply{Operation: OperationStat, Kind: ramfs.KindSymlink, Node: 5, Value: 8, Unix: unix}, 3, &message, cache) {
		t.Fatal("encode")
	}
	var name [ramfs.MaxName]byte
	reply, ok := DecodeReply(&message, cache, &name)
	if !ok || reply.Unix != unix || reply.Kind != ramfs.KindSymlink {
		t.Fatalf("decoded %+v %v", reply, ok)
	}
	if EncodeReply(Reply{Operation: OperationStat, Kind: ramfs.KindFile, Unix: unix}, 3, &message, cache) {
		t.Fatal("a link's mode encoded for a file")
	}
}
