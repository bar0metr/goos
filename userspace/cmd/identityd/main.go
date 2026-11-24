//go:build goos_userspace

// identityd is the supervised identity service (ADR 0233). It holds the one
// bounded account/group/key store, a single durable snapshot on /state, and a
// versioned IPC surface: PID 1 opens administration and lookup sessions over
// the control channel, and every request is answered over its session. The
// store itself lives in the host-tested userspace/identityd package; this file
// is the native entry point and event loop.
package main

import (
	"errors"
	"strings"

	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/cryptosys"
	"goos/userspace/identity"
	"goos/userspace/identityd"
	"goos/userspace/identityproto"
	syslog "goos/userspace/log"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	userwait "goos/userspace/wait"
)

const (
	// dbPath is the single snapshot file. PID 1 confines the vfs session to
	// /state/identity, so the absolute path is the whole reachable subtree.
	dbPath = "/state/identity/db"
	// Every outbound VFS request uses a bounded deadline.
	vfsReplyDeadline = uint64(200)
	vfsReplyAttempts = 5
	// maxSessions bounds the admin and lookup sessions served at once.
	maxSessions = 16
	// identityReplyHeader and identityRecordFixed track the version-2 identity
	// codec layout (identityproto replyHeader and recordFixed) so list replies
	// can be chunked against ipc.MaxPayload. identityproto owns the codec;
	// these sizes are used only for chunking, never to encode records by hand.
	identityReplyHeader = 40
	identityRecordFixed = 56
)

var (
	service *identityd.Service

	controlHandle handle.ID
	vfsHandle     handle.ID
	logHandle     handle.ID

	wire    [ipc.WireMaxSize]byte
	vfsWire [ipc.WireMaxSize]byte

	controlMsg ipc.Message
	requestMsg ipc.Message

	// vfsCorrelation numbers outbound VFS requests.
	vfsCorrelation uint64
	// vfsDead is set when the confined VFS session's peer has gone. identityd
	// cannot renew it in place; the supervisor restarts it with fresh grants.
	vfsDead bool

	sessions [maxSessions]session
)

type session struct {
	endpoint handle.ID
	admin    bool
	used     bool
}

