package main

import (
	"testing"

	"goos/kernel/boot"
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
