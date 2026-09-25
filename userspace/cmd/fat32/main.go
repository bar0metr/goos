//go:build goos_userspace

// The FAT32 filesystem service (ADR 0096). It owns the persistent volume's
// on-disk format and the block endpoint it is read through, and serves the
// VFS over the filesystem-service protocol. It holds no namespace authority:
// paths, mounts, and open files belong to the VFS, which reaches this service
// only through the channel PID 1 created for the two of them.
package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/mm/vm"
	"goos/kernel/ramfs"
	blockproto "goos/userspace/block"
	"goos/userspace/fat32"
	"goos/userspace/fsproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

// requestWait bounds one wait for the next request. Waking without one is not
// an error; the bound only keeps the wait a timed one.
const requestWait = 100000

var (
	consoleHandle  handle.ID
	vfsHandle      handle.ID
	blockHandle    handle.ID
	startupWire    [startup.Size]byte
	wire           [ipc.WireMaxSize]byte
	blockWire      [ipc.WireMaxSize]byte
	requestMessage ipc.Message
	replyMessage   ipc.Message
	listedName     [ramfs.MaxName]byte
	// cache is the file cache this service shares with the VFS. A read fills,
	// and a write takes its bytes from, the slot a request names (ADR 0098).
	cache         []byte
	volume        fat32.Volume
	correlation   uint64
	blockCapacity uint64
)

// blockDevice is the volume's device: the block service, one bounded exchange
// per chunk. The filesystem checks the geometry the image declares against
// Capacity, because every offset it computes comes from untrusted numbers.
type blockDevice struct{}

func (blockDevice) ReadAt(offset uint64, target []byte) bool {
	for len(target) != 0 {
		chunk := len(target)
		if chunk > blockproto.MaxData {
			chunk = blockproto.MaxData
		}
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

func (blockDevice) WriteAt(offset uint64, data []byte) bool {
	for len(data) != 0 {
		chunk := len(data)
		if chunk > blockproto.MaxData {
			chunk = blockproto.MaxData
		}
		reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(chunk), Data: data[:chunk]})
		if !ok || reply.Status != blockproto.StatusOK {
			return false
		}
		data = data[chunk:]
		offset += uint64(chunk)
	}
	return true
}

func (blockDevice) Flush() bool {
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationFlush})
	return ok && reply.Status == blockproto.StatusOK
}

