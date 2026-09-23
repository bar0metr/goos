//go:build goos_userspace

// Command readline copies one line of standard input to standard output.
package main

import (
	"goos/kernel/kcall"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "readline",
	Summary: "copy one line of standard input to standard output",
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

const maxLine = 128

func run(startup.Data) {
	var line [maxLine]byte
	size := 0
	for {
		value, status := utility.ReadByte()
		if utility.Retry(status) {
			continue
		}
		if status == uint64(kcall.StatusPeerDied) {
			// A closed input is end of input, not a fault: a pipeline whose
			// producer never started ends here. With nothing read there is no
			// line, which the exit status says without a message.
			if size == 0 {
				utility.Exit(1)
			}
			break
		}
		if status != 0 {
			utility.Fail(nil, "cannot read standard input")
		}
		if value == '\r' || value == '\n' {
			break
		}
		if size == len(line) {
			utility.Fail(nil, "line longer than 128 bytes")
		}
		line[size] = value
		size++
	}
	utility.Print(line[:size])
	utility.EndLine()
	utility.Mark("STDIN_READ_OK\n")
}
