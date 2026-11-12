package ext4vol

import (
	"os"
	"path/filepath"
	"testing"

	"goos/kernel/ramfs"
)

// TestGuestImages checks the partitions tests/boot/ext4-rw.sh copied out of
// its guest, in the directory GOOS_EXT4_GUEST names: clean.img was unmounted
// by GoOS and must mount read-only; dirty.img was cut off while mounted and
// must be refused read-only, then recovered by a writable mount of a copy.
func TestGuestImages(t *testing.T) {
	dir := os.Getenv("GOOS_EXT4_GUEST")
	if dir == "" {
		t.Skip("GOOS_EXT4_GUEST is not set")
	}
	load := func(name string) *writableDevice {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return &writableDevice{memDevice: memDevice{data: data}}
	}
	expect := func(volume *Volume, path []string, want string) {
		t.Helper()
		node, status := volume.Root()
		must(t, status)
		for _, component := range path {
			node, status = volume.OpenName(node, name(t, component))
			if status != ramfs.StatusOK {
				t.Fatalf("%v: %d", path, status)
			}
		}
		if want != "" {
			if got := readAll(t, volume, node); got != want {
				t.Fatalf("%v: %q, want %q", path, got, want)
			}
		}
	}

	var clean Volume
	if !clean.Mount(&load("clean.img").memDevice) {
		t.Fatalf("clean image refused: %s", clean.Refusal())
	}
	expect(&clean, []string{"moved.txt"}, "payload\n")
	expect(&clean, []string{"made"}, "")
	root, _ := clean.Root()
	if _, status := clean.OpenName(root, name(t, "hello.txt")); status != ramfs.StatusNotFound {
		t.Fatalf("unlinked name: %d", status)
	}

	dirty := load("dirty.img")
	var readOnly Volume
	if readOnly.Mount(&dirty.memDevice) {
		t.Fatal("a volume cut off while mounted passed as clean")
	}
	var recovered Volume
	if !recovered.MountWritable(dirty, clock()) {
		t.Fatalf("recovery: %s", recovered.Refusal())
	}
	expect(&recovered, []string{"journal.txt"}, "before-fault\n")
	expect(&recovered, []string{"second.txt"}, "after-fault\n")
	must(t, recovered.Unmount())
}
