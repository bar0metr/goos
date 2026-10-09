package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goos/kernel/ramfs"
	"goos/userspace/fat32"
)

func TestAutoSizeMiB(t *testing.T) {
	if size, err := autoSizeMiB(nil, 512, 8, 2, "none"); err != nil || size != 512 {
		t.Fatalf("empty tree: size=%d, err=%v", size, err)
	}
	root := t.TempDir()
	large := filepath.Join(root, "large")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1400 * int64(mib)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	puts := []string{large + ":/apps/large"}
	size, err := autoSizeMiB(puts, 512, 8, 2, "none")
	if err != nil {
		t.Fatal(err)
	}
	if size <= 1400 || size*mib < 1400*mib*105/100 {
		t.Fatalf("size = %d MiB for 1400 MiB of files", size)
	}
	for index := range 2000 {
		puts = append(puts, ":/apps/tools/dir"+strings.Repeat("x", index%8)+"/nested"+strings.Repeat("y", index/8))
	}
	withDirectories, err := autoSizeMiB(puts, 512, 8, 2, "none")
	if err != nil {
		t.Fatal(err)
	}
	if withDirectories <= size {
		t.Fatalf("directory entries did not increase size: %d -> %d MiB", size, withDirectories)
	}

	image := filepath.Join(root, "disk.img")
	create(image, size, "none", 512, 8, 2)
	disk, err := os.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	device := fileDevice{file: disk, size: size * mib}
	target, err := volume(device)
	if err != nil {
		t.Fatal(err)
	}
	var fs fat32.FileSystem
	if _, ok := fs.Mount(target); !ok {
		t.Fatal("automatically sized disk did not mount")
	}
	free, status := fs.FreeClusters()
	if status != ramfs.StatusOK || uint64(free) < ceilDiv(1400*mib, 4096)*105/100 {
		t.Fatalf("free clusters = %d, status=%v", free, status)
	}
}

func TestAutoSizeRejectsOversizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "too-large")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(1 << 32); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := autoSizeMiB([]string{path + ":/apps/too-large"}, 512, 8, 2, "none"); err == nil {
		t.Fatal("FAT32 file limit was not checked")
	}
}
