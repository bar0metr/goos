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
	// Both names lead to the same bytes, so they share one key.
	_, linkKey, linkOK := Locate(output.Bytes(), []byte("system/bin/cat"))
	_, fileKey, fileOK := Locate(output.Bytes(), []byte("system/bin/coreutils"))
	if !linkOK || !fileOK || linkKey == 0 || linkKey != fileKey {
		t.Fatalf("keys = %d, %d", linkKey, fileKey)
	}
}

func TestLocateKeysDistinctMembers(t *testing.T) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, name := range []string{"system/a", "system/b"} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o555, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_, first, _ := Locate(output.Bytes(), []byte("system/a"))
	_, second, _ := Locate(output.Bytes(), []byte("system/b"))
	if first == 0 || second == 0 || first == second {
		t.Fatalf("keys = %d, %d", first, second)
	}
	if _, key, ok := Locate(output.Bytes(), []byte("system/missing")); ok || key != 0 {
		t.Fatal("missing member located")
	}
}
