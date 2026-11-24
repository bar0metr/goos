//go:build goos_userspace

// Command user manages identity accounts through the identity administration
// endpoint (ADR 0233).
package main

import (
	"encoding/base64"

	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/identity"
	"goos/userspace/identityproto"
	"goos/userspace/remotepolicy"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "user list|show NAME|add NAME|disable NAME|enable NAME|delete NAME|uid|gid|home|groups|key",
	Summary: "manage identity accounts",
	Options: "list; show NAME; add NAME; disable NAME; enable NAME; delete NAME; uid NAME UID; gid NAME GID; home NAME PATH; groups NAME [GID...]; key add NAME KEY; key remove NAME FINGERPRINT",
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
		list(channel, identityproto.AdminListAccounts)
	case utility.Equal(argument, "show") && data.ArgCount() == 2:
		show(data, channel)
	case utility.Equal(argument, "add") && data.ArgCount() == 2:
		named(data, channel, identityproto.AdminAddAccount)
	case utility.Equal(argument, "disable") && data.ArgCount() == 2:
		setEnabled(data, channel, false)
	case utility.Equal(argument, "enable") && data.ArgCount() == 2:
		setEnabled(data, channel, true)
	case utility.Equal(argument, "delete") && data.ArgCount() == 2:
		named(data, channel, identityproto.AdminDeleteAccount)
	case utility.Equal(argument, "uid") && data.ArgCount() == 3:
		setUID(data, channel)
	case utility.Equal(argument, "gid") && data.ArgCount() == 3:
		setGID(data, channel)
	case utility.Equal(argument, "home") && data.ArgCount() == 3:
		setHome(data, channel)
	case utility.Equal(argument, "groups") && data.ArgCount() >= 2:
		setGroups(data, channel)
	case utility.Equal(argument, "key"):
		key(data, channel)
	default:
		utility.Usage()
	}
}

// show prints one account: name, uid, primary gid, home, enabled, key count.
func show(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	reply := exchange(channel, identityproto.Request{Opcode: identityproto.AdminGetAccount, Name: name})
	requireResult(reply.Result)
	record := reply.Records[0]
	utility.Print(record.Name)
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.UID))
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.GID))
	utility.PrintText(" ")
	utility.Print(record.Home)
	utility.PrintText(" ")
	printEnabled(record.Enabled)
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.KeyCount))
	utility.EndLine()
}

// named runs an account mutation that takes only a name and prints nothing.
func named(data startup.Data, channel handle.ID, opcode identityproto.Opcode) {
	name := requiredName(data, 2)
	requireResult(exchange(channel, identityproto.Request{Opcode: opcode, Name: name}).Result)
}

func setEnabled(data startup.Data, channel handle.ID, enabled bool) {
	name := requiredName(data, 2)
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetEnabled, Name: name, Enabled: enabled}).Result)
}

func setUID(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	uid, ok := parseUint32(required(data, 3))
	if !ok {
		utility.Usage()
	}
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetUID, Name: name, UID: uid}).Result)
}

func setGID(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	gid, ok := parseUint32(required(data, 3))
	if !ok {
		utility.Usage()
	}
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetGID, Name: name, GID: gid}).Result)
}

func setHome(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	home := required(data, 3)
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetHome, Name: name, Home: home}).Result)
}

func setGroups(data startup.Data, channel handle.ID) {
	name := requiredName(data, 2)
	var ids []uint32
	for index := 3; ; index++ {
		argument, found := data.Arg(index)
		if !found {
			break
		}
		gid, ok := parseUint32(argument)
		if !ok {
			utility.Usage()
		}
		ids = append(ids, gid)
	}
	if len(ids) > identity.MaxSupplementaryGroups {
		utility.Fail(nil, "too many supplementary groups")
	}
	requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminSetGroups, Name: name, IDs: ids}).Result)
}

