//go:build goos_userspace

// Package netbench measures the native network sessions over loopback: one
// process holds both ends and talks GTCP and GUDP to netd the way any
// command does, with no Linux socket layer in between. The netbench command
// measures a GTCP 2 session, the one ordinary commands get; netbench3 a
// GTCP 3 session, the protocol linuxsockd speaks, with its stream pipelined
// as linuxsockd pipelines it. The lines match netprobe bench with a
// "native-v2-" or "native-v3-" prefix, so the lab sets each Linux metric
// beside the native one of the same protocol.
package netbench

import (
	"goos/kernel/clock"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
	"goos/userspace/utility"
)

const (
	rounds = 2000
	// connects stays inside one command's TIME_WAIT budget: netd keeps 32
	// TIME_WAIT four-tuples for a session of the ordinary quota and answers
	// Open with busy past that, until one expires. Both ends of a loopback
	// connection belong to this session, and the pair the other
	// measurements use closes into TIME_WAIT too.
	connects = 30
	wakes    = 200
	datagram = 64
	// wait bounds every reply; a parked request answers well within it.
	wait = 10 * clock.TicksPerSecond
)

var loopback = [4]byte{127, 0, 0, 1}

// prefix names the protocol in every TCP line.
var prefix string

// waiting names the call whose reply await waits for, for its failure.
var waiting = "reply"

// offsets keeps each endpoint's stream offset: the bytes netd took from it.
// GTCP 3 writes name it; GTCP 2 ignores it.
var offsets [8]struct {
	endpoint uint32
	offset   uint32
}

// noOffset stands in for every endpoint of a GTCP 2 session, whose writes
// must carry zero.
var noOffset uint32

func streamOffset(endpoint uint32) *uint32 {
	if prefix != "native-v3-" {
		noOffset = 0
		return &noOffset
	}
	free := -1
	for index := range offsets {
		if offsets[index].endpoint == endpoint {
			return &offsets[index].offset
		}
		if offsets[index].endpoint == 0 && free < 0 {
			free = index
		}
	}
	if free < 0 {
		utility.Fail(nil, "too many endpoints")
	}
	offsets[free].endpoint = endpoint
	return &offsets[free].offset
}

var (
	line  [160]byte
	sink  [tcpapi.SlotSize3]byte
	early [4]held
	heldN int
)

// held is a TCP reply that arrived while the command waited for another.
type held struct {
	correlation uint64
	reply       tcpapi.Reply
}

// Run measures the session the command was granted: GTCP 3 when bulk,
// with UDP only on GTCP 2.
func Run(data startup.Data, bulk bool) {
	opened := utility.OpenTCP(data) && utility.OpenUDP(data)
	prefix = "native-v2-"
	if bulk {
		opened, prefix = utility.OpenTCPBulk(data), "native-v3-"
	}
	if !opened {
		utility.Fail(nil, "network sessions not granted")
	}
	total := uint64(32) << 20
	if data.ArgCount() == 1 {
		value, _ := data.Arg(1)
		mib, ok := decimal(value)
		if !ok || mib < 1 || mib > 4096 {
			utility.Usage()
		}
		total = mib << 20
	}
	listener, port := listen()
	client, server := pair(listener, port)
	if bulk {
		streamBulk(client, server, total)
	} else {
		stream(client, server, total)
	}
	requestReply(client, server)
	waitWake(client, server)
	closeEndpoint(client)
	closeEndpoint(server)
	connectClose(listener, port)
	closeEndpoint(listener)
	if !bulk {
		datagrams()
	}
	utility.PrintText("bench native ok")
	utility.EndLine()
}

// stream writes total bytes from client to server, one slot per Write,
// with a Read on server always waiting.
func stream(client, server uint32, total uint64) {
	var sent, received uint64
	start := utility.ClockNanos()
	post(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: uint16(utility.TCPSlotSize())})
	post(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Length: chunk(total - sent)})
	writing := true
	for received < total || writing {
		var reply tcpapi.Reply
		if _, ok := utility.TCPWait(wait, &reply); !ok {
			utility.Fail([]byte("stream"), "no reply")
		}
		switch reply.Opcode {
		case tcpapi.OpWrite:
			check("stream write", reply.Result)
			sent += uint64(reply.Length)
			writing = sent < total
			if writing {
				post(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Length: chunk(total - sent)})
			}
		case tcpapi.OpRead:
			check("stream read", reply.Result)
			received += uint64(copy(sink[:], utility.TCPReceived(&reply)))
			utility.TCPRelease(reply.Slot)
			if received < total {
				post(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: uint16(utility.TCPSlotSize())})
			}
		default:
			utility.Fail([]byte("stream"), "unexpected reply")
		}
	}
	elapsed := (utility.ClockNanos() - start) / 1000
	out := begin(prefix + "tcp-stream")
	out = field(out, "bytes", total)
	out = field(out, "us", elapsed)
	if elapsed != 0 {
		out = field(out, "MiBps", total*1000000/elapsed>>20)
	}
	finish(out)
}

