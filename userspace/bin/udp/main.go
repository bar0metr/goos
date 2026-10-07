//go:build goos_userspace

// Command udp sends and receives UDP datagrams through the network service
// with nothing but a UDP session (ADR 0195), to show the transport working
// on its own:
//
//	udp send [-s SIZE] [-r] [-W MS] ADDRESS PORT [TEXT]
//	udp listen [-c COUNT] [-W MS] [-b ADDRESS] PORT...
//	udp self [-s SIZE] ADDRESS PORT [TEXT]
//
// send connects an endpoint to ADDRESS:PORT and sends TEXT, or SIZE bytes of
// a fill pattern; -r then waits for one reply from that peer. listen binds
// one endpoint per PORT and prints COUNT datagrams as they arrive. self binds
// a listener at ADDRESS:PORT, sends to it from a second endpoint and prints
// what the listener got: a round trip that stays inside the machine.
package main

import (
	"goos/kernel/clock"
	"goos/userspace/commands"
	"goos/userspace/network"
	"goos/userspace/startup"
	"goos/userspace/udpapi"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "udp send|listen|self [OPTIONS] ...",
	Summary: "send and receive UDP datagrams",
	Options: "send [-s SIZE] [-r] [-W MS] ADDRESS PORT [TEXT]; listen [-c COUNT] [-W MS] [-b ADDRESS] PORT...; self [-s SIZE] ADDRESS PORT [TEXT]; -W waits 100-60000 ms (default 3000), -c 1-64 (default 1), up to 4 PORTs",
	MinArgs: 2,
	MaxArgs: 12,
	Grants:  commands.GrantUDP,
}

const (
	defaultWait = 3000
	maxPorts    = 4
	maxCount    = 64
	// shown bounds the payload text printed for one datagram.
	shown = 64
)

var (
	text    [256]byte
	request udpapi.Request
	reply   udpapi.Reply
)

// options are the parsed flags and operands of one invocation.
type options struct {
	size     int
	reply    bool
	wait     uint64
	count    int
	bind     network.IPv4
	operands [11][]byte
	operandN int
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	if !utility.OpenUDP(data) {
		utility.Fail(nil, "UDP session not granted")
	}
	verb, _ := data.Arg(1)
	parsed := parse(data)
	switch {
	case utility.Equal(verb, "send"):
		send(&parsed)
	case utility.Equal(verb, "listen"):
		listen(&parsed)
	case utility.Equal(verb, "self"):
		self(&parsed)
	default:
		utility.Usage()
	}
}

func parse(data startup.Data) options {
	parsed := options{size: -1, wait: defaultWait, count: 1}
	for index := 2; index <= data.ArgCount(); index++ {
		argument, _ := data.Arg(index)
		value, _ := data.Arg(index + 1)
		switch {
		case utility.Equal(argument, "-r"):
			parsed.reply = true
			continue
		case utility.Equal(argument, "-s"):
			number, valid := decimal(value)
			if !valid || number > udpapi.MaxPayload {
				utility.Fail(nil, "SIZE must be 0-4096")
			}
			parsed.size = int(number)
		case utility.Equal(argument, "-W"):
			number, valid := decimal(value)
			if !valid || number < 100 || number > 60000 {
				utility.Fail(nil, "MS must be 100-60000")
			}
			parsed.wait = number
		case utility.Equal(argument, "-c"):
			number, valid := decimal(value)
			if !valid || number < 1 || number > maxCount {
				utility.Fail(nil, "COUNT must be 1-64")
			}
			parsed.count = int(number)
		case utility.Equal(argument, "-b"):
			address, valid := network.ParseIPv4(value)
			if !valid {
				utility.Fail(value, "invalid IPv4 address")
			}
			parsed.bind = address
		default:
			if parsed.operandN == len(parsed.operands) {
				utility.Usage()
			}
			parsed.operands[parsed.operandN] = argument
			parsed.operandN++
			continue
		}
		index++
	}
	return parsed
}

func address(operand []byte) network.IPv4 {
	value, valid := network.ParseIPv4(operand)
	if !valid {
		utility.Fail(operand, "invalid IPv4 address")
	}
	return value
}

func port(operand []byte) uint16 {
	value, valid := decimal(operand)
	if !valid || value == 0 || value > 65535 {
		utility.Fail(operand, "invalid port")
	}
	return uint16(value)
}

// payload fills TX slot 0 with the TEXT words, joined by single spaces, or
// with the fill pattern, and returns its length.
func payload(parsed *options, operand int) int {
	slot := utility.UDPPayload()
	if parsed.size >= 0 {
		if parsed.operandN > operand {
			utility.Usage()
		}
		for index := 0; index < parsed.size; index++ {
			slot[index] = 'a' + byte(index%26)
		}
		return parsed.size
	}
	if parsed.operandN <= operand {
		utility.Usage()
	}
	length := 0
	for index := operand; index < parsed.operandN; index++ {
		if index > operand && length < len(slot) {
			slot[length] = ' '
			length++
		}
		length += copy(slot[length:], parsed.operands[index])
	}
	return length
}

