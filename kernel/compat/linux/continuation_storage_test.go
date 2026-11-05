package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/objects"
	"goos/kernel/sched"
)

// A slot's record comes from storage on the slot's first call and serves
// every later task of the slot; a slot storage refuses fails its call.
func TestContinuationRecordsComeFromStorageOncePerSlot(t *testing.T) {
	var table ContinuationTable
	storage := &objects.Limited[Continuation]{Limit: 2}
	table.SetStorage(storage)
	request := Request{Number: abi.SysPpoll}
	first, _ := sched.NewTaskID(sched.MaxTasks-1, 1)
	if !table.Begin(first, request, []byte("/a")) || storage.Live != 1 {
		t.Fatalf("first call took %d records", storage.Live)
	}
	if !table.Finish(first) {
		t.Fatal("finish failed")
	}
	next, _ := sched.NewTaskID(sched.MaxTasks-1, 2)
	if !table.Begin(next, request, nil) || storage.Live != 1 {
		t.Fatalf("a later task of the slot took a new record: %d", storage.Live)
	}
	if pending, ok := table.Pending(next); !ok || pending.Number != abi.SysPpoll {
		t.Fatal("the reused record lost the call")
	}
	if path, _ := table.Paths(next); len(path) != 0 {
		t.Fatalf("the reused record kept the old path %q", path)
	}
	other, _ := sched.NewTaskID(64, 1)
	if !table.Begin(other, request, nil) || storage.Live != 2 {
		t.Fatal("a second slot did not get its record")
	}
	refused, _ := sched.NewTaskID(0, 1)
	if table.Begin(refused, request, nil) {
		t.Fatal("a call began without a record")
	}
	if _, ok := table.Pending(refused); ok {
		t.Fatal("a refused call is pending")
	}
}