//export userspaceMain
func userspaceMain() {
	arena.InitService()
	arena.OnExhausted(func() { fatal("IDENTITY_OUT_OF_MEMORY\n") })
	data, ok := startup.Read()
	if !ok {
		fatal("IDENTITY_STARTUP_INVALID\n")
	}
	if logHandle, ok = data.Handle("log"); ok {
		syslog.Init(logHandle, "identity")
	}
	controlHandle, ok = data.Handle("control")
	if !ok {
		fatal("IDENTITY_CONTROL_MISSING\n")
	}
	vfsHandle, ok = data.Handle("vfs")
	if !ok {
		fatal("IDENTITY_VFS_MISSING\n")
	}
	load()
	if vfsDead {
		fatal("IDENTITY_VFS_DIED\n")
	}
	syslog.Info("identity service ready", syslog.Str("path", dbPath))

	for {
		if vfsDead {
			fatal("IDENTITY_VFS_DIED\n")
		}
		var interests [userwait.MaxObjects]userwait.Interest
		var owners [1 + 1 + maxSessions]int
		interests[0] = userwait.Interest{Handle: controlHandle, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
		owners[0] = -1
		// The VFS session cannot be renewed in place (the control protocol has
		// no renewal op), so its peer closing is fatal: PID 1 restarts this
		// service to hand it a fresh confined session.
		interests[1] = userwait.Interest{Handle: vfsHandle, Events: kcall.WaitPeerClosed}
		owners[1] = -1
		count := 2
		for index := range sessions {
			if !sessions[index].used {
				continue
			}
			interests[count] = userwait.Interest{Handle: sessions[index].endpoint, Events: kcall.WaitReadable | kcall.WaitPeerClosed}
			owners[count] = index
			count++
		}
		result, status := userwait.Any(&interests, count, kcall.WaitAnyInfiniteTimeout)
		if status != kcall.StatusOK {
			fatal("IDENTITY_WAIT_FAILED\n")
		}
		for index := 0; index < count; index++ {
			if result.ReadyMask&(1<<uint(index)) == 0 {
				continue
			}
			switch {
			case index == 0:
				addSession()
			case index == 1:
				fatal("IDENTITY_VFS_DIED\n")
			default:
				serve(&sessions[owners[index]])
			}
		}
	}
}

// secureEpoch obtains a fresh 128-bit database epoch from the kernel's trusted
// random source, as the provisioning/recovery path requires. It fails closed
// when generation fails or the read is short, never returning a weak epoch.
func secureEpoch() ([16]byte, error) {
	var epoch [16]byte
	n, err := (cryptosys.Reader{}).Read(epoch[:])
	if err != nil {
		return [16]byte{}, err
	}
	if n != len(epoch) {
		return [16]byte{}, errors.New("identityd: short epoch read")
	}
	return epoch, nil
}

// load opens the durable snapshot through the confined VFS session, setting the
// service availability state: missing → Unprovisioned, valid → Available,
// corrupt or ambiguous → Unavailable. An open failure never stops the service:
// the control and admin endpoints stay alive so provisioning and recovery can
// proceed. The service never silently creates or regenerates a database.
func load() {
	service = identityd.New(vfsFiles{}, dbPath, secureEpoch)
	err := service.Open()
	switch service.State() {
	case identityproto.StateAvailable:
		syslog.Info("identity snapshot loaded")
	case identityproto.StateUnprovisioned:
		syslog.Info("identity unprovisioned")
	default:
		reason := "unknown"
		if err != nil {
			reason = err.Error()
		}
		syslog.Warn("identity snapshot unavailable", syslog.Str("reason", reason))
	}
}

// addSession records the session handle PID 1 attached to a control request.
// OpAddAdmin and OpAddLookup both install the attached endpoint into the next
// free slot; the opcode decides whether the session dispatches admin or lookup
// operations. A control-channel peer death is fatal: PID 1 restarts the service.
func addSession() {
	status := sys.IPCReceive(controlHandle, wire[:])
	if status == ipc.StatusPeerDied {
		fatal("IDENTITY_CONTROL_DIED\n")
	}
	if status != ipc.StatusOK || !ipc.DecodeBuffer(wire[:], &controlMsg) {
		return
	}
	op, endpoint, correlation, valid := identityproto.DecodeControlRequest(&controlMsg)
	result := identityproto.ResultInvalid
	if valid {
		admin := op == identityproto.OpAddAdmin
		if install(endpoint, admin) {
			result = identityproto.ResultOK
		} else {
			sys.HandleClose(endpoint)
			result = identityproto.ResultExhausted
		}
	} else {
		// A malformed control request must not strand its attached handles:
		// close every one, then attribute the opcode if it is known so the
		// caller is answered rather than left waiting.
		for index := 0; index < int(controlMsg.HandleCount); index++ {
			if controlMsg.Handles[index] != 0 {
				sys.HandleClose(controlMsg.Handles[index])
			}
		}
		if controlMsg.PayloadSize > 0 {
			op = identityproto.Opcode(controlMsg.Payload[0])
		}
		correlation = controlMsg.Correlation
	}
	var reply ipc.Message
	if identityproto.EncodeControlReply(op, result, correlation, &reply) {
		if size, ok := ipc.EncodeFrom(&reply, wire[:]); ok {
			sys.IPCSend(controlHandle, wire[:size])
		}
	}
}

// install records an opened session endpoint in the next free slot.
func install(endpoint handle.ID, admin bool) bool {
	for index := range sessions {
		if sessions[index].used {
			continue
		}
		sessions[index] = session{endpoint: endpoint, admin: admin, used: true}
		return true
	}
	return false
}

// serve answers one session request. A client peer death is recoverable: the
// handle is closed, the slot cleared, and the service continues.
func serve(current *session) {
	if sys.IPCReceive(current.endpoint, wire[:]) != ipc.StatusOK {
		dropSession(current)
		return
	}
	if !ipc.DecodeBuffer(wire[:], &requestMsg) {
		return
	}
	var req identityproto.Request
	correlation, valid := identityproto.DecodeRequest(&requestMsg, &req)
	if !valid {
		// Attribute a malformed request if it names a known data opcode, and
		// answer it so the caller is never stranded.
		if op, corr, attributed := identityproto.RequestOpcode(&requestMsg); attributed {
			sendReply(current.endpoint, identityproto.Reply{Opcode: op, Result: identityproto.ResultInvalid, State: service.State()}, corr)
		}
		return
	}
	if !sessionAllows(current, req.Opcode) {
		sendReply(current.endpoint, identityproto.Reply{Opcode: req.Opcode, Result: identityproto.ResultDenied, State: service.State()}, correlation)
		return
	}
	if isListOpcode(req.Opcode) {
		reply := service.HandleRequest(&req)
		sendList(current.endpoint, req.Opcode, reply.Result, reply.Records, correlation)
		return
	}
	reply := service.HandleRequest(&req)
	sendReply(current.endpoint, reply, correlation)
}

// sessionAllows enforces the endpoint kind PID 1 granted: lookup operations run
// only on lookup sessions, administration operations only on admin sessions.
// A lookup session therefore cannot provision, reset or otherwise mutate.
func sessionAllows(current *session, op identityproto.Opcode) bool {
	if op == identityproto.LookupAuthorizeKey || op == identityproto.LookupGetAccount {
		return !current.admin
	}
	return current.admin
}

// isListOpcode reports whether op answers with a multipart list reply.
func isListOpcode(op identityproto.Opcode) bool {
	return op == identityproto.AdminListAccounts || op == identityproto.AdminListGroups
}

// sendReply encodes and sends one non-list reply.
func sendReply(endpoint handle.ID, reply identityproto.Reply, correlation uint64) {
	var message ipc.Message
	if !identityproto.EncodeReply(&reply, correlation, &message) {
		return
	}
	if size, ok := ipc.EncodeFrom(&message, wire[:]); ok {
		sys.IPCSend(endpoint, wire[:size])
	}
}

// sendList sends a multipart list reply: notifications for every chunk but the
// last, then a final reply carrying the request correlation.
func sendList(endpoint handle.ID, op identityproto.Opcode, result identityproto.Result, records []identityproto.Record, correlation uint64) {
	if result != identityproto.ResultOK {
		var message ipc.Message
		if identityproto.EncodeListReply(op, result, 0, false, nil, correlation, &message) {
			if size, ok := ipc.EncodeFrom(&message, wire[:]); ok {
				sys.IPCSend(endpoint, wire[:size])
			}
		}
		return
	}
	var sequence uint64
	for {
		size := identityReplyHeader
		count := 0
		for count < len(records) && size+recordSize(records[count]) <= ipc.MaxPayload {
			size += recordSize(records[count])
			count++
		}
		chunk := records[:count]
		records = records[count:]
		more := len(records) > 0
		var message ipc.Message
		if !identityproto.EncodeListReply(op, identityproto.ResultOK, sequence, more, chunk, correlation, &message) {
			return
		}
		if size, ok := ipc.EncodeFrom(&message, wire[:]); ok {
			sys.IPCSend(endpoint, wire[:size])
		}
		sequence++
		if !more {
			return
		}
	}
}

// recordSize is the encoded size of one identity record: the version-2 fixed
// header plus the variable name, home and supplementary-group fields. It
// mirrors identityproto's codec sizing only so list chunks fit ipc.MaxPayload;
// records themselves are encoded by identityproto.
func recordSize(record identityproto.Record) int {
	return identityRecordFixed + len(record.Name) + len(record.Home) + 4*len(record.Groups)
}

// dropSession closes a client endpoint and frees its slot.
func dropSession(current *session) {
	sys.HandleClose(current.endpoint)
	*current = session{}
}

// dirOf returns the directory containing path, the confined session's own
// directory for the snapshot and its temporary file.
func dirOf(path string) string {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		if slash == 0 {
			return "/"
		}
		return path[:slash]
	}
	return ""
}

