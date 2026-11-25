//go:build goos_userspace && goos_validation

// Command identityprobe exercises the real identity lookup session (M4 FIX
// §5). It is a validation-only command: PID 1 hands it a lookup session over
// identityd's control channel for declaring GrantIdentityLookup, and the
// command proves that session authorizes only the exact key of an enabled
// account and refuses a wrong key, a disabled account, a removed key, and any
// administration opcode.
//
// The accounts and keys are the fixed fixtures tests/boot/identity.sh builds:
// alice holds key_a, bob's key_b is removed, and carol is disabled. The probe
// itself can only look up, so it never mutates or provisions; the state it
// checks must be committed by the administration command first.
package main

import (
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
	Usage:   "identityprobe",
	Summary: "exercise the identity lookup session",
	Grants:  commands.GrantIdentityLookup,
}

const (
	// correlation is the single outstanding request the command keeps pending.
	correlation = uint64(1)
	// identityTicks bounds one request's send and receive.
	identityTicks = 30 * clock.TicksPerSecond

	// The fixed fixtures tests/boot/identity.sh uses for the accounts the
	// probe checks. They are the same authorized_keys lines, so parsing them
	// with the exact parser `user key add` uses yields the same keys.
	aliceLine = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAOhB7/zzhC+HXDdGOdLwJln5NYwm6UNXx3chmQSVTG4 KEY_A"
	bobLine   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGFb9FqqZXpOZNdSxu9TYozevd20kdyu+w/IyqWktj8j KEY_B"
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	channel, ok := data.Handle("lookup")
	if !ok {
		utility.Fail(nil, "lookup service unavailable")
	}
	aliceKey, ok := parseKey([]byte(aliceLine))
	if !ok {
		fail("key-parse-alice")
	}
	bobKey, ok := parseKey([]byte(bobLine))
	if !ok {
		fail("key-parse-bob")
	}

	// (a) The correct key of an enabled account authorizes.
	expect(channel, identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("alice"), Key: aliceKey[:]}, identityproto.ResultOK, "authorize-alice")
	// (b) A different key for the same account denies.
	expect(channel, identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("alice"), Key: bobKey[:]}, identityproto.ResultDenied, "authorize-wrong-key")
	// (c) A disabled account denies, even with a well-formed key.
	expect(channel, identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("carol"), Key: aliceKey[:]}, identityproto.ResultDenied, "authorize-disabled")
	// (d) An account whose key was removed denies.
	expect(channel, identityproto.Request{Opcode: identityproto.LookupAuthorizeKey, Name: []byte("bob"), Key: bobKey[:]}, identityproto.ResultDenied, "authorize-removed")
	// (e) An administration opcode on the lookup session is refused. A list
	// opcode such as AdminListAccounts is answered through the list codec, which
	// a session-kind refusal does not follow, so a non-list admin opcode is used
	// to obtain the deterministic ResultDenied reply.
	expect(channel, identityproto.Request{Opcode: identityproto.AdminStatus}, identityproto.ResultDenied, "admin-opcode-denied")

	utility.PrintText("identityprobe-ok")
	utility.EndLine()
}

// expect sends one request and requires exactly want as its result.
func expect(channel handle.ID, request identityproto.Request, want identityproto.Result, name string) {
	if exchange(channel, request).Result != want {
		fail(name)
	}
}

// fail reports a deterministic marker and exits nonzero.
func fail(name string) {
	utility.PrintText("identityprobe-fail ")
	utility.PrintText(name)
	utility.EndLine()
	utility.Exit(1)
}

// exchange sends one lookup request and returns its decoded reply.
func exchange(channel handle.ID, request identityproto.Request) identityproto.Reply {
	var message ipc.Message
	if !identityproto.EncodeRequest(&request, correlation, &message) {
		fail("encode")
	}
	deadline := now() + identityTicks
	if !sendWithin(channel, &message, deadline) {
		fail("send")
	}
	current := now()
	if current >= deadline || receive(channel, deadline-current, &message) != kcall.StatusOK {
		fail("receive")
	}
	var reply identityproto.Reply
	if !identityproto.DecodeReply(&message, correlation, request.Opcode, &reply) {
		fail("decode")
	}
	return reply
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
