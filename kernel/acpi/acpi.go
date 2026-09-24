// Package acpi finds what the kernel needs from the firmware's ACPI tables: the
// register and value that put the machine into the soft-off state S5.
//
// The tables come from firmware and are checked as untrusted input: every
// table's signature, length and checksum is verified before a field is read,
// every length is bounded, and nothing is followed that does not lie inside a
// table already checked (ADR 0092).
package acpi

import "encoding/binary"

// Memory reads physical memory. It returns false for a range it cannot read.
type Memory interface {
	Read(address uint64, target []byte) bool
}

// PowerOff is how to enter S5: write Value to the 16-bit I/O port Port.
type PowerOff struct {
	Port  uint16
	Value uint16
}

const (
	// maxTable bounds any table read; QEMU's DSDT is about 8 KiB.
	maxTable = 64 << 10
	// maxEntries bounds the root table's entries.
	maxEntries = 64
	headerSize = 36
	// sleepEnable is SLP_EN in PM1 control; SLP_TYP sits at bits 10-12.
	sleepEnable = 1 << 13
	sleepShift  = 10
)

// buffer holds one table at a time; the kernel's heap never frees.
var buffer [maxTable]byte

// FindPowerOff follows the RSDP at root to the FADT and the DSDT and returns the
// S5 transition, or false when any table is missing or malformed.
func FindPowerOff(memory Memory, root uint64) (PowerOff, bool) {
	fadt, ok := findFADT(memory, root)
	if !ok {
		return PowerOff{}, false
	}
	port, dsdt, ok := parseFADT(fadt)
	if !ok {
		return PowerOff{}, false
	}
	table, ok := readTable(memory, dsdt, "DSDT")
	if !ok {
		return PowerOff{}, false
	}
	sleepType, ok := findS5(table[headerSize:])
	if !ok {
		return PowerOff{}, false
	}
	return PowerOff{Port: port, Value: uint16(sleepType)<<sleepShift | sleepEnable}, true
}

// findFADT validates the RSDP and walks the XSDT, or the RSDT of an ACPI 1.0
// RSDP, to the table signed FACP, which it returns in buffer.
func findFADT(memory Memory, root uint64) ([]byte, bool) {
	var rsdp [36]byte
	if root == 0 || !memory.Read(root, rsdp[:20]) || string(rsdp[:8]) != "RSD PTR " || !checksum(rsdp[:20]) {
		return nil, false
	}
	width, rootTable, signature := uint64(4), uint64(binary.LittleEndian.Uint32(rsdp[16:])), "RSDT"
	if rsdp[15] >= 2 {
		if !memory.Read(root, rsdp[:]) || binary.LittleEndian.Uint32(rsdp[20:]) < 36 || !checksum(rsdp[:36]) {
			return nil, false
		}
		width, rootTable, signature = 8, binary.LittleEndian.Uint64(rsdp[24:]), "XSDT"
	}
	table, ok := readTable(memory, rootTable, signature)
	if !ok {
		return nil, false
	}
	entries := uint64(len(table)-headerSize) / width
	if entries > maxEntries {
		entries = maxEntries
	}
	var addresses [maxEntries]uint64
	for index := uint64(0); index < entries; index++ {
		at := headerSize + index*width
		if width == 8 {
			addresses[index] = binary.LittleEndian.Uint64(table[at:])
		} else {
			addresses[index] = uint64(binary.LittleEndian.Uint32(table[at:]))
		}
	}
	for index := uint64(0); index < entries; index++ {
		if fadt, found := readTable(memory, addresses[index], "FACP"); found {
			return fadt, true
		}
	}
	return nil, false
}

// parseFADT returns the PM1a control port and the DSDT address. The 64-bit
// fields of ACPI 2.0 are preferred when the table is long enough to hold them
// and they are set; the control block must be in I/O space.
func parseFADT(fadt []byte) (uint16, uint64, bool) {
	if len(fadt) < 116 {
		return 0, 0, false
	}
	dsdt := uint64(binary.LittleEndian.Uint32(fadt[40:]))
	port := uint64(binary.LittleEndian.Uint32(fadt[64:]))
	if len(fadt) >= 148 {
		if extended := binary.LittleEndian.Uint64(fadt[140:]); extended != 0 {
			dsdt = extended
		}
	}
	if len(fadt) >= 184 {
		// X_PM1a_CNT_BLK is a generic address: space 1 is system I/O.
		if extended := binary.LittleEndian.Uint64(fadt[176:]); extended != 0 && fadt[172] == 1 {
			port = extended
		}
	}
	if port == 0 || port > 0xffff || dsdt == 0 {
		return 0, 0, false
	}
	return uint16(port), dsdt, true
}

// readTable reads the table at address into buffer, requiring its signature,
// a length between the header and maxTable, and a zero checksum.
func readTable(memory Memory, address uint64, signature string) ([]byte, bool) {
	if address == 0 || address+headerSize < address {
		return nil, false
	}
	if !memory.Read(address, buffer[:headerSize]) || string(buffer[:4]) != signature {
		return nil, false
	}
	length := uint64(binary.LittleEndian.Uint32(buffer[4:]))
	if length < headerSize || length > maxTable || address+length < address {
		return nil, false
	}
	table := buffer[:length]
	if !memory.Read(address, table) || !checksum(table) {
		return nil, false
	}
	return table, true
}

func checksum(data []byte) bool {
	var sum byte
	for _, value := range data {
		sum += value
	}
	return sum == 0
}

// findS5 finds the \_S5_ package in the AML of a DSDT and returns its first
// element, SLP_TYPa. Only the encodings firmware uses for a named package of
// small integers are accepted; anything else is refused rather than guessed.
func findS5(aml []byte) (uint8, bool) {
	for index := 1; index+4 < len(aml); index++ {
		if string(aml[index:index+4]) != "_S5_" {
			continue
		}
		// NameOp, optionally followed by the root prefix.
		if aml[index-1] != 0x08 && !(aml[index-1] == '\\' && index >= 2 && aml[index-2] == 0x08) {
			continue
		}
		at := index + 4
		if at >= len(aml) || aml[at] != 0x12 {
			continue
		}
		at++
		// PkgLength: the top two bits of the lead byte count the bytes after it.
		if at >= len(aml) {
			return 0, false
		}
		at += 1 + int(aml[at]>>6)
		// NumElements, then the first element.
		at++
		if at >= len(aml) {
			return 0, false
		}
		switch aml[at] {
		case 0x00:
			return 0, true
		case 0x01:
			return 1, true
		case 0x0a, 0x0b:
			if at+1 >= len(aml) {
				return 0, false
			}
			return aml[at+1] & 0x7, true
		}
		return 0, false
	}
	return 0, false
}
