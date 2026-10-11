//go:build goos_userspace

package main

import (
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "hexdump FILE", Summary: "offset hex ASCII rows",
	Options: "16 bytes/row; max 64 MiB input", MinArgs: 1, MaxArgs: 1, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	file := admincmd.OpenFile(utility.Path(data, 1))
	status := admincmd.Hexdump(file, admincmd.NativeOutput{})
	file.Close()
	admincmd.Finish(status)
}
