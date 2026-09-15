package ipc

import "testing"

func TestRegistryCreatesUniqueBoundedChannels(t *testing.T) {
	var registry Registry
	first, firstID, ok := registry.Create()
	if !ok || first == nil || firstID&channelNamespace == 0 {
		t.Fatal("first channel was not created")
	}
	if found, ok := registry.Lookup(firstID); !ok || found != first {
		t.Fatal("created channel cannot be resolved")
	}
	for index := 1; index < MaxChannels; index++ {
		_, id, created := registry.Create()
		if !created || id == firstID {
			t.Fatalf("channel %d was not unique", index)
		}
	}
	if channel, id, created := registry.Create(); created || channel != nil || id != 0 {
		t.Fatal("registry exceeded its bound")
	}
	if _, ok := registry.Lookup(channelNamespace | 0xffff); ok {
		t.Fatal("unknown channel resolved")
	}
}
