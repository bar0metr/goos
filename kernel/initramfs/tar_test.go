package initramfs

import (
	"archive/tar"
	"bytes"
	"testing"
)

func archiveWith(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: "system/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestFindValidatedFile(t *testing.T) {
	archive := archiveWith(t, "system/init", []byte("native image"))
	data, ok := Find(archive, []byte("system/init"))
	if !ok || string(data) != "native image" {
		t.Fatalf("find = %q, %v", data, ok)
	}
	if _, ok := Find(archive, []byte("missing")); ok {
		t.Fatal("missing file found")
	}
}

func TestRejectsMalformedArchiveAndPath(t *testing.T) {
	archive := archiveWith(t, "system/init", []byte("image"))
	archive[148] ^= 1
	for _, path := range []string{"", "/system/init", "system//init", "../init", "system/../init"} {
		if _, ok := Find(archive, []byte(path)); ok {
			t.Fatalf("accepted path %q", path)
		}
	}
	if _, ok := Find(archive, []byte("system/init")); ok {
		t.Fatal("accepted corrupt checksum")
	}
}

func TestFindValidatedHardlink(t *testing.T) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	payload := []byte("coreutils")
	if err := writer.WriteHeader(&tar.Header{Name: "system/bin/coreutils", Mode: 0o555, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "system/bin/cat", Mode: 0o555, Typeflag: tar.TypeLink, Linkname: "system/bin/coreutils"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, ok := Find(output.Bytes(), []byte("system/bin/cat"))
	if !ok || string(data) != "coreutils" {
		t.Fatalf("hardlink = %q, %v", data, ok)
	}
}
