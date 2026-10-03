package cpu

import (
	"goos/internal/cpuinfo"
	"goos/kernel/acpi"
	"goos/kernel/cpu/x86"
	"testing"
)

func testProbe() x86.Inventory {
	var probe x86.Inventory
	probe.Identity.Vendor.Length = uint8(copy(probe.Identity.Vendor.Bytes[:], "GenuineIntel"))
	probe.Identity.Family, probe.Identity.Model, probe.Identity.Stepping = 6, 85, 7
	probe.Identity.BootstrapAPICID = 2
	probe.Topology = x86.Topology{ThreadsPerCore: 2, LogicalCPUsPerPackage: 4, CoresPerPackage: 2, SMTShift: 1, PackageShift: 2, BootstrapX2APICID: 2, Valid: true}
	return probe
}

func TestNormalizeReordersBootstrapAndMarksOnlyItOnline(t *testing.T) {
	processors := acpi.Processors{Count: 4}
	for index, id := range []uint32{0, 1, 2, 3} {
		processors.Entries[index] = acpi.Processor{APICID: id, UID: id + 10}
	}
	var inventory Inventory
	probe := testProbe()
	Normalize(&inventory, &probe, &processors, true)
	if inventory.CPUCount != 4 || inventory.CPUs[0].APICID != 2 || inventory.Summary.OnlineCPUs != 1 {
		t.Fatalf("inventory: %+v", inventory)
	}
	if inventory.CPUs[0].Flags&(cpuinfo.CPUEntryBootstrap|cpuinfo.CPUEntryOnline) == 0 || inventory.CPUs[1].Flags&cpuinfo.CPUEntryOnline != 0 {
		t.Fatalf("flags: %#x %#x", inventory.CPUs[0].Flags, inventory.CPUs[1].Flags)
	}
	if inventory.Summary.Validity&cpuinfo.ValidTopology == 0 || inventory.Summary.Sockets != 1 {
		t.Fatalf("topology: %+v", inventory.Summary)
	}
}

func TestNormalizeFallsBackWhenFirmwareMissesBootstrap(t *testing.T) {
	processors := acpi.Processors{Count: 1, Entries: [acpi.MaxProcessors]acpi.Processor{{APICID: 9}}}
	var inventory Inventory
	probe := testProbe()
	Normalize(&inventory, &probe, &processors, true)
	if inventory.CPUCount != 1 || inventory.CPUs[0].APICID != 2 || inventory.CPUs[0].Flags&cpuinfo.CPUEntryACPIUIDValid != 0 {
		t.Fatalf("fallback: %+v", inventory)
	}
}

func TestNormalizeClearsInexactTopology(t *testing.T) {
	processors := acpi.Processors{Count: 2, Entries: [acpi.MaxProcessors]acpi.Processor{{APICID: 2}, {APICID: 3}}}
	var inventory Inventory
	probe := testProbe()
	Normalize(&inventory, &probe, &processors, true)
	if inventory.Summary.Validity&cpuinfo.ValidTopology != 0 {
		t.Fatalf("accepted partial package: %+v", inventory.Summary)
	}
}
