//go:build goos_userspace

// Command bench measures VFS round trips, file reads, metadata operations, and
// stream transfers from inside the running system.
package main

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/commands"
	"goos/userspace/startup"
	"goos/userspace/utility"
	userwait "goos/userspace/wait"
)

var manifest = commands.Manifest{
	Usage:   "bench vfs N | bench read|meta PATH N | bench io PATH SIZE | bench stream|send|recv N SIZE",
	Summary: "measure VFS requests, file reads and bulk I/O, metadata, or stream transfers",
	MinArgs: 2,
	MaxArgs: 3,
	Grants:  commands.GrantVFS,
}

// payload is what the stream modes move; its contents do not matter.
var payload [utility.MaxBenchChunk]byte

// samples holds per-operation intervals for the stream modes.
var samples [utility.MaxBenchSamples]uint64

//export userspaceMain
func userspaceMain() { utility.Run(manifest, run) }

func main() {}

// run times repeated operations from inside one process, so the result
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
		start := utility.ClockNanos()
		size := 0
		for index := uint64(0); index < count; index++ {
			size = readFile(path)
		}
		reportRate("read", count, size, utility.ClockNanos()-start, 0, false, -1)
	case utility.Equal(mode, "meta"):
		path := utility.Path(data, 2)
		count := argument(data, 3)
		mark, start := utility.HeapMark(), utility.ClockNanos()
		for index := uint64(0); index < count; index++ {
			created, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
			utility.Check(created, ok, path, true)
			removed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationUnlink, Path: path})
			utility.Check(removed, ok, path, true)
			utility.HeapReset(mark)
		}
		report("meta", count, utility.ClockNanos()-start)
	case utility.Equal(mode, "io"):
		bulkIO(utility.Path(data, 2), size(data, 3))
	case utility.Equal(mode, "stream"):
		private(argument(data, 2), chunk(data, 3))
	case utility.Equal(mode, "send"):
		send(argument(data, 2), chunk(data, 3))
	case utility.Equal(mode, "recv"):
		receive(argument(data, 2), chunk(data, 3))
	default:
		utility.Usage()
	}
	utility.Mark("BENCH_PROCESS_OK\n")
}

// private moves count chunks through a stream this process holds both ends
// of. Each operation is a write, the read that drains it, the read that finds
// the stream empty, and a clock read: the native calls `bench send | bench
// recv` makes per chunk when the two alternate, without the receiver's wait
// and without a context switch.
func private(count uint64, size int) {
	endpoints, ok := utility.CreateStream()
	if !ok {
		utility.Fail(nil, "stream unavailable")
	}
	buffer := payload[:size]
	kept := 0
	start := utility.ClockNanos()
	last := start
	for index := uint64(0); index < count; index++ {
		if status, moved := utility.StreamWrite(endpoints.Writer, buffer); status != 0 || moved != size {
			utility.Fail(nil, "stream write failed")
		}
		if status, moved := utility.StreamRead(endpoints.Reader, buffer); status != 0 || moved != size {
			utility.Fail(nil, "stream read failed")
		}
		if status, _ := utility.StreamRead(endpoints.Reader, buffer); status != uint64(kcall.StatusResourceExhausted) {
			utility.Fail(nil, "stream not empty")
		}
		now := utility.ClockNanos()
		if kept < len(samples) {
			samples[kept] = now - last
			kept++
		}
		last = now
	}
	endpoints.Close()
	reportRate("stream", count, size, last-start, 0, false, kept)
}

// send writes count chunks to standard output, the first stage of
// `bench send COUNT SIZE | bench recv COUNT SIZE`. It first sleeps a tenth of
// a second so that the receiver is running and blocked on the empty stream.
func send(count uint64, size int) {
	userwait.Sleep(clock.TicksPerSecond/10 + 1)
	for index := uint64(0); index < count; index++ {
		utility.Write(payload[:size])
	}
}

