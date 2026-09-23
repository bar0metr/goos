//go:build goos_userspace

// The configuration service holds the system's typed settings. Configuration is
// not a directory of text files each component parses its own way, and it is not
// a bag of strings: keys are declared with a type, a write is checked against
// that declaration, and a client reaches the store over a session whose scope
// and rights decide what it may see and change.
//
// The service holds a console capability for its own logging, a VFS session for
// its file on the state volume, and one server endpoint per client session. It
// has no other authority, and it never hands a client anything but answers.
package main

import (
	"goos/internal/validation"
	"goos/internal/version"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/config"
	syslog "goos/userspace/log"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
	"runtime"
	"unsafe"
)

const (
	// configPath is on /state: settings are mutable system state, not user data
	// and not part of the immutable system image.
	configPath       = "/state/config"
	vfsReplyDeadline = uint64(200)
	vfsReplyAttempts = 5
	// exportSize bounds the whole exported file. Every entry fits a key, a type
	// name and a value, and the store is bounded, so the file is too.
	exportSize = config.MaxEntries * (config.MaxKey + config.MaxText + 8)
	// maxSessions bounds the clients served at once. Configuration has few
	// readers, and each costs a channel PID 1 must create.
	maxSessions = 6
)

var (
	store         config.Store
	consoleHandle handle.ID
	vfsHandle     handle.ID
	controlHandle handle.ID
	// Each session carries the scope and rights PID 1 granted it. The service
	// applies them to every request on that endpoint; the message carries no
	// authority of its own, so a client cannot ask for more than it was given.
	sessions     [maxSessions]session
	liveSessions int
	// ownScope is the service's own view for seeding and exporting. It is not
	// reachable from any endpoint.
	ownScope      config.Scope
	startupWire   [startup.Size]byte
	wire          [ipc.WireMaxSize]byte
	vfsWire       [ipc.WireMaxSize]byte
	descriptor    [3]uint64
	exportBuffer  [exportSize]byte
	requestMsg    ipc.Message
	heapPointer   uintptr
	arenaBase     uintptr
	correlation   uint64
	persistFailed bool
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

//go:linkname ipcReceive goos_ipc_receive
func ipcReceive(channel uint64, address unsafe.Pointer, size uintptr) uint64

//go:linkname startupRead goos_startup_read
func startupRead(address unsafe.Pointer, size uintptr) uint64

//go:linkname handleClose goos_handle_close
func handleClose(id uint64) uint64

type session struct {
	endpoint handle.ID
	scope    config.Scope
	rights   config.Rights
	used     bool
}

//export userspaceMain
func userspaceMain() {
	runtimeInitHeap()
	if startupRead(unsafe.Pointer(&startupWire[0]), startup.Size) != 0 {
		fatal("CONFIG_STARTUP_UNAVAILABLE\n")
	}
	data, ok := startup.Decode(startupWire[:])
	if !ok {
		fatal("CONFIG_STARTUP_INVALID\n")
	}
	consoleHandle, ok = data.Handle("console")
	syslog.Init(consoleHandle, "config")
	if !ok {
		fatal("CONFIG_CONSOLE_MISSING\n")
	}
	vfsHandle, ok = data.Handle("vfs")
	if !ok {
		fatal("CONFIG_VFS_MISSING\n")
	}
	controlHandle, ok = data.Handle("control")
	if !ok {
		fatal("CONFIG_CONTROL_MISSING\n")
	}
	ownScope, ok = config.MakeScope("")
	if !ok {
		fatal("CONFIG_SCOPE_INVALID\n")
	}
	declareSchema()
	restore()
	arenaBase = heapPointer
	mark("CONFIG_SERVICE_OK\n")
	syslog.Info("ready", syslog.Str("path", configPath))
	for {
		heapPointer = arenaBase
		var interests [userwait.MaxObjects]userwait.Interest
		var indexes [maxSessions]int
		interests[0] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		count := 1
		for index := range sessions {
			if !sessions[index].used {
				continue
			}
			interests[count] = userwait.Interest{Handle: sessions[index].endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			indexes[count-1] = index
			count++
		}
		result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			fatal("CONFIG_WAIT_FAILED\n")
		}
		if result.ReadyMask&1 != 0 {
			addSession()
		}
		for position := 1; position < count; position++ {
			if result.ReadyMask&(1<<uint(position)) != 0 {
				serve(&sessions[indexes[position-1]])
			}
		}
	}
}

// addSession installs an endpoint PID 1 has decided the scope and rights for.
// The service never chooses either: it is told, by the one process holding the
// control endpoint, and it enforces what it was told.
func addSession() {
	if ipcReceive(uint64(controlHandle), unsafe.Pointer(&wire[0]), ipc.WireMaxSize) != 0 {
		return
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMsg) {
		return
	}
	endpoint, scope, rights, valid := config.DecodeAddSession(requestMsg)
	status := config.StatusOK
	if !valid {
		status = config.StatusInvalidArgument
	} else if !install(endpoint, scope, rights) {
		status = config.StatusResourceExhausted
	}
	if message, ok := config.AddSessionReply(status, requestMsg.Correlation); ok {
		if size, encoded := ipc.Encode(message, wire[:]); encoded {
			ipcSend(uint64(controlHandle), unsafe.Pointer(&wire[0]), uintptr(size))
		}
	}
	if status == config.StatusOK {
		syslog.Info("session added", syslog.Uint("rights", uint64(rights)))
	}
}

func install(endpoint handle.ID, scope config.Scope, rights config.Rights) bool {
	for index := range sessions {
		if sessions[index].used {
			continue
		}
		sessions[index] = session{endpoint: endpoint, scope: scope, rights: rights, used: true}
		liveSessions++
		return true
	}
	return false
}

func dropSession(current *session) {
	handleClose(uint64(current.endpoint))
	*current = session{}
	liveSessions--
}

// declareSchema is the set of settings this image publishes. A key must be
// declared before it can be set, so the schema is what makes a write checkable
// and a typo a failure rather than a new setting nothing reads.
func declareSchema() {
	name, _ := config.Text([]byte(version.Name))
	release, _ := config.Text([]byte(version.Release))
	machine, _ := config.Text([]byte(version.Machine))
	declarations := [...]struct {
		key      string
		value    config.Value
		readOnly bool
	}{
		// Published by the running image and writable by nobody: a stored copy
		// would be a second source of truth about what is executing.
		{"system.build.name", name, true},
		{"system.build.release", release, true},
		{"system.build.machine", machine, true},
		{"system.log.persist", config.Bool(true), false},
		{"system.shell.prompt", mustText("goos> "), false},
		{"user.name", mustText("goos"), false},
	}
	for _, declaration := range declarations {
		if store.Declare(declaration.key, declaration.value, declaration.readOnly) != config.StatusOK {
			fatal("CONFIG_SCHEMA_INVALID\n")
		}
	}
}

func mustText(value string) config.Value {
	result, ok := config.Text(unsafe.Slice(unsafe.StringData(value), len(value)))
	if !ok {
		fatal("CONFIG_SCHEMA_INVALID\n")
	}
	return result
}

// restore applies the persisted file over the schema. A line that does not
// parse, names an undeclared key, or carries the wrong type is skipped and
// counted: a damaged file must not stop the system from booting with its
// declared defaults.
func restore() {
	size, ok := readFile(exportBuffer[:])
	if !ok {
		return
	}
	skipped := 0
	start := 0
	for index := 0; index < size; index++ {
		if exportBuffer[index] != '\n' {
			continue
		}
		key, value, parsed := config.ParseEntry(exportBuffer[start:index])
		if !parsed || store.Set(ownScope, config.Read|config.Write, key, value) != config.StatusOK {
			skipped++
		}
		start = index + 1
	}
	if skipped != 0 {
		syslog.Warn("configuration lines skipped", syslog.Uint("count", uint64(skipped)))
	}
}

func serve(current *session) {
	if ipcReceive(uint64(current.endpoint), unsafe.Pointer(&wire[0]), ipc.WireMaxSize) != 0 {
		// A peer that has gone leaves its endpoint readable forever otherwise.
		dropSession(current)
		return
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMsg) {
		return
	}
	request, valid := config.DecodeRequest(requestMsg)
	reply := config.Reply{Operation: request.Operation, Status: config.StatusInvalidArgument}
	if valid {
		reply = store.Serve(current.scope, current.rights, request)
	}
	if valid && request.Operation == config.OperationSet && reply.Status == config.StatusOK {
		persist()
	}
	message, encoded := config.EncodeReply(reply, requestMsg.Correlation)
	if !encoded {
		// A reply that cannot be encoded still has to be answered, or the
		// client waits for a message that will never come.
		message, encoded = config.EncodeReply(config.Reply{Operation: request.Operation, Status: config.StatusInvalidArgument}, requestMsg.Correlation)
	}
	if size, ok := ipc.Encode(message, wire[:]); encoded && ok {
		ipcSend(uint64(current.endpoint), unsafe.Pointer(&wire[0]), uintptr(size))
	}
}

// persist writes the whole store out. Configuration is small and bounded, so
// rewriting it is simpler than tracking which entry changed, and it leaves the
// file consistent with the store after every accepted write.
func persist() {
	size := 0
	for cursor := 0; ; {
		key, value, readOnly, next, status := store.At(ownScope, config.Read, cursor)
		if status != config.StatusOK {
			break
		}
		cursor = next
		if readOnly {
			continue
		}
		line, ok := config.AppendEntry(exportBuffer[size:size], key, value)
		if !ok {
			continue
		}
		size += len(line)
	}
	if !writeFile(exportBuffer[:size]) {
		if !persistFailed {
			persistFailed = true
			syslog.Error("configuration not persisted", syslog.Str("path", configPath))
		}
		return
	}
	persistFailed = false
}

func readFile(target []byte) (int, bool) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte(configPath)})
	if !ok || opened.Status != vfs.StatusOK {
		return 0, false
	}
	size := 0
	for size < len(target) {
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if !ok || reply.Status != vfs.StatusOK || len(reply.Data) == 0 {
			break
		}
		size += copy(target[size:], reply.Data)
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	return size, true
}