// requestReply sends one byte each way per round, with the Read posted
// before the Write it waits for.
func requestReply(client, server uint32) {
	start := utility.ClockNanos()
	for index := 0; index < rounds; index++ {
		exchange(client, server)
		exchange(server, client)
	}
	report(prefix+"tcp-request-reply", "rounds", rounds, "us-per-round", (utility.ClockNanos()-start)/1000/rounds)
}

func exchange(from, to uint32) {
	read := post(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: to, Length: uint16(utility.TCPSlotSize())})
	write := post(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: from, Length: 1, Offset: *streamOffset(from)})
	reply := await(write)
	check("write", reply.Result)
	*streamOffset(from) += uint32(reply.Length)
	reply = await(read)
	check("read", reply.Result)
	utility.TCPRelease(reply.Slot)
}

// waitWake parks a WaitReady on server and times a one-byte Write on client
// until the WaitReady answers.
func waitWake(client, server uint32) {
	var spent uint64
	for index := 0; index < wakes; index++ {
		reply := call(tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: server}, "ready")
		ready := post(tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: reply.Mask})
		start := utility.ClockNanos()
		write := post(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Length: 1, Offset: *streamOffset(client)})
		reply = await(ready)
		spent += utility.ClockNanos() - start
		check("wait ready", reply.Result)
		written := await(write)
		check("write", written.Result)
		*streamOffset(client) += uint32(written.Length)
		reply = call(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: uint16(utility.TCPSlotSize())}, "read")
		utility.TCPRelease(reply.Slot)
	}
	report(prefix+"wait-wake", "count", wakes, "us-per-wake", spent/1000/wakes)
}

// connectClose opens, connects, accepts and closes connections one at a
// time, timing the whole loop and each step.
func connectClose(listener uint32, port uint16) {
	var spent [5]uint64
	start := utility.ClockNanos()
	for index := 0; index < connects; index++ {
		mark := utility.ClockNanos()
		client := open()
		now := utility.ClockNanos()
		spent[0] += now - mark
		mark = now
		call(tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: loopback, Port: port}, "connect")
		now = utility.ClockNanos()
		spent[1] += now - mark
		mark = now
		server := call(tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}, "accept").Endpoint
		now = utility.ClockNanos()
		spent[2] += now - mark
		mark = now
		closeEndpoint(client)
		now = utility.ClockNanos()
		spent[3] += now - mark
		mark = now
		closeEndpoint(server)
		now = utility.ClockNanos()
		spent[4] += now - mark
	}
	report(prefix+"tcp-connect-close", "count", connects, "us-per-connection", (utility.ClockNanos()-start)/1000/connects)
	report(prefix+"tcp-open", "count", connects, "us-per-op", spent[0]/1000/connects)
	report(prefix+"tcp-connect", "count", connects, "us-per-op", spent[1]/1000/connects)
	report(prefix+"tcp-accept", "count", connects, "us-per-op", spent[2]/1000/connects)
	report(prefix+"tcp-close-client", "count", connects, "us-per-op", spent[3]/1000/connects)
	report(prefix+"tcp-close-server", "count", connects, "us-per-op", spent[4]/1000/connects)
}

func listen() (uint32, uint16) {
	listener := open()
	port := call(tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: loopback}, "bind").Port
	call(tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 8}, "listen")
	return listener, port
}

func pair(listener uint32, port uint16) (uint32, uint32) {
	client := open()
	call(tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: loopback, Port: port}, "connect")
	return client, call(tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}, "accept").Endpoint
}

func open() uint32 { return call(tcpapi.Request{Opcode: tcpapi.OpOpen}, "open").Endpoint }

func closeEndpoint(endpoint uint32) {
	call(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}, "close")
}

func chunk(left uint64) uint16 {
	return uint16(min(left, uint64(utility.TCPSlotSize())))
}

