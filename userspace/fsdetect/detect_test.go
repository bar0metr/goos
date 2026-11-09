package fsdetect

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"

	"goos/internal/imagefixture"
	"goos/kernel/ramfs"
)

func image(t *testing.T, path string) []byte {
	t.Helper()
	compressed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fatImage unpacks a sparse FAT32 fixture of the fat32 package.
func fatImage(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "fat")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := imagefixture.Unpack(path, file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func probe(data []byte) Result { return Probe(data[:min(len(data), ProbeSize)], uint64(len(data))) }

func TestLinuxExt4Images(t *testing.T) {
	for _, name := range []string{"canonical", "legacy", "hash-tea", "blockmap-1k"} {
		data := image(t, "../ext4/testdata/"+name+".img.gz")
		result := probe(data)
		if result.Status != Recognized || result.Type != ramfs.FilesystemExt4 || result.IdentitySize != 16 {
			t.Fatalf("%s: %+v", name, result)
		}
		if string(result.Label[:result.LabelSize]) != name {
			t.Fatalf("%s: label %q", name, result.Label[:result.LabelSize])
		}
		driver, status, ok := Choose(&result, 0)
		if !ok || status != Recognized || driver.Executable != "/bootstrap/ext4" || driver.Writable {
			t.Fatalf("%s: driver %+v %v", name, driver, status)
		}
		if _, _, ok := Choose(&result, ramfs.FilesystemFAT32); ok {
			t.Fatalf("%s: forced to fat32", name)
		}
	}
}

func TestHostFAT32Images(t *testing.T) {
	for _, name := range []string{"host-512b-1x-2fat", "host-512b-8x-1fat"} {
		data := fatImage(t, "../fat32/testdata/"+name+".img.gz")
		result := probe(data)
		if result.Status != Recognized || result.Type != ramfs.FilesystemFAT32 || result.IdentitySize != 4 {
			t.Fatalf("%s: %+v", name, result)
		}
		// FAT32 is recognized but served only as the persistent volume.
		if _, status, ok := Choose(&result, 0); ok || status != Unknown {
			t.Fatalf("%s: FAT32 chosen for an additional volume", name)
		}
	}
}

func TestRefusals(t *testing.T) {
	fat := fatImage(t, "../fat32/testdata/host-512b-1x-2fat.img.gz")
	ext := image(t, "../ext4/testdata/canonical.img.gz")
	both := append([]byte(nil), fat...)
	copy(both[1024:2048], ext[1024:2048])
	// The ext4 block count must fit the FAT image for both to match.
	copy(both[1024+0x4:], []byte{16, 0, 0, 0})
	both[1024+0x60] &^= 0x80
	cases := []struct {
		name string
		data []byte
		want Status
	}{
		{"zeros", make([]byte, 1<<20), Unknown},
		{"tiny", make([]byte, 2048), TooSmall},
		{"both signatures", both, Ambiguous},
	}
	for _, tc := range cases {
		if result := probe(tc.data); result.Status != tc.want {
			t.Fatalf("%s: %v, want %v", tc.name, result.Status, tc.want)
		}
	}
	// An ext4 superblock whose block count exceeds the slice is not ext4.
	truncated := ext[:4<<20]
	if result := probe(truncated); result.Status != Unknown {
		t.Fatalf("truncated ext4: %v", result.Status)
	}
}

func TestSameVolume(t *testing.T) {
	first := probe(image(t, "../ext4/testdata/canonical.img.gz"))
	second := probe(image(t, "../ext4/testdata/legacy.img.gz"))
	again := first
	if !first.SameVolume(&again) {
		t.Fatal("a volume differs from itself")
	}
	second.Identity[0] ^= 1
	if first.SameVolume(&second) {
		t.Fatal("different identities match")
	}
	missing := first
	missing.IdentitySize = 0
	if missing.SameVolume(&missing) {
		t.Fatal("missing identity matched")
	}
	missing.IdentitySize = 16
	missing.Identity = [MaxIdentity]byte{}
	if missing.SameVolume(&missing) {
		t.Fatal("zero identity matched")
	}
	missing.IdentitySize = MaxIdentity + 1
	if missing.SameVolume(&missing) {
		t.Fatal("oversized identity matched")
	}
}

func TestTypeNames(t *testing.T) {
	for name, want := range map[string]ramfs.FilesystemType{"auto": 0, "ext4": ramfs.FilesystemExt4, "fat32": ramfs.FilesystemFAT32} {
		if got, ok := TypeNamed([]byte(name)); !ok || got != want {
			t.Fatalf("%s: %v %v", name, got, ok)
		}
	}
	if _, ok := TypeNamed([]byte("xfs")); ok {
		t.Fatal("xfs resolved")
	}
}

// FuzzProbe feeds arbitrary windows to the probe: it must never panic.
func FuzzProbe(f *testing.F) {
	f.Add(make([]byte, Window), uint64(1<<20))
	f.Fuzz(func(t *testing.T, data []byte, size uint64) {
		Probe(data, size)
	})
}
