package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

var testStamp = time.Unix(1700000000, 0).UTC()

func testFiles() map[string][]byte {
	kernel := bytes.Repeat([]byte("kernel"), 50000)
	initramfs := bytes.Repeat([]byte{1, 2, 3}, 70000)
	return map[string][]byte{kernelPath: kernel, initramfsPath: initramfs}
}

// fatLookup reads a file back from a FAT16 image by walking its directories
// the way a firmware driver does, matching long names case-insensitively.
func fatLookup(t *testing.T, image []byte, path string) []byte {
	t.Helper()
	sectors := int(binary.LittleEndian.Uint16(image[22:]))
	perCluster := int(image[13])
	fatOffset := int(binary.LittleEndian.Uint16(image[14:])) * 512
	rootOffset := fatOffset + int(image[16])*sectors*512
	rootSize := int(binary.LittleEndian.Uint16(image[17:])) * 32
	dataOffset := rootOffset + rootSize
	chain := func(cluster uint16) []byte {
		var out []byte
		for cluster >= 2 && cluster < 0xfff8 {
			at := dataOffset + (int(cluster)-2)*perCluster*512
			out = append(out, image[at:at+perCluster*512]...)
			cluster = binary.LittleEndian.Uint16(image[fatOffset+int(cluster)*2:])
		}
		return out
	}
	dir := image[rootOffset : rootOffset+rootSize]
	parts := strings.Split(path, "/")
	for index, part := range parts {
		var long []uint16
		var longSum byte
		found := false
		for at := 0; at+32 <= len(dir) && dir[at] != 0; at += 32 {
			entry := dir[at : at+32]
			if entry[11] == attrLongName {
				var units []uint16
				for _, offset := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
					units = append(units, binary.LittleEndian.Uint16(entry[offset:]))
				}
				long = append(units, long...)
				longSum = entry[13]
				continue
			}
			name := strings.TrimSpace(string(entry[0:8]))
			if ext := strings.TrimSpace(string(entry[8:11])); ext != "" {
				name += "." + ext
			}
			if long != nil {
				end := len(long)
				for i, u := range long {
					if u == 0 {
						end = i
						break
					}
				}
				var short [11]byte
				copy(short[:], entry[0:11])
				if longSum != shortChecksum(short) {
					t.Fatalf("%s: long name checksum does not match %q", path, short[:])
				}
				name = string(utf16.Decode(long[:end]))
				long = nil
			}
			if !strings.EqualFold(name, part) {
				continue
			}
			found = true
			data := chain(binary.LittleEndian.Uint16(entry[26:]))
			if index == len(parts)-1 {
				return data[:binary.LittleEndian.Uint32(entry[28:])]
			}
			dir = data
			break
		}
		if !found {
			t.Fatalf("%s: %s not found", path, part)
		}
	}
	return nil
}

