package fsdisplay

import (
	"goos/kernel/ramfs"
	"testing"
)

func TestAppendSize(t *testing.T) {
	for _, test := range []struct {
		size      uint64
		available bool
		want      string
	}{
		{0, false, "-"},
		{0, true, "0B"},
		{1023, true, "1023B"},
		{1024, true, "1.0K"},
		{1536, true, "1.5K"},
		{32 << 30, true, "32.0G"},
		{12<<20 + 410<<10, true, "12.4M"},
		{^uint64(0), true, "16384.0P"},
	} {
		if got := string(AppendSize(nil, test.size, test.available)); got != test.want {
			t.Fatalf("AppendSize(%d, %v)=%q want %q", test.size, test.available, got, test.want)
		}
	}
}

func TestFilesystemFields(t *testing.T) {
	info := ramfs.FilesystemInfo{Type: ramfs.FilesystemFAT32, TotalBytes: 9 << 30, FreeBytes: 4 << 30, CapacityAvailable: true}
	if used, ok := Used(info); !ok || used != 5<<30 {
		t.Fatalf("used=%d ok=%v", used, ok)
	}
	if TypeName(info.Type) != "fat32" || Flag(info.ReadOnly) != "rw" || Flag(true) != "ro" {
		t.Fatal("filesystem labels")
	}
	info.CapacityAvailable = false
	if _, ok := Used(info); ok {
		t.Fatal("unavailable capacity reported as used")
	}
	info.CapacityAvailable, info.FreeBytes = true, info.TotalBytes+1
	if _, ok := Used(info); ok {
		t.Fatal("invalid capacity underflowed")
	}
}