func key(data startup.Data, channel handle.ID) {
	action, _ := data.Arg(2)
	switch {
	case utility.Equal(action, "add") && data.ArgCount() == 4:
		name := requiredName(data, 3)
		key, ok := parseKey(required(data, 4))
		if !ok {
			utility.Usage()
		}
		requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminAddKey, Name: name, Key: key[:]}).Result)
		printFingerprint(key)
	case utility.Equal(action, "remove") && data.ArgCount() == 4:
		name := requiredName(data, 3)
		fingerprint, ok := parseFingerprint(required(data, 4))
		if !ok {
			utility.Usage()
		}
		requireResult(exchange(channel, identityproto.Request{Opcode: identityproto.AdminRemoveKey, Name: name, Fingerprint: fingerprint[:]}).Result)
	default:
		utility.Usage()
	}
}

// list prints one record per line for a list opcode, in service order.
func list(channel handle.ID, opcode identityproto.Opcode) {
	var message ipc.Message
	deadline := now() + identityTicks
	if !identityproto.EncodeRequest(&identityproto.Request{Opcode: opcode}, correlation, &message) || !sendWithin(channel, &message, deadline) {
		utility.Fail(nil, "identity request failed")
	}
	for {
		current := now()
		if current >= deadline || receive(channel, deadline-current, &message) != kcall.StatusOK {
			utility.Fail(nil, "identity service did not answer")
		}
		result, _, more, records, valid := identityproto.DecodeListReply(&message, correlation, opcode)
		if !valid {
			utility.Fail(nil, "invalid identity reply")
		}
		if result != identityproto.ResultOK {
			failResult(result)
		}
		for _, record := range records {
			printAccount(record)
		}
		if !more {
			return
		}
	}
}

func printAccount(record identityproto.Record) {
	utility.Print(record.Name)
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.UID))
	utility.PrintText(" ")
	utility.PrintUint(uint64(record.GID))
	utility.PrintText(" ")
	printEnabled(record.Enabled)
	utility.PrintText(" ")
	utility.Print(record.Home)
	utility.EndLine()
}

func printEnabled(enabled bool) {
	if enabled {
		utility.PrintText("yes")
	} else {
		utility.PrintText("no")
	}
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
	case identityproto.ResultConflict:
		utility.Fail(nil, "group still referenced")
	case identityproto.ResultExhausted:
		utility.Fail(nil, "limit reached")
	case identityproto.ResultUnavailable:
		utility.Fail(nil, "unavailable")
	case identityproto.ResultUnprovisioned:
		utility.Fail(nil, "unprovisioned")
	case identityproto.ResultDenied:
		utility.Fail(nil, "denied")
	default:
		utility.Fail(nil, "invalid")
	}
}

// parseKey accepts one authorized_keys line and returns its raw Ed25519 key.
func parseKey(value []byte) (identity.Key, bool) {
	var key identity.Key
	keys, err := remotepolicy.ParseAuthorizedKeys(value)
	if err != nil || keys.Len() != 1 {
		return key, false
	}
	public, ok := keys.At(0)
	if !ok || len(public) != identity.KeySize {
		return key, false
	}
	copy(key[:], public)
	return key, true
}

// parseFingerprint accepts "SHA256:<base64>" and returns the 32-byte digest.
func parseFingerprint(value []byte) ([identity.FingerprintSize]byte, bool) {
	const prefix = "SHA256:"
	var fingerprint [identity.FingerprintSize]byte
	if len(value) <= len(prefix) || !utility.Equal(value[:len(prefix)], prefix) {
		return fingerprint, false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(string(value[len(prefix):]))
	if err != nil || len(decoded) != identity.FingerprintSize {
		return fingerprint, false
	}
	copy(fingerprint[:], decoded)
	return fingerprint, true
}

func printFingerprint(key identity.Key) {
	fingerprint := identity.Fingerprint(key)
	utility.PrintText("SHA256:")
	utility.PrintText(base64.RawStdEncoding.EncodeToString(fingerprint[:]))
	utility.EndLine()
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
