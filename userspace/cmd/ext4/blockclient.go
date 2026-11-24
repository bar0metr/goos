//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/sys"
)

// bulkPages is the size of one bulk buffer in pages (32 × 4 KiB = 128 KiB).
const bulkPages = 32

// blockClient is one caller's view of the device: a shared multiplexed
// transport plus a private bulk buffer. The ReadAt/WriteAt loops own the bulk
// buffer between exchanges; the transport owns the wire and correlation.
type blockClient struct {
	transport  *blockTransport
	bulkObject handle.ID
	bulkMemory []byte
	capacity   uint64
	writable   bool
}

func newBlockClient(transport *blockTransport) *blockClient {
	client := &blockClient{transport: transport}
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
	reply, ok := client.transport.exchange(blockproto.Request{Operation: operation, Offset: offset, Length: uint32(count), Object: duplicate})
	if !ok {
		// The transport could not send (or its table was exhausted): the
		// object never reached the block service, so it is still here.
		sys.HandleClose(duplicate)
		return false
	}
	return reply.Status == blockproto.StatusOK
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
		reply, ok := client.transport.exchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(chunk)})
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
		reply, ok := client.transport.exchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(chunk), Data: data[:chunk]})
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
	reply, ok := client.transport.exchange(blockproto.Request{Operation: blockproto.OperationFlush})
	return ok && reply.Status == blockproto.StatusOK
}

// Capacity is asked once; the same reply says whether the slice is writable.
func (client *blockClient) Capacity() uint64 {
	if client.capacity != 0 {
		return client.capacity
	}
	reply, ok := client.transport.exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || reply.Status != blockproto.StatusOK {
		return 0
	}
	client.capacity, client.writable = reply.Capacity, reply.Writable
	return client.capacity
}

// pooledDevice dispatches device operations across a bounded ownership pool.
// Each client owns its bulk buffer; their shared transport serializes block IPC.
type pooledDevice struct {
	transport *blockTransport
	clients   chan *blockClient
	capacity  uint64
	writable  bool
	asked     bool
}

func newPooledDevice(count int) *pooledDevice {
	transport := newBlockTransport()
	transport.send = func(wire []byte, size int) kcall.Status {
		return sys.IPCSend(blockHandle, wire[:size])
	}
	transport.recv = func(wire []byte, timeout uint64) kcall.Status {
		return sys.IPCWaitReceive(blockHandle, wire, timeout)
	}
	pool := &pooledDevice{
		transport: transport,
		clients:   make(chan *blockClient, count),
	}
	for index := 0; index < count; index++ {
		pool.clients <- newBlockClient(transport)
	}
	return pool
}

func (pool *pooledDevice) ReadAt(offset uint64, target []byte) bool {
	client := <-pool.clients
	ok := client.ReadAt(offset, target)
	pool.clients <- client
	return ok
}

func (pool *pooledDevice) WriteAt(offset uint64, data []byte) bool {
	client := <-pool.clients
	ok := client.WriteAt(offset, data)
	pool.clients <- client
	return ok
}

func (pool *pooledDevice) Flush() bool {
	client := <-pool.clients
	ok := client.Flush()
	pool.clients <- client
	return ok
}

func (pool *pooledDevice) Capacity() uint64 {
	if pool.asked {
		return pool.capacity
	}
	client := <-pool.clients
	pool.capacity = client.Capacity()
	pool.writable = client.writable
	pool.clients <- client
	pool.asked = true
	return pool.capacity
}
