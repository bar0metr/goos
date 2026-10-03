// Package cpu normalizes architectural and firmware CPU discovery into the
// immutable records exported by the native CPU information ABI.
package cpu

import (
	"goos/internal/cpuinfo"
	"goos/kernel/acpi"
	"goos/kernel/cpu/x86"
)

type Inventory struct {
	Summary    cpuinfo.Summary
	CPUs       [acpi.MaxProcessors]cpuinfo.CPUEntry
	CPUCount   uint16
	Caches     [cpuinfo.MaxCaches]cpuinfo.CacheInfo
	CacheCount uint8
}

// Normalize combines CPUID facts with the enabled processors in the MADT.
// Missing or inconsistent firmware data produces the conservative one-CPU
// inventory rather than preventing boot.
//
//go:noinline
func Normalize(inventory *Inventory, probe *x86.Inventory, processors *acpi.Processors, firmwareValid bool) {
	*inventory = Inventory{}
	bspID := probe.Identity.BootstrapAPICID
	if probe.Topology.Valid {
		bspID = probe.Topology.BootstrapX2APICID
	}
	if !firmwareValid || processors.Count == 0 || !moveBootstrapFirst(processors, bspID) {
		*processors = acpi.Processors{Count: 1}
		processors.Entries[0] = acpi.Processor{APICID: bspID}
		firmwareValid = false
	}

	inventory.CPUCount = processors.Count
	summary := &inventory.Summary
	summary.Validity = cpuinfo.ValidIdentity
	summary.PresentLogicalCPUs = uint32(processors.Count)
	summary.OnlineCPUs = 1
	summary.Family, summary.Model, summary.Stepping = probe.Identity.Family, probe.Identity.Model, probe.Identity.Stepping
	summary.OperatingModes = cpuinfo.Mode32 | cpuinfo.Mode64
	summary.ByteOrder = cpuinfo.ByteOrderLittle
	summary.BootstrapAPICID = bspID
	summary.MaximumBasicLeaf, summary.MaximumExtendedLeaf = probe.Identity.MaximumBasicLeaf, probe.Identity.MaximumExtendedLeaf
	copy(summary.Vendor[:], probe.Identity.Vendor.Bytes[:probe.Identity.Vendor.Length])
	copy(summary.Brand[:], probe.Identity.Brand.Bytes[:probe.Identity.Brand.Length])
	copy(summary.HypervisorVendor[:], probe.Identity.HypervisorVendor.Bytes[:probe.Identity.HypervisorVendor.Length])
	summary.HardwareFeatures, summary.UsableFeatures = probe.Hardware, probe.Usable
	if probe.Identity.AddressWidthsValid {
		summary.Validity |= cpuinfo.ValidAddressWidths
		summary.PhysicalAddressBits, summary.LinearAddressBits = probe.Identity.PhysicalAddressBits, probe.Identity.LinearAddressBits
	}
	if probe.Identity.FrequencyValid {
		summary.Validity |= cpuinfo.ValidFrequency
		summary.BaseMHz, summary.MaximumMHz, summary.BusMHz = probe.Identity.BaseMHz, probe.Identity.MaximumMHz, probe.Identity.BusMHz
	}
	if probe.Identity.HypervisorPresent {
		summary.Validity |= cpuinfo.ValidHypervisor
	}

	for index := uint16(0); index < processors.Count; index++ {
		processor := processors.Entries[index]
		entry := &inventory.CPUs[index]
		entry.LogicalID, entry.APICID = uint32(index), processor.APICID
		entry.Flags = cpuinfo.CPUEntryPresent
		if index == 0 {
			entry.Flags |= cpuinfo.CPUEntryOnline | cpuinfo.CPUEntryBootstrap
		}
		if firmwareValid {
			entry.ACPIUID = processor.UID
			entry.Flags |= cpuinfo.CPUEntryACPIUIDValid
		}
		if processor.X2APIC {
			entry.Flags |= cpuinfo.CPUEntryX2APIC
		}
	}
	applyTopology(inventory, probe.Topology)

	for index := uint8(0); index < probe.CacheCount; index++ {
		cache := probe.Caches[index]
		inventory.Caches[index] = cpuinfo.CacheInfo{
			Validity: cpuinfo.CacheGeometryValid, Level: cache.Level, Type: cache.Type,
			LineSize: cache.LineSize, Partitions: cache.Partitions, Ways: cache.Ways,
			Sets: cache.Sets, SharingCount: cache.SharingCount, Size: cache.Size,
		}
	}
	inventory.CacheCount = probe.CacheCount
}

func moveBootstrapFirst(processors *acpi.Processors, bspID uint32) bool {
	for index := uint16(0); index < processors.Count; index++ {
		if processors.Entries[index].APICID == bspID {
			processors.Entries[0], processors.Entries[index] = processors.Entries[index], processors.Entries[0]
			return true
		}
	}
	return false
}

func applyTopology(inventory *Inventory, topology x86.Topology) {
	if !topology.Valid || topology.LogicalCPUsPerPackage == 0 {
		return
	}
	var packages [acpi.MaxProcessors]uint32
	var packageCounts [acpi.MaxProcessors]uint16
	var packageCount uint16
	for index := uint16(0); index < inventory.CPUCount; index++ {
		ids, ok := topology.ID(inventory.CPUs[index].APICID)
		if !ok {
			return
		}
		for other := uint16(0); other < index; other++ {
			if inventory.CPUs[other].APICID == inventory.CPUs[index].APICID ||
				(inventory.CPUs[other].PackageID == ids.Package && inventory.CPUs[other].CoreID == ids.Core && inventory.CPUs[other].ThreadID == ids.Thread) {
				return
			}
		}
		inventory.CPUs[index].PackageID, inventory.CPUs[index].CoreID, inventory.CPUs[index].ThreadID = ids.Package, ids.Core, ids.Thread
		found := false
		for packageIndex := uint16(0); packageIndex < packageCount; packageIndex++ {
			if packages[packageIndex] == ids.Package {
				packageCounts[packageIndex]++
				found = true
				break
			}
		}
		if !found {
			packages[packageCount], packageCounts[packageCount] = ids.Package, 1
			packageCount++
		}
	}
	for index := uint16(0); index < packageCount; index++ {
		if packageCounts[index] != topology.LogicalCPUsPerPackage {
			return
		}
	}
	for index := uint16(0); index < inventory.CPUCount; index++ {
		inventory.CPUs[index].Flags |= cpuinfo.CPUEntryTopologyValid
	}
	inventory.Summary.Validity |= cpuinfo.ValidTopology
	inventory.Summary.ThreadsPerCore = topology.ThreadsPerCore
	inventory.Summary.CoresPerSocket = topology.CoresPerPackage
	inventory.Summary.Sockets = packageCount
	inventory.Summary.LogicalCPUsPerPackage = topology.LogicalCPUsPerPackage
}
