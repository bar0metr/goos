package vfs

import (
	"bytes"
	"goos/internal/limits"
	"goos/kernel/ipc"
	"testing"
)

func TestRenameTargetTravelsInItsOwnField(t *testing.T) {
	message, ok := EncodeRequest(Request{Operation: OperationRename, Path: []byte("/tmp/a"), Target: []byte("/tmp/a-much-longer-target-than-thirty-two-bytes")}, 3)
	if !ok {
		t.Fatal("encode")
	}
	request, ok := DecodeRequest(message)
	if !ok || string(request.Path) != "/tmp/a" || string(request.Target) != "/tmp/a-much-longer-target-than-thirty-two-bytes" || len(request.Data) != 0 {
		t.Fatalf("decoded %q -> %q", request.Path, request.Target)
	}
}

// TestLongPathsTravelAttached covers paths too long for a message: the
// request says how long they are, and the receiver copies them out of the
// attached object.
func TestLongPathsTravelAttached(t *testing.T) {
	path := append([]byte("/"), bytes.Repeat([]byte{'p'}, limits.PathMax-1)...)
	target := append([]byte("/"), bytes.Repeat([]byte{'t'}, 300)...)
	request := Request{Operation: OperationRename, Path: path, Target: target}
	if FitsInline(request) {
		t.Fatal("a PathMax path fits a message")
	}
	if _, ok := EncodeRequest(request, 1); ok {
		t.Fatal("long paths encoded without an attachment")
	}
	request.PathsAttached = true
	message, ok := EncodeRequest(request, 1)
	if !ok || message.PayloadSize != protocolHeader {
		t.Fatalf("attached encode: %v, %d", ok, message.PayloadSize)
	}
	decoded, ok := DecodeRequest(message)
	if !ok || !decoded.PathsAttached || decoded.Path != nil || !decoded.Attaches() {
		t.Fatal("attached request not decoded as such")
	}
	if pathSize, targetSize := decoded.AttachedPaths(); pathSize != len(path) || targetSize != len(target) {
		t.Fatalf("sizes %d %d", pathSize, targetSize)
	}
	object := append(append([]byte(nil), path...), target...)
	var pathCopy, targetCopy [limits.PathMax]byte
	if decoded.TakePaths(object[:len(object)-1], &pathCopy, &targetCopy) {
		t.Fatal("paths taken from an object too short for them")
	}
	if !decoded.TakePaths(object, &pathCopy, &targetCopy) || !bytes.Equal(decoded.Path, path) || !bytes.Equal(decoded.Target, target) {
		t.Fatal("paths not taken")
	}
	object[1] = 'x'
	if decoded.Path[1] != 'p' {
		t.Fatal("taken path aliases the shared object")
	}
	// Paths that fit may not claim an attachment.
	short := Request{Operation: OperationStat, Path: []byte("/tmp"), PathsAttached: true}
	message, _ = EncodeRequest(short, 1)
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("a short path claimed an attachment")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationReadInto, PathsAttached: true, Path: path}, 1); ok {
		t.Fatal("a bulk request with attached paths encoded")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationStat, Path: path[:limits.PathMax], Target: nil, PathsAttached: true}, 1); !ok {
		t.Fatal("a PathMax path refused")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationStat, Path: append(path, 'x'), PathsAttached: true}, 1); ok {
		t.Fatal("a path past PathMax encoded")
	}
}

func TestLongListedNamesComeAttached(t *testing.T) {
	message, ok := EncodeRequest(Request{Operation: OperationReadDir, Path: []byte("/tmp"), NameReturn: true}, 2)
	if !ok {
		t.Fatal("encode")
	}
	if request, ok := DecodeRequest(message); !ok || !request.NameReturn || !request.Attaches() {
		t.Fatal("name return lost")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationStat, Path: []byte("/tmp"), NameReturn: true}, 2); ok {
		t.Fatal("name return on a stat encoded")
	}
	reply, ok := EncodeReply(Reply{Operation: OperationReadDir, AttachedName: limits.NameMax, Cursor: 4}, 2)
	if !ok || reply.PayloadSize != replyHeader {
		t.Fatal("attached name reply")
	}
	decoded, ok := DecodeReply(reply)
	if !ok || decoded.AttachedName != limits.NameMax || len(decoded.Name) != 0 || decoded.Cursor != 4 {
		t.Fatalf("decoded %+v", decoded)
	}
	if _, ok := EncodeReply(Reply{Operation: OperationReadDir, AttachedName: 10}, 2); ok {
		t.Fatal("a short name claimed an attachment")
	}
	long := bytes.Repeat([]byte{'n'}, MaxInlineName)
	if _, ok := EncodeReply(Reply{Operation: OperationReadDir, Name: long}, 2); !ok {
		t.Fatal("a name that fits was refused")
	}
	if _, ok := EncodeReply(Reply{Operation: OperationReadDir, Name: append(long, 'n')}, 2); ok {
		t.Fatal("a name past the message encoded inline")
	}
	for _, damage := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Payload[7] = 2 },
		func(m *ipc.Message) { m.Payload[3], m.Payload[4] = 10, 0 },
	} {
		corrupt := reply
		damage(&corrupt)
		if _, ok := DecodeReply(corrupt); ok {
			t.Fatal("damaged reply accepted")
		}
	}
	request, _ := EncodeRequest(Request{Operation: OperationStat, Path: []byte("/tmp")}, 2)
	request.Payload[28] = 4
	if _, ok := DecodeRequest(request); ok {
		t.Fatal("unknown request flag accepted")
	}
}
