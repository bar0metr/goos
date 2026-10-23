//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/netapi"
	"goos/userspace/runtime/sys"
)

// networkSession is the command's private session with the network service,
// when its manifest asked for a network grant and PID 1 granted it
// (ADR 0191). The service decided what the session may do when PID 1 opened
// it; nothing the command sends can change that.
var networkSession session[nativeIPC]

// noInterests is the empty interest list Sleep waits on. Nothing writes it.
var noInterests [kcall.WaitAnyMaxObjects]sys.Interest

// networkGrace is how long past a request's own deadline a reply may take.
// The service answers every accepted request by its deadline, so waiting
// longer only covers scheduling.
const networkGrace = 2 * clock.TicksPerSecond

// CallNetwork sends one request on the network session and waits, bounded by
// the request's timeout and a grace period, for its reply. It returns false
// when the service is gone or answers out of protocol. Concurrent callers
// take turns on the session.
func CallNetwork(request netapi.Request) (netapi.Reply, bool) {
	var reply netapi.Reply
	if networkSession.bound() == 0 {
		return reply, false
	}
	correlation := networkSession.begin()
	defer networkSession.end()
	if networkSession.bound() == 0 {
		return reply, false
	}
	if !netapi.EncodeRequest(&request, correlation, &networkSession.outgoing) || !networkSession.send(&networkSession.outgoing) {
		return reply, false
	}
	deadline := networkGrace + uint64(request.TimeoutMS)*clock.TicksPerSecond/1000
	if !networkSession.received(deadline) {
		return reply, false
	}
	valid := netapi.DecodeReply(&networkSession.incoming, correlation, request.Opcode, &reply)
	if !valid {
		networkSession.reject()
		return netapi.Reply{}, false
	}
	return reply, true
}

// Sleep waits ticks without watching anything.
func Sleep(ticks uint64) {
	if ticks != 0 {
		sys.WaitAny(&noInterests, 0, ticks)
	}
}