func call(what string) {
	if !utility.UDPCall(&request, 0, &reply) {
		utility.Fail(nil, "network service unavailable")
	}
	if reply.Result != udpapi.ResultOK {
		utility.Fail([]byte(what), reply.Result.Text())
	}
}

func open() uint32 {
	request = udpapi.Request{Opcode: udpapi.OpOpen}
	call("open")
	return reply.Endpoint
}

func send(parsed *options) {
	if parsed.operandN < 2 {
		utility.Usage()
	}
	destination, to := address(parsed.operands[0]), port(parsed.operands[1])
	length := payload(parsed, 2)
	endpoint := open()
	request = udpapi.Request{Opcode: udpapi.OpConnect, Endpoint: endpoint, Address: destination, Port: to}
	call("connect")
	local, localPort := reply.Local, reply.LocalPort
	request = udpapi.Request{Opcode: udpapi.OpSend, Endpoint: endpoint, Length: uint16(length)}
	call("send")
	line := appendDecimal(text[:0], uint64(length))
	line = append(line, " bytes to "...)
	line = appendEndpoint(line, destination, to)
	line = append(line, " from "...)
	line = appendEndpoint(line, local, localPort)
	utility.Print(line)
	utility.EndLine()
	if parsed.reply {
		receive(endpoint, parsed.wait)
	}
}

func listen(parsed *options) {
	if parsed.operandN < 1 || parsed.operandN > maxPorts || parsed.size >= 0 || parsed.reply {
		utility.Usage()
	}
	var endpoints [maxPorts]uint32
	var waiting [maxPorts]uint64
	for index := 0; index < parsed.operandN; index++ {
		endpoints[index] = open()
		request = udpapi.Request{Opcode: udpapi.OpBind, Endpoint: endpoints[index], Address: parsed.bind,
			Port: port(parsed.operands[index])}
		call("bind")
	}
	for index := 0; index < parsed.operandN; index++ {
		request = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: endpoints[index]}
		started, ok := utility.UDPStart(&request)
		if !ok {
			utility.Fail(nil, "network service unavailable")
		}
		waiting[index] = started
	}
	// The receives are on their way: a datagram sent after this mark finds
	// them waiting in the network service.
	utility.Mark("UDP_LISTENING\n")
	for received := 0; received < parsed.count; {
		answered, ok := utility.UDPWait(parsed.wait*clock.TicksPerSecond/1000, &reply)
		if !ok {
			utility.Fail(nil, "timeout")
		}
		for index := 0; index < parsed.operandN; index++ {
			if waiting[index] != answered || reply.Opcode != udpapi.OpRecvFrom {
				continue
			}
			if reply.Result != udpapi.ResultOK {
				utility.Fail(nil, reply.Result.Text())
			}
			show(&reply)
			received++
			request = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: endpoints[index]}
			waiting[index], _ = utility.UDPStart(&request)
		}
	}
}

func self(parsed *options) {
	if parsed.operandN < 2 {
		utility.Usage()
	}
	local, to := address(parsed.operands[0]), port(parsed.operands[1])
	length := payload(parsed, 2)
	listener := open()
	request = udpapi.Request{Opcode: udpapi.OpBind, Endpoint: listener, Address: local, Port: to}
	call("bind")
	sender := open()
	request = udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: sender, Address: local, Port: to, Length: uint16(length)}
	call("send")
	receive(listener, parsed.wait)
}

// receive waits for one datagram on endpoint and prints it.
func receive(endpoint uint32, wait uint64) {
	request = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: endpoint}
	if !utility.UDPCall(&request, wait*clock.TicksPerSecond/1000, &reply) {
		utility.Fail(nil, "timeout")
	}
	if reply.Result != udpapi.ResultOK {
		utility.Fail(nil, reply.Result.Text())
	}
	show(&reply)
}

// show prints one received datagram, its payload when it is short printable
// text, and hands the RX slot back.
func show(received *udpapi.Reply) {
	data := utility.UDPReceived(received)
	line := appendDecimal(text[:0], uint64(received.Length))
	line = append(line, " bytes from "...)
	line = appendEndpoint(line, received.Address, received.Port)
	line = append(line, " to "...)
	line = network.AppendIPv4(line, received.Local)
	if len(data) > 0 && len(data) <= shown && printable(data) {
		line = append(line, ": "...)
		line = append(line, data...)
	}
	utility.UDPRelease(received.Slot)
	utility.Print(line)
	utility.EndLine()
}

func printable(data []byte) bool {
	for _, value := range data {
		if value < ' ' || value > '~' {
			return false
		}
	}
	return true
}

func appendEndpoint(line []byte, address network.IPv4, port uint16) []byte {
	line = network.AppendIPv4(line, address)
	line = append(line, ':')
	return appendDecimal(line, uint64(port))
}

func decimal(value []byte) (uint64, bool) {
	if len(value) == 0 || len(value) > 5 {
		return 0, false
	}
	result := uint64(0)
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
