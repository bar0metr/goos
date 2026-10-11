//go:build goos_userspace

package main

import (
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "tail [-n N] FILE", Summary: "last lines",
	Options: "N: uint64, default 10; 64 MiB/pass; two scans, edits may affect output",
	MinArgs: 1, MaxArgs: 3, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	count, path := admincmd.LineArguments(data)
	file := admincmd.OpenFile(path)
	status := admincmd.Tail(file, admincmd.NativeOutput{}, count)
	file.Close()
	admincmd.Finish(status)
}
