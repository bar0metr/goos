// Package dnsq is the dnsq command's logic: one DNS query sent straight to a
// named server over GUDP1 or GTCP1, with no resolver, cache or hosts table in
// between, and its answer printed for diagnosis. The command binary only
// supplies the sessions, the clock and the terminal, so everything here runs
// in host tests.
package dnsq

import (
	"goos/userspace/dnswire"
	"goos/userspace/network"
)

const (
	// DefaultTimeout bounds the whole query, TCP fallback included, in
	// milliseconds. Closing the endpoint afterwards may take up to a second
	// more.
	DefaultTimeout = 5000
	MinTimeout     = 1
	MaxTimeout     = 60000
	// Port is the DNS server port.
	Port = 53
)

// Mode picks the transports.
type Mode uint8

const (
	// Auto tries UDP and repeats a truncated answer over TCP.
	Auto Mode = iota
	UDPOnly
	TCPOnly
)

// Options is one parsed command line.
type Options struct {
	Server  network.IPv4
	Name    dnswire.Name
	Type    uint16
	Mode    Mode
	EDNS    bool
	Timeout uint64
	Short   bool
	Raw     bool
}

// Args is the argument vector as the startup data holds it: argument 0 is
// the command name.
type Args interface {
	ArgCount() int
	Arg(index int) ([]byte, bool)
}

// Parse reads "@SERVER [OPTIONS] NAME [TYPE]", with options anywhere. On a
// usage error it returns the reason and the argument at fault, if any.
func Parse(args Args, out *Options) (string, []byte) {
	// Field by field: a composite literal this size goes to the heap. Name
	// is set when the operand is read, and Parse fails without one.
	out.Server, out.Type, out.Mode, out.EDNS = network.IPv4{}, dnswire.TypeA, Auto, true
	out.Timeout, out.Short, out.Raw = DefaultTimeout, false, false
	server, name, kind := false, false, false
	for index := 1; index <= args.ArgCount(); index++ {
		argument, _ := args.Arg(index)
		switch {
		case equal(argument, "-t") || equal(argument, "--tcp"):
			if out.Mode == UDPOnly {
				return "--tcp and --udp exclude each other", nil
			}
			out.Mode = TCPOnly
		case equal(argument, "-u") || equal(argument, "--udp"):
			if out.Mode == TCPOnly {
				return "--tcp and --udp exclude each other", nil
			}
			out.Mode = UDPOnly
		case equal(argument, "--no-edns"):
			out.EDNS = false
		case equal(argument, "--short"):
			out.Short = true
		case equal(argument, "--raw"):
			out.Raw = true
		case equal(argument, "--timeout"):
			index++
			value, _ := args.Arg(index)
			number, ok := decimal(value)
			if !ok || number < MinTimeout || number > MaxTimeout {
				return "MS must be 1-60000", value
			}
			out.Timeout = number
		case len(argument) > 1 && argument[0] == '-':
			return "unknown option", argument
		case len(argument) != 0 && argument[0] == '@':
			if server {
				return "one @SERVER only", argument
			}
			address, ok := network.ParseIPv4(argument[1:])
			if !ok || address == (network.IPv4{}) {
				return "invalid server address", argument
			}
			out.Server, server = address, true
		case !name:
			if !out.Name.SetText(argument) {
				return "invalid name", argument
			}
			name = true
		case !kind:
			value, ok := dnswire.ParseType(argument)
			if !ok {
				return "unsupported type", argument
			}
			out.Type, kind = value, true
		default:
			return "too many operands", argument
		}
	}
	switch {
	case !server:
		return "@SERVER is required", nil
	case !name:
		return "NAME is required", nil
	case out.Short && out.Raw:
		return "--short and --raw exclude each other", nil
	}
	return "", nil
}

func equal(value []byte, text string) bool {
	if len(value) != len(text) {
		return false
	}
	for index := range value {
		if value[index] != text[index] {
			return false
		}
	}
	return true
}

// decimal reads at most five digits, enough for MaxTimeout.
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
