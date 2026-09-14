// iso-uefi validates and converts hdiutil's El Torito boot catalog to UEFI.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	sectorSize       = 2048
	bootRecordSector = 17
	bootCatalogLBAAt = bootRecordSector*sectorSize + 71
	uefiPlatform     = 0xef
)

func patch(image []byte) error {
	if len(image) < (bootRecordSector+1)*sectorSize {
		return errors.New("ISO is too small")
	}
	record := image[bootRecordSector*sectorSize:]
	if record[0] != 0 || string(record[1:6]) != "CD001" || string(record[7:30]) != "EL TORITO SPECIFICATION" {
		return errors.New("El Torito boot record not found")
	}
	catalogOffset := int(binary.LittleEndian.Uint32(image[bootCatalogLBAAt:])) * sectorSize
	if catalogOffset < 0 || catalogOffset+64 > len(image) {
		return errors.New("boot catalog is outside ISO")
	}
	catalog := image[catalogOffset : catalogOffset+64]
	if catalog[0] != 1 || catalog[30] != 0x55 || catalog[31] != 0xaa || catalog[32] != 0x88 {
		return errors.New("invalid El Torito validation or boot entry")
	}

	catalog[1] = uefiPlatform
	catalog[28], catalog[29] = 0, 0
	var sum uint32
	for offset := 0; offset < 32; offset += 2 {
		sum += uint32(binary.LittleEndian.Uint16(catalog[offset:]))
	}
	binary.LittleEndian.PutUint16(catalog[28:], uint16(-sum))

	// The EFI boot image is an 8 MiB FAT volume. El Torito counts virtual
	// sectors in 512-byte units; 0x4000 describes the complete image.
	binary.LittleEndian.PutUint16(catalog[38:], 0x4000)
	return nil
}

func run(path string) error {
	image, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := patch(image); err != nil {
		return err
	}
	return os.WriteFile(path, image, 0o644)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: iso-uefi <image.iso>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "iso-uefi: %v\n", err)
		os.Exit(1)
	}
}
