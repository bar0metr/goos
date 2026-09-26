package stream

import (
	"goos/kernel/objects"
	"testing"
)

// TestRegistryTakesMemoryAsItGrows is the stream registry's counterpart of
// ipc's (ADR 0111).
func TestRegistryTakesMemoryAsItGrows(t *testing.T) {
	storage := &objects.Limited[Stream]{}
	var registry Registry
	registry.SetStorage(storage)
	ids := make([]uint64, MaxStreams)
	for index := range ids {
		_, id, ok := registry.Create(1)
		if !ok {
			t.Fatalf("create %d refused", index)
		}
		ids[index] = id
	}
	if _, _, ok := registry.Create(1); ok || storage.Live != MaxStreams {
		t.Fatalf("created past MaxStreams, or %d live", storage.Live)
	}
	for index, id := range ids {
		stream, _ := registry.Lookup(id)
		stream.CloseReader()
		stream.CloseWriter()
		if _, released := registry.Release(id); !released {
			t.Fatalf("release %d refused", index)
		}
	}
	if storage.Live != 0 {
		t.Fatalf("%d streams not returned", storage.Live)
	}
	if _, found := registry.Lookup(ids[len(ids)-1]); found {
		t.Fatal("a released stream was found")
	}
}
