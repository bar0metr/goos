//go:build goos_userspace

// The log service is the persistent backend of the system log. The kernel ring
// is bounded and volatile: it holds the newest records and loses them at reset,
// which is exactly when a log is most wanted. This service drains the ring into
// a file on the persistent volume so that what happened before a reboot can be
// read after one.
//
// It holds no authority beyond the log capability that authorises reading the
// ring, console write for its own records, and a VFS session for its own file, and it never becomes a path that
// other services log through: they keep writing to the kernel ring, which stays
// the single ordering point.
package main

import (
	"goos/internal/logfmt"
	"goos/internal/validation"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
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
	// logHandle authorizes reading the kernel ring. It is not the console:
	// the service can drain the log without being able to read the keyboard.
	logHandle    handle.ID
	vfsHandle    handle.ID
	configHandle handle.ID
	// controlHandle is the channel PID 1 hands a new filesystem session over
	// when the VFS was restarted and the old one died with it (ADR 0099).
	controlHandle handle.ID
	configWire    [ipc.WireMaxSize]byte
	// persisting is the setting that governs this service. It is consulted on
	// every pass, so turning persistence off takes effect at the next one
	// rather than at the next boot.
	persisting  = true
	startupWire [startup.Size]byte
	wire        [ipc.WireMaxSize]byte
	descriptor  [3]uint64
	logLine     [kcall.LogText]byte
	lostLine    [64]byte
	arenaBase   arena.Mark
	correlation uint64
	// cursor is the sequence number of the next record to persist.
	cursor uint64
	// reported bounds how often a persistent failure is announced, so a full
	// or broken volume cannot turn every drain into more log records.
	reported bool
)

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("LOG_OUT_OF_MEMORY\n") })
	if sys.StartupRead(&startupWire) != 0 {
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
	logHandle, ok = data.Handle("log")
	if !ok {
		fatal("LOG_RING_CAPABILITY_MISSING\n")
	}
	vfsHandle, ok = data.Handle("vfs")
	if !ok {
		fatal("LOG_VFS_MISSING\n")
	}
	// The configuration session is optional: without one the service keeps its
	// default, which is to persist.
	configHandle, _ = data.Handle("config")
	controlHandle, _ = data.Handle("control")
	_, restarted := data.Env(startup.RestartVariable)
	if !openLog(restarted) {
		fatal("LOG_RING_UNAVAILABLE\n")
	}
	arenaBase = arena.Current()
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
// openLog fixes the starting cursor. The file itself is created by the first
// pass that has something to write, so storage that is not ready yet delays
// persistence rather than disabling it.
func openLog(restarted bool) bool {
	oldest, total, status := sys.LogBounds(logHandle)
	if status != kcall.StatusOK {
		return false
	}
	cursor = oldest
	// A service started again in the same boot finds the records its
	// predecessor wrote at the end of the file, and the ring still holds
	// them. It resumes after the last one, so a crash of this service writes
	// nothing twice (ADR 0100). After a reboot the file ends with an earlier
	// boot's records, whose times say nothing about this one's.
	if last, found := lastPersisted(); restarted && found {
		for cursor < total && sys.LogRead(logHandle, cursor-oldest, &logLine) == kcall.StatusOK {
			written, ok := logfmt.TimeOf(logLine[:])
			if !ok || written > last {
				break
			}
			cursor++
		}
		syslog.Info("resumed", syslog.Uint("skipped", cursor-oldest))
		mark("LOG_RESUMED_OK\n")
	}
	return true
}

// tailSize is how much of a log file is read to find its last record: more
// than the longest record and its newline, so the last one is always whole.
const tailSize = kcall.LogText + 16

var tail [tailSize]byte

// lastPersisted returns the time of the last record in the log, which is in
// the rotated file when the current one has just been started.
func lastPersisted() (uint64, bool) {
	for _, path := range [...]string{logPath, rotatePath} {
		if last, found := lastRecordTime([]byte(path)); found {
			return last, true
		}
	}
	return 0, false
}

func lastRecordTime(path []byte) (uint64, bool) {
	stat, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: path})
	if !ok || stat.Status != vfs.StatusOK || stat.Size == 0 {
		return 0, false
	}
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	start := stat.Size - min(stat.Size, tailSize)
	size := 0
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: opened.Handle, Offset: start}); ok && reply.Status == vfs.StatusOK {
		for size < len(tail) {
			reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: uint32(min(len(tail)-size, vfs.MaxReplyData))})
			if !ok || reply.Status != vfs.StatusOK || len(reply.Data) == 0 {
				break
			}
			size += copy(tail[size:], reply.Data)
		}
	}
	closeFile(uint64(opened.Handle))
	return logfmt.TimeOf(lastLine(tail[:size]))
}

