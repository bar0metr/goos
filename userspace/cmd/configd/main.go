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
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

const (
	// configPath is on /state: settings are mutable system state, not user data
	// and not part of the immutable system image.
	configPath = "/state/config"
	// hostsPath holds the hosts table, apart from the settings: it is
	// records, not typed keys, and has its own format and bound.
	hostsPath        = "/state/hosts"
	vfsReplyDeadline = uint64(200)
	vfsReplyAttempts = 5
	// exportSize bounds the whole exported file. Every entry fits a key, a type
	// name and a value, and the store is bounded, so the file is too.
	exportSize = config.MaxEntries * (config.MaxKey + config.MaxText + 8)
	// maxSessions bounds the clients served at once. Configuration has few
	// readers, the shell and the services, and each costs a channel PID 1
	// must create.
	maxSessions = 16
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
	wire          [ipc.WireMaxSize]byte
	vfsWire       [ipc.WireMaxSize]byte
	descriptor    [3]uint64
	exportBuffer  [exportSize]byte
	requestMsg    ipc.Message
	arenaBase     arena.Mark
	correlation   uint64
	persistFailed bool
	hosts         config.Hosts
	// hostsBuffer is one byte past the file bound, so a longer file is seen
	// to be longer and its cut line is skipped.
	hostsBuffer        [config.HostsFileSize + 1]byte
	hostsPersistFailed bool
)

type session struct {
	endpoint handle.ID
	scope    config.Scope
	rights   config.Rights
	used     bool
}

//export userspaceMain
func userspaceMain() {
	arena.Init()
	arena.OnExhausted(func() { fatal("CONFIG_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
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
	// A volume that is absent or unreadable leaves the declared defaults in
	// place. Configuration that cannot be loaded is not a reason to refuse to
	// serve it: the shell needs a configuration service to repair a broken
	// configuration.
	restore()
	restoreHosts()
	arenaBase = arena.Current()
	mark("CONFIG_SERVICE_OK\n")
	syslog.Info("ready", syslog.Str("path", configPath))
	for {
		arena.Reset(arenaBase)
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
	if sys.IPCReceive(controlHandle, wire[:]) != 0 {
		return
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMsg) {
		return
	}
	// A restarted VFS takes the old filesystem session with it; PID 1 hands a
	// new one over this channel. A setting changed while the VFS was down is
	// written out now rather than at the next change (ADR 0099).
	if files, renewed := vfs.DecodeSessionNotice(&requestMsg); renewed {
		sys.HandleClose(vfsHandle)
		vfsHandle = files
		syslog.Info("filesystem session renewed")
		mark("CONFIG_SESSION_RENEWED_OK\n")
		if persistFailed {
			persist()
		}
		if hostsPersistFailed {
			persistHosts()
		}
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
			sys.IPCSend(controlHandle, wire[:size])
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
	sys.HandleClose(current.endpoint)
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
		// The network service's settings (ADR 0191) and timer policy (ADR 0192).
		// The service validates them as a whole when it loads them; the store
		// only checks their types. Times are in milliseconds.
		{"system.net.mode", mustText("dhcp"), false},
		{"system.net.ipv4.address", mustText("0.0.0.0"), false},
		{"system.net.ipv4.prefix", config.Uint(24), false},
		{"system.net.ipv4.gateway", mustText("0.0.0.0"), false},
		{"system.net.ipv4.default-ttl", config.Uint(64), false},
		{"system.net.ipv4.probe-count", config.Uint(2), false},
		{"system.net.ipv4.probe-interval-ms", config.Uint(500), false},
		{"system.net.arp.cache-timeout-ms", config.Uint(60000), false},
		{"system.net.arp.resolve-interval-ms", config.Uint(1000), false},
		{"system.net.arp.resolve-attempts", config.Uint(3), false},
		{"system.net.icmp.echo-reply-rate", config.Uint(20), false},
		{"system.net.dhcp.initial-retry-ms", config.Uint(4000), false},
		{"system.net.dhcp.max-retry-ms", config.Uint(64000), false},
		{"system.net.dhcp.conflict-retry-ms", config.Uint(10000), false},
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
	result, ok := config.Text(sys.Bytes(value))
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
	size, ok := readFile(configPath, exportBuffer[:])
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
	if sys.IPCReceive(current.endpoint, wire[:]) != 0 {
		// A peer that has gone leaves its endpoint readable forever otherwise.
		dropSession(current)
		return
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &requestMsg) {
		return
	}
	if requestMsg.ProtocolID == config.HostsProtocolID {
		serveHosts(current)
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
		sys.IPCSend(current.endpoint, wire[:size])
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
	if !writeFile(configPath, exportBuffer[:size]) {
		if !persistFailed {
			persistFailed = true
			syslog.Error("configuration not persisted", syslog.Str("path", configPath))
		}
		return
	}
	persistFailed = false
}

// restoreHosts loads the hosts table. A missing file is an empty table; a
// line that does not parse, repeats a name or names localhost is skipped and
// counted, as a configuration line is.
func restoreHosts() {
	size, ok := readFile(hostsPath, hostsBuffer[:])
	if !ok {
		return
	}
	if skipped := config.ParseHosts(hostsBuffer[:size], &hosts); skipped != 0 {
		syslog.Warn("hosts lines skipped", syslog.Uint("count", uint64(skipped)))
	}
}

// serveHosts answers a GHST request on a configuration session. The session's
// hosts rights decide; its scope does not apply to the table.
func serveHosts(current *session) {
	request, valid := config.DecodeHostsRequest(&requestMsg)
	reply := config.HostsReply{Operation: config.HostsOperation(requestMsg.Payload[0]), Status: config.StatusInvalidArgument, Generation: hosts.Generation}
	if valid {
		generation := hosts.Generation
		reply = hosts.Serve(current.rights, request)
		if hosts.Generation != generation {
			persistHosts()
		}
	}
	message, encoded := config.EncodeHostsReply(reply, requestMsg.Correlation)
	if size, ok := ipc.Encode(message, wire[:]); encoded && ok {
		sys.IPCSend(current.endpoint, wire[:size])
	}
}

// persistHosts rewrites /state/hosts from the table, as persist does for the
// settings.
func persistHosts() {
	data := config.AppendHosts(hostsBuffer[:0], &hosts)
	if !writeFile(hostsPath, data) {
		if !hostsPersistFailed {
			hostsPersistFailed = true
			syslog.Error("hosts not persisted", syslog.Str("path", hostsPath))
		}
		return
	}
	hostsPersistFailed = false
}

func readFile(path string, target []byte) (int, bool) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: sys.Bytes(path)})
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

func writeFile(path string, data []byte) bool {
	created, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: sys.Bytes(path)})
	if !ok || created.Status != vfs.StatusOK && created.Status != vfs.StatusAlreadyExists {
		return false
	}
	// Truncating first keeps the file from retaining the tail of a longer
	// previous version behind the new one.
	if reply, ok := callVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: sys.Bytes(path), Offset: 0}); !ok || reply.Status != vfs.StatusOK {
		return false
	}
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: sys.Bytes(path)})
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
	if !ok || sys.IPCSend(vfsHandle, vfsWire[:size]) != 0 {
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status := sys.IPCWaitReceive(vfsHandle, vfsWire[:], vfsReplyDeadline)
		if status == ipc.StatusTimeout {
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
