package handle

import (
	"testing"

	"goos/internal/capability"
)

func owned(owner uint64) *Table {
	table := new(Table)
	table.SetOwner(owner)
	return table
}

func TestHolderFollowsTransferAndClose(t *testing.T) {
	const endpoint = 0x7001
	sender, receiver := owned(2), owned(3)
	id, ok := sender.Insert(TypeChannel, capability.New(endpoint, capability.Read|capability.Transfer))
	if holder, found := Holder(endpoint); !ok || !found || holder != 2 {
		t.Fatalf("holder %d %v after insert", holder, found)
	}
	moved, ok := sender.Transfer(receiver, id, TypeChannel, capability.Read)
	if !ok || !sender.Close(id) {
		t.Fatal("transfer")
	}
	if holder, found := Holder(endpoint); !found || holder != 3 {
		t.Fatalf("holder %d %v after the move", holder, found)
	}
	receiver.Close(moved)
	if _, found := Holder(endpoint); found {
		t.Fatal("an endpoint nobody holds still has a holder")
	}
}

// A launch that keeps its copy leaves two holders; either closing leaves the
// other.
func TestHolderKeepsEveryHolder(t *testing.T) {
	const endpoint = 0x7002
	parent, child := owned(1), owned(9)
	kept, _ := parent.Insert(TypeChannel, capability.New(endpoint, capability.Read|capability.Transfer))
	given, _ := parent.Transfer(child, kept, TypeChannel, capability.Read)
	child.Close(given)
	if holder, found := Holder(endpoint); !found || holder != 1 {
		t.Fatalf("holder %d %v after the child closed", holder, found)
	}
	parent.CloseAll(nil)
	if _, found := Holder(endpoint); found {
		t.Fatal("CloseAll left the endpoint held")
	}
}

// Endpoints come and go far more often than the index has slots; freed
// entries are found again.
func TestHolderIndexReusesClosedEntries(t *testing.T) {
	table := owned(4)
	for round := uint64(0); round < 4*indexSize; round++ {
		id, ok := table.Insert(TypeChannel, capability.New(0x10000+round, capability.Read))
		if !ok {
			t.Fatalf("round %d: insert", round)
		}
		if holder, found := Holder(0x10000 + round); !found || holder != 4 {
			t.Fatalf("round %d: holder %d %v", round, holder, found)
		}
		table.Close(id)
	}
	// Other types are not indexed.
	table.Insert(TypeMemory, capability.New(0x9000, capability.Read))
	if _, found := Holder(0x9000); found {
		t.Fatal("a memory handle was indexed")
	}
}
