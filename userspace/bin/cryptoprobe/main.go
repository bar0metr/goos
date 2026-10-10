//go:build goos_userspace

// Command cryptoprobe checks the selected native crypto dependency.
package main

import (
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/cryptoprobe"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "cryptoprobe", Summary: "check native crypto compatibility",
}

//export userspaceMain
func userspaceMain() { initCryptoPackages(); utility.Run(manifest, run) }
func main()          {}

func run(startup.Data) {
	utility.PrintText("CRYPTO_BEGIN ")
	utility.PrintText(cryptoprobe.Name)
	utility.EndLine()
	before := arena.Current()
	start, clockStatus := sys.ClockNow(timekeeping.Monotonic)
	err := cryptoprobe.Run()
	end, endStatus := sys.ClockNow(timekeeping.Monotonic)
	utility.PrintText("CRYPTO_RESOURCE ")
	utility.PrintText(cryptoprobe.Name)
	utility.PrintText(" allocated=")
	utility.PrintUint(uint64(arena.Current() - before))
	utility.PrintText(" heap_high=")
	utility.PrintUint(arena.Touched())
	if clockStatus == kcall.StatusOK && endStatus == kcall.StatusOK {
		duration := (end.Seconds-start.Seconds)*1000000000 + int64(end.Nanoseconds) - int64(start.Nanoseconds)
		if duration >= 0 {
			utility.PrintText(" elapsed_ns=")
			utility.PrintUint(uint64(duration))
		}
	}
	utility.EndLine()
	if err != nil {
		utility.PrintText("CRYPTO_FAIL ")
		utility.PrintText(cryptoprobe.Name)
		utility.EndLine()
		utility.FailStatus(1, nil, err.Error())
	}
	utility.PrintText("CRYPTO_PASS ")
	utility.PrintText(cryptoprobe.Name)
	utility.EndLine()
}
