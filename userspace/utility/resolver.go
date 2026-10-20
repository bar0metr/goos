//go:build goos_userspace

package utility

import (
	"goos/kernel/clock"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/resolvapi"
)

// resolverSession is the command's private session with the resolver
// service, when its manifest asked for a resolver grant and PID 1 granted
// it (ADR 0194).
var resolverSession session[nativeIPC]

// resolverWait bounds a resolver request: the longest resolution the
// resolver's settings allow is 90 s, the default 6 s.
const resolverWait = 95 * clock.TicksPerSecond

// CallResolver sends one request on the resolver session and waits, bounded,
// for its reply. It returns false when there is no session, the service is
// gone or it answers out of protocol. Concurrent callers take turns on the
// session.
func CallResolver(request *resolvapi.Request, reply *resolvapi.Reply) bool {
	if resolverSession.bound() == 0 {
		return false
	}
	correlation := resolverSession.begin()
	defer resolverSession.end()
	if resolverSession.bound() == 0 {
		return false
	}
	if !resolvapi.EncodeRequest(request, correlation, &resolverSession.outgoing) || !resolverSession.send(&resolverSession.outgoing) {
		return false
	}
	if !resolverSession.received(resolverWait) {
		return false
	}
	if !resolvapi.DecodeReply(&resolverSession.incoming, correlation, request.Opcode, reply) {
		resolverSession.reject()
		*reply = resolvapi.Reply{}
		return false
	}
	return true
}

// Lookup is how a command turns its argument into an address: strict dotted
// decimal first, without asking anyone, and only then the resolver. It
// reports whether the text was numeric, and for a name the resolver's
// result; resolved is false when the resolver could not be asked.
func Lookup(text []byte) (address network.IPv4, numeric bool, result resolvapi.Result, resolved bool) {
	if parsed, ok := network.ParseIPv4(text); ok {
		return parsed, true, resolvapi.ResultOK, true
	}
	var request resolvapi.Request
	name, valid := hostname.Parse(text)
	if !valid {
		return network.IPv4{}, false, resolvapi.ResultInvalidName, true
	}
	request.Opcode, request.Name = resolvapi.OpResolve, name
	var reply resolvapi.Reply
	if !CallResolver(&request, &reply) {
		return network.IPv4{}, false, 0, false
	}
	if reply.Result != resolvapi.ResultOK {
		return network.IPv4{}, false, reply.Result, true
	}
	return reply.Addresses[0], false, resolvapi.ResultOK, true
}
