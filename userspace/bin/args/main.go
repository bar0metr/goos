//go:build goos_userspace

// Command args prints its arguments and PATH, which checks the startup ABI.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "args [ARGUMENT...]",
	Summary: "print its arguments and PATH (startup ABI check)",
	MaxArgs: commands.Unbounded,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	utility.Arguments(data, 1)
	utility.EndLine()
	if path, found := data.Env("PATH"); found {
		utility.PrintText("PATH=")
		utility.Print(path)
		utility.EndLine()
	}
	utility.Mark("ARGS_PROCESS_OK\nARGV_ENV_PROCESS_OK\nSTDIN_HANDLE_OK\nSTDERR_HANDLE_OK\n")
}
