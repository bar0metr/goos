package main

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/remoteservice"
)

// inputQueue is exactly the SSH receive window. Credit is returned after native
// stdin consumes bytes, so a blocked reader cannot grow this queue indefinitely.
type inputQueue struct {
	data        [64 * 1024]byte
	start, size int
}

func (queue *inputQueue) put(data []byte) bool {
	if len(data) > len(queue.data)-queue.size {
		return false
	}
	for _, value := range data {
		queue.data[(queue.start+queue.size)%len(queue.data)] = value
		queue.size++
	}
	return true
}
func (queue *inputQueue) peek() []byte {
	count := min(queue.size, len(queue.data)-queue.start, int(kcall.MaxStreamIO))
	return queue.data[queue.start : queue.start+count]
}
func (queue *inputQueue) drop(count int) {
	queue.start = (queue.start + count) % len(queue.data)
	queue.size -= count
}

// drainOutputPair alternates SSH packets across independent stdout/stderr
// queues. Both spend the same channel window, and neither can monopolize it.
func drainOutputPair(stdout, stderr *remoteservice.Queue, nextError *bool, write func([]byte, bool) (int, error), budget int) bool {
	for sent := 0; sent < budget; sent++ {
		extended := *nextError
		queue := stdout
		if extended {
			queue = stderr
		}
		if queue.Len() == 0 {
			extended = !extended
			queue = stdout
			if extended {
				queue = stderr
			}
		}
		if queue.Len() == 0 {
			return true
		}
		data := queue.Peek(16 * 1024)
		count, err := write(data, extended)
		if err != nil || count < 0 || count > len(data) {
			return false
		}
		if count == 0 {
			return true
		}
		queue.Drop(count)
		*nextError = !extended
	}
	return true
}

func outputReadyToExit(exitKnown bool, stdout, stderr handle.ID, output, errorOutput int, rekey bool) bool {
	return exitKnown && stdout == 0 && stderr == 0 && output == 0 && errorOutput == 0 && !rekey
}
func sshExitCode(native uint64) uint32 {
	if native > 0xffffffff {
		return 1
	}
	return uint32(native)
}
