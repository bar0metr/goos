//go:build goos_userspace

// Command netbench4 measures native TCP over loopback on a GTCP 4 session,
// whose stream data moves through shared rings, and UDP request and reply on
// a GUDP 3 session, whose datagrams do (package netbench).
package main

import (
	"goos/userspace/commands"
	"goos/userspace/netbench"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "netbench4 [MiB]", Summary: "measure native GTCP 4 and GUDP 3 over loopback",
	Options: "MiB: stream size, 1-4096, default 32",
	MinArgs: 0, MaxArgs: 1, Grants: commands.GrantTCPRing | commands.GrantUDPRing,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	netbench.RunUDPRing(data)
	netbench.RunRing(data)
}
