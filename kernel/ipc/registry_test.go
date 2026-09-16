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

func TestEndpointObjectRoundTrip(t *testing.T) {
	channel := channelNamespace | 42
	for side := uint8(0); side < 2; side++ {
		object, ok := EndpointObject(channel, side)
		if !ok {
			t.Fatalf("side %d was not encoded", side)
		}
		decodedChannel, decodedSide, ok := DecodeEndpointObject(object)
		if !ok || decodedChannel != channel || decodedSide != side {
			t.Fatalf("side %d round trip = %#x, %d, %v", side, decodedChannel, decodedSide, ok)
		}
	}
	if _, ok := EndpointObject(42, 0); ok {
		t.Fatal("object outside the channel namespace was accepted")
	}
	if _, ok := EndpointObject(channel, 2); ok {
		t.Fatal("invalid endpoint side was accepted")
	}
	if _, _, ok := DecodeEndpointObject(42); ok {
		t.Fatal("object outside the channel namespace was decoded")
	}
}

func TestRegistryReusesReleasedChannelSlot(t *testing.T) {
	var registry Registry
	channel, id, ok := registry.Create()
	if !ok {
		t.Fatal("channel was not created")
	}
	if registry.Release(id) {
		t.Fatal("live channel was released")
	}
	if !channel.Close(0) || !channel.Close(1) || !registry.Release(id) {
		t.Fatal("closed channel was not released")
	}
	if _, found := registry.Lookup(id); found {
		t.Fatal("released channel still resolves")
	}
	reused, reusedID, created := registry.Create()
	if !created || reused == nil || reusedID == id {
		t.Fatalf("channel slot was not safely reused: id=%#x reused=%#x", id, reusedID)
	}
}
