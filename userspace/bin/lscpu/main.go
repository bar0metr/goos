//go:build goos_userspace

// Command lscpu displays the CPU inventory published by the kernel.
package main

import (
	"goos/internal/cpuinfo"
	"goos/internal/version"
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/runtime/sys"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{Usage: "lscpu [OPTION]...", Summary: "display CPU architecture information", Options: "-h, --help; -V, --version; -B, --bytes; -J, --json; -e, --extended[=LIST]; -p, --parse[=LIST]; -C, --caches[=LIST]; -H, --list-columns; -a, --all; -b, --online; -c, --offline; -r, --raw; -x, --hex; -y, --physical; --output-all", MaxArgs: commands.Unbounded}
var outputBuffer [32 << 10]byte

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	option := defaultOptions()
	for index := 1; index <= data.ArgCount(); index++ {
		argument, _ := data.Arg(index)
		if problem := option.parse(argument); problem != "" {
			utility.Fail(argument, problem)
		}
	}
	if problem := option.validate(); problem != "" {
		utility.Fail(nil, problem)
	}
	if option.help {
		printHelp()
		return
	}
	if option.version {
		utility.PrintText("lscpu (GoOS " + version.Release + ")")
		utility.EndLine()
		return
	}
	model := readCPUData()
	result := format(model, option, outputBuffer[:0])
	if len(result) > len(outputBuffer) {
		utility.Fail(nil, "output too large")
	}
	utility.Write(result)
}

func readCPUData() cpuData {
	var data cpuData
	var summaryWire [kcall.CPUInfoSize]byte
	if sys.CPUInfo(&summaryWire) != kcall.StatusOK {
		utility.Fail(nil, "CPU information unavailable")
	}
	var ok bool
	data.summary, ok = cpuinfo.DecodeSummary(summaryWire[:])
	if !ok {
		utility.Fail(nil, "invalid CPU information")
	}
	for index := uint64(0); index < uint64(len(data.entries)); index++ {
		var wire [kcall.CPUEntrySize]byte
		status := sys.CPUEntry(index, &wire)
		if status == kcall.StatusNotFound {
			break
		}
		if status != kcall.StatusOK {
			utility.Fail(nil, "CPU entry unavailable")
		}
		entry, valid := cpuinfo.DecodeCPUEntry(wire[:])
		if !valid {
			utility.Fail(nil, "invalid CPU entry")
		}
		data.entries[data.entryCount] = entry
		data.entryCount++
	}
	for index := uint64(0); index < uint64(len(data.caches)); index++ {
		var wire [kcall.CPUCacheInfoSize]byte
		status := sys.CPUCacheInfo(index, &wire)
		if status == kcall.StatusNotFound {
			break
		}
		if status != kcall.StatusOK {
			utility.Fail(nil, "CPU cache unavailable")
		}
		cache, valid := cpuinfo.DecodeCacheInfo(wire[:])
		if !valid {
			utility.Fail(nil, "invalid CPU cache")
		}
		data.caches[data.cacheCount] = cache
		data.cacheCount++
	}
	if uint32(data.entryCount) != data.summary.PresentLogicalCPUs {
		utility.Fail(nil, "incomplete CPU inventory")
	}
	return data
}

func printHelp() {
	utility.PrintText("usage: " + manifest.Usage)
	utility.EndLine()
	utility.PrintText("  " + manifest.Summary)
	utility.EndLine()
	utility.PrintText("  " + manifest.Options)
	utility.EndLine()
}
