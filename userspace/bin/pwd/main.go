//go:build goos_userspace

// Command pwd prints the working directory.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "pwd",
	Summary: "print the working directory",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	utility.PrintName(data.CWD())
	utility.EndLine()
	utility.Mark("PWD_PROCESS_OK\n")
}
