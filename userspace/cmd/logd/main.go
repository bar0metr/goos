//go:build goos_userspace

// The log service is the persistent backend of the system log. The kernel ring
// is bounded and volatile: it holds the newest records and loses them at reset,
// which is exactly when a log is most wanted. This service drains the ring into
// a file on the persistent volume so that what happened before a reboot can be
// read after one.
//
// It holds no authority beyond the console capability that authorises reading
// the ring and a VFS session for its own file, and it never becomes a path that
// other services log through: they keep writing to the kernel ring, which stays
// the single ordering point.
package main

import (
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	syslog "goos/userspace/log"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
	"runtime"
	"unsafe"
)

const (
	// logPath is on /state because a log is mutable system state rather than
	// user data or part of the immutable system image.
	logPath    = "/state/system.log"
	rotatePath = "/state/system.log.1"
	// releaseLogBytes bounds the live file. FAT32 here holds at most 64 KiB per
	// file, and one rotation keeps between this and twice this much history.
	releaseLogBytes = uint64(24 * 1024)
	// A boot produces a few kilobytes of records, so the validation build uses
	// a bound a single session crosses. Only the threshold differs; the
	// rotation the boot test exercises is the one a release build performs.
	validationLogBytes = uint64(1024)
	// drainInterval is how long the service sleeps between passes, in timer
	// ticks. The ring holds 64 records, so a burst larger than that inside one
	// interval loses the oldest of them, which the service reports rather than
	// hides.
	drainInterval    = uint64(100)
	vfsReplyDeadline = uint64(200)
	vfsReplyAttempts = 5
)

var (
	consoleHandle handle.ID
	vfsHandle     handle.ID
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	descriptor    [3]uint64
	logLine       [kcall.LogText]byte
	lostLine      [64]byte
	boundsWire    [kcall.LogBoundsSize]byte
	heapPointer   uintptr
	arenaBase     uintptr
	correlation   uint64
	// cursor is the sequence number of the next record to persist.
	cursor uint64
	// reported bounds how often a persistent failure is announced, so a full
	// or broken volume cannot turn every drain into more log records.
	reported bool
)

//go:extern _heap_start
var heapStartSymbol [0]byte

//go:extern _heap_end
var heapEndSymbol [0]byte

//go:linkname diagnostics goos_diagnostics
func diagnostics(device uint64, address uintptr) uint64

//go:linkname consoleWrite goos_console_write
func consoleWrite(device uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcSend goos_ipc_send
func ipcSend(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname ipcWaitReceive goos_ipc_wait_receive
func ipcWaitReceive(channel uint64, descriptor unsafe.Pointer, size uintptr) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname logRead goos_log_read
func logRead(device uint64, index uint64, address unsafe.Pointer) uint64

//go:linkname logBounds goos_log_bounds
func logBounds(device uint64, address unsafe.Pointer) uint64

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("LOG_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("LOG_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "log")
	if !ok {
		fatal("LOG_CONSOLE_MISSING\n")
	}
	vfsHandle, ok = data.Handle("vfs")
	if !ok {
		fatal("LOG_VFS_MISSING\n")
	}
	if !openLog() {
		fatal("LOG_FILE_UNAVAILABLE\n")
	}
	arenaBase = heapPointer
	mark("LOG_SERVICE_OK\n")
	syslog.Info("persisting", syslog.Str("path", logPath))
	for {
		userwait.Sleep(drainInterval)
		drain()
	}
}

// openLog makes sure the file exists and starts the cursor at the end of the
// ring. Records already in the ring at startup are persisted: they describe
// this boot, and losing them would mean the log never covers its own start.
func openLog() bool {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: []byte(logPath)})
	if !ok || reply.Status != vfs.StatusOK && reply.Status != vfs.StatusAlreadyExists {
		return false
	}
	if logBounds(uint64(consoleHandle), unsafe.Pointer(&boundsWire[0])) != 0 {
		return false
	}
	cursor = readUint64(boundsWire[0:8])
	return true
}

// drain appends every record produced since the last pass. A cursor below the
// ring's oldest retained sequence means records were overwritten before this
// pass reached them; the count is written into the file rather than skipped, so
// a gap in the persisted log is visible as a gap.
func drain() {
	heapPointer = arenaBase
	if logBounds(uint64(consoleHandle), unsafe.Pointer(&boundsWire[0])) != 0 {
		return
	}
	oldest, total := readUint64(boundsWire[0:8]), readUint64(boundsWire[8:16])
	if cursor >= total && cursor >= oldest {
		// Nothing new. An idle pass costs one native call and touches neither
		// the filesystem nor the disk, so a quiet system pays almost nothing
		// for having a log service.
		return
	}
	// The file is opened once for the whole pass. Reopening per record would
	// triple the requests a pass makes, and a pass holds the single-threaded
	// filesystem service for as long as it runs.
	file, offset, ok := openForAppend()
	if !ok {
		announce("log file unavailable")
		return
	}
	if cursor < oldest {
		lost := oldest - cursor
		cursor = oldest
		size := copy(lostLine[:], "[lost ")
		size += putUint(lostLine[size:], lost)
		size += copy(lostLine[size:], " records]\n")
		written, ok := appendRecord(file, lostLine[:size])
		offset += written
		if !ok {
			closeFile(file)
			announce("log write failed")
			return
		}
	}
	for cursor < total {
		// One record at a time out of a fresh arena. A drain can persist the
		// whole ring, and the request messages of sixty-four records at once
		// would not fit the bump allocator this runtime has. The open handle is
		// a value, so it survives the reset.
		heapPointer = arenaBase
		if logRead(uint64(consoleHandle), cursor-oldest, unsafe.Pointer(&logLine[0])) != 0 {
			break
		}
		size := 0
		for size < len(logLine) && logLine[size] != 0 {
			size++
		}
		written, ok := appendRecord(file, logLine[:size])
		offset += written
		if !ok {
			closeFile(file)
			announce("log write failed")
			return
		}
		cursor++
	}
	closeFile(file)
	heapPointer = arenaBase
	reported = false
	if offset >= rotationBound() {
		rotate()
	}
}

