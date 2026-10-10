//go:build goos_userspace

// Command remote reads service status or atomically reloads remote settings.
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

var manifest = commands.Manifest{Usage: "remote status|reload", Summary: "inspect or reload remote administration", MinArgs: 1, MaxArgs: 1, Grants: commands.GrantRemote}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func run(data startup.Data) {
	channel, ok := data.Handle("remote")
	if !ok {
		utility.Fail(nil, "remote service unavailable")
	}
	argument, _ := data.Arg(1)
	op := remoteapi.AdminStatus
	if utility.Equal(argument, "reload") {
		op = remoteapi.AdminReload
	} else if !utility.Equal(argument, "status") {
		utility.Usage()
	}
	var message ipc.Message
	if !remoteapi.EncodeAdminRequest(op, 1, &message) || !remoteapi.Send(channel, &message) {
		utility.Fail(nil, "remote request failed")
	}
	deadline := remoteapi.Now() + 15*clock.TicksPerSecond
	var text [remoteapi.MaxStatus * remoteapi.MaxStatusParts]byte
	used := 0
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
		used += copy(text[used:], chunk)
		if !more {
			utility.Write(text[:used])
			return
		}
	}
	utility.Fail(nil, "remote reply exceeded bounds")
}
func main() {}
