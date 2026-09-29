package main

import (
	"testing"

	"goos/kernel/boot"
	linuxelf "goos/kernel/compat/linux/elf"
)

func TestArchiveSizeIsBoundedByTheKernelLoader(t *testing.T) {
	if err := checkSize(int64(boot.MaxInitramfs)); err != nil {
		t.Fatalf("an archive at the bound was refused: %v", err)
	}
	if checkSize(int64(boot.MaxInitramfs)+1) == nil {
		t.Fatal("an archive over the bound was accepted")
	}
	if checkSize(0) == nil {
		t.Fatal("an empty archive was accepted")
	}
}

func TestLinuxProbeIsAcceptedStaticELF(t *testing.T) {
	fixture := linuxProbe()
	image, failure := linuxelf.Parse(fixture, linuxelf.GoOSConfig())
	if failure != linuxelf.OK || image.Entry != 0x4000b0 || image.SegmentCount != 2 {
		t.Fatalf("probe ELF: failure=%d image=%#v", failure, image)
	}
	if len(fixture) != 0x1000 || image.Segments[0].FileSize != 0x814 || image.Segments[1].MemorySize != 4096 {
		t.Fatalf("probe layout: size=%#x image=%#v", len(fixture), image)
	}
}
