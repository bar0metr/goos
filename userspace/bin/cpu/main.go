//go:build goos_userspace

// Command cpu shows what each processor is doing: its busy share, the task
// it runs, and its scheduler counters.
package main

import (
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "cpu [-r]",
	Summary: "processor load and counters",
	Options: "-r, --raw: print the counters instead of busy and idle shares",
	MaxArgs: 1,
}

const maxCPUs = 64

var (
	records      [maxCPUs]kcall.CPURuntime
	outputBuffer [8 << 10]byte
)

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

func run(data startup.Data) {
	raw := false
	if data.ArgCount() == 1 {
		argument, _ := data.Arg(1)
		if !utility.Equal(argument, "-r") && !utility.Equal(argument, "--raw") {
			utility.Fail(argument, "unknown option")
		}
		raw = true
	}
	count := 0
	var wire [kcall.CPURuntimeSize]byte
	for index := uint64(0); index < maxCPUs; index++ {
		status := sys.CPURuntime(index, &wire)
		if status == kcall.StatusNotFound {
			break
		}
		record, ok := kcall.DecodeCPURuntime(wire[:])
		if status != kcall.StatusOK || !ok {
			utility.Fail(nil, "processor counters unavailable")
		}
		records[count] = record
		count++
	}
	utility.Write(appendTable(outputBuffer[:0], records[:count], raw))
}
