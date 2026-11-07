package main

import (
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"goos/userspace/fat32"
)

const testVariant = "HZ=250;DIAG=0;VALIDATION=0;VFS_PROBE=0;VFS_RUNTIME=1;OPT=2"

func TestUpdateNoSpacePreservesImage(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	input := filepath.Join(dir, "input")
	large := filepath.Join(dir, "large")
	create(disk, 40, "none", 512, 1, 2)
	if err := os.WriteFile(input, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateImage(disk, testVariant, true, []string{input + ":/system/bin/file"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(41 << 20); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	hash := func() [32]byte {
		f, err := os.Open(disk)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			t.Fatal(err)
		}
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		return sum
	}
	before := hash()
	other := "HZ=100;DIAG=0;VALIDATION=0;VFS_PROBE=0;VFS_RUNTIME=1;OPT=2"
	if err := updateImage(disk, other, true, []string{input + ":/system/bin/another", large + ":/system/bin/file"}); err == nil {
		t.Fatal("oversized update accepted")
	}
	if after := hash(); after != before {
		t.Fatal("failed update changed disk")
	}
	if err := updateImage(disk, testVariant, false, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateImage(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	input := filepath.Join(dir, "input")
	if err := os.WriteFile(input, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateImage(disk, testVariant, true, nil); err == nil {
		t.Fatal("missing disk accepted")
	}
	if err := os.WriteFile(disk, []byte("not FAT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateImage(disk, testVariant, true, nil); err == nil {
		t.Fatal("invalid disk accepted")
	}
	os.Remove(disk)
	create(disk, 300, "none", 512, 8, 2)
	before, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	vol, err := volume(fileDevice{file: seed, size: uint64(before.Size())})
	if err != nil {
		t.Fatal(err)
	}
	var seeded fat32.FileSystem
	seedRoot, ok := seeded.Mount(vol)
	if !ok {
		t.Fatal("mount seed")
	}
	for _, path := range []string{"/home/user/file", "/state/data"} {
		if err := putFile(&seeded, seedRoot, input, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := updateImage(disk, testVariant, false, nil); err == nil {
		t.Fatal("unassociated disk accepted")
	}
	if err := updateImage(disk, testVariant, true, []string{input + ":/system/bin/one"}); err != nil {
		t.Fatal(err)
	}
	other := "HZ=100;DIAG=0;VALIDATION=0;VFS_PROBE=0;VFS_RUNTIME=1;OPT=2"
	if err := updateImage(disk, other, false, []string{input + ":/system/bin/two"}); err == nil {
		t.Fatal("wrong variant accepted")
	}
	for _, target := range []string{"/home/user/file", "/HOME/user/file", "/State/file"} {
		if err := updateImage(disk, testVariant, false, []string{input + ":" + target}); err == nil {
			t.Fatalf("protected path %s accepted", target)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, name := range []string{"two", "three", "four", "five"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- updateImage(disk, testVariant, false, []string{input + ":/system/bin/" + name})
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	after, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("disk inode changed")
	}
	vol, err = volume(fileDevice{file: f, size: uint64(after.Size())})
	if err != nil {
		t.Fatal(err)
	}
	var fs fat32.FileSystem
	root, ok := fs.Mount(vol)
	if !ok {
		t.Fatal("mount")
	}
	for _, path := range []string{"/home/user/file", "/state/data"} {
		out := filepath.Join(dir, "protected")
		if err := getFile(&fs, root, path, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil || string(data) != "hello" {
			t.Fatalf("%s: %q %v", path, data, err)
		}
	}
	for _, name := range []string{"one", "two", "three", "four", "five"} {
		out := filepath.Join(dir, "out")
		if err := getFile(&fs, root, "/system/bin/"+name, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil || string(data) != "hello" {
			t.Fatalf("%s: %q %v", name, data, err)
		}
	}
}
