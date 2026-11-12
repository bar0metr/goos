package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"goos/kernel/ramfs"
	"goos/userspace/fsdetect"
	"goos/userspace/partition"
	"goos/userspace/volume"
)

// TestComposedDiskSelectsAndProbes builds a disk the way the volume boot
// test does and resolves each source the mount command accepts through the
// partition scanner and the probe, as the block service and PID 1 do.
func TestComposedDiskSelectsAndProbes(t *testing.T) {
	output := filepath.Join(t.TempDir(), "disk.img")
	command := exec.Command("go", "run", ".",
		"-part", "goos-ext4-a:linux:../../userspace/ext4/testdata/canonical.img.gz",
		"-part", "goos-ext4-b:linux:../../userspace/ext4/testdata/blockmap-1k.img.gz",
		"-part", "goos-junk:data:zero:2", output)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("diskcompose: %v\n%s", err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var scanner partition.Scanner
	var table partition.Table
	if status := scanner.Scan(imageReader{data}, uint64(len(data)), 512, &table); status != partition.StatusOK || table.Count != 3 {
		t.Fatalf("scan: %v, %d partitions", status, table.Count)
	}
	for _, tc := range []struct {
		source string
		want   fsdetect.Status
		kind   ramfs.FilesystemType
	}{
		{"name:goos-ext4-a", fsdetect.Recognized, ramfs.FilesystemExt4},
		{"part:2", fsdetect.Recognized, ramfs.FilesystemExt4},
		{"name:goos-junk", fsdetect.Unknown, 0},
	} {
		selector, ok := volume.ParseSource([]byte(tc.source))
		if !ok {
			t.Fatalf("%s: not a source", tc.source)
		}
		chosen, status := partition.Select(&table, &selector)
		if status != partition.StatusOK {
			t.Fatalf("%s: select %v", tc.source, status)
		}
		slice := data[chosen.Start : chosen.Start+chosen.Length]
		result := fsdetect.Probe(slice[:fsdetect.Window], chosen.Length)
		if result.Status != tc.want || result.Type != tc.kind {
			t.Fatalf("%s: %+v", tc.source, result)
		}
	}
}

func TestExtractPartition(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	if out, err := exec.Command("go", "run", ".", "-part", "a:linux:zero:1",
		"-part", "b:linux:../../userspace/ext4/testdata/canonical.img.gz", disk).CombinedOutput(); err != nil {
		t.Fatalf("compose: %v\n%s", err, out)
	}
	extracted := filepath.Join(dir, "b.img")
	if out, err := exec.Command("go", "run", ".", "-extract", "b", disk, extracted).CombinedOutput(); err != nil {
		t.Fatalf("extract: %v\n%s", err, out)
	}
	data, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	if result := fsdetect.Probe(data[:fsdetect.Window], uint64(len(data))); result.Type != ramfs.FilesystemExt4 {
		t.Fatalf("extracted partition probes as %+v", result)
	}
	if err := exec.Command("go", "run", ".", "-extract", "missing", disk, extracted).Run(); err == nil {
		t.Fatal("extracted a missing partition")
	}
}