func writeFile(data []byte) bool {
	created, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: []byte(configPath)})
	if !ok || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		return false
	}
	// Truncating first keeps the file from retaining the tail of a longer
	// previous version behind the new one.
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: []byte(configPath), Offset: 0}); !ok || reply.Status != vfs.StatusOK {
		return false
	}
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: []byte(configPath)})
	if !ok || opened.Status != vfs.StatusOK {
		return false
	}
	for len(data) != 0 {
		chunk := len(data)
		if chunk > vfs.MaxProtocolData {
			chunk = vfs.MaxProtocolData
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: opened.Handle, Data: data[:chunk]})
		if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
			callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
			return false
		}
		data = data[chunk:]
	}
	callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	return true
}

func callVFS(request vfs.Request) (vfs.Reply, bool) {
	correlation++
	message, ok := vfs.EncodeRequest(request, correlation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, vfsWire[:])
	if !ok || ipcSend(uint64(vfsHandle), unsafe.Pointer(&vfsWire[0]), uintptr(size)) != 0 {
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		descriptor[0], descriptor[1], descriptor[2] = uint64(uintptr(unsafe.Pointer(&vfsWire[0]))), ipc.WireMaxSize, vfsReplyDeadline
		status := ipcWaitReceive(uint64(vfsHandle), unsafe.Pointer(&descriptor[0]), uintptr(len(descriptor)*8))
		if status == uint64(ipc.StatusTimeout) {
			continue
		}
		if status != 0 {
			return vfs.Reply{}, false
		}
		payloadSize := int(vfsWire[24]) | int(vfsWire[25])<<8
		received, valid := ipc.Decode(vfsWire[:ipc.WireHeaderSize+payloadSize+int(vfsWire[26])*8])
		if !valid || received.Correlation != correlation {
			return vfs.Reply{}, false
		}
		reply, valid := vfs.DecodeReply(received)
		return reply, valid && reply.Operation == request.Operation
	}
	return vfs.Reply{}, false
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
		fatal("CONFIG_OUT_OF_MEMORY\n")
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