func post(request tcpapi.Request) uint64 {
	correlation, ok := utility.TCPStart(&request)
	if !ok {
		utility.Fail(nil, "TCP request not sent")
	}
	return correlation
}

// await returns the reply to correlation and keeps any other reply that
// comes first for a later await.
func await(correlation uint64) tcpapi.Reply {
	for index := 0; index < heldN; index++ {
		if early[index].correlation == correlation {
			reply := early[index].reply
			heldN--
			early[index] = early[heldN]
			return reply
		}
	}
	for {
		var reply tcpapi.Reply
		answered, ok := utility.TCPWait(wait, &reply)
		if !ok {
			utility.Fail([]byte(waiting), "no TCP reply")
		}
		if answered == correlation {
			return reply
		}
		if heldN == len(early) {
			utility.Fail(nil, "unexpected TCP reply")
		}
		early[heldN] = held{correlation: answered, reply: reply}
		heldN++
	}
}

func call(request tcpapi.Request, what string) tcpapi.Reply {
	waiting = what
	reply := await(post(request))
	check(what, reply.Result)
	return reply
}

func check(what string, result tcpapi.Result) {
	if result != tcpapi.ResultOK {
		utility.Fail([]byte(what), result.Text())
	}
}

// datagrams bounces a 64-byte datagram between two connected UDP endpoints,
// with each RecvFrom posted before the Send it waits for.
func datagrams() {
	first, firstPort := udpBound()
	second, secondPort := udpBound()
	udpCall(udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: first, Address: loopback, Port: secondPort}, "connect")
	udpCall(udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: second, Address: loopback, Port: firstPort}, "connect")
	start := utility.ClockNanos()
	for index := 0; index < rounds; index++ {
		bounce(first, second)
		bounce(second, first)
	}
	report("native-udp-request-reply", "rounds", rounds, "us-per-round", (utility.ClockNanos()-start)/1000/rounds)
	udpCall(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: first}, "close")
	udpCall(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: second}, "close")
}

func udpBound() (uint32, uint16) {
	endpoint := udpCall(udpapi.Request{Opcode: udpapi.OpOpen}, "open").Endpoint
	return endpoint, udpCall(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: endpoint, Address: loopback}, "bind").Port
}

// bounce posts a RecvFrom on to and a Send on from, then takes both replies
// in whichever order netd sends them.
func bounce(from, to uint32) {
	request := udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: to}
	receive, ok := utility.UDPStart(&request)
	request = udpapi.Request{Opcode: udpapi.OpSend, Endpoint: from, Length: datagram}
	send, sent := utility.UDPStart(&request)
	if !ok || !sent {
		utility.Fail(nil, "UDP request not sent")
	}
	for pending := 2; pending > 0; pending-- {
		var reply udpapi.Reply
		answered, ok := utility.UDPWait(wait, &reply)
		switch {
		case !ok:
			utility.Fail(nil, "no UDP reply")
		case answered == send && reply.Result == udpapi.ResultOK:
		case answered == receive && reply.Result == udpapi.ResultOK && reply.Length == datagram:
			utility.UDPRelease(reply.Slot)
		default:
			utility.Fail([]byte("bounce"), reply.Result.Text())
		}
	}
}

// udpCall waits for its own reply; datagrams calls it only while no other
// request is in flight.
func udpCall(request udpapi.Request, what string) udpapi.Reply {
	var reply udpapi.Reply
	if !utility.UDPCall(&request, wait, &reply) {
		utility.Fail([]byte(what), "no UDP reply")
	}
	if reply.Result != udpapi.ResultOK {
		utility.Fail([]byte(what), reply.Result.Text())
	}
	return reply
}

func report(name, countKey string, count uint64, valueKey string, value uint64) {
	out := begin(name)
	out = field(out, countKey, count)
	finish(field(out, valueKey, value))
}

func begin(name string) []byte {
	out := append(line[:0], "bench "...)
	return append(out, name...)
}

func field(out []byte, key string, value uint64) []byte {
	out = append(out, ' ')
	out = append(out, key...)
	out = append(out, '=')
	return appendDecimal(out, value)
}

func finish(out []byte) {
	utility.Print(out)
	utility.EndLine()
}

func decimal(value []byte) (uint64, bool) {
	if len(value) == 0 || len(value) > 4 {
		return 0, false
	}
	var result uint64
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		result = result*10 + uint64(digit-'0')
	}
	return result, true
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}
