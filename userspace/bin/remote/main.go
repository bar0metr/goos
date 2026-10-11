//go:build goos_userspace

// Command remote manages remote access through the administration endpoint.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/remoteapi"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{Usage: "remote status|reload|keys|key add LINE|key remove SHA256", Summary: "manage remote access", Options: "keys: 64 KiB, 64 entries, 4096-byte lines; quote LINE; remove requires full SHA256", MinArgs: 1, MaxArgs: 3, Grants: commands.GrantRemote}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func run(data startup.Data) {
	channel, ok := data.Handle("remote")
	if !ok {
		utility.Fail(nil, "remote service unavailable")
	}
	argument, _ := data.Arg(1)
	op := remoteapi.AdminStatus
	var keyArgument []byte
	switch {
	case utility.Equal(argument, "status") && data.ArgCount() == 1:
	case utility.Equal(argument, "reload") && data.ArgCount() == 1:
		op = remoteapi.AdminReload
	case utility.Equal(argument, "keys") && data.ArgCount() == 1:
		op = remoteapi.AdminKeys
	case utility.Equal(argument, "key") && data.ArgCount() == 3:
		action, _ := data.Arg(2)
		keyArgument, _ = data.Arg(3)
		if utility.Equal(action, "add") {
			op = remoteapi.AdminKeyAdd
		} else if utility.Equal(action, "remove") {
			op = remoteapi.AdminKeyRemove
		} else {
			utility.Usage()
		}
	default:
		utility.Usage()
	}
	var message ipc.Message
	deadline := remoteapi.Now() + 30*clock.TicksPerSecond
	if len(keyArgument) != 0 {
		for offset := 0; offset < len(keyArgument); offset += remoteapi.MaxStatus {
			if !remoteapi.EncodeKeyRequestPart(op, keyArgument, offset, 1, &message) || !remoteapi.SendWithin(channel, &message, deadline) {
				utility.Fail(nil, "remote key request failed")
			}
		}
	} else if op == remoteapi.AdminKeyAdd || op == remoteapi.AdminKeyRemove {
		utility.Usage()
	} else if !remoteapi.EncodeAdminRequest(op, 1, &message) || !remoteapi.SendWithin(channel, &message, deadline) {
		utility.Fail(nil, "remote request failed")
	}
	for part := 0; part < remoteapi.MaxStatusParts; part++ {
		now := remoteapi.Now()
		if now >= deadline || remoteapi.Receive(channel, deadline-now, &message) != kcall.StatusOK {
			utility.Fail(nil, "remote service did not answer")
		}
		result, chunk, more, valid := remoteapi.DecodeAdminPart(&message, op, 1)
		if !valid {
			remoteapi.CloseHandles(&message)
			utility.Fail(nil, "invalid remote reply")
		}
		if result != remoteapi.AdminOK {
			utility.Fail(nil, string(chunk))
		}
		utility.Write(chunk)
		if !more {
			return
		}
	}
	utility.Fail(nil, "remote reply exceeded bounds")
}
func main() {}
