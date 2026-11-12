//go:build goos_userspace

// The ext4 filesystem service (ADR 0221, ADR 0222, ADR 0226). One process
// serves one additional volume: it parses the untrusted on-disk format,
// reaches its block slice through the block service, and answers the VFS over
// the filesystem protocol. It holds no namespace authority, no device, and no
// other volume's slice. The slice decides the mode: a writable slice gets a
// writable mount, which recovers the journal first; a read-only slice gets
// a read-only mount. A volume it cannot serve safely is refused, and the
// service keeps answering with the refusal rather than crashing in a restart
// loop.
package main

import (
	"goos/internal/capability"
	"goos/internal/timekeeping"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/ramfs"
	blockproto "goos/userspace/block"
	"goos/userspace/ext4"
	"goos/userspace/ext4vol"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

// requestWait bounds one wait for the next request; waking without one is not
// an error.
const requestWait = 100000

var (
	consoleHandle  handle.ID
	vfsHandle      handle.ID
	blockHandle    handle.ID
	wire           [ipc.WireMaxSize]byte
	blockWire      [ipc.WireMaxSize]byte
	requestMessage ipc.Message
	replyMessage   ipc.Message
	listedName     [ramfs.MaxName]byte
	requestedName  [ramfs.MaxName]byte
	// cache is the file cache this instance shares with the VFS. PID 1
	// creates it for this launch only (ADR 0222).
	cache         []byte
	volume        ext4vol.Volume
	correlation   uint64
	blockCapacity uint64
	sliceWritable bool
	bulkObject    handle.ID
	bulkMemory    []byte
)

const bulkPages = 32

type blockDevice struct{}

func setUpBulk() {
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
	bulkObject, bulkMemory = object, sys.Window(base, size)
}

// bulkTransfer moves count bytes between the bulk buffer and the slice.
func bulkTransfer(operation blockproto.Operation, offset uint64, count int) bool {
	duplicate, status := sys.HandleDuplicate(bulkObject, capability.Read|capability.Write|capability.Transfer)
	if status != kcall.StatusOK {
		return false
	}
	reply, ok := blockExchange(blockproto.Request{Operation: operation, Offset: offset, Length: uint32(count), Object: duplicate})
	return ok && reply.Status == blockproto.StatusOK
}

// ReadAt reads through the bulk buffer in large pieces and through messages
// for the rest. The block service checks every range against the slice.
func (blockDevice) ReadAt(offset uint64, target []byte) bool {
	for len(target) > blockproto.MaxData && bulkMemory != nil {
		chunk := min(len(target), len(bulkMemory))
		if !bulkTransfer(blockproto.OperationReadBulk, offset, chunk) {
			return false
		}
		copy(target[:chunk], bulkMemory[:chunk])
		target = target[chunk:]
		offset += uint64(chunk)
	}
	for len(target) != 0 {
		chunk := min(len(target), blockproto.MaxData)
		reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(chunk)})
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
func (blockDevice) WriteAt(offset uint64, data []byte) bool {
	for len(data) > blockproto.MaxData && bulkMemory != nil {
		chunk := min(len(data), len(bulkMemory))
		copy(bulkMemory[:chunk], data[:chunk])
		if !bulkTransfer(blockproto.OperationWriteBulk, offset, chunk) {
			return false
		}
		data = data[chunk:]
		offset += uint64(chunk)
	}
	for len(data) != 0 {
		chunk := min(len(data), blockproto.MaxData)
		reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(chunk), Data: data[:chunk]})
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
func (blockDevice) Flush() bool {
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationFlush})
	return ok && reply.Status == blockproto.StatusOK
}

// Capacity is asked once; the same reply says whether the slice is
// writable.
func (blockDevice) Capacity() uint64 {
	if blockCapacity != 0 {
		return blockCapacity
	}
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || reply.Status != blockproto.StatusOK {
		return 0
	}
	blockCapacity, sliceWritable = reply.Capacity, reply.Writable
	return blockCapacity
}

// realtime stamps inode times with UTC Unix time; a clock that cannot be
// read stamps zero.
func realtime() ext4.Timestamp {
	now, status := sys.ClockNow(timekeeping.Realtime)
	if status != kcall.StatusOK || now.Seconds < 0 || !now.Valid() {
		return ext4.Timestamp{}
	}
	return ext4.Timestamp{Seconds: now.Seconds, Nanos: now.Nanoseconds}
}

