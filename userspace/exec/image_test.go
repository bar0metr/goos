package exec

import (
	"archive/tar"
	"bytes"
	"testing"
)

func systemImage(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	members := []struct {
		header  tar.Header
		payload string
	}{
		{tar.Header{Name: "system/", Mode: 0o755, Typeflag: tar.TypeDir}, ""},
		{tar.Header{Name: "system/vfs", Mode: 0o555, Typeflag: tar.TypeReg}, "VFS IMAGE"},
		{tar.Header{Name: "system/bin/", Mode: 0o755, Typeflag: tar.TypeDir}, ""},
		{tar.Header{Name: "system/bin/ls", Mode: 0o555, Typeflag: tar.TypeReg}, "LS IMAGE"},
		{tar.Header{Name: "system/bin/dir", Mode: 0o555, Typeflag: tar.TypeLink, Linkname: "system/bin/ls"}, ""},
	}
	for _, member := range members {
		header := member.header
		header.Size = int64(len(member.payload))
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(member.payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestLocateImageNamesTheBytesOfAnExecutable(t *testing.T) {
	image := systemImage(t)
	for path, want := range map[string]string{"/system/vfs": "VFS IMAGE", "/system/bin/ls": "LS IMAGE"} {
		offset, size, status := LocateImage(image, []byte(path))
		if status != ImageFound || string(image[offset:offset+size]) != want {
			t.Fatalf("%s = %d+%d, %d", path, offset, size, status)
		}
	}
	// A hard link names the same bytes, so the kernel files both launches
	// under one extent and shares their read-only pages.
	linkOffset, linkSize, _ := LocateImage(image, []byte("/system/bin/dir"))
	fileOffset, fileSize, _ := LocateImage(image, []byte("/system/bin/ls"))
	if linkOffset != fileOffset || linkSize != fileSize {
		t.Fatal("hard link resolved to other bytes")
	}
}

func TestLocateImageRefusesPathsOutsideItsPolicy(t *testing.T) {
	image := systemImage(t)
	for _, path := range []string{"", "/", "/system", "/system/", "system/vfs", "/persist/bin/ls", "/systemx/vfs", "/system//vfs", "/system/./vfs", "/system/bin/../vfs", "/system/vfs/"} {
		if _, _, status := LocateImage(image, []byte(path)); status != ImageOutsideSystem {
			t.Fatalf("%q = %d", path, status)
		}
	}
	for _, path := range []string{"/system/bin/missing", "/system/bin"} {
		if _, _, status := LocateImage(image, []byte(path)); status != ImageNotFound {
			t.Fatalf("%q = %d", path, status)
		}
	}
	image[148] ^= 1
	if _, _, status := LocateImage(image, []byte("/system/vfs")); status != ImageNotFound {
		t.Fatal("corrupt image resolved")
	}
}
