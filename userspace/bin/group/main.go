//go:build goos_userspace

// Command group manages identity groups through the identity administration
// endpoint (ADR 0233).
package main

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "group list|add NAME|delete NAME|members NAME [UID...]",
	Summary: "manage identity groups",
	MinArgs: 1,
	MaxArgs: commands.Unbounded,
	Grants:  commands.GrantIdentityAdmin,
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
	argument, _ := data.Arg(1)
	switch {
	case utility.Equal(argument, "list") && data.ArgCount() == 1:
		list(channel)
	case utility.Equal(argument, "add") && data.ArgCount() == 2:
		named(data, channel, identityproto.AdminAddGroup)
	case utility.Equal(argument, "delete") && data.ArgCount() == 2:
		named(data, channel, identityproto.AdminDeleteGroup)
	case utility.Equal(argument, "members") && data.ArgCount() >= 2:
		setMembers(data, channel)
	default:
		utility.Usage()
	}
}

// named runs a group mutation that takes only a name and prints nothing.
func named(data startup.Data, channel handle.ID, opcode identityproto.Opcode) {
	name := requiredName(data, 2)
	requireResult(exchange(channel, identityproto.Request{Opcode: opcode, Name: name}).Result)
}

func setMembers(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	var ids []uint32
	for index := 3; ; index++ {
		argument, found := data.Arg(index)
		if !found {
			break
		}
		uid, ok := parseUint32(argument)
		if !ok {
			utility.Usage()
		}
		ids = append(ids, uid)
	}
	if len(ids) > identity.MaxAccounts {
		utility.Fail(nil, "too many members")
	}
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetMembers, Name: name, IDs: ids}).Result)
}

// list prints one group per line: name, gid, in service order.
func list(channel handle.ID) {
	var message ipc.Message
	deadline := now() + identityTicks
	if !identityproto.EncodeRequest(&identityproto.Request{Opcode: identityproto.AdminListGroups}, correlation, &message) || !sendWithin(channel, &message, deadline) {
		utility.Fail(nil, "identity request failed")
	}
	var expected uint64
	for {
		current := now()
		if current >= deadline || receive(channel, deadline-current, &message) != kcall.StatusOK {
			utility.Fail(nil, "identity service did not answer")
		}
		result, sequence, more, records, valid := identityproto.DecodeListReply(&message, correlation, identityproto.AdminListGroups)
		if !valid {
			utility.Fail(nil, "invalid identity reply")
		}
		if sequence != expected {
			utility.Fail(nil, "identity reply out of order")
		}
		expected++
		if result != identityproto.ResultOK {
			failResult(result)
		}
		for _, record := range records {
			printGroup(record)
		}
		if !more {
			return
		}
	}
}

func printGroup(record identityproto.Record) {
	utility.Print(record.Name)
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.GID))
	utility.EndLine()
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
		utility.Fail(nil, "no such group")
	case identityproto.ResultExists:
		utility.Fail(nil, "already exists")
	case identityproto.ResultConflict:
		utility.Fail(nil, "group still referenced")
	case identityproto.ResultExhausted:
		utility.Fail(nil, "limit reached")
	case identityproto.ResultUnavailable:
		utility.Fail(nil, "identity store unavailable")
	case identityproto.ResultUnprovisioned:
		utility.Fail(nil, "identity store unprovisioned; run 'user provision' first")
	case identityproto.ResultDenied:
		utility.Fail(nil, "denied")
	default:
		utility.Fail(nil, "invalid")
	}
}

func parseUint32(value []byte) (uint32, bool) {
	if len(value) == 0 {
		return 0, false
	}
	var result uint64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		result = result*10 + uint64(digit-'0')
		if result > 0xFFFFFFFF {
			return 0, false
		}
	}
	return uint32(result), true
}

func required(data startup.Data, index int) []byte {
	value, found := data.Arg(index)
	if !found {
		utility.Usage()
	}
	return value
}

func requiredName(data startup.Data, index int) []byte {
	value := required(data, index)
	if len(value) == 0 {
		utility.Usage()
	}
	return value
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
