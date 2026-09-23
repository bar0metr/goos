//go:build goos_userspace

package utility

import (
	"goos/kernel/vfs"
	"goos/userspace/startup"
	"unsafe"
)

var benchClock uint64

//go:linkname clockReadNanos goos_clock_read_nanos
func clockReadNanos(address unsafe.Pointer, size uintptr) uint64

// runBench times repeated VFS requests from inside one process, so the result
// excludes process launch and shell overhead. Replies are discarded, so the
// arena is rewound after each operation.
func runBench(data startup.Data) {
	mode, _ := data.Arg(1)
	switch {
	case equal(mode, "vfs"):
		count := benchArgument(data, 2)
		root := []byte("/")
		mark, start := heapPointer, benchNow()
		for index := uint64(0); index < count; index++ {
			reply, ok := callVFS(vfs.Request{Operation: vfs.OperationStat, Path: root})
			check(reply, ok, root, false)
			heapPointer = mark
		}
		benchReport("vfs", count, benchNow()-start)
	case equal(mode, "read"):
		path := requiredPath(data, 2)
		count := benchArgument(data, 3)
		mark, start := heapPointer, benchNow()
		for index := uint64(0); index < count; index++ {
			benchReadFile(path)
			heapPointer = mark
		}
		benchReport("read", count, benchNow()-start)
	default:
		usage()
	}
	mark("BENCH_PROCESS_OK\n")
}

func benchReadFile(path []byte) {
	opened, ok := callVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	check(opened, ok, path, false)
	for {
		reply, ok := callVFS(vfs.Request{Operation: vfs.OperationRead, Handle: opened.Handle, Size: vfs.MaxReplyData})
		check(reply, ok, path, false)
		if len(reply.Data) == 0 {
			break
		}
	}
	closed, ok := callVFS(vfs.Request{Operation: vfs.OperationClose, Handle: opened.Handle})
	check(closed, ok, path, false)
}

func benchArgument(data startup.Data, index int) uint64 {
	value, _ := data.Arg(index)
	count, ok := benchCount(value)
	if !ok {
		failf(value, "count must be 1..10000")
	}
	return count
}

func benchNow() uint64 {
	if clockReadNanos(unsafe.Pointer(&benchClock), 8) != 0 {
		failf(nil, "clock unavailable")
	}
	return benchClock
}

func benchReport(name string, count, nanos uint64) {
	var line [128]byte
	lineBytes(appendBenchResult(line[:0], name, count, nanos))
	lineFlush()
}
