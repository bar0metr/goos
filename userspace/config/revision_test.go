package config

import "testing"

func TestScopedRevisionBracketsCandidate(t *testing.T) {
	var store Store
	store.Declare("system.remote.port", Uint(22), false)
	store.Declare("system.other.value", Uint(1), false)
	scope, _ := MakeScope("system.remote.")
	all, _ := MakeScope("")
	before, status := store.Revision(scope, Read)
	if status != StatusOK || before == 0 {
		t.Fatal("initial revision", before, status)
	}
	store.Set(all, Write, []byte("system.other.value"), Uint(2))
	after, _ := store.Revision(scope, Read)
	if after != before {
		t.Fatal("unrelated key changed scoped revision")
	}
	store.Set(all, Write, []byte("system.remote.port"), Uint(2222))
	store.Set(all, Write, []byte("system.remote.port"), Uint(22))
	after, _ = store.Revision(scope, Read)
	if after <= before {
		t.Fatal("ABA not detected")
	}
	store.Set(all, Write, []byte("system.remote.port"), Uint(22))
	unchanged, _ := store.Revision(scope, Read)
	if unchanged != after {
		t.Fatal("same value changed revision")
	}
	if _, status = store.Revision(scope, Write); status != StatusPermissionDenied {
		t.Fatal("revision without read")
	}
}

func TestRevisionProtocolRoundTrip(t *testing.T) {
	var store Store
	store.Declare("system.remote.port", Uint(22), false)
	scope, _ := MakeScope("system.remote.")
	message, ok := EncodeRequest(Request{Operation: OperationRevision}, 3)
	if !ok {
		t.Fatal("request encode")
	}
	request, ok := DecodeRequest(message)
	if !ok {
		t.Fatal("request decode")
	}
	reply := store.Serve(scope, Read, request)
	message, ok = EncodeReply(reply, 3)
	if !ok {
		t.Fatal("reply encode")
	}
	decoded, ok := DecodeReply(message)
	if !ok || decoded.Value.Kind != KindUint || decoded.Value.Uint == 0 {
		t.Fatal("reply decode", decoded)
	}
	for _, bad := range []Request{
		{Operation: OperationRevision, Key: []byte("system.remote.port")},
		{Operation: OperationRevision, Value: Uint(1)},
		{Operation: OperationRevision, Cursor: 1},
	} {
		if _, ok := EncodeRequest(bad, 3); ok {
			t.Fatal("revision with extra fields")
		}
	}
	message, _ = EncodeRequest(Request{Operation: OperationRevision}, 3)
	message.Payload[16] = 1
	if _, ok := DecodeRequest(message); ok {
		t.Fatal("revision cursor accepted")
	}
}

func TestRevisionWrapFailsWithoutMutation(t *testing.T) {
	var store Store
	store.Declare("system.remote.port", Uint(22), false)
	store.revision = ^uint64(0)
	scope, _ := MakeScope("")
	if status := store.Set(scope, Write, []byte("system.remote.port"), Uint(2222)); status != StatusResourceExhausted {
		t.Fatal("wrap set", status)
	}
	value, _ := store.Get(scope, Read, []byte("system.remote.port"))
	if value.Uint != 22 {
		t.Fatal("failed set mutated value")
	}
	if status := store.Declare("system.remote.port", Uint(2222), false); status != StatusResourceExhausted {
		t.Fatal("wrap declare", status)
	}
	if status := store.Declare("system.remote.enabled", Bool(false), false); status != StatusResourceExhausted {
		t.Fatal("wrap new declaration", status)
	}
}