// receive reads what send writes and times it from the first chunk to the
// last, so launch and the sender's sleep are excluded. It counts its waits on
// an empty stream and reads the clock after every chunk.
//
// While the receiver is blocked, each write wakes it and the kernel switches
// to it on the write's return; it drains the chunk and blocks again, and the
// sender resumes. Every chunk is then one round trip between the processes,
// preceded by exactly one wait. The sender never blocks, though, so a timer
// tick that preempts the receiver hands the sender a time slice to fill the
// stream, and the pair moves many chunks per switch until the receiver drains
// it. The average over all chunks is therefore the pipeline's throughput;
// the intervals of chunks that followed exactly one wait are the round trips,
// and their median is reported separately.
func receive(count uint64, size int) {
	if count < 2 {
		utility.Fail(nil, "recv needs a COUNT of at least 2")
	}
	buffer := payload[:size]
	remaining := count * uint64(size)
	var start, last, waits, pending uint64
	timed, kept := false, 0
	for remaining != 0 {
		status, moved := utility.ReadInput(buffer)
		if utility.Retry(status) {
			pending++
			continue
		}
		if status != 0 || moved == 0 || uint64(moved) > remaining {
			utility.Fail(nil, "input ended early")
		}
		remaining -= uint64(moved)
		now := utility.ClockNanos()
		if !timed {
			// The clock starts after the first chunk; what follows it is
			// the measured transfer.
			if moved != size {
				utility.Fail(nil, "short first read")
			}
			start, timed = now, true
		} else {
			waits += pending
			if pending == 1 && kept < len(samples) {
				samples[kept] = now - last
				kept++
			}
		}
		last, pending = now, 0
	}
	reportRate("recv", count-1, size, last-start, waits, true, kept)
}

// maxIOSize bounds the file bench io writes.
const maxIOSize = 16 << 20

// bulkIO writes size bytes to path through a bulk buffer, reads them back the
// same way and checks them, then reads them once more inline, and reports the
// exchanges each took with the VFS, the time, and the rate (ADR 0107).
func bulkIO(path []byte, size uint64) {
	buffer, ok := utility.NewBulk(utility.BulkSize)
	if !ok {
		utility.Fail(nil, "no memory for the buffer")
	}
	mark := utility.HeapMark()
	created, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationCreate, Path: path})
	if created.Status == vfs.StatusAlreadyExists {
		created.Status = vfs.StatusOK
	}
	utility.Check(created, ok, path, true)
	truncated, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationTruncate, Path: path})
	utility.Check(truncated, ok, path, true)
	file, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read | vfs.Write, Path: path})
	utility.Check(file, ok, path, true)
	exchanges, start := uint64(0), utility.ClockNanos()
	for done := uint64(0); done < size; {
		utility.HeapReset(mark)
		chunk := min(size-done, uint64(len(buffer.Memory)))
		for index := uint64(0); index < chunk; index++ {
			buffer.Memory[index] = byte((done + index) * 7)
		}
		written, ok := buffer.WriteFile(file.Handle, int(chunk))
		utility.CheckWritten(written, ok, path, int(chunk))
		done += chunk
		exchanges++
	}
	reportIO("write", size, exchanges, utility.ClockNanos()-start)
	seek, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationSeek, Handle: file.Handle})
	utility.Check(seek, ok, path, false)
	exchanges, start = 0, utility.ClockNanos()
	for done := uint64(0); done < size; {
		utility.HeapReset(mark)
		read, ok := buffer.ReadFile(file.Handle, len(buffer.Memory))
		utility.Check(read, ok, path, false)
		exchanges++
		if read.Size == 0 {
			utility.Fail(path, "file ended early")
		}
		for index := uint64(0); index < read.Size; index++ {
			if buffer.Memory[index] != byte((done+index)*7) {
				utility.Fail(path, "read back different bytes")
			}
		}
		done += read.Size
	}
	reportIO("read", size, exchanges, utility.ClockNanos()-start)
	utility.HeapReset(mark)
	closed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: file.Handle})
	utility.Check(closed, ok, path, false)
	start = utility.ClockNanos()
	inline := readFile(path)
	if uint64(inline) != size {
		utility.Fail(path, "inline read found a different size")
	}
	reportIO("inline read", size, (size+vfs.MaxReplyData-1)/vfs.MaxReplyData+1, utility.ClockNanos()-start)
}

