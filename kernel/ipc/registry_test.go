package ipc

import "testing"

func TestRegistryCreatesUniqueBoundedChannels(t *testing.T) {
	var registry Registry
	first, firstID, ok := registry.Create(7)
	if !ok || first == nil || firstID&channelNamespace == 0 {
		t.Fatal("first channel was not created")
	}
	if found, ok := registry.Acquire(firstID); !ok || found != first {
		t.Fatal("created channel cannot be resolved")
	}
	first.Unlock()
	for index := 1; index < MaxChannels; index++ {
		_, id, created := registry.Create(7)
		if !created || id == firstID {
			t.Fatalf("channel %d was not unique", index)
		}
	}
	if channel, id, created := registry.Create(7); created || channel != nil || id != 0 {
		t.Fatal("registry exceeded its bound")
	}
	if registry.Live(channelNamespace | 0xffff) {
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
	channel, id, ok := registry.Create(7)
	if !ok {
		t.Fatal("channel was not created")
	}
	if closed, _, released := registry.Close(id, 0); !closed || released {
		t.Fatalf("first side closed = %v, released = %v", closed, released)
	}
	if !registry.Live(id) || channel.Alive(0) || !channel.Alive(1) {
		t.Fatal("channel with one side open is not live")
	}
	if closed, _, _ := registry.Close(id, 0); closed {
		t.Fatal("a side closed twice")
	}
	closed, owner, released := registry.Close(id, 1)
	if !closed || !released || owner != 7 {
		t.Fatalf("closed channel released = %v, charged to %d", released, owner)
	}
	if registry.Live(id) {
		t.Fatal("released channel still resolves")
	}
	reused, reusedID, created := registry.Create(7)
	if !created || reused == nil || reusedID == id {
		t.Fatalf("channel slot was not safely reused: id=%#x reused=%#x", id, reusedID)
	}
}

func TestRegistryAtAndQueueState(t *testing.T) {
	var registry Registry
	channel, id, _ := registry.Create(7)
	message := Message{ProtocolID: 1, Version: 1, Kind: KindRequest, Correlation: 5}
	if channel.Send(0, message, nil, nil, nil) != StatusOK {
		t.Fatal("request was not queued")
	}
	found, at, ok := registry.AcquireAt(0)
	if !ok || found != id || at != channel {
		t.Fatal("live channel not reported")
	}
	at.Unlock()
	if channel.Queued(1) != 1 || channel.Queued(0) != 0 || channel.Pending(0) != 1 || channel.Pending(1) != 0 {
		t.Fatalf("queued %d/%d pending %d/%d", channel.Queued(0), channel.Queued(1), channel.Pending(0), channel.Pending(1))
	}
	if _, _, ok := registry.AcquireAt(1); ok {
		t.Fatal("empty slot reported")
	}
}