// vfsFiles adapts the persistence layer's Files interface to the confined VFS
// session. Replace maps statuses that may follow a committed rename to
// identityd.ErrUncertain (the same set configd treats as unknown).
type vfsFiles struct{}

func (vfsFiles) Read(path string) ([]byte, error) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: sys.Bytes(path)})
	if !ok {
		return nil, errors.New("identityd: snapshot open failed")
	}
	switch opened.Status {
	case vfs.StatusNotFound:
		return nil, identityd.ErrNotFound
	case vfs.StatusOK:
	default:
		return nil, errors.New("identityd: snapshot open failed")
	}
	defer callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	var data []byte
	for {
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		if !ok || reply.Status != vfs.StatusOK {
			return nil, errors.New("identityd: snapshot read failed")
		}
		if len(reply.Data) == 0 {
			// End of file only after a successful zero-byte read.
			return data, nil
		}
		if len(data)+len(reply.Data) > identity.MaxSnapshotBytes {
			return nil, errors.New("identityd: snapshot exceeds size bound")
		}
		data = append(data, reply.Data...)
	}
}

func (vfsFiles) Create(path string) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationCreate, Path: sys.Bytes(path)})
	if !ok {
		return errors.New("identityd: temporary create failed")
	}
	switch reply.Status {
	case vfs.StatusOK:
		return nil
	case vfs.StatusAlreadyExists:
		return identityd.ErrExists
	default:
		return errors.New("identityd: temporary create failed")
	}
}

func (vfsFiles) Unlink(path string) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: sys.Bytes(path)})
	if !ok {
		return errors.New("identityd: unlink failed")
	}
	switch reply.Status {
	case vfs.StatusOK:
		return nil
	case vfs.StatusNotFound:
		return identityd.ErrNotFound
	default:
		return errors.New("identityd: unlink failed")
	}
}

func (vfsFiles) OpenWrite(path string) (identityd.Handle, error) {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Write, Path: sys.Bytes(path)})
	if !ok || reply.Status != vfs.StatusOK {
		return 0, errors.New("identityd: temporary open failed")
	}
	return identityd.Handle(reply.Handle), nil
}

