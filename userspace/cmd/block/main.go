//go:build goos_userspace

package main

import (
	"goos/internal/capability"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	blockproto "goos/userspace/block"
	syslog "goos/userspace/log"
	"goos/userspace/partition"
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

// Transfer serves a bulk request through this service's own buffer, which it
// shares with the driver: the caller's object is mapped for the request, and
// its bytes are copied to or from the buffer a chunk at a time. A handle that
// arrived over IPC cannot be passed on — the kernel strips Transfer when a
// handle moves — so the caller's memory never reaches the driver (ADR 0107).
func (backend *driverBackend) Transfer(request blockproto.Request) blockproto.Status {
	transferred = true
	read := request.Operation == blockproto.OperationReadBulk
	base, size, status := sys.MemoryMapAnywhere(request.Object, read)
	sys.HandleClose(request.Object)
	if status != kcall.StatusOK {
		return blockproto.StatusInvalidArgument
	}
	result := blockproto.StatusOK
	length := uint64(request.Length)
	if uint64(request.Buffer)+length > size || bulkMemory == nil {
		result = blockproto.StatusInvalidArgument
	}
	client := sys.Window(base, size)
	for done := uint64(0); done < length && result == blockproto.StatusOK; {
		chunk := min(length-done, uint64(len(bulkMemory)))
		part := client[uint64(request.Buffer)+done : uint64(request.Buffer)+done+chunk]
		rights := capability.Read | capability.Transfer
		if read {
			rights |= capability.Write
		} else {
			copy(bulkMemory[:chunk], part)
		}
		duplicate, dupStatus := sys.HandleDuplicate(bulkObject, rights)
		if dupStatus != kcall.StatusOK {
			result = blockproto.StatusIOError
			break
		}
		reply, ok := exchange(blockproto.Request{Operation: request.Operation, Offset: request.Offset + done, Length: uint32(chunk), Object: duplicate})
		switch {
		case !ok:
			result = blockproto.StatusIOError
		case reply.Status != blockproto.StatusOK:
			result = reply.Status
		case read:
			copy(part, bulkMemory[:chunk])
		}
		done += chunk
	}
	sys.VMUnmap(base, size)
	return result
}

// bulkObject is the buffer bulk requests move through on their way to and
// from the driver; bulkMemory is this service's mapping of it.
var (
	bulkObject handle.ID
	bulkMemory []byte
)

const bulkPages = 32

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
	wire             [ipc.WireMaxSize]byte
	driverWire       [ipc.WireMaxSize]byte
	requestMessage   ipc.Message
	replyMessage     ipc.Message
	driverRequest    ipc.Message
	driverReply      ipc.Message
	driverDescriptor [3]uint64
	arenaBase        arena.Mark
	correlation      uint64
	// transferred says the backend took the object a bulk request carried;
	// one the service refused first is closed after the reply.
	transferred bool
)

// The filesystem service reaches one volume of the device, not the device
// (ADR 0115). The partition table is read when the device is first
// described, and again whenever PID 1 hands over a filesystem endpoint with a
// selector; filesystemSlice is what the selector chose, and until a volume is
// chosen the filesystem endpoint is answered with I/O errors.
var (
	scanner            partition.Scanner
	table              partition.Table
	filesystemSelector partition.Selector
	filesystemSlice    blockproto.Slice
	sliceChosen        bool
)

// deviceReader reads the device for the partition scanner, through the bulk
// buffer when it has one and in message-sized pieces otherwise.
type deviceReader struct{}

func (deviceReader) ReadAt(offset uint64, target []byte) bool {
	for len(target) != 0 {
		chunk := min(len(target), blockproto.MaxData)
		if bulkMemory != nil && len(target) > blockproto.MaxData {
			chunk = min(len(target), len(bulkMemory))
			duplicate, status := sys.HandleDuplicate(bulkObject, capability.Read|capability.Write|capability.Transfer)
			if status != kcall.StatusOK {
				return false
			}
			reply, ok := exchange(blockproto.Request{Operation: blockproto.OperationReadBulk, Offset: offset, Length: uint32(chunk), Object: duplicate})
			if !ok || reply.Status != blockproto.StatusOK {
				return false
			}
			copy(target[:chunk], bulkMemory[:chunk])
		} else if backend.ReadAt(offset, target[:chunk]) != blockproto.StatusOK {
			return false
		}
		target = target[chunk:]
		offset += uint64(chunk)
	}
	return true
}

// chooseVolume reads the partition table and resolves the filesystem
// service's selector against it. What it finds is logged; a device with no
// usable volume leaves the filesystem endpoint without one, and the service
// behind it reports its volume refused (ADR 0077).
func chooseVolume() {
	sliceChosen = false
	status := scanner.Scan(deviceReader{}, backend.capacity, backend.blockSize, &table)
	if status != partition.StatusOK {
		syslog.Error("partition table unusable", syslog.Uint("status", uint64(status)))
		mark("BLOCK_VOLUME_NONE\n")
		return
	}
	if table.GPT.Degraded() {
		syslog.Warn("partition table degraded",
			syslog.Uint("used-backup", boolValue(table.GPT.UsedBackup)),
			syslog.Uint("backup-invalid", boolValue(table.GPT.BackupInvalid)),
			syslog.Uint("backup-differs", boolValue(table.GPT.BackupDiffers)),
			syslog.Uint("backup-misplaced", boolValue(table.GPT.BackupMisplaced)))
	}
	volume, status := partition.Select(&table, &filesystemSelector)
	if status != partition.StatusOK {
		syslog.Error("no volume for the filesystem", syslog.Str("table", table.Scheme.String()),
			syslog.Uint("partitions", uint64(table.Count)), syslog.Uint("status", uint64(status)))
		mark("BLOCK_VOLUME_NONE\n")
		return
	}
	filesystemSlice = blockproto.Slice{Start: volume.Start, Length: volume.Length, Writable: true}
	sliceChosen = service.Contains(filesystemSlice)
	syslog.Info("volume chosen", syslog.Str("table", table.Scheme.String()), syslog.Uint("partition", uint64(volume.Index)),
		syslog.Uint("start-kib", volume.Start>>10), syslog.Uint("size-mib", volume.Length>>20))
	switch table.Scheme {
	case partition.SchemeGPT:
		mark("BLOCK_VOLUME_GPT_OK\n")
	case partition.SchemeMBR:
		mark("BLOCK_VOLUME_MBR_OK\n")
	default:
		mark("BLOCK_VOLUME_WHOLE_OK\n")
	}
}

