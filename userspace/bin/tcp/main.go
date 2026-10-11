//go:build goos_userspace

// Command tcp exercises GTCP1 without importing the network stack internals.
package main

import (
	"goos/kernel/clock"
	"goos/userspace/commands"
	"goos/userspace/network"
	"goos/userspace/startup"
	"goos/userspace/tcpapi"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "tcp connect|listen|self [OPTIONS] ...", Summary: "TCP streams",
	Options: "connect [-W MS] [-s SIZE] ADDRESS PORT [TEXT]; listen [-W MS] [-c COUNT] [-b ADDRESS] PORT; self [-s SIZE] ADDRESS PORT [TEXT]",
	MinArgs: 2, MaxArgs: 10, Grants: commands.GrantTCP,
}

const defaultWait = 30000

var request tcpapi.Request
var reply tcpapi.Reply
var line [256]byte

type options struct {
	wait     uint64
	count    int
	size     int
	bind     network.IPv4
	operands [7][]byte
	operandN int
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	if !utility.OpenTCP(data) {
		utility.Fail(nil, "TCP session not granted")
	}
	verb, _ := data.Arg(1)
	parsed := parse(data)
	switch {
	case utility.Equal(verb, "connect"):
		connect(&parsed)
	case utility.Equal(verb, "listen"):
		listen(&parsed)
	case utility.Equal(verb, "self"):
		self(&parsed)
	default:
		utility.Usage()
	}
}

func parse(data startup.Data) options {
	parsed := options{wait: defaultWait, count: 1, size: -1}
	for index := 2; index <= data.ArgCount(); index++ {
		argument, _ := data.Arg(index)
		value, _ := data.Arg(index + 1)
		switch {
		case utility.Equal(argument, "-W"):
			number, ok := decimal(value)
			if !ok || number < 100 || number > 60000 {
				utility.Fail(nil, "MS must be 100-60000")
			}
			parsed.wait = number
		case utility.Equal(argument, "-c"):
			number, ok := decimal(value)
			if !ok || number < 1 || number > 8 {
				utility.Fail(nil, "COUNT must be 1-8")
			}
			parsed.count = int(number)
		case utility.Equal(argument, "-b"):
			address, ok := network.ParseIPv4(value)
			if !ok {
				utility.Fail(value, "invalid IPv4 address")
			}
			parsed.bind = address
		case utility.Equal(argument, "-s"):
			number, ok := decimal(value)
			if !ok || number > tcpapi.SlotSize {
				utility.Fail(nil, "SIZE must be 0-4096")
			}
			parsed.size = int(number)
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

func connect(parsed *options) {
	if parsed.operandN < 2 {
		utility.Usage()
	}
	endpoint := open()
	request = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: endpoint, Address: address(parsed.operands[0]), Port: port(parsed.operands[1])}
	call("connect", parsed.wait)
	if parsed.operandN > 2 || parsed.size >= 0 {
		length := payload(parsed, parsed.operands[2:parsed.operandN])
		request = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: endpoint, Length: uint16(length)}
		call("write", parsed.wait)
	}
	request = tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: endpoint}
	call("shutdown", parsed.wait)
	for {
		_, eof := read(endpoint, parsed.wait)
		if eof {
			utility.PrintText("eof")
			utility.EndLine()
			break
		}
	}
}

func listen(parsed *options) {
	if parsed.operandN != 1 {
		utility.Usage()
	}
	listener := open()
	request = tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: parsed.bind, Port: port(parsed.operands[0])}
	call("bind", parsed.wait)
	request = tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: uint8(parsed.count)}
	call("listen", parsed.wait)
	utility.Mark("TCP_LISTENING\n")
	for handled := 0; handled < parsed.count; handled++ {
		request = tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}
		call("accept", parsed.wait)
		child := reply.Endpoint
		for {
			length, eof := read(child, parsed.wait)
			if length != 0 {
				request = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: child, Length: uint16(length)}
				call("write", parsed.wait)
			}
			if eof {
				request = tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: child}
				call("shutdown", parsed.wait)
				break
			}
		}
	}
}

func self(parsed *options) {
	if parsed.operandN < 2 {
		utility.Usage()
	}
	local, targetPort := address(parsed.operands[0]), port(parsed.operands[1])
	listener := open()
	request = tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: local, Port: targetPort}
	call("bind", parsed.wait)
	request = tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 1}
	call("listen", parsed.wait)
	client := open()
	request = tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: local, Port: targetPort}
	call("connect", parsed.wait)
	request = tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}
	call("accept", parsed.wait)
	server := reply.Endpoint
	if parsed.operandN > 2 || parsed.size >= 0 {
		length := payload(parsed, parsed.operands[2:parsed.operandN])
		request = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Length: uint16(length)}
		call("write", parsed.wait)
	}
	read(server, parsed.wait)
}

func open() uint32 {
	request = tcpapi.Request{Opcode: tcpapi.OpOpen}
	call("open", defaultWait)
	return reply.Endpoint
}

func call(what string, wait uint64) {
	if !utility.TCPCall(&request, wait*clock.TicksPerSecond/1000, &reply) {
		utility.Fail([]byte(what), "timeout")
	}
	if reply.Result != tcpapi.ResultOK {
		utility.Fail([]byte(what), reply.Result.Text())
	}
}

func read(endpoint uint32, wait uint64) (int, bool) {
	request = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: tcpapi.SlotSize}
	if !utility.TCPCall(&request, wait*clock.TicksPerSecond/1000, &reply) {
		utility.Fail(nil, "timeout")
	}
	if reply.Result == tcpapi.ResultEOF {
		return 0, true
	}
	if reply.Result != tcpapi.ResultOK {
		utility.Fail(nil, reply.Result.Text())
	}
	data := utility.TCPReceived(&reply)
	copy(utility.TCPPayload(), data)
	output := appendDecimal(line[:0], uint64(len(data)))
	output = append(output, " bytes"...)
	if printable(data) {
		output = append(output, ": "...)
		output = append(output, data...)
	}
	utility.Print(output)
	utility.EndLine()
	utility.TCPRelease(reply.Slot)
	return len(data), false
}

func payload(parsed *options, words [][]byte) int {
	target, length := utility.TCPPayload(), 0
	if parsed.size >= 0 {
		if len(words) != 0 {
			utility.Usage()
		}
		for index := 0; index < parsed.size; index++ {
			target[index] = 'a' + byte(index%26)
		}
		return parsed.size
	}
	for index, word := range words {
		if index != 0 && length < len(target) {
			target[length] = ' '
			length++
		}
		length += copy(target[length:], word)
	}
	return length
}

func address(value []byte) network.IPv4 {
	parsed, ok := network.ParseIPv4(value)
	if !ok {
		utility.Fail(value, "invalid IPv4 address")
	}
	return parsed
}
func port(value []byte) uint16 {
	parsed, ok := decimal(value)
	if !ok || parsed == 0 || parsed > 65535 {
		utility.Fail(value, "invalid port")
	}
	return uint16(parsed)
}
func decimal(value []byte) (uint64, bool) {
	if len(value) == 0 || len(value) > 5 {
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
func printable(data []byte) bool {
	for _, value := range data {
		if value < ' ' || value > '~' {
			return false
		}
	}
	return true
}