// lastLine returns the last newline-terminated line of data, without its
// newline.
func lastLine(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return nil
	}
	end := len(data) - 1
	start := end
	for start > 0 && data[start-1] != '\n' {
		start--
	}
	return data[start:end]
}

// drain appends every record produced since the last pass. A cursor below the
// ring's oldest retained sequence means records were overwritten before this
// pass reached them; the count is written into the file rather than skipped, so
// a gap in the persisted log is visible as a gap.
func drain() {
	arena.Reset(arenaBase)
	takeSession()
	oldest, total, status := sys.LogBounds(logHandle)
	if status != kcall.StatusOK {
		return
	}
	if !shouldPersist() {
		// The cursor still advances. Re-enabling persistence should start from
		// the present rather than write out everything that happened while it
		// was off, which the ring has mostly lost anyway.
		cursor = total
		return
	}
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
		arena.Reset(arenaBase)
		if sys.LogRead(logHandle, cursor-oldest, &logLine) != kcall.StatusOK {
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
	arena.Reset(arenaBase)
	reported = false
	if offset >= rotationBound() {
		rotate()
	}
}

// takeSession installs a filesystem session PID 1 has handed over, if there
// is one. The records produced while the VFS was down are still in the ring,
// and the cursor still names the first of them, so the next pass persists them
// without writing any twice.
func takeSession() {
	for controlHandle != 0 && sys.IPCReceive(controlHandle, wire[:]) == 0 {
		payloadSize := int(wire[24]) | int(wire[25])<<8
		message, valid := ipc.Decode(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8])
		if !valid {
			continue
		}
		if settings, ok := config.DecodeSessionNotice(&message); ok {
			// The configuration service was restarted and took the old
			// session with it (ADR 0100).
			if configHandle != 0 {
				sys.HandleClose(configHandle)
			}
			configHandle = settings
			mark("LOG_CONFIG_RENEWED_OK\n")
			continue
		}
		session, ok := vfs.DecodeSessionNotice(&message)
		if !ok {
			sys.HandleClose(message.Handles[0])
			continue
		}
		sys.HandleClose(vfsHandle)
		vfsHandle, reported = session, false
		syslog.Info("filesystem session renewed")
		mark("LOG_SESSION_RENEWED_OK\n")
	}
}

// shouldPersist asks the configuration service whether this service should be
// writing. A session it cannot reach, or an answer it cannot use, leaves the
// last known value in place: configuration governs the service, it does not
// decide whether the service works.
func shouldPersist() bool {
	if configHandle == 0 {
		return persisting
	}
	reply, ok := callConfig(config.Request{Operation: config.OperationGet, Key: []byte("system.log.persist")})
	if !ok || reply.Status != config.StatusOK || reply.Value.Kind != config.KindBool {
		return persisting
	}
	enabled := reply.Value.Uint == 1
	if enabled != persisting {
		persisting = enabled
		syslog.Info("persistence changed", syslog.Uint("enabled", reply.Value.Uint))
	}
	return persisting
}

func callConfig(request config.Request) (config.Reply, bool) {
	correlation++
	message, ok := config.EncodeRequest(request, correlation)
	if !ok {
		return config.Reply{}, false
	}
	size, ok := ipc.Encode(message, configWire[:])
	if !ok || sys.IPCSend(configHandle, configWire[:size]) != 0 {
		return config.Reply{}, false
	}
	if sys.IPCWaitReceive(configHandle, configWire[:], vfsReplyDeadline) != 0 {
		return config.Reply{}, false
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	received, valid := ipc.Decode(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8])
	if !valid || received.Correlation != correlation {
		return config.Reply{}, false
	}
	reply, valid := config.DecodeReply(received)
	return reply, valid && reply.Operation == request.Operation
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
	// The file is created here rather than only at startup. Storage can be
	// absent when this service starts and arrive later, and the file can be
	// removed from under it; either way the next pass makes one rather than
	// giving up until a restart.
	created, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: []byte(logPath)})
	if !ok || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		return 0, 0, false
	}
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
	if !ok || sys.IPCSend(vfsHandle, wire[:size]) != 0 {
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status := sys.IPCWaitReceive(vfsHandle, wire[:], vfsReplyDeadline)
		if status == ipc.StatusTimeout {
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