func TestFATHoldsFilesUnderTheirNames(t *testing.T) {
	files := testFiles()
	image, err := buildFAT(files, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(image) != fatImageSize || image[510] != 0x55 || image[511] != 0xaa {
		t.Fatal("not an 8 MiB volume with a boot signature")
	}
	if string(image[54:62]) != "FAT16   " {
		t.Fatalf("file system type = %q", image[54:62])
	}
	if _, clusters := fatGeometry(); clusters < 4085 || clusters > 65524 {
		t.Fatalf("%d clusters is outside the FAT16 range", clusters)
	}
	// The kernel opens \initramfs.tar, which only a long name spells.
	if got := fatLookup(t, image, "initramfs.tar"); !bytes.Equal(got, files[initramfsPath]) {
		t.Fatal("initramfs.tar contents differ")
	}
	if got := fatLookup(t, image, "efi/boot/bootx64.efi"); !bytes.Equal(got, files[kernelPath]) {
		t.Fatal("BOOTX64.EFI contents differ")
	}
}

func TestLongNameChecksumMatchesShortEntry(t *testing.T) {
	short, exact := shortName("initramfs.tar", map[[11]byte]bool{})
	if exact || string(short[:]) != "INITRA~1TAR" {
		t.Fatalf("short name = %q exact=%v", short[:], exact)
	}
	if _, exact := shortName("BOOTX64.EFI", map[[11]byte]bool{}); !exact {
		t.Fatal("an 8.3 upper-case name needs no long name")
	}
	entries := make([]byte, 64)
	putLongName(entries, "initramfs.tar", short)
	if entries[0] != 0x41 || entries[11] != attrLongName || entries[13] != shortChecksum(short) {
		t.Fatalf("long-name entry = % x", entries[:32])
	}
}

func TestImagesAreDeterministic(t *testing.T) {
	first, err := buildFAT(testFiles(), testStamp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildFAT(testFiles(), testStamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two FAT images from the same input differ")
	}
	isoFiles := testFiles()
	isoFiles[bootImagePath] = first
	a, err := buildISO(isoFiles, bootImagePath, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildISO(isoFiles, bootImagePath, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two ISOs from the same input differ")
	}
}

func TestISOBootsTheEFIPartitionOnUEFI(t *testing.T) {
	esp, err := buildFAT(testFiles(), testStamp)
	if err != nil {
		t.Fatal(err)
	}
	files := testFiles()
	files[bootImagePath] = esp
	image, err := buildISO(files, bootImagePath, testStamp)
	if err != nil {
		t.Fatal(err)
	}
	pvd := image[isoPVDSector*isoSectorSize:]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatal("primary volume descriptor missing")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:]); int(got)*isoSectorSize != len(image) {
		t.Fatalf("volume size %d sectors, image %d bytes", got, len(image))
	}
	if string(pvd[813:827]) != "20231114221320" {
		t.Fatalf("creation time = %q", pvd[813:830])
	}
	record := image[isoBootSector*isoSectorSize:]
	if record[0] != 0 || string(record[1:6]) != "CD001" || string(record[7:30]) != "EL TORITO SPECIFICATION" {
		t.Fatal("El Torito boot record missing")
	}
	catalog := image[int(binary.LittleEndian.Uint32(record[71:]))*isoSectorSize:]
	if catalog[0] != 1 || catalog[1] != uefiPlatform || catalog[30] != 0x55 || catalog[31] != 0xaa {
		t.Fatalf("validation entry = % x", catalog[:32])
	}
	var sum uint16
	for offset := 0; offset < 32; offset += 2 {
		sum += binary.LittleEndian.Uint16(catalog[offset:])
	}
	if sum != 0 {
		t.Fatalf("validation checksum = %#x", sum)
	}
	if catalog[32] != 0x88 || catalog[33] != 0 {
		t.Fatal("default entry is not a bootable no-emulation entry")
	}
	if got := binary.LittleEndian.Uint16(catalog[38:]); got != 0x4000 {
		t.Fatalf("sector count = %#x", got)
	}
	at := int(binary.LittleEndian.Uint32(catalog[40:])) * isoSectorSize
	if !bytes.Equal(image[at:at+fatImageSize], esp) {
		t.Fatal("boot entry does not point at the EFI system partition")
	}
}

func TestISORejectsWrongBootImage(t *testing.T) {
	files := testFiles()
	files[bootImagePath] = make([]byte, 512)
	if _, err := buildISO(files, bootImagePath, testStamp); err == nil {
		t.Fatal("a truncated boot image was accepted")
	}
}

func TestSourceDate(t *testing.T) {
	if got, err := sourceDate(""); err != nil || !got.Equal(time.Unix(0, 0)) {
		t.Fatalf("unset = %v, %v", got, err)
	}
	if _, err := sourceDate("yesterday"); err == nil {
		t.Fatal("a non-numeric SOURCE_DATE_EPOCH was accepted")
	}
	if date, clock := fatTimestamp(time.Unix(0, 0)); date != 0x21 || clock != 0 {
		t.Fatalf("1970 clamps to 1980-01-01, got %#x %#x", date, clock)
	}
}
