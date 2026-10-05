package cpu

import "goos/internal/cpuinfo"

// MaxCPUs bounds the processors GoOS starts. Present processors beyond it
// stay offline.
const MaxCPUs = 64

// MaxXAPICID is the highest APIC ID an xAPIC interrupt command can name.
// 0xff is the broadcast destination.
const MaxXAPICID = 0xfe

// State is where a processor is in its start-up.
type State uint8

const (
	// Present is a processor the firmware listed that has not been started.
	Present State = iota
	// Started has entered the kernel on its own stack and waits to be
	// brought online.
	Started
	// Online runs tasks.
	Online
	// Failed did not answer its start-up signals in time.
	Failed
	// Unsupported cannot be started: its APIC ID does not fit the interrupt
	// destination, or it lies beyond MaxCPUs.
	Unsupported
)

// Table is the live state of the processors in the boot inventory. Index is
// the dense logical CPU number; index 0 is the bootstrap processor. The APIC
// ID is a hardware name and never an index.
type Table struct {
	count  int
	apicID [MaxCPUs]uint32
	state  [MaxCPUs]State
}

// Load fills the table from the inventory. The bootstrap processor is online.
// It refuses an inventory whose first entry is not the bootstrap processor or
// that names one APIC ID twice.
func (table *Table) Load(inventory *Inventory) bool {
	*table = Table{}
	if inventory.CPUCount == 0 || inventory.CPUs[0].Flags&cpuinfo.CPUEntryBootstrap == 0 {
		return false
	}
	count := int(inventory.CPUCount)
	if count > MaxCPUs {
		count = MaxCPUs
	}
	for index := 0; index < count; index++ {
		id := inventory.CPUs[index].APICID
		for other := 0; other < index; other++ {
			if table.apicID[other] == id {
				*table = Table{}
				return false
			}
		}
		table.apicID[index] = id
		if index != 0 && id > MaxXAPICID {
			table.state[index] = Unsupported
		}
	}
	table.count = count
	table.state[0] = Online
	return true
}

// Count returns how many processors the table holds.
func (table *Table) Count() int { return table.count }

// APICID returns the APIC ID of a processor.
func (table *Table) APICID(index int) (uint32, bool) {
	if index < 0 || index >= table.count {
		return 0, false
	}
	return table.apicID[index], true
}

// Index returns the processor with an APIC ID.
func (table *Table) Index(apicID uint32) (int, bool) {
	for index := 0; index < table.count; index++ {
		if table.apicID[index] == apicID {
			return index, true
		}
	}
	return 0, false
}

// State returns where a processor is in its start-up.
func (table *Table) State(index int) State {
	if index < 0 || index >= table.count {
		return Unsupported
	}
	return table.state[index]
}

// Startable reports whether a processor should be sent start-up signals.
func (table *Table) Startable(index int) bool {
	return index > 0 && index < table.count && table.state[index] == Present
}

// Transition moves a processor forward in its start-up. States only advance:
// Present to Started or Failed, Started to Online or Failed.
func (table *Table) Transition(index int, next State) bool {
	if index <= 0 || index >= table.count {
		return false
	}
	current := table.state[index]
	switch {
	case current == Present && (next == Started || next == Failed):
	case current == Started && (next == Online || next == Failed):
	default:
		return false
	}
	table.state[index] = next
	return true
}

// OnlineMask returns the online processors, bit n for index n.
func (table *Table) OnlineMask() uint64 {
	mask := uint64(0)
	for index := 0; index < table.count; index++ {
		if table.state[index] == Online {
			mask |= 1 << uint(index)
		}
	}
	return mask
}

// Publish copies the online state into the inventory the CPU information
// calls report.
func (table *Table) Publish(inventory *Inventory) {
	online := uint32(0)
	for index := 0; index < int(inventory.CPUCount); index++ {
		entry := &inventory.CPUs[index]
		if index < table.count && table.state[index] == Online {
			entry.Flags |= cpuinfo.CPUEntryOnline
			online++
		} else {
			entry.Flags &^= cpuinfo.CPUEntryOnline
		}
	}
	inventory.Summary.OnlineCPUs = online
}
