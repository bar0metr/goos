//go:build goos_userspace

// Command migrate performs the explicit, locally authorized legacy remote
// cutover: it imports the single legacy account, its home and its validated
// Ed25519 keys into the identity database in one durable commit, recording the
// persisted import marker. An incoming SSH connection never triggers it.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
	"goos/userspace/remotepolicy"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "migrate NAME HOME AUTHORIZED_KEYS_PATH",
	Summary: "import the legacy remote account",
	MinArgs: 3,
	MaxArgs: 3,
	Grants:  commands.GrantIdentityAdmin | commands.GrantVFS,
}

const (
	// correlation is the single outstanding request the command keeps pending.
	correlation = uint64(1)
	// identityTicks bounds one request's send and receive.
	identityTicks = 30 * clock.TicksPerSecond
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	channel, ok := data.Handle("identity")
	if !ok {
		utility.Fail(nil, "identity service unavailable")
	}
	name, _ := data.Arg(1)
	home, _ := data.Arg(2)
	keys, ok := readKeys(utility.Path(data, 3))
	if !ok {
		utility.Fail(nil, "authorized keys unreadable")
	}
	// An empty database is provisioned once before the one-shot import; this is
	// the explicit local recovery path, never an incoming-connection action.
	if state := exchange(channel, identityproto.Request{Opcode: identityproto.AdminStatus}).State; state == identityproto.StateUnprovisioned {
		requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminProvision}).Result)
	}
	reply := exchange(channel, identityproto.Request{Opcode: identityproto.AdminImportLegacy,
		Name: name, Home: home, Key: identityproto.EncodeImportKeys(keys)})
	if reply.Result == identityproto.ResultExists {
		utility.Mark("MIGRATE_EXISTS_OK\n")
		return
	}
	requireResult(reply.Result)
	utility.Mark("MIGRATE_OK\n")
}

// readKeys reads the bounded legacy authorized_keys file through the granted
// VFS session and returns its validated raw Ed25519 keys.
func readKeys(path []byte) ([][]byte, bool) {
	opened, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, received, path, false)
	var buffer [remotepolicy.MaxAuthorizedKeysBytes]byte
	size := 0
	for {
		reply, received := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		utility.Check(reply, received, path, false)
		if len(reply.Data) == 0 {
			break
		}
		if size+len(reply.Data) > len(buffer) {
			utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
			utility.Fail(nil, "authorized keys too large")
		}
		copy(buffer[size:], reply.Data)
		size += len(reply.Data)
	}
	utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	parsed, err := remotepolicy.ParseAuthorizedKeys(buffer[:size])
	if err != nil {
		return nil, false
	}
	var keys [][]byte
	for index := 0; index < parsed.Len(); index++ {
		public, ok := parsed.At(index)
		if !ok || len(public) != identity.KeySize {
			return nil, false
		}
		keys = append(keys, public)
	}
	return keys, true
}

// exchange sends one administration request and returns its decoded reply.
func exchange(channel handle.ID, request identityproto.Request) identityproto.Reply {
	var message ipc.Message
	if !identityproto.EncodeRequest(&request, correlation, &message) {
		utility.Fail(nil, "invalid identity request")
	}
	deadline := now() + identityTicks
	if !sendWithin(channel, &message, deadline) {
		utility.Fail(nil, "identity request failed")
	}
	current := now()
	if current >= deadline || receive(channel, deadline-current, &message) != kcall.StatusOK {
		utility.Fail(nil, "identity service did not answer")
	}
	var reply identityproto.Reply
	if !identityproto.DecodeReply(&message, correlation, request.Opcode, &reply) {
		utility.Fail(nil, "invalid identity reply")
	}
	return reply
}

func requireResult(result identityproto.Result) {
	if result != identityproto.ResultOK {
		failResult(result)
	}
}

func failResult(result identityproto.Result) {
	switch result {
	case identityproto.ResultNotFound:
		utility.Fail(nil, "no such account")
	case identityproto.ResultExists:
		utility.Fail(nil, "already exists")
	case identityproto.ResultExhausted:
		utility.Fail(nil, "limit reached")
	case identityproto.ResultUnavailable:
		utility.Fail(nil, "identity store unavailable")
	case identityproto.ResultUnprovisioned:
		utility.Fail(nil, "identity store unprovisioned")
	case identityproto.ResultDenied:
		utility.Fail(nil, "denied")
	default:
		utility.Fail(nil, "invalid")
	}
}

func now() uint64 {
	ticks, _ := sys.ClockRead()
	return ticks
}

// sendWithin retries a bounded, handle-free send while the queue is full,
// yielding one tick at a time, mirroring remoted's admin pattern.
func sendWithin(channel handle.ID, message *ipc.Message, deadline uint64) bool {
	if message.HandleCount != 0 {
		return false
	}
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		return false
	}
	for now() < deadline {
		status := sys.IPCSend(channel, wire[:size])
		if status == kcall.StatusOK {
			return true
		}
		if status != kcall.StatusResourceExhausted {
			return false
		}
		var interests [kcall.WaitAnyMaxObjects]sys.Interest
		if _, status = sys.WaitAny(&interests, 0, 1); status != kcall.StatusOK && status != kcall.StatusTimeout {
			return false
		}
	}
	return false
}

// receive takes one message, waiting at most wait ticks, and decodes the wire.
func receive(channel handle.ID, wait uint64, out *ipc.Message) kcall.Status {
	var wire [ipc.WireMaxSize]byte
	status := sys.IPCReceive(channel, wire[:])
	if status == kcall.StatusNotFound && wait != 0 {
		status = sys.IPCWaitReceive(channel, wire[:], wait)
	}
	if status != kcall.StatusOK {
		return status
	}
	if !ipc.DecodeBuffer(wire[:], out) {
		return kcall.StatusCorruptData
	}
	return kcall.StatusOK
}
