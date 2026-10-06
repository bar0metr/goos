//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/netapi"
	"goos/userspace/runtime/sys"
)

// networkHandle is the command's private session with the network service,
// when its manifest asked for a network grant and PID 1 granted it
// (ADR 0191). The service decided what the session may do when PID 1 opened
// it; nothing the command sends can change that.
var networkHandle handle.ID

var (
	networkMessage ipc.Message
	networkReply   ipc.Message
	noInterests    [kcall.WaitAnyMaxObjects]sys.Interest
)

// networkGrace is how long past a request's own deadline a reply may take.
// The service answers every accepted request by its deadline, so waiting
// longer only covers scheduling.
const networkGrace = 2 * clock.TicksPerSecond

// CallNetwork sends one request on the network session and waits, bounded by
// the request's timeout and a grace period, for its reply. It returns false
// when the service is gone or answers out of protocol.
func CallNetwork(request netapi.Request) (netapi.Reply, bool) {
	var reply netapi.Reply
	if networkHandle == 0 {
		return reply, false
	}
	correlation++
	if !netapi.EncodeRequest(&request, correlation, &networkMessage) {
		return reply, false
	}
	size, ok := ipc.EncodeFrom(&networkMessage, wire[:])
	if !ok || sys.IPCSend(networkHandle, wire[:size]) != kcall.StatusOK {
		return reply, false
	}
	deadline := networkGrace + uint64(request.TimeoutMS)*clock.TicksPerSecond/1000
	if sys.IPCWaitReceive(networkHandle, wire[:], deadline) != kcall.StatusOK {
		return reply, false
	}
	payloadSize := int(wire[24]) | int(wire[25])<<8
	if !ipc.DecodeInto(wire[:ipc.WireHeaderSize+payloadSize+int(wire[26])*8], &networkReply) {
		return reply, false
	}
	return reply, netapi.DecodeReply(&networkReply, correlation, request.Opcode, &reply)
}

// Sleep waits ticks without watching anything.
func Sleep(ticks uint64) {
	if ticks != 0 {
		sys.WaitAny(&noInterests, 0, ticks)
	}
}
