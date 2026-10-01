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
	for path, want := range map[string]string{"/bootstrap/vfs": "VFS IMAGE", "/bootstrap/bin/ls": "LS IMAGE"} {
		offset, size, status := LocateImage(image, []byte(path))
		if status != ImageFound || string(image[offset:offset+size]) != want {
			t.Fatalf("%s = %d+%d, %d", path, offset, size, status)
		}
	}
	// A hard link names the same bytes, so the kernel files both launches
	// under one extent and shares their read-only pages.
	linkOffset, linkSize, _ := LocateImage(image, []byte("/bootstrap/bin/dir"))
	fileOffset, fileSize, _ := LocateImage(image, []byte("/bootstrap/bin/ls"))
	if linkOffset != fileOffset || linkSize != fileSize {
		t.Fatal("hard link resolved to other bytes")
	}
}

func TestLocateImageRefusesPathsOutsideItsPolicy(t *testing.T) {
	image := systemImage(t)
	for _, path := range []string{"", "/", "/bootstrap/", "bootstrap/vfs", "/bootstrap//vfs", "/bootstrap/./vfs", "/bootstrap/bin/../vfs", "/bootstrap/vfs/"} {
		if _, _, status := LocateImage(image, []byte(path)); status != ImageInvalidPath {
			t.Fatalf("%q = %d", path, status)
		}
	}
	// The installed system is a filesystem, never the archive: a path under
	// /system is not looked up there, whatever the archive holds.
	for _, path := range []string{"/system/vfs", "/system/bin/ls", "/persist/bin/ls", "/bootstrapx/vfs", "/home/demo/args", "/tmp/bootstrap/vfs"} {
		if _, _, status := LocateImage(image, []byte(path)); status != ImageOutsideArchive {
			t.Fatalf("%q = %d", path, status)
		}
	}
	for _, path := range []string{"/bootstrap/bin/missing", "/bootstrap/bin", "/bootstrap"} {
		if _, _, status := LocateImage(image, []byte(path)); status != ImageNotFound {
			t.Fatalf("%q = %d", path, status)
		}
	}
	image[148] ^= 1
	if _, _, status := LocateImage(image, []byte("/bootstrap/vfs")); status != ImageNotFound {
		t.Fatal("corrupt image resolved")
	}
}

func TestOriginIsExplicit(t *testing.T) {
	for path, want := range map[string]Origin{
		"/bootstrap/bin/ls": OriginArchive, "/bootstrap/shell": OriginArchive,
		"/system/bin/ls": OriginFile, "/system/services/shell": OriginFile, "/home/user/bin/ls": OriginFile,
		"/bootstrapx/ls": OriginFile, "/persist/bootstrap/bin/ls": OriginFile,
	} {
		if got := OriginOf([]byte(path)); got != want {
			t.Fatalf("%q = %d, want %d", path, got, want)
		}
	}
	if member, ok := ArchiveMember([]byte("/bootstrap/bin/ls")); !ok || string(member) != "system/bin/ls" {
		t.Fatalf("member = %q, %v", member, ok)
	}
	for _, path := range []string{"/bootstrap", "/bootstrap/", "/system/bin/ls"} {
		if _, ok := ArchiveMember([]byte(path)); ok {
			t.Fatalf("%q mapped to a member", path)
		}
	}
}

func TestInSystem(t *testing.T) {
	for path, want := range map[string]bool{"/system": true, "/system/vfs": true, "/system/bin/ls": true, "/systemx": false, "/persist/system": false, "/": false, "/bootstrap/bin/ls": false} {
		if got := InSystem([]byte(path)); got != want {
			t.Fatalf("%q = %v", path, got)
		}
	}
}
