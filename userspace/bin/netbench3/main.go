//go:build goos_userspace

// Command netbench3 measures native TCP over loopback on a GTCP 3 session,
// the protocol linuxsockd speaks (package netbench).
package main

import (
	"goos/userspace/commands"
	"goos/userspace/netbench"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "netbench3 [MiB]", Summary: "measure native GTCP 3 over loopback",
	Options: "MiB: stream size, 1-4096, default 32",
	MinArgs: 0, MaxArgs: 1, Grants: commands.GrantTCPBulk,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) { netbench.Run(data, true) }
