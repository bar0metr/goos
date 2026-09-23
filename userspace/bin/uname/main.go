//go:build goos_userspace

// Command uname prints the system name, release and machine.
package main

import (
	"goos/internal/version"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "uname",
	Summary: "print the system name, release and machine",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	utility.PrintText(version.Name + " " + version.Release + " " + version.Machine)
	utility.EndLine()
	utility.Mark("UNAME_PROCESS_OK\n")
}
