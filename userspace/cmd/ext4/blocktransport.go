package main

import (
	"sync"

	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
)

// maxReadWorkers bounds the read pool (ADR 0232).
const maxReadWorkers = 4

// maxBlockSlots bounds the block transport's outstanding requests to the read
// worker limit plus the mutation owner (ADR 0232 FIX).
const maxBlockSlots = maxReadWorkers + 1

// blockResult is one routed reply.
type blockResult struct {
	reply blockproto.Reply
	ok    bool
}

// blockSlot is one outstanding request slot in the transport table.
type blockSlot struct {
	used        bool
	correlation uint64
	reply       chan blockResult
}

// blockTransport is the single owner of the block endpoint: one monotonic
// correlation sequence and one receive path that routes every reply to its
// requester. It replaces the earlier per-client receive, where two callers
// could reuse a correlation or consume each other's reply on the shared
// endpoint. The endpoint I/O is injected so the routing is host-testable.
type blockTransport struct {
	mu    sync.Mutex
	next  uint64
	slots [maxBlockSlots]blockSlot
	send  func(wire []byte, size int) kcall.Status
	recv  func(wire []byte, timeout uint64) kcall.Status
}

// newBlockTransport builds a transport whose I/O is wired by start.
func newBlockTransport() *blockTransport {
	transport := &blockTransport{next: 1}
	for index := range transport.slots {
		transport.slots[index].reply = make(chan blockResult, 1)
	}
	return transport
}

// start runs the receive loop on the injected recv.
func (transport *blockTransport) start() {
	go transport.receive()
}

// receive is the single dispatcher on the block endpoint.
func (transport *blockTransport) receive() {
	var wire [ipc.WireMaxSize]byte
	for {
		status := transport.recv(wire[:], blockproto.RequestDeadline)
		if status != kcall.StatusOK {
			transport.failAll()
			continue
		}
		received, valid := ipc.Decode(wire[:wireSize(wire[:])])
		if !valid {
			continue
		}
		reply, validReply := blockproto.DecodeReply(received)
		if !validReply {
			continue
		}
		transport.mu.Lock()
		slot := transport.find(received.Correlation)
		transport.mu.Unlock()
		if slot != nil {
			slot.reply <- blockResult{reply: reply, ok: validReply}
		}
	}
}

func (transport *blockTransport) find(correlation uint64) *blockSlot {
	for index := range transport.slots {
		if transport.slots[index].used && transport.slots[index].correlation == correlation {
			return &transport.slots[index]
		}
	}
	return nil
}

func (transport *blockTransport) freeSlot() *blockSlot {
	for index := range transport.slots {
		if !transport.slots[index].used {
			return &transport.slots[index]
		}
	}
	return nil
}

// failAll wakes every outstanding requester when the endpoint dies or stalls,
// without discarding a reply that was already routed.
func (transport *blockTransport) failAll() {
	transport.mu.Lock()
	for index := range transport.slots {
		if transport.slots[index].used {
			select {
			case transport.slots[index].reply <- blockResult{}:
			default:
			}
		}
	}
	transport.mu.Unlock()
}

// exchange sends one request and waits for its routed reply.
func (transport *blockTransport) exchange(request blockproto.Request) (blockproto.Reply, bool) {
	transport.mu.Lock()
	slot := transport.freeSlot()
	if slot == nil {
		transport.mu.Unlock()
		return blockproto.Reply{}, false
	}
	slot.used = true
	slot.correlation = transport.next
	transport.next++
	correlation := slot.correlation
	reply := make(chan blockResult, 1)
	slot.reply = reply
	transport.mu.Unlock()

	message, ok := blockproto.EncodeRequest(request, correlation)
	var wire [ipc.WireMaxSize]byte
	var size int
	if ok {
		size, ok = ipc.Encode(message, wire[:])
	}
	if !ok || transport.send(wire[:size], size) != kcall.StatusOK {
		transport.mu.Lock()
		slot.used = false
		transport.mu.Unlock()
		return blockproto.Reply{}, false
	}

	result := <-reply
	transport.mu.Lock()
	slot.used = false
	transport.mu.Unlock()
	return result.reply, result.ok
}

func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}
