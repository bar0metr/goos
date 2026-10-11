package config

import "testing"

func TestDurableHostnameRefusalsAndSameValue(t *testing.T) {
	value, _ := Text([]byte("goos"))
	scope, _ := MakeExactScope(HostnameKey)
	other, _ := MakeExactScope("system.net.mode")
	request := Request{Operation: OperationSetDurable, Key: []byte(HostnameKey), Value: value}
	var store Store
	if reply := store.ServeDurable(scope, DurableWrite, Request{Operation: OperationGet}, func(*Store) Status { return StatusOK }); reply.Status != StatusInvalidArgument {
		t.Fatal("wrong operation", reply)
	}
	if reply := store.ServeDurable(other, DurableWrite, request, func(*Store) Status { return StatusOK }); reply.Status != StatusPermissionDenied {
		t.Fatal("wrong exact scope", reply)
	}
	if reply := store.ServeDurable(scope, DurableWrite, request, nil); reply.Status != StatusPersistFailed {
		t.Fatal("missing persistence", reply)
	}
	if reply := store.ServeDurable(scope, DurableWrite, request, func(*Store) Status { return StatusOK }); reply.Status != StatusNotFound {
		t.Fatal("undeclared key", reply)
	}
	store.Declare(HostnameKey, value, true)
	if reply := store.ServeDurable(scope, DurableWrite, request, func(*Store) Status { t.Fatal("read-only persisted"); return StatusOK }); reply.Status != StatusReadOnly {
		t.Fatal("read-only", reply)
	}
	store.Declare(HostnameKey, value, false)
	called := false
	if reply := store.ServeDurable(scope, DurableWrite, request, func(*Store) Status { called = true; return StatusPersistFailed }); reply.Status != StatusPersistFailed || !called {
		t.Fatal("unchanged runtime value bypassed persistence", reply)
	}
	store.revision = ^uint64(0)
	request.Value, _ = Text([]byte("new-name"))
	if reply := store.ServeDurable(scope, DurableWrite, request, func(*Store) Status { t.Fatal("overflow persisted"); return StatusOK }); reply.Status != StatusResourceExhausted {
		t.Fatal("revision overflow", reply)
	}
}
