package handle

import (
	"goos/internal/capability"
	"testing"
)

func TestHandleLookupChecksTypeAndRights(t *testing.T) {
	var table Table
	id, ok := table.Insert(TypeMemory, capability.New(42, capability.Read|capability.Transfer))
	if !ok {
		t.Fatal("insert failed")
	}
	cap, ok := table.Lookup(id, TypeMemory, capability.Read)
	if !ok || cap.Object() != 42 {
		t.Fatal("valid lookup failed")
	}
	if _, ok := table.Lookup(id, TypeTask, capability.Read); ok {
		t.Fatal("wrong handle type accepted")
	}
	if _, ok := table.Lookup(id, TypeMemory, capability.Write); ok {
		t.Fatal("missing right accepted")
	}
}

func TestClosedAndForgedHandlesAreRejected(t *testing.T) {
	var table Table
	id, _ := table.Insert(TypeTask, capability.New(7, capability.Wait))
	if !table.Close(id) || table.Close(id) {
		t.Fatal("invalid close behavior")
	}
	if _, ok := table.Lookup(id, TypeTask, capability.Wait); ok {
		t.Fatal("stale handle accepted")
	}
	forged := ID(uint64(id) + 1<<32)
	if _, ok := table.Lookup(forged, TypeTask, capability.Wait); ok {
		t.Fatal("forged generation accepted")
	}
	replacement, ok := table.Insert(TypeTask, capability.New(8, capability.Wait))
	if !ok || replacement == id {
		t.Fatal("slot generation was not changed")
	}
}

func TestTransferCannotEscalateRights(t *testing.T) {
	var source, target Table
	id, _ := source.Insert(TypeMemory, capability.New(9, capability.Read|capability.Transfer))
	transferred, ok := source.Transfer(&target, id, TypeMemory, capability.Read)
	if !ok {
		t.Fatal("valid transfer failed")
	}
	if _, ok := target.Lookup(transferred, TypeMemory, capability.Read); !ok {
		t.Fatal("transferred capability missing")
	}
	if _, ok := source.Transfer(&target, id, TypeMemory, capability.Write); ok {
		t.Fatal("transfer escalated rights")
	}
	withoutTransfer, _ := source.Insert(TypeMemory, capability.New(10, capability.Read))
	if _, ok := source.Transfer(&target, withoutTransfer, TypeMemory, capability.Read); ok {
		t.Fatal("transfer without authority succeeded")
	}
}

func TestInvalidInsertAndFullTable(t *testing.T) {
	var table Table
	if table.Available() != MaxHandles {
		t.Fatal("empty table capacity is incorrect")
	}
	if _, ok := table.Insert(TypeInvalid, capability.New(1, capability.Read)); ok {
		t.Fatal("invalid type accepted")
	}
	for index := 0; index < MaxHandles; index++ {
		if _, ok := table.Insert(TypeTask, capability.New(uint64(index+1), capability.Wait)); !ok {
			t.Fatalf("insert %d failed", index)
		}
	}
	if _, ok := table.Insert(TypeTask, capability.New(999, capability.Wait)); ok {
		t.Fatal("table capacity exceeded")
	}
	if table.Available() != 0 {
		t.Fatal("full table reports available slots")
	}
}

func TestCloseAllInvalidatesAndReportsHandles(t *testing.T) {
	var table Table
	first, _ := table.Insert(TypeMemory, capability.New(11, capability.Read))
	second, _ := table.Insert(TypeChannel, capability.New(12, capability.Write))
	seen := uint64(0)
	table.CloseAll(func(typeID Type, cap capability.Capability) {
		seen += cap.Object()
		if cap.Object() == 12 && typeID != TypeChannel {
			t.Fatal("channel reported with wrong type")
		}
	})
	if seen != 23 || table.Available() != MaxHandles {
		t.Fatalf("close all left state behind: seen=%d available=%d", seen, table.Available())
	}
	if _, ok := table.Lookup(first, TypeMemory, capability.Read); ok {
		t.Fatal("first closed handle still resolves")
	}
	if _, ok := table.Lookup(second, TypeChannel, capability.Write); ok {
		t.Fatal("second closed handle still resolves")
	}
}

func TestAtDescribesOnlyOpenHandles(t *testing.T) {
	var table Table
	id, _ := table.Insert(TypeChannel, capability.New(7, capability.Read))
	found, typeID, cap, ok := table.At(0)
	if !ok || found != id || typeID != TypeChannel || cap.Object() != 7 || cap.Rights() != capability.Read {
		t.Fatal("open handle not described")
	}
	table.Close(id)
	if _, _, _, ok := table.At(0); ok {
		t.Fatal("closed handle described")
	}
	if _, _, _, ok := table.At(MaxHandles); ok {
		t.Fatal("out-of-range slot described")
	}
}
