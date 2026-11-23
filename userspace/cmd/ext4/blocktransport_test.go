package main

import (
	"encoding/binary"
	"sync"
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
)

// fakeEndpoint wires the transport's I/O to in-memory channels so the routing
// is exercised without the block service.
type fakeEndpoint struct {
	sent    chan sentRequest
	replies chan []byte
}

type sentRequest struct {
	correlation uint64
	offset      uint64
}

func newFakeEndpoint() *fakeEndpoint {
	return &fakeEndpoint{sent: make(chan sentRequest, 16), replies: make(chan []byte, 16)}
}

func (endpoint *fakeEndpoint) send(wire []byte, size int) kcall.Status {
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		return kcall.StatusInvalidArgument
	}
	request, ok := blockproto.DecodeRequestFrom(&message)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	endpoint.sent <- sentRequest{correlation: message.Correlation, offset: request.Offset}
	return kcall.StatusOK
}

func (endpoint *fakeEndpoint) recv(wire []byte, timeout uint64) kcall.Status {
	reply := <-endpoint.replies
	copy(wire, reply)
	return kcall.StatusOK
}

// reply answers one read with the request's offset echoed as its data.
func (endpoint *fakeEndpoint) reply(t *testing.T, correlation, offset uint64) {
	t.Helper()
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], offset)
	message, ok := blockproto.EncodeReply(blockproto.Reply{Operation: blockproto.OperationRead, Status: blockproto.StatusOK, Data: data[:]}, correlation)
	if !ok {
		t.Fatal("encode reply")
	}
	wire := make([]byte, ipc.WireMaxSize)
	size, ok := ipc.Encode(message, wire)
	if !ok {
		t.Fatal("encode wire")
	}
	endpoint.replies <- wire[:size]
}

// TestBlockTransportRoutesReplies runs four simultaneous reads, verifies
// unique correlations, answers out of order, and checks each requester gets
// its own reply. The pre-multiplexer design (per-client IPCWaitReceive) could
// not pass this: it reused correlations and consumed each other's replies.
func TestBlockTransportRoutesReplies(t *testing.T) {
	endpoint := newFakeEndpoint()
	transport := newBlockTransport()
	transport.send = endpoint.send
	transport.recv = endpoint.recv
	transport.start()

	const n = 4
	offsets := make([]uint64, n)
	var group sync.WaitGroup
	for index := 0; index < n; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			reply, ok := transport.exchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: uint64(index) * 100, Length: 8})
			if !ok {
				t.Error("exchange failed")
				return
			}
			offsets[index] = binary.LittleEndian.Uint64(reply.Data)
		}(index)
	}

	sent := make([]sentRequest, 0, n)
	for range n {
		sent = append(sent, <-endpoint.sent)
	}
	for index := 0; index < n; index++ {
		for other := index + 1; other < n; other++ {
			if sent[index].correlation == sent[other].correlation {
				t.Fatalf("duplicate correlation %d", sent[index].correlation)
			}
		}
	}
	// Reply in reverse order; each requester must still get its own offset.
	for index := n - 1; index >= 0; index-- {
		endpoint.reply(t, sent[index].correlation, sent[index].offset)
	}
	group.Wait()
	for index := 0; index < n; index++ {
		if offsets[index] != uint64(index)*100 {
			t.Fatalf("requester %d got offset %d, want %d", index, offsets[index], uint64(index)*100)
		}
	}
}

// TestBlockTransportTableExhaustion verifies the outstanding table is bounded:
// an exchange beyond maxBlockSlots fails instead of growing.
func TestBlockTransportTableExhaustion(t *testing.T) {
	endpoint := newFakeEndpoint()
	transport := newBlockTransport()
	transport.send = endpoint.send
	// recv blocks forever, so the first maxBlockSlots requests stay outstanding.
	transport.recv = func(wire []byte, timeout uint64) kcall.Status {
		select {}
	}
	transport.start()

	for index := 0; index < maxBlockSlots; index++ {
		go transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	}
	for index := 0; index < maxBlockSlots; index++ {
		<-endpoint.sent
	}
	// One more must fail: no free slot.
	if _, ok := transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo}); ok {
		t.Fatal("exchange beyond the bounded table succeeded")
	}
}

// TestBlockTransportEndpointDeath verifies that a dead endpoint wakes every
// outstanding requester with failure.
func TestBlockTransportEndpointDeath(t *testing.T) {
	endpoint := newFakeEndpoint()
	transport := newBlockTransport()
	transport.send = endpoint.send
	transport.recv = func(wire []byte, timeout uint64) kcall.Status {
		return kcall.StatusPeerDied
	}
	transport.start()

	var group sync.WaitGroup
	results := make([]bool, maxBlockSlots)
	for index := 0; index < maxBlockSlots; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			_, ok := transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo})
			results[index] = ok
		}(index)
	}
	group.Wait()
	for index := 0; index < maxBlockSlots; index++ {
		if results[index] {
			t.Fatalf("requester %d survived endpoint death", index)
		}
	}
}
