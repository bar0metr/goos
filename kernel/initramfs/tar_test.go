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
	data, ok := Find(archive, "system/init")
	if !ok || string(data) != "native image" {
		t.Fatalf("find = %q, %v", data, ok)
	}
	if _, ok := Find(archive, "missing"); ok {
		t.Fatal("missing file found")
	}
}

func TestRejectsMalformedArchiveAndPath(t *testing.T) {
	archive := archiveWith(t, "system/init", []byte("image"))
	archive[148] ^= 1
	for _, path := range []string{"", "/system/init", "system//init", "../init", "system/../init"} {
		if _, ok := Find(archive, path); ok {
			t.Fatalf("accepted path %q", path)
		}
	}
	if _, ok := Find(archive, "system/init"); ok {
		t.Fatal("accepted corrupt checksum")
	}
}