// Capacity is asked once per mount; the answer is kept, since it does not
// change while the device is attached.
func (blockDevice) Capacity() uint64 {
	if blockCapacity != 0 {
		return blockCapacity
	}
	reply, ok := blockExchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || reply.Status != blockproto.StatusOK {
		return 0
	}
	blockCapacity = reply.Capacity
	return blockCapacity
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("FAT32_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != kcall.StatusOK {
		fatal("FAT32_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("FAT32_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "fat32")
	if !ok {
		fatal("FAT32_CONSOLE_MISSING\n")
	}
	if vfsHandle, ok = data.Handle("vfs"); !ok {
		fatal("FAT32_VFS_MISSING\n")
	}
	if blockHandle, ok = data.Handle("block"); !ok {
		fatal("FAT32_BLOCK_MISSING\n")
	}
	cacheHandle, ok := data.Handle("cache")
	if !ok {
		fatal("FAT32_CACHE_MISSING\n")
	}
	if mapped, status := sys.MemoryMap(cacheHandle, vm.UserSharedAddress, fsproto.CacheSize, true); status == kcall.StatusOK {
		cache = mapped
	} else {
		// Without it no file data can move; metadata still can.
		syslog.Error("file cache unavailable", syslog.Uint("status", uint64(status)))
	}
	// A volume that is refused is not a reason to stop: the service keeps
	// answering, with I/O errors, so the VFS reports the volume as absent
	// rather than the service as crashing in a restart loop (ADR 0077).
	if volume.Mount(blockDevice{}) {
		mark("FAT32_SERVICE_OK\n")
		syslog.Info("volume mounted", syslog.Uint("capacity-mib", blockCapacity>>20))
	} else {
		syslog.Error("volume refused", syslog.Str("reason", "image rejected or device unavailable"))
	}
	base := arena.Current()
	for {
		arena.Reset(base)
		// The block endpoint is watched as well as the VFS one. A block
		// service that restarts takes this channel with it, and an idle
		// service would otherwise find out only on the next request, which
		// would fail (ADR 0101).
		var interests [userwait.MaxObjects]userwait.Interest
		interests[0] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		interests[1] = userwait.Interest{Handle: blockHandle, Events: kcall.WaitPeerClosed}
		ready, waited := userwait.Any(&interests, 2, requestWait)
		if waited == kcall.StatusTimeout {
			continue
		}
		if waited != kcall.StatusOK {
			fatal("FAT32_WAIT_FAILED\n")
		}
		if ready.ReadyMask&2 != 0 {
			syslog.Warn("block endpoint closed")
			sys.Exit(exitBlockGone)
		}
		status := sys.IPCReceive(vfsHandle, wire[:])
		if status == kcall.StatusNotFound {
			continue
		}
		if status == kcall.StatusPeerDied {
			// The VFS is gone and nothing else can reach this service. When
			// the VFS is restarted, it needs a filesystem service again, and
			// a new channel to it; exiting with a failure has the supervisor
			// start one once the VFS is back (ADR 0099).
			syslog.Warn("vfs endpoint closed")
			sys.Exit(exitVFSGone)
		}
		if status != kcall.StatusOK {
			fatal("FAT32_RECEIVE_FAILED\n")
		}
		if !ipc.DecodeInto(wire[:wireSize(wire[:])], &requestMessage) {
			continue
		}
		serve()
	}
}

func serve() {
	request, valid := fsproto.DecodeRequest(&requestMessage)
	var reply fsproto.Reply
	if !valid {
		// Answer an unusable request rather than drop it: the VFS is waiting
		// for exactly this correlation, and silence would cost it a deadline.
		operation := fsproto.Operation(requestMessage.Payload[0])
		if requestMessage.Correlation == 0 || operation < fsproto.OperationMount || operation > fsproto.OperationFlush {
			return
		}
		reply = fsproto.Reply{Operation: operation, Status: ramfs.StatusInvalidArgument}
	} else {
		reply = fsproto.Serve(&volume, request, cache, &listedName)
	}
	if !fsproto.EncodeReply(reply, requestMessage.Correlation, &replyMessage) {
		fatal("FAT32_REPLY_FAILED\n")
	}
	size, ok := ipc.EncodeFrom(&replyMessage, wire[:])
	if !ok {
		fatal("FAT32_REPLY_FAILED\n")
	}
	if sys.IPCSend(vfsHandle, wire[:size]) != kcall.StatusOK {
		syslog.Warn("reply undeliverable")
	}
}

func blockExchange(request blockproto.Request) (blockproto.Reply, bool) {
	correlation++
	message, ok := blockproto.EncodeRequest(request, correlation)
	if !ok {
		return blockproto.Reply{}, false
	}
	size, ok := ipc.Encode(message, blockWire[:])
	if !ok || sys.IPCSend(blockHandle, blockWire[:size]) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	if sys.IPCWaitReceive(blockHandle, blockWire[:], blockproto.RequestDeadline) != kcall.StatusOK {
		return blockproto.Reply{}, false
	}
	received, valid := ipc.Decode(blockWire[:wireSize(blockWire[:])])
	reply, validReply := blockproto.DecodeReply(received)
	return reply, valid && validReply && received.Correlation == correlation
}

// wireSize is the length of the message at the start of data, from its header.
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

// Exit codes. Neither is zero, so the supervisor starts the service again.
const (
	exitFailed    = 1
	exitVFSGone   = 2
	exitBlockGone = 3
)

// fatal reports why the service stops and exits, so that the supervisor
// restarts it rather than the VFS waiting out a deadline on every request.
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		sys.ConsoleWriteString(consoleHandle, message)
	}
	sys.Diagnostics(consoleHandle)
	sys.Exit(exitFailed)
}

func main() {}