func reportIO(name string, size, exchanges, nanos uint64) {
	var line [224]byte
	result := append(line[:0], "io "...)
	result = append(result, name...)
	result = utility.AppendBenchCount(result, size, "bytes")
	result = utility.AppendBenchCount(result, exchanges, "exchanges")
	result = utility.AppendBenchCount(result, nanos/1000, "us")
	if nanos != 0 {
		result = utility.AppendBenchCount(result, size*1000000000/nanos/1024, "KiB/s")
	}
	utility.Print(result)
	utility.EndLine()
}

// size parses a byte count with an optional K or M suffix.
func size(data startup.Data, index int) uint64 {
	value, found := data.Arg(index)
	if !found || len(value) == 0 {
		utility.Usage()
	}
	multiplier := uint64(1)
	switch value[len(value)-1] {
	case 'K', 'k':
		multiplier, value = 1<<10, value[:len(value)-1]
	case 'M', 'm':
		multiplier, value = 1<<20, value[:len(value)-1]
	}
	count := uint64(0)
	for _, digit := range value {
		if digit < '0' || digit > '9' || count > maxIOSize {
			utility.Fail(value, "size must be a number of bytes, KiB (K) or MiB (M)")
		}
		count = count*10 + uint64(digit-'0')
	}
	if count == 0 || count*multiplier > maxIOSize {
		utility.Fail(value, "size must be 1 byte to 16 MiB")
	}
	return count * multiplier
}

// readFile opens, reads to EOF, and closes path, and returns its size. Replies
// are discarded, so the arena is rewound after each request.
func readFile(path []byte) int {
	mark := utility.HeapMark()
	opened, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: path})
	utility.Check(opened, ok, path, false)
	handle, size := opened.Handle, 0
	for {
		utility.HeapReset(mark)
		reply, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationRead, Handle: handle, Size: vfs.MaxReplyData})
		utility.Check(reply, ok, path, false)
		if len(reply.Data) == 0 {
			break
		}
		size += len(reply.Data)
	}
	utility.HeapReset(mark)
	closed, ok := utility.CallVFS(vfs.Request{Operation: vfs.OperationClose, Handle: handle})
	utility.Check(closed, ok, path, false)
	utility.HeapReset(mark)
	return size
}

func argument(data startup.Data, index int) uint64 {
	value, _ := data.Arg(index)
	count, ok := utility.BenchCount(value)
	if !ok {
		utility.Fail(value, "count must be 1..10000")
	}
	return count
}

func chunk(data startup.Data, index int) int {
	value, found := data.Arg(index)
	if !found {
		utility.Usage()
	}
	size, ok := utility.BenchChunk(value)
	if !ok {
		utility.Fail(value, "size must be 1..256")
	}
	return size
}

func report(name string, count, nanos uint64) {
	var line [128]byte
	utility.Print(utility.AppendBenchResult(line[:0], name, count, nanos))
	utility.EndLine()
}

// reportRate prints a result with its transfer rate. A kept count of zero or
// more appends the number of sampled intervals and their median; a negative
// one means the mode keeps none.
func reportRate(name string, count uint64, size int, nanos, waits uint64, withWaits bool, kept int) {
	var line [224]byte
	result := utility.AppendBenchResult(line[:0], name, count, nanos)
	result = utility.AppendBenchRate(result, count, uint64(size), nanos)
	if withWaits {
		result = utility.AppendBenchCount(result, waits, "waits")
	}
	if kept >= 0 {
		result = utility.AppendBenchCount(result, uint64(kept), "sampled")
		result = utility.AppendBenchCount(result, utility.Median(samples[:kept]), "ns median")
	}
	utility.Print(result)
	utility.EndLine()
}
