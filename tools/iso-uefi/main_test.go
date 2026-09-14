package main

import (
	"encoding/binary"
	"testing"
)

func TestPatchMarksCatalogAsUEFI(t *testing.T) {
	image := make([]byte, 24*sectorSize)
	record := image[bootRecordSector*sectorSize:]
	record[0] = 0
	copy(record[1:6], "CD001")
	copy(record[7:30], "EL TORITO SPECIFICATION")
	binary.LittleEndian.PutUint32(image[bootCatalogLBAAt:], 20)
	catalog := image[20*sectorSize:]
	catalog[0], catalog[30], catalog[31], catalog[32] = 1, 0x55, 0xaa, 0x88

	if err := patch(image); err != nil {
		t.Fatal(err)
	}
	if catalog[1] != uefiPlatform {
		t.Fatalf("platform = %#x", catalog[1])
	}
	if got := binary.LittleEndian.Uint16(catalog[38:]); got != 0x4000 {
		t.Fatalf("sector count = %#x", got)
	}
	var sum uint16
	for offset := 0; offset < 32; offset += 2 {
		sum += binary.LittleEndian.Uint16(catalog[offset:])
	}
	if sum != 0 {
		t.Fatalf("validation checksum = %#x", sum)
	}
}

func TestPatchRejectsMalformedISO(t *testing.T) {
	if err := patch(make([]byte, 18*sectorSize)); err == nil {
		t.Fatal("malformed image accepted")
	}
}
