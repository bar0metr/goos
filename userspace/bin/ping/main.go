//go:build goos_userspace

// Command ping sends ICMP echo requests through the network service and
// reports each reply (ADR 0191). It builds no packets itself: the service
// does, and the command holds only a session that may ask for echoes, and
// one that may resolve a host name (ADR 0194).
package main

import (
	"goos/kernel/clock"
	"goos/userspace/commands"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/resolvapi"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "ping [-c COUNT] [-W TIMEOUT_MS] HOST",
	Summary: "send ICMP echo requests to an IPv4 address or host name",
	Options: "-c COUNT requests, 1-16 (default 4); -W TIMEOUT_MS per reply, 100-5000 (default 1000)",
	MinArgs: 1,
	MaxArgs: 5,
	Grants:  commands.GrantNetworkPing | commands.GrantResolve,
}

const (
	defaultCount   = 4
	maxCount       = 16
	defaultTimeout = 1000
	// interval separates the starts of two requests.
	interval = clock.TicksPerSecond
	// icmpBytes is the echo the service sends: an 8-byte header and its
	// payload.
	icmpBytes = 8 + network.EchoPayload
)

var text [320]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	count, timeout := uint64(defaultCount), uint64(defaultTimeout)
	var target []byte
	for index := 1; index <= data.ArgCount(); index++ {
		argument, _ := data.Arg(index)
		switch {
		case utility.Equal(argument, "-c") || utility.Equal(argument, "-W"):
			value, found := data.Arg(index + 1)
			number, valid := decimal(value)
			if !found || !valid {
				utility.Usage()
			}
			if utility.Equal(argument, "-c") {
				if number < 1 || number > maxCount {
					utility.Fail(nil, "COUNT must be 1-16")
				}
				count = number
			} else {
				if number < netapi.MinEchoTimeoutMS || number > netapi.MaxEchoTimeoutMS {
					utility.Fail(nil, "TIMEOUT_MS must be 100-5000")
				}
				timeout = number
			}
			index++
		case target == nil && len(argument) != 0 && argument[0] != '-':
			target = argument
		default:
			utility.Usage()
		}
	}
	if target == nil {
		utility.Usage()
	}
	destination := parseTarget(target)
	received := uint64(0)
	for sequence := uint64(1); sequence <= count; sequence++ {
		started := utility.Clock()
		if echo(destination, uint16(sequence), uint16(timeout)) {
			received++
		}
		if sequence < count {
			if elapsed := utility.Clock() - started; elapsed < interval {
				utility.Sleep(interval - elapsed)
			}
		}
	}
	line := appendDecimal(text[:0], count)
	line = append(line, " sent, "...)
	line = appendDecimal(line, received)
	line = append(line, " received, "...)
	line = appendDecimal(line, (count-received)*100/count)
	line = append(line, "% lost"...)
	utility.Print(line)
	utility.EndLine()
	if received == 0 {
		utility.Exit(1)
	}
}

// parseTarget takes a numeric address as it is and resolves anything else,
// then refuses an address no echo can go to before a request is sent. A
// resolved name is announced with its address.
func parseTarget(target []byte) network.IPv4 {
	address, numeric, result, resolved := utility.Lookup(target)
	switch {
	case !resolved:
		utility.Fail(target, "name resolution unavailable")
	case result == resolvapi.ResultInvalidName:
		utility.Fail(target, "invalid IPv4 address or host name")
	case result != resolvapi.ResultOK:
		utility.Fail(target, result.Text())
	}
	if !address.IsUnicast() && !address.IsLoopback() {
		utility.Fail(target, "unicast IPv4 address required")
	}
	if !numeric {
		line := append(text[:0], "PING "...)
		line = append(line, target...)
		line = append(line, " ("...)
		line = network.AppendIPv4(line, address)
		line = append(line, ')')
		utility.Print(line)
		utility.EndLine()
	}
	return address
}

// echo asks for one reply and prints its outcome. It is not inlined: its
// frame in the loop that calls it would otherwise grow on every request.
//
//go:noinline
func echo(destination network.IPv4, sequence, timeout uint16) bool {
	reply, ok := utility.CallNetwork(netapi.Request{Opcode: netapi.OpEcho, Destination: destination, Sequence: sequence, TimeoutMS: timeout})
	if !ok {
		utility.Fail(nil, "network service unavailable")
	}
	if reply.Sequence != sequence {
		utility.Fail(nil, "reply for another request")
	}
	line := text[:0]
	switch reply.Result {
	case netapi.ResultOK:
		line = appendDecimal(line, icmpBytes)
		line = append(line, " bytes from "...)
		line = network.AppendIPv4(line, reply.Responder)
		line = append(line, ": seq="...)
		line = appendDecimal(line, uint64(sequence))
		line = append(line, " time="...)
		line = appendMillis(line, reply.RTT)
		line = append(line, " ms"...)
	case netapi.ResultRemoteUnreachable, netapi.ResultTimeExceeded:
		line = append(line, "from "...)
		line = network.AppendIPv4(line, reply.Responder)
		line = append(line, ": seq="...)
		line = appendDecimal(line, uint64(sequence))
		line = append(line, ' ')
		line = append(line, reply.Result.Text()...)
	default:
		line = append(line, "seq="...)
		line = appendDecimal(line, uint64(sequence))
		line = append(line, ": "...)
		line = append(line, reply.Result.Text()...)
	}
	utility.Print(line)
	utility.EndLine()
	return reply.Result == netapi.ResultOK
}

// appendMillis writes nanoseconds as milliseconds with three decimals.
func appendMillis(target []byte, nanos uint64) []byte {
	micros := nanos / 1000
	target = appendDecimal(target, micros/1000)
	target = append(target, '.')
	fraction := micros % 1000
	target = append(target, byte('0'+fraction/100), byte('0'+fraction/10%10), byte('0'+fraction%10))
	return target
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
