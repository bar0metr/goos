//go:build goos_userspace

package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

type driverBackend struct {
	capacity  uint64
	blockSize uint32
}

func (backend *driverBackend) Capacity() uint64  { return backend.capacity }
func (backend *driverBackend) BlockSize() uint32 { return backend.blockSize }
func (backend *driverBackend) ReadAt(offset uint64, target []byte) blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationRead, Offset: offset, Length: uint32(len(target))})
	if !ok || reply.Status != blockproto.StatusOK || len(reply.Data) != len(target) {
		return blockproto.StatusIOError
	}
	copy(target, reply.Data)
	return blockproto.StatusOK
}
func (backend *driverBackend) WriteAt(offset uint64, data []byte) blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationWrite, Offset: offset, Length: uint32(len(data)), Data: data})
	if !ok {
		return blockproto.StatusIOError
	}
	return reply.Status
}
func (backend *driverBackend) Flush() blockproto.Status {
	reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationFlush})
	if !ok {
		return blockproto.StatusIOError
	}
	return reply.Status
}

var (
	backend          driverBackend
	service          blockproto.Service
	consoleHandle    handle.ID
	serviceHandle    handle.ID
	filesystemHandle handle.ID
	driverHandle     handle.ID
	controlHandle    handle.ID
	controlWire      [ipc.WireMaxSize]byte
	controlMessage   ipc.Message
	// driverLive is cleared when the driver's endpoint reports its peer gone.
	// Requests are then refused at once rather than each waiting out a deadline
	// for a process that is not there.
	driverLive       = true
	startupWire      [startup.Size]byte
	wire             [ipc.WireMaxSize]byte
	driverWire       [ipc.WireMaxSize]byte
	requestMessage   ipc.Message
	replyMessage     ipc.Message
	driverRequest    ipc.Message
	driverReply      ipc.Message
	driverDescriptor [3]uint64
	arenaBase        arena.Mark
	correlation      uint64
)

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("BLOCK_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != 0 {
		fatal("BLOCK_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("BLOCK_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "block")
	if !ok {
		fatal("BLOCK_CONSOLE_MISSING\n")
	}
	serviceHandle, ok = data.Handle("block")
	if !ok {
		fatal("BLOCK_ENDPOINT_MISSING\n")
	}
	driverHandle, ok = data.Handle("driver")
	if !ok {
		fatal("BLOCK_DRIVER_MISSING\n")
	}
	controlHandle, ok = data.Handle("control")
	if !ok {
		fatal("BLOCK_CONTROL_MISSING\n")
	}
	filesystemHandle, ok = data.Handle("filesystem")
	if !ok {
		fatal("BLOCK_FILESYSTEM_MISSING\n")
	}
	info, ok := exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || info.Status != blockproto.StatusOK || info.Capacity == 0 || info.BlockSize != 512 {
		fatal("BLOCK_DRIVER_INFO_FAILED\n")
	}
	backend.capacity, backend.blockSize = info.Capacity, info.BlockSize
	service, ok = blockproto.NewService(&backend)
	if !ok {
		fatal("BLOCK_SERVICE_INIT_FAILED\n")
	}
	arenaBase = arena.Current()
	mark("BLOCK_SERVICE_OK\n")
	syslog.Info("ready")
	for {
		arena.Reset(arenaBase)
		// The filesystem endpoint is absent while the filesystem service is
		// being restarted: waiting on a closed one would wake at once, forever.
		endpoints := [...]handle.ID{controlHandle, serviceHandle, filesystemHandle}
		count := len(endpoints)
		if filesystemHandle == 0 {
			count--
		}
		var interests [userwait.MaxObjects]userwait.Interest
		for index, endpoint := range endpoints[:count] {
			interests[index] = userwait.Interest{Handle: endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		}
		result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			fatal("BLOCK_WAIT_FAILED\n")
		}
		for index, endpoint := range endpoints[:count] {
			if result.ReadyMask&(1<<index) == 0 {
				continue
			}
			if index == 0 {
				control()
				continue
			}
			// A client that has gone is not a reason to stop serving the
			// others: this service outlives the processes that use it. A
			// filesystem service that has died is dropped until PID 1 hands
			// over its replacement's endpoint.
			if status := sys.IPCReceive(endpoint, wire[:]); status != ipc.StatusOK {
				if status == ipc.StatusPeerDied && endpoint == filesystemHandle {
					sys.HandleClose(filesystemHandle)
					filesystemHandle = 0
					syslog.Warn("filesystem endpoint closed")
				} else if status != ipc.StatusNotFound {
					syslog.Warn("client endpoint closed")
				}
				continue
			}
			serve(endpoint)
		}
	}
}

