//go:build goos_userspace

// Command trustprobe measures the native system root loader.
package main

import (
	"goos/userspace/commands"
	"goos/userspace/runtime/arena"
	"goos/userspace/startup"
	"goos/userspace/tlsclient"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "trustprobe [--production]", Summary: "native trust loading",
	MaxArgs: 1, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() {
	arena.InitPackages()
	utility.Run(manifest, run)
}
func main() {}

func run(data startup.Data) {
	var bulk utility.Bulk
	before := arena.HeapStats().TotalAlloc
	start := utility.ClockNanos()
	var roots *tlsclient.Roots
	var err error
	if data.ArgCount() == 1 {
		arg, _ := data.Arg(1)
		if string(arg) != "--production" {
			utility.Usage()
		}
		roots, err = tlsclient.LoadProductionProbeRoots(&bulk)
	} else {
		roots, err = tlsclient.LoadSystemRoots(&bulk)
	}
	if err != nil {
		utility.FailStatus(1, nil, err.Error())
	}
	utility.PrintText("TLSROOT_RESOURCE roots=")
	utility.PrintUint(uint64(roots.Count()))
	utility.PrintText(" der_bytes=")
	utility.PrintUint(uint64(roots.DERBytes()))
	utility.PrintText(" allocated=")
	utility.PrintUint(arena.HeapStats().TotalAlloc - before)
	utility.PrintText(" heap_high=")
	utility.PrintUint(arena.HeapStats().PeakHeld)
	utility.PrintText(" elapsed_ns=")
	utility.PrintUint(utility.ClockNanos() - start)
	utility.EndLine()
}
