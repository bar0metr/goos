//go:build goos_userspace

package main

// The hosts builtin, over the shell's configuration session.

import (
	"goos/kernel/ipc"
	"goos/userspace/config"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/runtime/sys"
	shellcmd "goos/userspace/shell"
)

// hostsMessage holds the last reply, so a listed name can be printed from it
// without a copy.
var hostsMessage ipc.Message

// runHosts is a builtin for the same reason config is: the table is reached
// through the shell's own configuration session.
func runHosts(invocation shellcmd.Invocation) {
	if configHandle == 0 {
		write("hosts: service unavailable\n")
		return
	}
	switch {
	case invocation.Count == 1:
		listHosts()
	case invocation.Count == 3 && argumentIs(invocation.Arguments[1], "get"):
		getHost(invocation.Arguments[2])
	case invocation.Count == 4 && argumentIs(invocation.Arguments[1], "set"):
		setHost(invocation.Arguments[2], invocation.Arguments[3])
	case invocation.Count == 3 && argumentIs(invocation.Arguments[1], "remove"):
		removeHost(invocation.Arguments[2])
	default:
		write("usage: hosts | hosts get NAME | hosts set IPV4 NAME | hosts remove NAME\n")
	}
}

func listHosts() {
	for cursor := uint8(0); ; {
		reply, ok := callHosts(config.HostsRequest{Operation: config.HostsList, Cursor: cursor})
		if !ok {
			write("hosts: service unavailable\n")
			return
		}
		if reply.Status == config.StatusNotFound {
			return
		}
		if reply.Status != config.StatusOK || reply.Next <= cursor {
			writeHostsStatus(reply.Status)
			return
		}
		writeHost(reply.Address, reply.Name)
		cursor = reply.Next
	}
}

func getHost(text []byte) {
	name, ok := hostArgument(text)
	if !ok {
		return
	}
	reply, ok := callHosts(config.HostsRequest{Operation: config.HostsGet, Name: name.Bytes()})
	if !ok {
		write("hosts: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeHostsStatus(reply.Status)
		return
	}
	writeHost(reply.Address, name.Bytes())
}

func setHost(addressText []byte, text []byte) {
	address, parsed := network.ParseIPv4(addressText)
	if !parsed || !config.ValidHostAddress(address) {
		write("hosts: invalid IPv4 address\n")
		return
	}
	name, ok := hostArgument(text)
	if !ok {
		return
	}
	changeHost(config.HostsRequest{Operation: config.HostsSet, Name: name.Bytes(), Address: address}, "HOSTS_SET_OK\n")
}

func removeHost(text []byte) {
	name, ok := hostArgument(text)
	if !ok {
		return
	}
	changeHost(config.HostsRequest{Operation: config.HostsRemove, Name: name.Bytes()}, "HOSTS_REMOVE_OK\n")
}

func changeHost(request config.HostsRequest, marker string) {
	reply, ok := callHosts(request)
	if !ok {
		write("hosts: service unavailable\n")
		return
	}
	if reply.Status != config.StatusOK {
		writeHostsStatus(reply.Status)
		return
	}
	mark(marker)
}

// hostArgument checks a name here, so a bad one is reported as such rather
// than as a refusal from the service.
func hostArgument(text []byte) (hostname.Name, bool) {
	name, ok := hostname.Parse(text)
	if !ok || name.Size > config.MaxHostName {
		write("hosts: invalid host name\n")
		return hostname.Name{}, false
	}
	return name, true
}

func writeHost(address [4]byte, name []byte) {
	var text [network.MaxIPv4Text]byte
	writeBytes(network.AppendIPv4(text[:0], address))
	write(" ")
	writeBytes(name)
	write("\n")
}

func writeHostsStatus(status config.Status) {
	switch status {
	case config.StatusNotFound:
		write("hosts: no such host\n")
	case config.StatusReadOnly:
		write("hosts: localhost is reserved\n")
	case config.StatusResourceExhausted:
		write("hosts: table full\n")
	case config.StatusPermissionDenied:
		write("hosts: not permitted in this session\n")
	case config.StatusInvalidArgument:
		write("hosts: invalid host name or address\n")
	default:
		write("hosts: request refused\n")
	}
}

func callHosts(request config.HostsRequest) (config.HostsReply, bool) {
	correlation++
	message, ok := config.EncodeHostsRequest(request, correlation)
	if !ok {
		return config.HostsReply{}, false
	}
	size, ok := ipc.Encode(message, configWire[:])
	if !ok || sys.IPCSend(configHandle, configWire[:size]) != 0 {
		return config.HostsReply{}, false
	}
	if sys.IPCWaitReceive(configHandle, configWire[:], 100000) != 0 {
		return config.HostsReply{}, false
	}
	payloadSize := int(configWire[24]) | int(configWire[25])<<8
	if !ipc.DecodeInto(configWire[:ipc.WireHeaderSize+payloadSize+int(configWire[26])*8], &hostsMessage) ||
		hostsMessage.Correlation != correlation {
		return config.HostsReply{}, false
	}
	reply, valid := config.DecodeHostsReply(&hostsMessage)
	return reply, valid && reply.Operation == request.Operation
}
