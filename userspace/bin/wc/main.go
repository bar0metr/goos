//go:build goos_userspace

package main

import (
	"goos/userspace/admincmd"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage: "wc [-l|-w|-c] [FILE]", Summary: "line word byte counts",
	Options: "default: LF lines, ASCII words, bytes; stdin if no FILE; max 64 MiB",
	MaxArgs: 2, Grants: commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }
func main()          {}

func run(data startup.Data) {
	mode, fileIndex, ok := admincmd.WCArguments(data)
	if !ok {
		utility.Usage()
	}
	var counts admincmd.Counts
	var status admincmd.Status
	if fileIndex == 0 {
		counts, status = admincmd.WC(admincmd.NativeInput{})
	} else {
		file := admincmd.OpenFile(utility.Path(data, fileIndex))
		counts, status = admincmd.WC(file)
		file.Close()
	}
	admincmd.Finish(status)
	var output [64]byte
	line := output[:0]
	if mode == 0 || mode == 'l' {
		line = admincmd.AppendUint(line, counts.Lines)
	}
	if mode == 0 {
		line = append(line, ' ')
	}
	if mode == 0 || mode == 'w' {
		line = admincmd.AppendUint(line, counts.Words)
	}
	if mode == 0 {
		line = append(line, ' ')
	}
	if mode == 0 || mode == 'c' {
		line = admincmd.AppendUint(line, counts.Bytes)
	}
	utility.Write(append(line, '\n'))
}
