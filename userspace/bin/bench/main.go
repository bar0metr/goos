//go:build goos_userspace

// Command bench measures VFS round trips or file reads from inside one process.
package main

import (
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
)

var manifest = commands.Manifest{
	Usage:   "bench vfs COUNT | bench read FILE COUNT",
	Summary: "measure VFS round trips or file reads",
	MinArgs: 2,
	MaxArgs: 3,
	Grants:  commands.GrantVFS,
}

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

// run times repeated VFS requests from inside one process, so the result
// excludes process launch and shell overhead. Replies are discarded, so the
// arena is rewound after each operation.
func run(data startup.Data) {
	mode, _ := data.Arg(1)
	switch {
	case utility.Equal(mode, "vfs"):
		count := argument(data, 2)
		root := []byte("/")
		mark, start := utility.HeapMark(), utility.ClockNanos()
		for index := uint64(0); index < count; index++ {
			reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationStat, Path: root})
			utility.Check(reply, ok, root, false)
			utility.HeapReset(mark)
		}
		report("vfs", count, utility.ClockNanos()-start)
	case utility.Equal(mode, "read"):
		path := utility.Path(data, 2)
		count := argument(data, 3)
		mark, start := utility.HeapMark(), utility.ClockNanos()
		for index := uint64(0); index < count; index++ {
			readFile(path)
			utility.HeapReset(mark)
		}
		report("read", count, utility.ClockNanos()-start)
	default:
		utility.Usage()
	}
	utility.Mark("BENCH_PROCESS_OK\n")
}

func readFile(path []byte) {
	opened, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, ok, path, false)
	for {
		reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		utility.Check(reply, ok, path, false)
		if len(reply.Data) == 0 {
			break
		}
	}
	closed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	utility.Check(closed, ok, path, false)
}

func argument(data startup.Data, index int) uint64 {
	value, _ := data.Arg(index)
	count, ok := utility.BenchCount(value)
	if !ok {
		utility.Fail(value, "count must be 1..10000")
	}
	return count
}

func report(name string, count, nanos uint64) {
	var line [128]byte
	utility.Print(utility.AppendBenchResult(line[:0], name, count, nanos))
	utility.EndLine()
}
