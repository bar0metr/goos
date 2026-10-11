package main

import (
	"bytes"
	"goos/kernel/handle"
	"goos/userspace/remoteservice"
	"testing"
)

func TestExecInputQueueWindowBoundAndBinaryWrap(t *testing.T) {
	var queue inputQueue
	input := bytes.Repeat([]byte{0, 255, 3, 4, '\r', '\n', 1, 2}, 8192)
	if !queue.put(input) || queue.put([]byte{1}) {
		t.Fatal("window bound")
	}
	var got []byte
	for queue.size > 0 {
		data := queue.peek()
		got = append(got, data...)
		queue.drop(len(data))
	}
	if !bytes.Equal(got, input) {
		t.Fatal("binary input changed")
	}
	queue.start = len(queue.data) - 3
	if !queue.put(input[:12]) {
		t.Fatal("wrap put")
	}
	got = nil
	for queue.size > 0 {
		data := queue.peek()
		got = append(got, data...)
		queue.drop(len(data))
	}
	if !bytes.Equal(got, input[:12]) {
		t.Fatal("wrapped binary input changed")
	}
}
func TestExecOutputFairnessSharedWindowAndDrain(t *testing.T) {
	var output, errorOutput remoteservice.Queue
	output.Put([]byte("stdout"))
	errorOutput.Put([]byte("stderr"))
	nextError := false
	var order []bool
	window := 3
	writer := func(data []byte, extended bool) (int, error) {
		count := min(len(data), window)
		if count > 0 {
			order = append(order, extended)
			window -= count
		}
		return count, nil
	}
	if !drainOutputPair(&output, &errorOutput, &nextError, writer, 8) || len(order) != 1 || order[0] {
		t.Fatal("first packet")
	}
	window = 3
	if !drainOutputPair(&output, &errorOutput, &nextError, writer, 8) || len(order) != 2 || !order[1] {
		t.Fatal("stderr starved")
	}
	window = 100
	if !drainOutputPair(&output, &errorOutput, &nextError, writer, 8) || output.Len() != 0 || errorOutput.Len() != 0 {
		t.Fatal("output drain")
	}
	if outputReadyToExit(true, handle.ID(1), 0, 0, 0, false) || outputReadyToExit(true, 0, handle.ID(2), 0, 0, false) || outputReadyToExit(true, 0, 0, 0, 1, false) || outputReadyToExit(true, 0, 0, 1, 0, false) || outputReadyToExit(false, 0, 0, 0, 0, false) || outputReadyToExit(true, 0, 0, 0, 0, true) {
		t.Fatal("completion preceded both output drains/status/KEX")
	}
	if !outputReadyToExit(true, 0, 0, 0, 0, false) {
		t.Fatal("fully drained output did not finish")
	}
}
func TestExecSSHStatusBounds(t *testing.T) {
	for _, test := range []struct {
		native uint64
		ssh    uint32
	}{{0, 0}, {7, 7}, {130, 130}, {0xffffffff, 0xffffffff}, {0x100000000, 1}, {^uint64(0), 1}} {
		if code := sshExitCode(test.native); code != test.ssh {
			t.Fatal(test, code)
		}
	}
}
