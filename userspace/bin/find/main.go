//go:build goos_userspace

package main

import (
	"goos/kernel/vfs"
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "find PATH [-name NAME]", Summary: "walk paths",
	Options: "exact basename; max depth 16, 4096 nodes, 4096-byte paths", MinArgs: 1, MaxArgs: 3, Grants: commands.GrantVFS,
}

var finder admincmd.Finder

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	name, ok := admincmd.FindArguments(data)
	if !ok {
		utility.Usage()
	}
	path := utility.Path(data, 1)
	kind, _, status := utility.Stat(path)
	if status != vfs.StatusOK {
		utility.Fail(path, "cannot stat path")
	}
	admincmd.Finish(finder.Walk(path, name, kind, admincmd.NativeDirectory{}, admincmd.NativeOutput{}))
}
