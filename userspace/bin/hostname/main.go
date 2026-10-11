//go:build goos_userspace

package main

import (
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/config"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "hostname [NAME]", Summary: "display or persist hostname",
	Options: "ASCII label 1..63: letters/digits/hyphen, no edge hyphen; default goos",
	MaxArgs: 1, Grants: commands.GrantHostname,
}

var wire [ipc.WireMaxSize]byte
var message ipc.Message

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	endpoint, found := data.Handle("config")
	if !found {
		utility.Fail(nil, "hostname configuration session unavailable")
	}
	request := config.Request{Operation: config.OperationGet, Key: []byte(config.HostnameKey)}
	if data.ArgCount() == 1 {
		name, _ := data.Arg(1)
		if !config.ValidHostname(name) {
			utility.Fail(nil, "NAME must be an ASCII label of 1..63 letters/digits/hyphens, without edge hyphens")
		}
		request.Operation = config.OperationSetDurable
		request.Value, _ = config.Text(name)
	}
	outbound, ok := config.EncodeRequest(request, 1)
	if !ok {
		utility.Fail(nil, "invalid configuration request")
	}
	var client config.Client
	transport := configTransport{endpoint: endpoint}
	outcome := client.Exchange(transport, &outbound, utility.Clock()+10*clock.TicksPerSecond, &message)
	if outcome != config.OutcomeOK {
		utility.Fail(nil, "configuration acknowledgement unavailable; mutation outcome may be unknown")
	}
	reply, ok := config.DecodeReply(message)
	if !ok || reply.Operation != request.Operation {
		utility.Fail(nil, "invalid configuration reply")
	}
	if reply.Status == config.StatusPersistUnknown {
		utility.Fail(nil, "persistence outcome unknown; hostname is tentative; repeat this mutation to confirm")
	}
	if reply.Status == config.StatusPersistFailed {
		utility.Fail(nil, "hostname not persisted; previous value retained")
	}
	if reply.Status != config.StatusOK {
		utility.Fail(nil, "configuration request refused")
	}
	if request.Operation == config.OperationGet {
		if reply.Value.Kind != config.KindText || !config.ValidHostname(reply.Value.Bytes()) {
			utility.Fail(nil, "invalid stored hostname")
		}
		utility.Write(reply.Value.Bytes())
		utility.Write([]byte{'\n'})
	}
}

type configTransport struct{ endpoint handle.ID }

func (transport configTransport) Send(message *ipc.Message) bool {
	size, ok := ipc.Encode(*message, wire[:])
	return ok && sys.IPCSend(transport.endpoint, wire[:size]) == ipc.StatusOK
}

func (transport configTransport) Receive(ticks uint64, out *ipc.Message) kcall.Status {
	status := sys.IPCWaitReceive(transport.endpoint, wire[:], ticks)
	if status != ipc.StatusOK {
		return kcall.Status(status)
	}
	size := ipc.WireHeaderSize + int(wire[24]) + int(wire[25])<<8
	size += int(wire[26]) * 8
	if size > len(wire) || !ipc.DecodeInto(wire[:size], out) {
		return kcall.StatusInvalidArgument
	}
	return kcall.StatusOK
}

func (configTransport) Now() uint64 { return utility.Clock() }
