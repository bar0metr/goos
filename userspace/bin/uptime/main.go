//go:build goos_userspace

// Command uptime shows how long the system has been running.
package main

import (
	"goos/kernel/clock"
	"goos/userspace/commands"
	"goos/userspace/shell"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "uptime",
	Summary: "show how long the system has been running",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(startup.Data) {
	var text [32]byte
	utility.Print(shell.FormatUptime(utility.Clock()/clock.TicksPerSecond, &text))
	utility.EndLine()
	utility.Mark("UPTIME_PROCESS_OK\n")
}