func boolValue(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("BLOCK_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
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
	controlHandle, ok = data.Handle("control")
	if !ok {
		fatal("BLOCK_CONTROL_MISSING\n")
	}
	// At boot the driver and the filesystem service are there before this
	// service. A restarted one starts with neither: PID 1 hands over the
	// endpoint of each as it starts again after this one (ADR 0101).
	driverHandle, driverLive = data.Handle("driver")
	filesystemHandle, _ = data.Handle("filesystem")
	// The bulk buffer comes first: describing the device reads its
	// partition table through it.
	setUpBulk()
	if driverLive && !describeDevice() {
		fatal("BLOCK_DRIVER_INFO_FAILED\n")
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
		// A restarted service has no driver until PID 1 hands one over,
		// and the restarted filesystem service may ask for its volume
		// first. Requests wait in their channels until the driver is here,
		// so the volume is not refused for a device that is on its way.
		if driverHandle == 0 {
			count = 1
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
	if filesystem, selector, valid := blockproto.DecodeSetFilesystem(controlMessage); valid {
		if filesystemHandle != 0 {
			sys.HandleClose(filesystemHandle)
		}
		filesystemHandle, filesystemSelector = filesystem, selector
		syslog.Info("filesystem client replaced")
		mark("BLOCK_FILESYSTEM_REPLACED_OK\n")
		// The volume is chosen again, from the table as it is now; a
		// restarted service without a driver chooses once it describes
		// the device.
		if backend.capacity != 0 {
			chooseVolume()
		}
	} else if replacement, valid := blockproto.DecodeSetDriver(controlMessage); !valid {
		status = blockproto.StatusInvalidArgument
	} else {
		if driverHandle != 0 {
			sys.HandleClose(driverHandle)
		}
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
	// A restarted service learns the device from the first request that
	// needs it, once a driver has been handed over.
	describeDevice()
	request, validRequest := blockproto.DecodeRequestFrom(&requestMessage)
	if !validRequest {
		// Whatever handles arrived are this process's now.
		for _, attached := range requestMessage.Handles[:requestMessage.HandleCount] {
			sys.HandleClose(attached)
		}
		reject(endpoint)
		return
	}
	transferred = false
	var reply blockproto.Reply
	switch {
	case endpoint != filesystemHandle:
		reply = service.Dispatch(request)
	case sliceChosen:
		reply = service.DispatchIn(request, filesystemSlice)
	default:
		reply = blockproto.Reply{Operation: request.Operation, Status: blockproto.StatusIOError}
	}
	if request.Operation.Bulk() && !transferred {
		// Refused before the backend took it: the object is still here.
		sys.HandleClose(request.Object)
	}
	encoded := blockproto.EncodeReplyInto(reply, requestMessage.Correlation, &replyMessage)
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
	if requestMessage.Correlation == 0 || !operation.Valid() {
		return
	}
	if !blockproto.EncodeReplyInto(blockproto.Reply{Operation: operation, Status: blockproto.StatusInvalidArgument}, requestMessage.Correlation, &replyMessage) {
		return
	}
	if size, ok := ipc.EncodeFrom(&replyMessage, wire[:]); ok {
		sys.IPCSend(endpoint, wire[:size])
	}
}

// describeDevice asks the driver for the device's geometry, once. Until it has
// an answer the service refuses every request with an I/O error.
func describeDevice() bool {
	if backend.capacity != 0 {
		return true
	}
	info, ok := exchange(blockproto.Request{Operation: blockproto.OperationInfo})
	if !ok || info.Status != blockproto.StatusOK || info.Capacity == 0 || info.BlockSize != 512 {
		return false
	}
	backend.capacity, backend.blockSize = info.Capacity, info.BlockSize
	if service, ok = blockproto.NewService(&backend); ok {
		chooseVolume()
	}
	return ok
}

// exchange sends a request to the driver and waits for its reply. A bulk
// request's object moves to the driver with it; if it cannot be sent, the
// object is closed here instead.
func exchange(request blockproto.Request) (blockproto.Reply, bool) {
	if !send(request) {
		if request.Operation.Bulk() {
			sys.HandleClose(request.Object)
		}
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

// send sends one request to the driver.
func send(request blockproto.Request) bool {
	if !driverLive {
		return false
	}
	correlation++
	if !blockproto.EncodeRequestInto(request, correlation, &driverRequest) {
		return false
	}
	size, ok := ipc.EncodeFrom(&driverRequest, driverWire[:])
	return ok && sys.IPCSend(driverHandle, driverWire[:size]) == 0
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	sys.ConsoleWriteString(consoleHandle, message)
}

// fatal reports why the service stops and exits, so that the supervisor
// restarts it (ADR 0101).
func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	sys.Diagnostics(consoleHandle)
	sys.Exit(1)
}
func main() {}
