//go:build !goos_validation

package linux

import (
	"sync"
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/ipc"
	"goos/kernel/lock"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

// TestContinuationSendVFSAndAbandon exercises the real IPC send, not the
// callback-only Send helper. Successful queueing and Sent must be atomic with
// abandonment; a failed or cancelled send must never create a request.
func TestContinuationSendVFSAndAbandon(t *testing.T) {
	lock.Install(lock.Hooks{})
	const correlation = 73
	setup := func(t *testing.T, generation uint32) (*ContinuationTable, sched.TaskID, *ipc.Channel, ipc.Message) {
		t.Helper()
		table := &ContinuationTable{}
		task, ok := sched.NewTaskID(6, generation)
		if !ok || !table.Begin(task, Request{Number: abi.SysOpenat}, []byte("/file")) || !table.Issue(task, OpenStageOpen, correlation) {
			t.Fatal("continuation setup failed")
		}
		message, ok := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/file")}, correlation)
		if !ok {
			t.Fatal("encode open request")
		}
		return table, task, ipc.NewChannel(), message
	}
	send := func(table *ContinuationTable, task sched.TaskID, channel *ipc.Channel, message *ipc.Message) (ipc.Status, bool) {
		channel.Lock()
		status, sent := table.SendVFS(task, OpenStageOpen, correlation, channel, 0, message, nil, nil, nil)
		channel.Unlock()
		return status, sent
	}
	check := func(t *testing.T, channel *ipc.Channel, pending Pending, active, sent bool) {
		t.Helper()
		if !active || pending.Sent != sent || pending.AbandonFailsVFS() != sent {
			t.Fatalf("abandoned=%+v active=%v sent=%v", pending, active, sent)
		}
		channel.Lock()
		message, status := channel.Receive(1)
		channel.Unlock()
		if !sent {
			if status != ipc.StatusNotFound {
				t.Fatalf("unsent request queued: status=%v message=%+v", status, message)
			}
			return
		}
		request, ok := vfs.DecodeRequest(message)
		if status != ipc.StatusOK || !ok || message.Correlation != correlation || request.Operation != vfs.OperationOpen || string(request.Path) != "/file" {
			t.Fatalf("delivered request status=%v decoded=%v message=%+v request=%+v", status, ok, message, request)
		}
	}

	t.Run("delivered", func(t *testing.T) {
		table, task, channel, message := setup(t, 1)
		status, sent := send(table, task, channel, &message)
		if status != ipc.StatusOK || !sent {
			t.Fatalf("send status=%v sent=%v", status, sent)
		}
		pending, active := table.Abandon(task)
		check(t, channel, pending, active, true)
	})
	t.Run("abandoned-before-send", func(t *testing.T) {
		table, task, channel, message := setup(t, 2)
		pending, active := table.Abandon(task)
		status, sent := send(table, task, channel, &message)
		if status != ipc.StatusNotFound || sent {
			t.Fatalf("send status=%v sent=%v", status, sent)
		}
		check(t, channel, pending, active, false)
	})
	t.Run("full-queue", func(t *testing.T) {
		table, task, channel, message := setup(t, 3)
		notice := ipc.Message{ProtocolID: vfs.ProtocolID, Version: vfs.ProtocolVersion, Kind: ipc.KindNotification}
		channel.Lock()
		for i := 0; i < ipc.QueueCapacity; i++ {
			if status := channel.Send(0, notice, nil, nil, nil); status != ipc.StatusOK {
				channel.Unlock()
				t.Fatalf("fill queue[%d]: %v", i, status)
			}
		}
		channel.Unlock()
		status, sent := send(table, task, channel, &message)
		if status != ipc.StatusResourceExhausted || sent {
			t.Fatalf("send to full queue status=%v sent=%v", status, sent)
		}
		pending, active := table.Abandon(task)
		channel.Lock()
		for i := 0; i < ipc.QueueCapacity; i++ {
			got, result := channel.Receive(1)
			if result != ipc.StatusOK || got.Kind != ipc.KindNotification {
				channel.Unlock()
				t.Fatalf("drain queue[%d]: status=%v message=%+v", i, result, got)
			}
		}
		channel.Unlock()
		check(t, channel, pending, active, false)
	})
	t.Run("racing", func(t *testing.T) {
		for round := uint32(1); round <= 500; round++ {
			table, task, channel, message := setup(t, round)
			var status ipc.Status
			var sent, active bool
			var pending Pending
			start := make(chan struct{})
			var done sync.WaitGroup
			done.Add(2)
			go func() {
				defer done.Done()
				<-start
				status, sent = send(table, task, channel, &message)
			}()
			go func() {
				defer done.Done()
				<-start
				pending, active = table.Abandon(task)
			}()
			close(start)
			done.Wait()
			if sent && status != ipc.StatusOK || !sent && status != ipc.StatusNotFound {
				t.Fatalf("round %d: status=%v sent=%v", round, status, sent)
			}
			check(t, channel, pending, active, sent)
			if _, exists := table.Pending(task); exists {
				t.Fatalf("round %d: continuation remained active", round)
			}
		}
	})
}
