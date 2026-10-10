//go:build goos_userspace

package main

import (
	"goos/kernel/ipc"
	"goos/userspace/tcpapi"
	"testing"
)

func TestCloseCompletionRetainedUntilPendingIOIsDrained(t *testing.T) {
	workers = [len(workers)]worker{}
	accept = 0
	state.Current = nil
	nextWorker = 0
	if !dispatch.Init(make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), func(*ipc.Message) bool { return true }) {
		t.Fatal("dispatch init")
	}
	w := &workers[0]
	w.endpoint = 42
	w.read, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 42, Length: 256}, nil)
	w.write, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: 42}, []byte("end"))
	w.close, _ = dispatch.Start(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: 42}, nil)
	complete := func(op tcpapi.Opcode, result tcpapi.Result, corr uint64) {
		t.Helper()
		var message ipc.Message
		if !tcpapi.EncodeReply(&tcpapi.Reply{Opcode: op, Result: result, Endpoint: 42}, corr, &message) || !dispatch.Receive(&message) {
			t.Fatal("completion")
		}
	}
	complete(tcpapi.OpClose, tcpapi.ResultOK, w.close)
	advance()
	if !w.closeDone || w.endpoint == 0 {
		t.Fatal("forgot close completion or reused live slot")
	}
	complete(tcpapi.OpRead, tcpapi.ResultClosed, w.read)
	advance()
	if w.endpoint == 0 {
		t.Fatal("reused slot with outstanding write")
	}
	complete(tcpapi.OpWrite, tcpapi.ResultClosed, w.write)
	advance()
	if w.endpoint != 0 || dispatch.Pending() != 0 {
		t.Fatal("failed to reap drained slot")
	}
}
