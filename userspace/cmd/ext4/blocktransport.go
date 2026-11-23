package main

import (
	"sync"

	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
)

// maxReadWorkers bounds the read pool (ADR 0232).
const maxReadWorkers = 4

// blockTransport is the single owner of the block endpoint: one monotonic
// correlation sequence and one receive path. The exchange is synchronous
// under the mutex, so no two callers can reuse an outstanding correlation or
// consume each other's reply on the shared endpoint (ADR 0232 FIX). The
// endpoint I/O is injected so the transport is host-testable.
type blockTransport struct {
	mu   sync.Mutex
	next uint64
	send func(wire []byte, size int) kcall.Status
	recv func(wire []byte, timeout uint64) kcall.Status
}

func newBlockTransport() *blockTransport {
	return &blockTransport{next: 1}
}

// exchange sends one request and waits for its reply. The mutex makes the
// whole send/receive critical section a single outstanding request, so the
// correlation is always unique and the reply is always the one for this
// request.
func (transport *blockTransport) exchange(request blockproto.Request) (blockproto.Reply, bool) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	correlation := transport.next
	transport.next++
	message, ok := blockproto.EncodeRequest(request, correlation)
	var wire [ipc.WireMaxSize]byte
	var size int
	if ok {
		size, ok = ipc.Encode(message, wire[:])
	}
	if !ok || transport.send(wire[:size], size) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	if transport.recv(wire[:], blockproto.RequestDeadline) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	received, valid := ipc.Decode(wire[:wireSize(wire[:])])
	reply, validReply := blockproto.DecodeReply(received)
	return reply, valid && validReply && received.Correlation == correlation
}

func wireSize(data []byte) int {
	size := ipc.WireHeaderSize + (int(data[24]) | int(data[25])<<8) + int(data[26])*8
	if size > len(data) {
		return len(data)
	}
	return size
}
