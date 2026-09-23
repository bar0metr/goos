package config

import (
	"goos/kernel/ipc"
	"testing"
)

func TestRequestRoundTripAndReservedFields(t *testing.T) {
	value, _ := Text([]byte("goos"))
	message, ok := EncodeRequest(Request{Operation: OperationSet, Key: []byte("user.name"), Value: value}, 5)
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeRequest(message)
	if !ok || decoded.Operation != OperationSet || string(decoded.Key) != "user.name" ||
		decoded.Value.Kind != KindText || string(decoded.Value.Bytes()) != "goos" {
		t.Fatalf("decoded %+v", decoded)
	}
	// Reserved bytes are part of the contract: a sender that sets them is
	// speaking a version this one does not know.
	for _, offset := range []int{4, 5, 6, 7, 20, 27} {
		corrupt := message
		corrupt.Payload[offset] = 1
		if _, ok := DecodeRequest(corrupt); ok {
			t.Fatalf("reserved byte %d ignored", offset)
		}
	}
	corrupt := message
	corrupt.Version = ProtocolVersion + 1
	if _, ok := DecodeRequest(corrupt); ok {
		t.Fatal("unknown version accepted")
	}
	corrupt = message
	corrupt.Kind = ipc.KindReply
	if _, ok := DecodeRequest(corrupt); ok {
		t.Fatal("reply accepted as a request")
	}
}

func TestReplyRoundTripAndReservedFields(t *testing.T) {
	message, ok := EncodeReply(Reply{Operation: OperationList, Status: StatusOK, Key: []byte("system.log.persist"), Value: Bool(true), ReadOnly: true, Cursor: 3}, 9)
	if !ok {
		t.Fatal("encode failed")
	}
	decoded, ok := DecodeReply(message)
	if !ok || decoded.Status != StatusOK || string(decoded.Key) != "system.log.persist" ||
		decoded.Value.Kind != KindBool || decoded.Value.Uint != 1 || !decoded.ReadOnly || decoded.Cursor != 3 {
		t.Fatalf("decoded %+v", decoded)
	}
	for _, offset := range []int{6, 7, 20, 25} {
		corrupt := message
		corrupt.Payload[offset] = 1
		if _, ok := DecodeReply(corrupt); ok {
			t.Fatalf("reserved byte %d ignored", offset)
		}
	}
	corrupt := message
	corrupt.Payload[5] = 2
	if _, ok := DecodeReply(corrupt); ok {
		t.Fatal("out-of-range read-only flag accepted")
	}
	corrupt = message
	corrupt.Payload[1] = byte(StatusReadOnly) + 1
	if _, ok := DecodeReply(corrupt); ok {
		t.Fatal("unknown status accepted")
	}
}

func TestMalformedRequestsAreRefused(t *testing.T) {
	if _, ok := EncodeRequest(Request{Operation: OperationGet, Key: []byte("nodots")}, 1); ok {
		t.Fatal("key without a namespace accepted")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationGet, Key: []byte("user.name")}, 0); ok {
		t.Fatal("zero correlation accepted")
	}
	if _, ok := EncodeRequest(Request{Operation: OperationSet, Key: []byte("user.name"), Value: Value{Kind: KindBool, Uint: 7}}, 1); ok {
		t.Fatal("malformed bool accepted")
	}
	if _, ok := EncodeRequest(Request{Operation: Operation(0)}, 1); ok {
		t.Fatal("unknown operation accepted")
	}
	// A size field that does not match the payload is a truncated or padded
	// message, not a short value.
	message, _ := EncodeRequest(Request{Operation: OperationGet, Key: []byte("user.name")}, 1)
	message.PayloadSize++
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("payload size mismatch accepted")
	}
	message, _ = EncodeRequest(Request{Operation: OperationGet, Key: []byte("user.name")}, 1)
	message.Payload[1] = MaxKey + 1
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("oversized key accepted")
	}
}

func TestServeAppliesSessionAuthority(t *testing.T) {
	store := seeded(t)
	userScope, _ := MakeScope("user.")
	readOnlySession := store.Serve(userScope, Read, Request{Operation: OperationSet, Key: []byte("user.name"), Value: Bool(true)})
	if readOnlySession.Status != StatusPermissionDenied {
		t.Fatalf("a read-only session wrote: %d", readOnlySession.Status)
	}
	outside := store.Serve(userScope, Read|Write, Request{Operation: OperationGet, Key: []byte("system.shell.history")})
	if outside.Status != StatusNotFound {
		t.Fatalf("a confined session read outside its scope: %d", outside.Status)
	}
	value, _ := Text([]byte("nikita"))
	if reply := store.Serve(userScope, Read|Write, Request{Operation: OperationSet, Key: []byte("user.name"), Value: value}); reply.Status != StatusOK {
		t.Fatalf("set: %d", reply.Status)
	}
	reply := store.Serve(userScope, Read, Request{Operation: OperationGet, Key: []byte("user.name")})
	if reply.Status != StatusOK || string(reply.Value.Bytes()) != "nikita" || string(reply.Key) != "user.name" {
		t.Fatalf("get: %+v", reply)
	}
	listed := store.Serve(userScope, Read, Request{Operation: OperationList})
	if listed.Status != StatusOK || string(listed.Key) != "user.name" || listed.Cursor == 0 {
		t.Fatalf("list: %+v", listed)
	}
	if end := store.Serve(userScope, Read, Request{Operation: OperationList, Cursor: listed.Cursor}); end.Status != StatusNotFound {
		t.Fatalf("list end: %d", end.Status)
	}
}
