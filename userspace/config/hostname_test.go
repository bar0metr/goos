package config

import (
	"bytes"
	"goos/kernel/handle"
	"testing"
)

func TestHostnameValidation(t *testing.T) {
	for _, name := range []string{"a", "GOOS-9", "123", string(bytes.Repeat([]byte{'a'}, 63))} {
		if !ValidHostname([]byte(name)) {
			t.Errorf("rejected %q", name)
		}
	}
	for _, name := range []string{"", "-a", "a-", "a.b", "a_b", "a b", "a\n", "é", string(bytes.Repeat([]byte{'a'}, 64))} {
		if ValidHostname([]byte(name)) {
			t.Errorf("accepted %q", name)
		}
		var store Store
		good, _ := Text([]byte("goos"))
		store.Declare(HostnameKey, good, false)
		value, ok := Text([]byte(name))
		if ok {
			scope, _ := MakeScope("")
			if store.Set(scope, Write, []byte(HostnameKey), value) == StatusOK {
				t.Errorf("store accepted %q", name)
			}
			if store.Declare(HostnameKey, value, false) == StatusOK {
				t.Errorf("schema accepted %q", name)
			}
		}
	}
}

func TestExactHostnameSessionRoundTrip(t *testing.T) {
	scope, _ := MakeExactScope(HostnameKey)
	message, ok := AddSessionRequest(handle.ID(7), scope, Read|DurableWrite, 9)
	if !ok {
		t.Fatal("encode")
	}
	_, scope, rights, ok := DecodeAddSession(message)
	if !ok || rights != Read|DurableWrite || !scope.covers([]byte(HostnameKey)) {
		t.Fatal("decode")
	}
	for _, key := range []string{"system.hostname.child", "system.hostnames", "system.net.mode", "user.name"} {
		if scope.covers([]byte(key)) {
			t.Errorf("scope escaped to %s", key)
		}
	}
	message.Payload[2] = 0x80
	message.PayloadSize = 3
	if _, _, _, ok := DecodeAddSession(message); ok {
		t.Fatal("empty exact scope")
	}
	if _, ok := MakeExactScope("system."); ok {
		t.Fatal("prefix accepted as key")
	}
}

func TestDurableHostnamePersistenceAndCompatibility(t *testing.T) {
	var store Store
	initial, _ := Text([]byte("goos"))
	store.Declare(HostnameKey, initial, false)
	store.Declare("system.hostname.child", initial, false)
	exact, _ := MakeExactScope(HostnameKey)
	root, _ := MakeScope("")
	value, _ := Text([]byte("lab-7"))
	request := Request{Operation: OperationSetDurable, Key: []byte(HostnameKey), Value: value}
	for _, authority := range []struct {
		scope  Scope
		rights Rights
	}{
		{root, Read | Write}, {root, Read | DurableWrite}, {exact, Read | Write}, {exact, Read},
	} {
		called := false
		reply := store.ServeDurable(authority.scope, authority.rights, request, func(*Store) Status { called = true; return StatusOK })
		if reply.Status != StatusPermissionDenied || called {
			t.Fatal("unscoped mutation accepted")
		}
	}
	before, _ := store.Revision(exact, Read)
	reply := store.ServeDurable(exact, Read|DurableWrite, request, func(candidate *Store) Status {
		got, _ := candidate.Get(exact, Read, request.Key)
		if got != value {
			t.Fatal("candidate not exported")
		}
		return StatusPersistFailed
	})
	got, _ := store.Get(exact, Read, request.Key)
	after, _ := store.Revision(exact, Read)
	if reply.Status != StatusPersistFailed || got != initial || before != after {
		t.Fatal("failure published value or revision")
	}
	var persisted []byte
	reply = store.ServeDurable(exact, Read|DurableWrite, request, func(candidate *Store) Status {
		got, _ := candidate.Get(exact, Read, request.Key)
		persisted, _ = AppendEntry(nil, request.Key, got)
		return StatusOK
	})
	if reply.Status != StatusOK {
		t.Fatal(reply)
	}
	var restarted Store
	restarted.Declare(HostnameKey, initial, false)
	key, restored, ok := ParseEntry(bytes.TrimSuffix(persisted, []byte{'\n'}))
	if !ok || restarted.Set(root, Write, key, restored) != StatusOK {
		t.Fatal("restore")
	}
	got, _ = restarted.Get(exact, Read, request.Key)
	if got != value {
		t.Fatal("hostname did not survive restart")
	}
	if store.Serve(exact, Read|DurableWrite, Request{Operation: OperationSet, Key: request.Key, Value: initial}).Status != StatusPermissionDenied {
		t.Fatal("durable session can Set")
	}
	if store.Serve(root, Write, Request{Operation: OperationSet, Key: request.Key, Value: initial}).Status != StatusOK {
		t.Fatal("existing Set changed")
	}
	bad, _ := Text([]byte("bad.name"))
	request.Value = bad
	if store.ServeDurable(exact, DurableWrite, request, func(*Store) Status { t.Fatal("invalid candidate persisted"); return StatusOK }).Status != StatusInvalidArgument {
		t.Fatal("invalid hostname accepted")
	}
	request.Key = []byte("system.hostname.child")
	if store.ServeDurable(exact, DurableWrite, request, func(*Store) Status { t.Fatal("descendant persisted"); return StatusOK }).Status != StatusNotFound {
		t.Fatal("descendant authority")
	}
}

func TestDurableProtocolRoundTrip(t *testing.T) {
	value, _ := Text([]byte("goos-1"))
	message, ok := EncodeRequest(Request{Operation: OperationSetDurable, Key: []byte(HostnameKey), Value: value}, 1)
	if !ok {
		t.Fatal("encode")
	}
	request, ok := DecodeRequest(message)
	if !ok || request.Operation != OperationSetDurable || request.Value != value {
		t.Fatal("decode")
	}
	for _, status := range []Status{StatusOK, StatusPersistFailed} {
		message, ok := EncodeReply(Reply{Operation: OperationSetDurable, Status: status}, 1)
		if !ok {
			t.Fatal("reply encode")
		}
		reply, ok := DecodeReply(message)
		if !ok || reply.Status != status {
			t.Fatal("reply decode")
		}
	}
}
