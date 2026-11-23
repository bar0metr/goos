//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// bulkPages is the size of one bulk buffer in pages (32 × 4 KiB = 128 KiB).
const bulkPages = 32

// blockClient reaches the block service through the shared block endpoint.
// Each instance owns its request wire, correlation counter and bulk buffer, so
// independent callers can overlap block IPC (ADR 0232).
type blockClient struct {
	wire        [ipc.WireMaxSize]byte
	correlation uint64
	bulkObject  handle.ID
	bulkMemory  []byte
	capacity    uint64
	writable    bool
}

// newBlockClient builds a client and allocates its bulk buffer. A client
// without a bulk buffer still reads and writes inline.
func newBlockClient() *blockClient {
	client := &blockClient{}
	client.setUpBulk()
	return client
}

func (client *blockClient) setUpBulk() {
	object, status := sys.MemoryCreate(bulkPages)
	if status != kcall.StatusOK {
		syslog.Warn("bulk buffer unavailable", syslog.Uint("status", uint64(status)))
		return
	}
	base, size, status := sys.MemoryMapAnywhere(object, true)
	if status != kcall.StatusOK {
		sys.HandleClose(object)
		return
	}
	client.bulkObject, client.bulkMemory = object, sys.Window(base, size)
}

// bulkTransfer moves count bytes between the bulk buffer and the slice.
func (client *blockClient) bulkTransfer(operation blockproto.Operation, offset uint64, count int) bool {
	duplicate, status := sys.HandleDuplicate(client.bulkObject, capability.Read|capability.Write|capability.Transfer)
	if status != kcall.StatusOK {
		return false
	}
	reply, ok := client.exchange(blockproto.Request{Operation: operation, Offset: offset, Length: uint32(count), Object: duplicate})
	return ok && reply.Status == blockproto.StatusOK
}

// ReadAt reads through the bulk buffer in large pieces and through messages
// for the rest. The block service checks every range against the slice.
func (client *blockClient) ReadAt(offset uint64, target []byte) bool {
	for len(target) > blockproto.MaxData && client.bulkMemory != nil {
		chunk := min(len(target), len(client.bulkMemory))
		if !client.bulkTransfer(blockproto.OperationReadBulk, offset, chunk) {
			return false
		}
		copy(target[:chunk], client.bulkMemory[:chunk])
		target = target[chunk:]
		offset += uint64(chunk)
	}
	for len(target) != 0 {
		chunk := min(len(target), blockproto.MaxData)
		reply, ok := client.exchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(chunk)})
		if !ok || reply.Status != blockproto.StatusOK || len(reply.Data) != chunk {
			return false
		}
		copy(target[:chunk], reply.Data)
		target = target[chunk:]
		offset += uint64(chunk)
	}
	return true
}

// WriteAt returns once the block service accepted the bytes; Flush makes
// them durable. A read-only slice refuses every write.
func (client *blockClient) WriteAt(offset uint64, data []byte) bool {
	for len(data) > blockproto.MaxData && client.bulkMemory != nil {
		chunk := min(len(data), len(client.bulkMemory))
		copy(client.bulkMemory[:chunk], data[:chunk])
		if !client.bulkTransfer(blockproto.OperationWriteBulk, offset, chunk) {
			return false
		}
		data = data[chunk:]
		offset += uint64(chunk)
	}
	for len(data) != 0 {
		chunk := min(len(data), blockproto.MaxData)
		reply, ok := client.exchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(chunk), Data: data[:chunk]})
		if !ok || reply.Status != blockproto.StatusOK {
			return false
		}
		data = data[chunk:]
		offset += uint64(chunk)
	}
	return true
}

// Flush returns once every accepted write is durable, or false when the
// device refused the flush (ADR 0225).
func (client *blockClient) Flush() bool {
	reply, ok := client.exchange(blockproto.Request{Operation: blockproto.OperationFlush})
	return ok && reply.Status == blockproto.StatusOK
}

// Capacity is asked once; the same reply says whether the slice is writable.
func (client *blockClient) Capacity() uint64 {
	if client.capacity != 0 {
		return client.capacity
	}
	reply, ok := client.exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || reply.Status != blockproto.StatusOK {
		return 0
	}
	client.capacity, client.writable = reply.Capacity, reply.Writable
	return client.capacity
}

// exchange sends one request to the block service and waits for its reply,
// matched by this client's correlation counter.
func (client *blockClient) exchange(request blockproto.Request) (blockproto.Reply, bool) {
	client.correlation++
	message, ok := blockproto.EncodeRequest(request, client.correlation)
	var size int
	if ok {
		size, ok = ipc.Encode(message, client.wire[:])
	}
	if !ok || sys.IPCSend(blockHandle, client.wire[:size]) != kcall.StatusOK {
		if request.Operation.Bulk() {
			sys.HandleClose(request.Object)
		}
		return blockproto.Reply{}, false
	}
	if sys.IPCWaitReceive(blockHandle, client.wire[:], blockproto.RequestDeadline) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	received, valid := ipc.Decode(client.wire[:wireSize(client.wire[:])])
	reply, validReply := blockproto.DecodeReply(received)
	return reply, valid && validReply && received.Correlation == client.correlation
}
