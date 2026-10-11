//go:build goos_userspace

// Command echo prints its arguments.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "echo [TEXT...]",
	Summary: "print arguments",
	MaxArgs: commands.Unbounded,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	utility.Arguments(data, 1)
	utility.EndLine()
	utility.Mark("ECHO_PROCESS_OK\n")
}
