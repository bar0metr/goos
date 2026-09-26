package ipc

import (
	"goos/kernel/objects"
	"testing"
)

// TestRegistryTakesMemoryAsItGrows covers ADR 0111: a channel's memory is
// taken when it is created and returned when it is released, the table holds
// MaxChannels at once and refuses the next, a released ID never names a later
// channel, and a failing storage is a refused create, not a broken table.
func TestRegistryTakesMemoryAsItGrows(t *testing.T) {
	storage := &objects.Limited[Channel]{}
	var registry Registry
	registry.SetStorage(storage)
	ids := make([]uint64, MaxChannels)
	for index := range ids {
		_, id, ok := registry.Create(uint64(index + 1))
		if !ok {
			t.Fatalf("create %d refused", index)
		}
		ids[index] = id
	}
	if storage.Live != MaxChannels {
		t.Fatalf("%d channels allocated, want %d", storage.Live, MaxChannels)
	}
	if _, _, ok := registry.Create(1); ok {
		t.Fatal("a channel past MaxChannels was created")
	}
	for index, id := range ids {
		channel, found := registry.Lookup(id)
		if !found {
			t.Fatalf("channel %d not found", index)
		}
		channel.Close(0)
		channel.Close(1)
		if owner, released := registry.Release(id); !released || owner != uint64(index+1) {
			t.Fatalf("release %d: owner %d, %v", index, owner, released)
		}
		if _, found := registry.Lookup(id); found {
			t.Fatalf("released channel %d still found", index)
		}
	}
	if storage.Live != 0 {
		t.Fatalf("%d channels not returned", storage.Live)
	}
	_, reused, ok := registry.Create(1)
	if !ok {
		t.Fatal("create after release refused")
	}
	for _, id := range ids {
		if id == reused {
			t.Fatal("an ID was reused")
		}
	}
	// A stale ID that lands on a live slot finds nothing.
	if _, found := registry.Lookup(ids[0]); found {
		t.Fatal("a stale ID found the slot's new channel")
	}
	storage.Limit = storage.Live
	if _, _, ok := registry.Create(2); ok {
		t.Fatal("a create succeeded without memory")
	}
	if _, found := registry.Lookup(0); found {
		t.Fatal("ID zero found a channel")
	}
}