func (vfsFiles) Write(file identityd.Handle, data []byte) (int, error) {
	written := 0
	for written < len(data) {
		chunk := len(data) - written
		if chunk > vfs.MaxProtocolData {
			chunk = vfs.MaxProtocolData
		}
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationWrite, Handle: vfs.Handle(file), Data: data[written : written+chunk]})
		if !ok || reply.Status != vfs.StatusOK || reply.Size != uint64(chunk) {
			return written, errors.New("identityd: temporary write failed")
		}
		written += chunk
	}
	return written, nil
}

func (vfsFiles) Sync(file identityd.Handle) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSync, Handle: vfs.Handle(file)})
	if !ok || reply.Status != vfs.StatusOK {
		// A required unsupported or failed sync is not success.
		return errors.New("identityd: temporary sync failed")
	}
	return nil
}

func (vfsFiles) Close(file identityd.Handle) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: vfs.Handle(file)})
	if !ok || reply.Status != vfs.StatusOK {
		return errors.New("identityd: temporary close failed")
	}
	return nil
}

func (vfsFiles) Replace(source, target string) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationReplace, Path: sys.Bytes(source), Target: sys.Bytes(target)})
	if !ok {
		return identityd.ErrUncertain
	}
	switch reply.Status {
	case vfs.StatusOK:
		return nil
	case vfs.StatusTimeout, vfs.StatusPeerDied, vfs.StatusBackendError, vfs.StatusCorrupt, vfs.StatusNoSpace:
		return identityd.ErrUncertain
	default:
		return errors.New("identityd: replace failed")
	}
}

func (vfsFiles) SyncDir(path string) error {
	reply, ok := callVFS(vfs.Request{Operation: vfs.OperationSync, Path: sys.Bytes(dirOf(path))})
	if !ok || reply.Status != vfs.StatusOK {
		// The final directory barrier is required: unsupported or failed sync
		// is not success.
		return errors.New("identityd: directory sync failed")
	}
	return nil
}

func (vfsFiles) ListNames(path string, max int) ([]string, error) {
	dir := dirOf(path)
	if dir == "" {
		return nil, errors.New("identityd: invalid snapshot directory")
	}
	var names []string
	var cursor uint64
	for {
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationReadDir, Path: sys.Bytes(dir), Offset: cursor})
		if !ok {
			return nil, errors.New("identityd: directory listing failed")
		}
		switch reply.Status {
		case vfs.StatusOK:
			if len(names) >= max {
				return nil, errors.New("identityd: directory holds too many entries")
			}
			if len(reply.Name) == 0 {
				return nil, errors.New("identityd: empty directory entry")
			}
			names = append(names, string(reply.Name))
			cursor = uint64(reply.Cursor)
		case vfs.StatusNotFound:
			return names, nil
		default:
			return nil, errors.New("identityd: directory listing failed")
		}
	}
}

// callVFS sends one VFS request and waits for its reply with a bounded
// deadline. A peer death on the confined session marks the service for exit.
func callVFS(request vfs.Request) (vfs.Reply, bool) {
	if vfsDead {
		return vfs.Reply{}, false
	}
	vfsCorrelation++
	message, ok := vfs.EncodeRequest(request, vfsCorrelation)
	if !ok {
		return vfs.Reply{}, false
	}
	size, ok := ipc.Encode(message, vfsWire[:])
	if !ok {
		return vfs.Reply{}, false
	}
	if status := sys.IPCSend(vfsHandle, vfsWire[:size]); status != kcall.StatusOK {
		if status == kcall.StatusPeerDied {
			vfsDead = true
		}
		return vfs.Reply{}, false
	}
	for attempt := 0; attempt < vfsReplyAttempts; attempt++ {
		status := sys.IPCWaitReceive(vfsHandle, vfsWire[:], vfsReplyDeadline)
		if status == ipc.StatusTimeout {
			continue
		}
		if status != ipc.StatusOK {
			if status == ipc.StatusPeerDied {
				vfsDead = true
			}
			return vfs.Reply{}, false
		}
		payloadSize := int(vfsWire[24]) | int(vfsWire[25])<<8
		received, valid := ipc.Decode(vfsWire[:ipc.WireHeaderSize+payloadSize+int(vfsWire[26])*8])
		if !valid || received.Correlation == 0 || received.Correlation > vfsCorrelation {
			return vfs.Reply{}, false
		}
		if received.Correlation < vfsCorrelation {
			continue
		}
		reply, valid := vfs.DecodeReply(received)
		return reply, valid && reply.Operation == request.Operation
	}
	return vfs.Reply{}, false
}

// fatal reports why the service stops and exits, so the supervisor restarts it
// (ADR 0101).
func fatal(message string) {
	syslog.Error(message[:len(message)-1])
	if logHandle != 0 {
		sys.Diagnostics(logHandle)
	}
	sys.Exit(1)
}

func main() {}