// control installs the endpoint of a restarted driver or filesystem service.
// PID 1 holds the only control endpoint, so nothing else can redirect where
// storage requests go or who may make them.
func control() {
	if sys.IPCReceive(controlHandle, controlWire[:]) != ipc.StatusOK {
		return
	}
	payloadSize := int(controlWire[24]) | int(controlWire[25])<<8
	if !ipc.DecodeInto(controlWire[:ipc.WireHeaderSize+payloadSize+int(controlWire[26])*8], &controlMessage) {
		return
	}
	status := blockproto.StatusOK
	if filesystem, valid := blockproto.DecodeSetFilesystem(controlMessage); valid {
		if filesystemHandle != 0 {
			sys.HandleClose(filesystemHandle)
		}
		filesystemHandle = filesystem
		syslog.Info("filesystem client replaced")
		mark("BLOCK_FILESYSTEM_REPLACED_OK\n")
	} else if replacement, valid := blockproto.DecodeSetDriver(controlMessage); !valid {
		status = blockproto.StatusInvalidArgument
	} else {
		sys.HandleClose(driverHandle)
		driverHandle, driverLive = replacement, true
		// The endpoint is installed without probing it. PID 1 hands it over
		// before the replacement driver exists, so a probe here would time out
		// against a process that has not started; the next real request is what
		// finds out whether the driver works, and reports it the ordinary way.
		syslog.Info("driver replaced")
		mark("BLOCK_DRIVER_REPLACED_OK\n")
	}
	if message, ok := blockproto.SetDriverReply(status, controlMessage.Correlation); ok {
		if size, encoded := ipc.EncodeFrom(&message, controlWire[:]); encoded {
			sys.IPCSend(controlHandle, controlWire[:size])
		}
	}
}

func serve(endpoint handle.ID) {
	payloadSize := int(wire[24]) | int(wire[25])<<8
	valid := ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMessage)
	if !valid {
		return
	}
	request, validRequest := blockproto.DecodeRequestFrom(&requestMessage)
	if !validRequest {
		reject(endpoint)
		return
	}
	encoded := blockproto.EncodeReplyInto(service.Dispatch(request), requestMessage.Correlation, &replyMessage)
	size, wireEncoded := ipc.EncodeFrom(&replyMessage, wire[:])
	if !encoded || !wireEncoded {
		return
	}
	if sys.IPCSend(endpoint, wire[:size]) != kcall.StatusOK {
		// The caller is gone. Its request dies with it; the service does not.
		syslog.Warn("reply undeliverable")
	}
}

// reject answers a request this service cannot parse. Staying silent would
// strand the caller until its deadline expires instead of reporting the fault.
func reject(endpoint handle.ID) {
	operation := blockproto.Operation(requestMessage.Payload[0])
	if requestMessage.Correlation == 0 || operation < blockproto.OperationInfo || operation > blockproto.OperationFlush {
		return
	}
	if !blockproto.EncodeReplyInto(blockproto.Reply{Operation: operation, Status: blockproto.StatusInvalidArgument}, requestMessage.Correlation, &replyMessage) {
		return
	}
	if size, ok := ipc.EncodeFrom(&replyMessage, wire[:]); ok {
		sys.IPCSend(endpoint, wire[:size])
	}
}

func exchange(request blockproto.Request) (blockproto.Reply, bool) {
	if !driverLive {
		return blockproto.Reply{}, false
	}
	correlation++
	if !blockproto.EncodeRequestInto(request, correlation, &driverRequest) {
		return blockproto.Reply{}, false
	}
	size, ok := ipc.EncodeFrom(&driverRequest, driverWire[:])
	if !ok || sys.IPCSend(driverHandle, driverWire[:size]) != 0 {
		return blockproto.Reply{}, false
	}
	if status := sys.IPCWaitReceive(driverHandle, driverWire[:], blockproto.RequestDeadline); status != 0 {
		if status == ipc.StatusPeerDied {
			// Every later request would wait out its deadline against a process
			// that is not there. They are refused until a driver is installed.
			driverLive = false
			syslog.Error("driver gone")
			mark("BLOCK_DRIVER_LOST_OK\n")
		}
		return blockproto.Reply{}, false
	}
	payloadSize := int(driverWire[24]) | int(driverWire[25])<<8
	valid := ipc.DecodeInto(driverWire[:ipc.WireHeaderSize+payloadSize+int(driverWire[26])*8], &driverReply)
	reply, validReply := blockproto.DecodeReplyFrom(&driverReply)
	return reply, valid && validReply && driverReply.Correlation == correlation
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	sys.ConsoleWriteString(consoleHandle, message)
}
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Diagnostics(consoleHandle)
	for {
	}
}
func main() {}
