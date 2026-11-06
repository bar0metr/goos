//go:build goos_userspace

// Command netbench measures native TCP and UDP over loopback on a GTCP 2
// session, the one ordinary commands get (package netbench).
package main

import (
	"goos/userspace/commands"
	"goos/userspace/netbench"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "netbench [MiB]", Summary: "measure native TCP and UDP over loopback",
	Options: "MiB: stream size, 1-4096, default 32",
	MinArgs: 0, MaxArgs: 1, Grants: commands.GrantTCP | commands.GrantUDP,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) { netbench.Run(data, false) }
