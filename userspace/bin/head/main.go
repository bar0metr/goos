//go:build goos_userspace

package main

import (
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "head [-n N] FILE", Summary: "first lines",
	Options: "N: uint64, default 10; max 64 MiB scanned",
	MinArgs: 1, MaxArgs: 3, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	count, path := admincmd.LineArguments(data)
	file := admincmd.OpenFile(path)
	status := admincmd.Head(file, admincmd.NativeOutput{}, count)
	file.Close()
	admincmd.Finish(status)
}
