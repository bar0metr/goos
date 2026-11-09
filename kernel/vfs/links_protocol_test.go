package vfs

import (
	"testing"

	"goos/kernel/ramfs"
)

func TestStatReplyCarriesUnixMetadata(t *testing.T) {
	unix := ramfs.Unix{Mode: ramfs.ModeRegular | 0o640, UID: 1234, GID: 5678, Links: 3, Generation: 9,
		Accessed: ramfs.Timestamp{Seconds: 1767225600, Available: true}, Changed: ramfs.Timestamp{Seconds: 1, Nanoseconds: 2, Available: true}, Available: true}
	message, ok := EncodeReply(Reply{Operation: OperationStat, Kind: ramfs.KindFile, Node: 9<<32 | 12, Size: 11, Unix: unix}, 2)
	if !ok || int(message.PayloadSize) != replyHeader+ramfs.UnixRecordSize {
		t.Fatalf("encode %v size %d", ok, message.PayloadSize)
	}
	reply, ok := DecodeReply(message)
	if !ok || reply.Unix != unix || len(reply.Data) != 0 || reply.Node != 9<<32|12 {
		t.Fatalf("decoded %+v", reply)
	}
	plain, _ := EncodeReply(Reply{Operation: OperationStat, Kind: ramfs.KindFile}, 2)
	if reply, ok := DecodeReply(plain); !ok || reply.Unix.Available {
		t.Fatal("a stat without metadata")
	}
	for _, bad := range []Reply{
		{Operation: OperationStat, Kind: ramfs.KindDirectory, Unix: unix},
		{Operation: OperationReadDir, Kind: ramfs.KindFile, Unix: unix},
		{Operation: OperationStat, Kind: ramfs.KindFile, Unix: unix, Data: []byte("x")},
		{Operation: OperationStat, Kind: ramfs.LastKind + 1},
	} {
		if _, ok := EncodeReply(bad, 2); ok {
			t.Fatalf("encoded %+v", bad)
		}
	}
	short := message
	short.PayloadSize--
	if _, ok := DecodeReply(short); ok {
		t.Fatal("a cut record accepted")
	}
}

func TestLinkRequestsAndNoFollow(t *testing.T) {
	for _, request := range []Request{
		{Operation: OperationStat, Rights: NoFollow, Path: []byte("/a")},
		{Operation: OperationOpen, Rights: Read | NoFollow, Path: []byte("/a")},
		{Operation: OperationLink, Rights: NoFollow, Path: []byte("/a"), Target: []byte("/b")},
		{Operation: OperationSymlink, Path: []byte("/a"), Target: []byte("../x")},
		{Operation: OperationReadLink, Path: []byte("/a"), NameReturn: true},
		{Operation: OperationSetMetadata, Rights: MetadataModified | NoFollow, Path: []byte("/a")},
	} {
		message, ok := EncodeRequest(request, 3)
		if !ok {
			t.Fatalf("encode %+v", request)
		}
		decoded, ok := DecodeRequest(message)
		if !ok || decoded.Rights != request.Rights || string(decoded.Target) != string(request.Target) {
			t.Fatalf("decoded %+v", decoded)
		}
	}
	if change, ok := MetadataChangeOf(Request{Operation: OperationSetMetadata, Rights: MetadataModified | NoFollow}); !ok || !change.SetModified {
		t.Fatal("no-follow metadata change")
	}
	for _, request := range []Request{
		{Operation: OperationCreate, Rights: NoFollow, Path: []byte("/a")},
		{Operation: OperationUnlink, Rights: NoFollow, Path: []byte("/a")},
	} {
		message, _ := EncodeRequest(request, 3)
		if _, ok := DecodeRequest(message); ok {
			t.Fatalf("decoded rights %#x on operation %d", request.Rights, request.Operation)
		}
	}
	long := make([]byte, ramfs.MaxLinkTarget)
	for index := range long {
		long[index] = 'x'
	}
	message, ok := EncodeReply(Reply{Operation: OperationReadLink, AttachedName: uint16(len(long))}, 3)
	if reply, decoded := DecodeReply(message); !ok || !decoded || int(reply.AttachedName) != len(long) {
		t.Fatal("a long target attached")
	}
	if _, ok := EncodeReply(Reply{Operation: OperationReadDir, AttachedName: uint16(len(long))}, 3); ok {
		t.Fatal("a listed name past NameMax")
	}
}
