//go:build uefi && amd64

package uefi

import (
	deviceuefi "device/uefi"
	"goos/kernel/boot"
	"unsafe"
)

// The configuration-table GUIDs of the ACPI 2.0 and ACPI 1.0 RSDP.
var (
	acpi20GUID = deviceuefi.EFI_GUID{Data1: 0x8868e871, Data2: 0xe4f1, Data3: 0x11d3, Data4: [8]byte{0xbc, 0x22, 0x00, 0x80, 0xc7, 0x3c, 0x88, 0x81}}
	acpi10GUID = deviceuefi.EFI_GUID{Data1: 0xeb9d2d30, Data2: 0x2d88, Data3: 0x11d3, Data4: [8]byte{0x9a, 0x16, 0x00, 0x90, 0x27, 0x3f, 0xc1, 0x4d}}
)

// configurationTable mirrors EFI_CONFIGURATION_TABLE.
type configurationTable struct {
	guid  deviceuefi.EFI_GUID
	table uintptr
}

// maxConfigurationTables bounds the firmware's table count.
const maxConfigurationTables = 64

// FindACPIRoot records the address of the RSDP the firmware publishes,
// preferring ACPI 2.0. It must run before ExitBootServices, while the system
// table is valid. Nothing is recorded when there is none.
func FindACPIRoot(info *boot.Info) {
	system := deviceuefi.ST()
	count := int(system.NumberOfTableEntries)
	if system.ConfigurationTable == nil || count <= 0 {
		return
	}
	if count > maxConfigurationTables {
		count = maxConfigurationTables
	}
	// SAFETY: the system table's configuration table is an array of count
	// EFI_CONFIGURATION_TABLE entries that firmware keeps valid until
	// ExitBootServices; count is bounded above.
	tables := unsafe.Slice((*configurationTable)(unsafe.Pointer(system.ConfigurationTable)), count)
	for _, entry := range tables {
		if entry.guid == acpi20GUID {
			info.ACPIRoot = uint64(entry.table)
			return
		}
		if entry.guid == acpi10GUID {
			info.ACPIRoot = uint64(entry.table)
		}
	}
}

// PhysicalMemory reads physical memory through the identity mapping, below
// limit only. It is used to read firmware tables at boot.
type PhysicalMemory struct {
	Limit uint64
}

func (memory PhysicalMemory) Read(address uint64, target []byte) bool {
	length := uint64(len(target))
	if address == 0 || address+length < address || address+length > memory.Limit {
		return false
	}
	// SAFETY: [address, address+length) lies below Limit, which the caller
	// sets to the range firmware and the kernel identity-map, and firmware
	// tables in it are only read.
	copy(target, unsafe.Slice((*byte)(unsafe.Pointer(uintptr(address))), len(target)))
	return true
}
