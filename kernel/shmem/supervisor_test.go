package shmem

import "testing"

func TestSupervisorObjectBudgetLeavesOrdinaryLimitUnchanged(t *testing.T) {
	var table Table
	memory := newMemory()
	for index := 0; index < 32; index++ {
		if _, ok := table.CreateWithLimit(memory, memory.view, zeroed, 1, 1, 32); !ok {
			t.Fatalf("supervisor object %d refused", index)
		}
	}
	if _, ok := table.CreateWithLimit(memory, memory.view, zeroed, 1, 1, 32); ok {
		t.Fatal("supervisor budget exceeded")
	}
	for index := 0; index < MaxObjectsPerProcess; index++ {
		if _, ok := table.Create(memory, memory.view, zeroed, 1, 2); !ok {
			t.Fatal("ordinary budget unexpectedly reduced", index)
		}
	}
	if _, ok := table.Create(memory, memory.view, zeroed, 1, 2); ok {
		t.Fatal("ordinary budget expanded")
	}
}

func TestObjectBudgetRejectsInvalidLimit(t *testing.T) {
	var table Table
	memory := newMemory()
	for _, limit := range []int{-1, 0, 33} {
		if _, ok := table.CreateWithLimit(memory, memory.view, zeroed, 1, 1, limit); ok {
			t.Fatal("invalid object limit accepted", limit)
		}
	}
	if table.Live() != 0 || len(memory.pages) != 0 {
		t.Fatal("invalid budget allocated resources")
	}
}
