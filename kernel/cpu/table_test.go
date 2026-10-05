package cpu

import (
	"goos/internal/cpuinfo"
	"goos/kernel/acpi"
	"goos/kernel/sched"
	"testing"
)

func inventoryOf(t *testing.T, ids ...uint32) *Inventory {
	t.Helper()
	processors := acpi.Processors{Count: uint16(len(ids))}
	for index, id := range ids {
		processors.Entries[index] = acpi.Processor{APICID: id}
	}
	probe := testProbe()
	var inventory Inventory
	Normalize(&inventory, &probe, &processors, true)
	return &inventory
}

func TestBoundMatchesScheduler(t *testing.T) {
	if MaxCPUs != sched.MaxCPUs {
		t.Fatalf("cpu.MaxCPUs = %d, sched.MaxCPUs = %d", MaxCPUs, sched.MaxCPUs)
	}
}

func TestTableMapsIndexToAPICIDWithBootstrapFirst(t *testing.T) {
	var table Table
	if !table.Load(inventoryOf(t, 0, 1, 2, 3)) {
		t.Fatal("load refused")
	}
	if table.Count() != 4 {
		t.Fatalf("count = %d", table.Count())
	}
	// The probe names APIC ID 2 as the bootstrap processor.
	if id, _ := table.APICID(0); id != 2 {
		t.Fatalf("CPU 0 has APIC ID %d, want the bootstrap processor's 2", id)
	}
	for index := 0; index < 4; index++ {
		id, _ := table.APICID(index)
		if back, found := table.Index(id); !found || back != index {
			t.Fatalf("APIC ID %d maps back to %d", id, back)
		}
	}
	if table.State(0) != Online || table.OnlineMask() != 1 {
		t.Fatal("only the bootstrap processor starts online")
	}
}

func TestTableRefusesDuplicateAPICIDs(t *testing.T) {
	inventory := inventoryOf(t, 2, 1)
	inventory.CPUs[1].APICID = 2
	var table Table
	if table.Load(inventory) {
		t.Fatal("duplicate APIC ID accepted")
	}
}

func TestTableBoundsProcessors(t *testing.T) {
	ids := make([]uint32, 80)
	for index := range ids {
		ids[index] = uint32(index)
	}
	var table Table
	if !table.Load(inventoryOf(t, ids...)) || table.Count() != MaxCPUs {
		t.Fatalf("count = %d, want %d", table.Count(), MaxCPUs)
	}
}

func TestUnsupportedDestinationStaysOffline(t *testing.T) {
	inventory := inventoryOf(t, 2, 1, 300)
	var table Table
	if !table.Load(inventory) {
		t.Fatal("load refused")
	}
	index, _ := table.Index(300)
	if table.State(index) != Unsupported || table.Startable(index) {
		t.Fatal("an APIC ID beyond the xAPIC destination would be started")
	}
	if table.Transition(index, Started) {
		t.Fatal("unsupported processor moved to started")
	}
}

func TestTransitionsOnlyAdvance(t *testing.T) {
	var table Table
	table.Load(inventoryOf(t, 2, 1, 3))
	if !table.Transition(1, Started) || !table.Transition(1, Online) {
		t.Fatal("start-up transitions refused")
	}
	if table.Transition(1, Started) || table.Transition(0, Failed) {
		t.Fatal("a processor moved backwards or the bootstrap processor changed")
	}
	if !table.Transition(2, Failed) || table.Startable(2) {
		t.Fatal("failed processor still startable")
	}
	if table.OnlineMask() != 0b011 {
		t.Fatalf("online mask = %b", table.OnlineMask())
	}
}

func TestPublishReportsLiveOnlineState(t *testing.T) {
	inventory := inventoryOf(t, 2, 0, 1, 3)
	var table Table
	table.Load(inventory)
	table.Transition(1, Started)
	table.Transition(1, Online)
	table.Transition(2, Failed)
	table.Transition(3, Started)
	table.Transition(3, Online)
	table.Publish(inventory)
	if inventory.Summary.OnlineCPUs != 3 {
		t.Fatalf("online = %d", inventory.Summary.OnlineCPUs)
	}
	for index, want := range []bool{true, true, false, true} {
		if got := inventory.CPUs[index].Flags&cpuinfo.CPUEntryOnline != 0; got != want {
			t.Fatalf("CPU %d online = %v", index, got)
		}
	}
}
