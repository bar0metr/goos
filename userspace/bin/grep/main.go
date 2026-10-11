//go:build goos_userspace

package main

import (
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "grep [-i] PATTERN [FILE]", Summary: "literal matching lines",
	Options: "-i: ASCII fold; stdin if no FILE; 256-byte pattern, 8192-byte line, 64 MiB input; status 1: no match",
	MinArgs: 1, MaxArgs: 3, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	pattern, insensitive, fileIndex, ok := admincmd.GrepArguments(data)
	if !ok {
		utility.Usage()
	}
	if len(pattern) > admincmd.MaxPattern {
		utility.Fail(nil, "pattern exceeds 256 bytes")
	}
	var matched bool
	var status admincmd.Status
	if fileIndex == 0 {
		matched, status = admincmd.Grep(admincmd.NativeInput{}, admincmd.NativeOutput{}, pattern, insensitive)
	} else {
		file := admincmd.OpenFile(utility.Path(data, fileIndex))
		matched, status = admincmd.Grep(file, admincmd.NativeOutput{}, pattern, insensitive)
		file.Close()
	}
	admincmd.Finish(status)
	if !matched {
		utility.Exit(1)
	}
}
