package handle

import (
	"sync"
	"testing"
	"time"

	"goos/internal/capability"
)

// These tests run the handle tables the way processors share them (ADR
// 0187): each table under its own lock, two tables in rank order. A barrier
// starts the racing goroutines together, and a deadline turns a deadlock
// into a failure.

const raceDeadline = 10 * time.Second

func finish(t *testing.T, group *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(raceDeadline):
		t.Fatal("racing goroutines did not finish")
	}
}

// lockPair takes two tables for writing, the lower rank first, as a call
// that moves handles does.
func lockPair(first *Table, second *Table) func() {
	if second.Lock().Rank() < first.Lock().Rank() {
		first, second = second, first
	}
	first.Lock().Lock()
	second.Lock().LockAfter(first.Lock(), nil)
	return func() {
		second.Lock().Unlock()
		first.Lock().Unlock()
	}
}

// A lookup that races a close sees the handle until the close and never
// after it.
func TestTableCloseRacesLookup(t *testing.T) {
	table := owned(4)
	ids := make([]ID, 16)
	for index := range ids {
		ids[index], _ = table.Insert(TypeMemory, capability.New(uint64(0x9000+index), capability.Read))
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	failures := make(chan string, 8)
	for reader := 0; reader < 3; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			gone := make([]bool, len(ids))
			for round := 0; round < 2000; round++ {
				table.Lock().RLock()
				for index, id := range ids {
					_, found := table.Lookup(id, TypeMemory, capability.Read)
					if found && gone[index] {
						failures <- "a closed handle was found again"
					}
					gone[index] = !found
				}
				table.Lock().RUnlock()
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for _, id := range ids {
			table.Lock().Lock()
			if !table.Close(id) {
				failures <- "close of an open handle failed"
			}
			table.Lock().Unlock()
		}
	}()
	close(start)
	finish(t, &group)
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
}

// Two calls moving channel endpoints in opposite directions take the two
// tables in rank order, so neither waits for the other forever, and every
// endpoint ends in exactly one table with the index naming it.
func TestCrossTableTransfersBothWays(t *testing.T) {
	left, right := owned(2), owned(5)
	const each = 8
	var objects []uint64
	for index := 0; index < each; index++ {
		for _, side := range []struct {
			table *Table
			base  uint64
		}{{left, 0x7100}, {right, 0x7200}} {
			object := side.base + uint64(index)
			side.table.Insert(TypeChannel, capability.New(object, capability.Read|capability.Transfer))
			objects = append(objects, object)
		}
	}
	move := func(from *Table, to *Table) {
		unlock := lockPair(from, to)
		defer unlock()
		for index := 0; index < MaxHandles; index++ {
			id, typeID, _, found := from.At(index)
			if !found || typeID != TypeChannel {
				continue
			}
			if _, moved := from.Transfer(to, id, TypeChannel, capability.Read|capability.Transfer); moved {
				from.Close(id)
			}
			return
		}
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, pair := range [][2]*Table{{left, right}, {right, left}} {
		group.Add(1)
		go func(from *Table, to *Table) {
			defer group.Done()
			<-start
			for round := 0; round < 500; round++ {
				move(from, to)
			}
		}(pair[0], pair[1])
	}
	close(start)
	finish(t, &group)
	for _, object := range objects {
		inLeft, inRight := left.HasObject(TypeChannel, object), right.HasObject(TypeChannel, object)
		if inLeft == inRight {
			t.Fatalf("endpoint %#x in left %v, right %v", object, inLeft, inRight)
		}
		holder, found := Holder(object)
		if want := map[bool]uint64{true: 2, false: 5}[inLeft]; !found || holder != want {
			t.Fatalf("endpoint %#x held by %d %v, want %d", object, holder, found, want)
		}
	}
	left.CloseAll(nil)
	right.CloseAll(nil)
}

// A process that exits closes its table under the table's lock while
// another process moves handles into it. A move that lands before the exit
// is closed with the rest; one after it finds the table dead and keeps its
// handle, as a call that finds the process gone does. No endpoint is lost
// or closed twice.
func TestExitRacesHandleActivity(t *testing.T) {
	exiting, other := owned(3), owned(8)
	const count = 32
	for index := 0; index < count; index++ {
		other.Insert(TypeChannel, capability.New(uint64(0x7300+index), capability.Read|capability.Transfer))
	}
	dead := false
	closed := map[uint64]int{}
	start := make(chan struct{})
	halfway := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < MaxHandles; index++ {
			if index == count/2 {
				close(halfway)
			}
			unlock := lockPair(other, exiting)
			id, _, _, found := other.At(index)
			if found && !dead {
				if _, moved := other.Transfer(exiting, id, TypeChannel, capability.Read); moved {
					other.Close(id)
				}
			}
			unlock()
		}
	}()
	go func() {
		defer group.Done()
		<-halfway
		exiting.Lock().Lock()
		exiting.CloseAll(func(typeID Type, cap capability.Capability) { closed[cap.Object()]++ })
		dead = true
		exiting.Lock().Unlock()
	}()
	close(start)
	finish(t, &group)
	for index := 0; index < count; index++ {
		object := uint64(0x7300 + index)
		kept := other.HasObject(TypeChannel, object)
		if closed[object] > 1 || kept == (closed[object] == 1) {
			t.Fatalf("endpoint %#x kept %v, closed %d times", object, kept, closed[object])
		}
		holder, found := Holder(object)
		if kept != found || found && holder != 8 {
			t.Fatalf("endpoint %#x index holder %d %v, kept %v", object, holder, found, kept)
		}
	}
	other.CloseAll(nil)
}
