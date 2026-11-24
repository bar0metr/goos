package main

import (
	"encoding/binary"
	"sync"
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
)

// echoTransport wires the transport's I/O so that each reply is matched to the
// request's own correlation, like the block service.
type echoTransport struct {
	mu      sync.Mutex
	sent    []uint64
	offsets []uint64
	dead    bool
}

func (echo *echoTransport) send(wire []byte, size int) kcall.Status {
	message, ok := ipc.Decode(wire[:size])
	if !ok {
		return kcall.StatusInvalidArgument
	}
	request, ok := blockproto.DecodeRequestFrom(&message)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	echo.mu.Lock()
	echo.sent = append(echo.sent, message.Correlation)
	echo.offsets = append(echo.offsets, request.Offset)
	echo.mu.Unlock()
	return kcall.StatusOK
}

func (echo *echoTransport) recv(wire []byte, timeout uint64) kcall.Status {
	if echo.dead {
		return kcall.StatusPeerDied
	}
	// The wire still holds the request; write a reply with the same
	// correlation, echoing the offset as the read data.
	message, _ := ipc.Decode(wire[:wireSize(wire)])
	request, _ := blockproto.DecodeRequestFrom(&message)
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], request.Offset)
	reply, ok := blockproto.EncodeReply(blockproto.Reply{Operation: blockproto.OperationRead, Status: blockproto.StatusOK, Data: data[:]}, message.Correlation)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	size, ok := ipc.Encode(reply, wire)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	_ = size
	return kcall.StatusOK
}

// TestBlockTransportUniqueCorrelationsAndRouting runs four exchanges, each
// through its own echo, and verifies every reply returns to its requester.
// The pre-multiplexer design (per-client IPCWaitReceive) could not pass this:
// it reused correlations and consumed each other's replies.
func TestBlockTransportUniqueCorrelationsAndRouting(t *testing.T) {
	echo := &echoTransport{}
	transport := newBlockTransport()
	transport.send = echo.send
	transport.recv = echo.recv

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
	group.Wait()
	for index := 0; index < n; index++ {
		if offsets[index] != uint64(index)*100 {
			t.Fatalf("requester %d got offset %d, want %d", index, offsets[index], uint64(index)*100)
		}
	}
	for index := 0; index < n; index++ {
		for other := index + 1; other < n; other++ {
			if echo.sent[index] == echo.sent[other] {
				t.Fatalf("duplicate correlation %d", echo.sent[index])
			}
		}
	}
}

// TestBlockTransportEndpointDeath verifies a dead endpoint fails the exchange.
func TestBlockTransportEndpointDeath(t *testing.T) {
	echo := &echoTransport{}
	transport := newBlockTransport()
	transport.send = echo.send
	transport.recv = echo.recv
	echo.dead = true
	if _, ok := transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo}); ok {
		t.Fatal("exchange survived endpoint death")
	}
}

// TestBlockTransportBadSendFails verifies a request that cannot be encoded or
// sent fails without waiting for a reply.
func TestBlockTransportBadSendFails(t *testing.T) {
	transport := newBlockTransport()
	transport.send = func(wire []byte, size int) kcall.Status { return kcall.StatusPeerDied }
	transport.recv = func(wire []byte, timeout uint64) kcall.Status { return kcall.StatusOK }
	if _, ok := transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo}); ok {
		t.Fatal("exchange survived a failed send")
	}
}
