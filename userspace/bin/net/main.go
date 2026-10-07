//go:build goos_userspace

// Command net shows the network interface or reloads its configuration
// (ADR 0191). Settings are changed with config set system.net.* and take
// effect at net reload; the command holds a session that may read the
// status and reload, and nothing else.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/netapi"
	"goos/userspace/network"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "net status|reload",
	Summary: "show the network interface or apply system.net.* settings",
	MinArgs: 1,
	MaxArgs: 1,
	Grants:  commands.GrantNetworkAdmin,
}

var text [96]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	argument, _ := data.Arg(1)
	switch {
	case utility.Equal(argument, "status"):
		status()
	case utility.Equal(argument, "reload"):
		reload()
	default:
		utility.Usage()
	}
}

func status() {
	reply, ok := utility.CallNetwork(netapi.Request{Opcode: netapi.OpStatus})
	if !ok || reply.Result != netapi.ResultOK {
		utility.Fail(nil, "network service unavailable")
	}
	field("state", append(text[:0], reply.State.Text()...))
	field("link", append(text[:0], reply.Link.Text()...))
	field("mac", network.AppendMAC(text[:0], reply.MAC))
	field("mode", append(text[:0], reply.Mode.Text()...))
	address := text[:0]
	if reply.Local == ([4]byte{}) {
		address = append(address, "none"...)
	} else {
		address = network.AppendIPv4(address, reply.Local)
		address = append(address, '/')
		address = appendDecimal(address, uint64(reply.Prefix))
	}
	field("address", address)
	gateway := text[:0]
	if reply.Gateway == ([4]byte{}) {
		gateway = append(gateway, "none"...)
	} else {
		gateway = network.AppendIPv4(gateway, reply.Gateway)
	}
	field("gateway", gateway)
	if reply.MTU != 0 {
		field("mtu", appendDecimal(text[:0], uint64(reply.MTU)))
	}
	if reply.Mode == netapi.ModeDHCP && reply.Lease != 0 {
		lease := appendDecimal(text[:0], reply.Lease)
		field("lease", append(lease, " s"...))
	}
	counters := append(text[:0], "rx "...)
	counters = appendDecimal(counters, reply.RX)
	counters = append(counters, " tx "...)
	counters = appendDecimal(counters, reply.TX)
	counters = append(counters, " drop "...)
	counters = appendDecimal(counters, reply.Drop)
	counters = append(counters, " malformed "...)
	counters = appendDecimal(counters, reply.Malformed)
	field("frames", counters)
	if reply.Fallback {
		field("settings", append(text[:0], "built-in defaults; stored system.net.* settings not applied"...))
	}
	utility.Mark("NET_STATUS_OK\n")
}

func reload() {
	reply, ok := utility.CallNetwork(netapi.Request{Opcode: netapi.OpReload})
	if !ok {
		utility.Fail(nil, "network service unavailable")
	}
	switch reply.Result {
	case netapi.ResultOK:
		utility.PrintText("network configuration applied")
		utility.EndLine()
	case netapi.ResultConfigInvalid:
		reason := append(text[:0], "invalid setting "...)
		reason = append(reason, reply.Field.Key()...)
		reason = append(reason, "; previous configuration kept"...)
		utility.Fail(nil, string(reason))
	default:
		utility.Fail(nil, reply.Result.Text())
	}
}

func field(name string, value []byte) {
	utility.PrintText(name)
	utility.PrintText(": ")
	utility.Print(value)
	utility.EndLine()
}

func appendDecimal(target []byte, value uint64) []byte {
	if value >= 10 {
		target = appendDecimal(target, value/10)
	}
	return append(target, byte('0'+value%10))
}