//export userspaceMain
func userspaceMain() {
	arena.InitService()
	arena.OnExhausted(func() { fatal("EXT4_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("EXT4_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "ext4")
	if !ok {
		fatal("EXT4_CONSOLE_MISSING\n")
	}
	if vfsHandle, ok = data.Handle("vfs"); !ok {
		fatal("EXT4_VFS_MISSING\n")
	}
	if blockHandle, ok = data.Handle("block"); !ok {
		fatal("EXT4_BLOCK_MISSING\n")
	}
	cacheHandle, ok := data.Handle("cache")
	if !ok {
		fatal("EXT4_CACHE_MISSING\n")
	}
	base, size, status := sys.MemoryMapAnywhere(cacheHandle, true)
	if status != kcall.StatusOK || size != fsproto.CacheSize {
		fatal("EXT4_CACHE_UNMAPPED\n")
	}
	cache = sys.Window(base, size)
	setUpBulk()
	device := blockDevice{}
	device.Capacity()
	mounted, mode := false, "read-only"
	if sliceWritable {
		mounted, mode = volume.MountWritable(device, realtime), "read-write"
	} else {
		mounted = volume.Mount(device)
	}
	if mounted {
		mark("EXT4_SERVICE_OK\n")
		status, _ := volume.Status()
		syslog.Info("volume mounted", syslog.Uint("capacity-mib", blockCapacity>>20), syslog.Str("mode", mode),
			syslog.Uint("replayed", uint64(status.Recovery.Transactions)), syslog.Uint("orphans", uint64(status.Recovery.Orphans)))
	} else {
		syslog.Error("volume refused", syslog.Str("reason", volume.Refusal()))
		mark("EXT4_VOLUME_REFUSED\n")
	}
	for {
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = userwait.Interest{Handle: blockHandle, Events: kcall.WaitPeerClosed}
		ready, waited := userwait.Any(&interests, 2, requestWait)
		if waited == kcall.StatusTimeout {
			continue
		}
		if waited != kcall.StatusOK {
			fatal("EXT4_WAIT_FAILED\n")
		}
		if ready.ReadyMask&2 != 0 {
			// The block service restarted, or PID 1 unmounted the volume
			// and closed the slot: either way this instance is done.
			syslog.Warn("block endpoint closed")
			sys.Exit(exitBlockGone)
		}
		status := sys.IPCReceive(vfsHandle, wire[:])
		if status == kcall.StatusNotFound {
			continue
		}
		if status == kcall.StatusPeerDied {
			syslog.Warn("vfs endpoint closed")
			sys.Exit(exitVFSGone)
		}
		if status != kcall.StatusOK {
			fatal("EXT4_RECEIVE_FAILED\n")
		}
		if !ipc.DecodeInto(wire[:wireSize(wire[:])], &requestMessage) {
			continue
		}
		serve()
		if volume.Failed() {
			// The device refused a write or a flush: the driver may have
			// been replaced under the volume. A new instance replays the
			// journal; this one could only answer errors.
			syslog.Error("volume failed", syslog.Str("action", "exit for recovery"))
			sys.Exit(exitVolumeFailed)
		}
	}
}

func serve() {
	request, valid := fsproto.DecodeRequest(&requestMessage, cache, &requestedName)
	var reply fsproto.Reply
	if !valid {
		operation := fsproto.Operation(requestMessage.Payload[0])
		if requestMessage.Correlation == 0 || operation < fsproto.OperationMount || operation > fsproto.OperationUnmount {
			return
		}
		reply = fsproto.Reply{Operation: operation, Status: ramfs.StatusInvalidArgument}
	} else {
		reply = fsproto.Serve(&volume, request, cache, &listedName)
	}
	if !fsproto.EncodeReply(reply, requestMessage.Correlation, &replyMessage, cache) {
		fatal("EXT4_REPLY_FAILED\n")
	}
	size, ok := ipc.EncodeFrom(&replyMessage, wire[:])
	if !ok {
		fatal("EXT4_REPLY_FAILED\n")
	}
	if sys.IPCSend(vfsHandle, wire[:size]) != kcall.StatusOK {
		syslog.Warn("reply undeliverable")
	}
}

func blockExchange(request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	message, ok := blockproto.EncodeRequest(request, correlation)
	var size int
	if ok {
		size, ok = ipc.Encode(message, blockWire[:])
	}
	if !ok || sys.IPCSend(blockHandle, blockWire[:size]) != kcall.StatusOK {
		if request.Operation.Bulk() {
			sys.HandleClose(request.Object)
		}
		return blockproto.Reply{}, false
	}
	if sys.IPCWaitReceive(blockHandle, blockWire[:], blockproto.RequestDeadline) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	received, valid := ipc.Decode(blockWire[:wireSize(blockWire[:])])
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

func mark(message string) {
	if validation.Enabled {
		sys.ConsoleWriteString(consoleHandle, message)
	}
}

// Exit codes. None is zero, so the supervisor counts each exit as a failure
// unless PID 1 planned the stop.
const (
	exitFailed    = 1
	exitVFSGone   = 2
	exitBlockGone = 3
	// exitVolumeFailed: a write or flush failed and the journal needs a
	// replay by the next writable mount.
	exitVolumeFailed = 4
)

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		sys.ConsoleWriteString(consoleHandle, message)
	}
	sys.Diagnostics(consoleHandle)
	sys.Exit(exitFailed)
}

func main() {}