func rotationBound() uint64 {
	if validation.Enabled {
		return validationLogBytes
	}
	return releaseLogBytes
}

// rotate keeps one previous file. The old one is removed first so the rename
// never has to replace a live entry, and a failure leaves the current file in
// place rather than losing it.
func rotate() {
	callVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: []byte(rotatePath)})
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRename, Path: []byte(logPath), Data: []byte(rotatePath)}); !ok || reply.Status != vfs.StatusOK {
		announce("log rotation failed")
		return
	}
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: []byte(logPath)}); !ok || reply.Status != vfs.StatusOK {
		announce("log rotation failed")
	}
}

// openForAppend opens the live file and positions it at the end, returning the
// handle and the size it starts at.
func openForAppend() (uint64, uint64, bool) {
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: []byte(logPath)})
	if !ok || stat.Status != vfs.StatusOK {
		return 0, 0, false
	}
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(logPath)})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, 0, false
	}
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: stat.Size}); !ok || reply.Status != vfs.StatusOK {
		closeFile(uint64(opened.Handle))
		return 0, 0, false
	}
	return uint64(opened.Handle), stat.Size, true
}

func closeFile(file uint64) {
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(file)})
}

// appendRecord writes one line through an already positioned handle and reports
// how many bytes reached the file. The VFS protocol carries a bounded amount of
// inline data per request, so a line goes out in chunks of that size and each
// write advances the handle's own offset.
func appendRecord(file uint64, data []byte) (uint64, bool) {
	written := uint64(0)
	for len(data) != 0 {
		chunk := len(data)
		if chunk > vfs.MaxProtocolData {
			chunk = vfs.MaxProtocolData
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: vfs.Handle(file), Data: data[:chunk]})
		if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
			return written, false
		}
		written += uint64(chunk)
		data = data[chunk:]
	}
	return written, true
}

// announce reports a persistent failure once. Logging from the log service
// produces records this service will itself try to persist, so repeating a
// failure every pass would turn a broken volume into a growing ring of
// complaints about it.
func announce(message string) {
	if reported {
		return
	}
	reported = true
	syslog.Error(message, syslog.Str("path", logPath))
}

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, wire[:])
	if !ok || ipcSend(uint64(vfsHandle), unsafe.Pointer(&wire[0]), uintptr(size)) != 0 {
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&wire[0]))), ipc.WireMaxSize, vfsReplyDeadline
		status := ipcWaitReceive(uint64(vfsHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			return vfs.Reply{}, false
		}
		payloadSize := int(wire[24]) | int(wire[25])<<8
		received, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		return reply, valid && reply.Operation == request.Operation
	}
	return vfs.Reply{}, false
}

func readUint64(data []byte) uint64 {
	value := uint64(0)
	for index := 7; index >= 0; index-- {
		value = value<<8 | uint64(data[index])
	}
	return value
}

func putUint(target []byte, value uint64) int {
	var digits [20]byte
	size := 0
	for {
		digits[size] = byte('0' + value%10)
		size++
		value /= 10
		if value == 0 {
			break
		}
	}
	for index := 0; index < size; index++ {
		target[index] = digits[size-1-index]
	}
	return size
}

func mark(message string) {
	if validation.Enabled {
		write(message)
	}
}

func write(message string) {
	consoleWrite(uint64(consoleHandle), unsafe.Pointer(unsafe.StringData(message)), uintptr(len(message)))
}

func fatal(message string) {
	if !syslog.Error(message[:len(message)-1]) {
		write(message)
	}
	diagnostics(uint64(consoleHandle), 0)
	for {
	}
}

func main() {}

//go:linkname runtimeInitHeap runtime.initHeap
func runtimeInitHeap() { heapPointer = uintptr(unsafe.Pointer(&heapStartSymbol)) }

//go:linkname runtimeAlloc runtime.alloc
func runtimeAlloc(size uintptr, _ unsafe.Pointer) unsafe.Pointer {
	size = (size + 15) &^ 15
	start, end := heapPointer, heapPointer+size
	if end < start || end > uintptr(unsafe.Pointer(&heapEndSymbol)) {
		fatal("LOG_OUT_OF_MEMORY\n")
	}
	heapPointer = end
	cleared := unsafe.Slice((*byte)(unsafe.Pointer(start)), size)
	for index := range cleared {
		cleared[index] = 0
	}
	return unsafe.Pointer(start)
}

//go:linkname runtimeFree runtime.free
func runtimeFree(unsafe.Pointer) {}

//go:linkname runtimeMarkRoots runtime.markRoots
func runtimeMarkRoots(uintptr, uintptr) {}

//go:linkname runtimeGC runtime.GC
func runtimeGC() {}

//go:linkname runtimeSetFinalizer runtime.SetFinalizer
func runtimeSetFinalizer(interface{}, interface{}) {}

//go:linkname runtimeReadMemStats runtime.ReadMemStats
func runtimeReadMemStats(*runtime.MemStats) {}
