//go:build goos_userspace

package main

import (
	"goos/internal/timekeeping"
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "date [-u]", Summary: "UTC realtime",
	Options: "YYYY-MM-DDTHH:MM:SSZ; UTC only", MaxArgs: 1,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	if !admincmd.DateArguments(data) {
		utility.Usage()
	}
	now := utility.ClockNow(timekeeping.Realtime)
	var output [21]byte
	line, ok := admincmd.UTC(now, &output)
	if !ok {
		utility.Fail(nil, "realtime outside years 0000..9999")
	}
	utility.Write(line)
}
